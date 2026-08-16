package caisse

import (
	"bytes"
	"context"
	stderrors "errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestWebhookNeedsASecretAndAStore(t *testing.T) {
	noSecret, err := New(Config{SecretKey: "sk_test_123"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := noSecret.Webhook(Hooks{Store: NewMemoryStore()}); err == nil {
		t.Error("built a webhook handler with no signing secret")
	}
	if _, err := webhookClient(t).Webhook(Hooks{}); err == nil {
		t.Error("built a webhook handler with no event store")
	}
}

func TestWebhookDeliversAPaidSession(t *testing.T) {
	client := webhookClient(t)
	var got Payment
	calls := 0

	handler, err := client.Webhook(Hooks{
		Store:  NewMemoryStore(),
		OnPaid: func(_ context.Context, payment Payment) error { calls++; got = payment; return nil },
	})
	if err != nil {
		t.Fatalf("Webhook: %v", err)
	}

	response := deliver(t, handler, eventBody(t, "evt_1", "checkout.session.completed", paidSession()), time.Now())
	if response.Code != http.StatusOK {
		t.Fatalf("status %d, body %q", response.Code, response.Body.String())
	}
	if calls != 1 {
		t.Fatalf("OnPaid called %d times, want 1", calls)
	}

	want := Payment{
		EventID:         "evt_1",
		Reference:       "ORD-42",
		SessionID:       "cs_test_1",
		PaymentIntentID: "pi_test_1",
		CustomerID:      "cus_test_1",
		CustomerEmail:   "buyer@shop.test",
		Amount:          7000,
		Currency:        "eur",
	}
	assertPaidPayment(t, got, want)
}

// assertPaidPayment checks every field a paid session must populate, plus the
// metadata and timestamp caisse is meant to carry through.
func assertPaidPayment(t *testing.T, got, want Payment) {
	t.Helper()
	if got.EventID != want.EventID || got.Reference != want.Reference || got.SessionID != want.SessionID ||
		got.PaymentIntentID != want.PaymentIntentID || got.CustomerID != want.CustomerID ||
		got.CustomerEmail != want.CustomerEmail || got.Amount != want.Amount || got.Currency != want.Currency {
		t.Errorf("payment = %+v, want %+v", got, want)
	}
	if got.Metadata["campaign"] != "spring" {
		t.Errorf("metadata not carried through: %v", got.Metadata)
	}
	if got.OccurredAt.IsZero() {
		t.Error("OccurredAt is zero")
	}
}

// The whole point of the store: Stripe redelivers, the handler must not run twice.
func TestWebhookRunsAHandlerOncePerEvent(t *testing.T) {
	client := webhookClient(t)
	calls := 0
	handler, err := client.Webhook(Hooks{
		Store:  NewMemoryStore(),
		OnPaid: func(context.Context, Payment) error { calls++; return nil },
	})
	if err != nil {
		t.Fatalf("Webhook: %v", err)
	}

	body := eventBody(t, "evt_dup", "checkout.session.completed", paidSession())
	for range 3 {
		if response := deliver(t, handler, body, time.Now()); response.Code != http.StatusOK {
			t.Fatalf("status %d", response.Code)
		}
	}
	if calls != 1 {
		t.Errorf("handler ran %d times across 3 deliveries, want 1", calls)
	}
}

// A handler that failed must leave the event claimable, or Stripe's retry finds
// it taken and the order is silently never fulfilled.
func TestWebhookReleasesTheClaimWhenAHandlerFails(t *testing.T) {
	client := webhookClient(t)
	attempts := 0
	handler, err := client.Webhook(Hooks{
		Store: NewMemoryStore(),
		OnPaid: func(context.Context, Payment) error {
			attempts++
			if attempts == 1 {
				return stderrors.New("database is down")
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Webhook: %v", err)
	}

	body := eventBody(t, "evt_retry", "checkout.session.completed", paidSession())

	if response := deliver(t, handler, body, time.Now()); response.Code != http.StatusInternalServerError {
		t.Fatalf("first delivery: status %d, want 500 so Stripe retries", response.Code)
	}
	if response := deliver(t, handler, body, time.Now()); response.Code != http.StatusOK {
		t.Fatalf("retry: status %d, want 200", response.Code)
	}
	if attempts != 2 {
		t.Errorf("handler ran %d times, want 2 (one failure, one successful retry)", attempts)
	}
}

func TestWebhookSurvivesAPanickingHandler(t *testing.T) {
	client := webhookClient(t)
	attempts := 0
	handler, err := client.Webhook(Hooks{
		Store: NewMemoryStore(),
		OnPaid: func(context.Context, Payment) error {
			attempts++
			if attempts == 1 {
				panic("nil map write")
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Webhook: %v", err)
	}

	body := eventBody(t, "evt_panic", "checkout.session.completed", paidSession())
	if response := deliver(t, handler, body, time.Now()); response.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500", response.Code)
	}
	if response := deliver(t, handler, body, time.Now()); response.Code != http.StatusOK {
		t.Fatalf("retry after panic: status %d, want 200", response.Code)
	}
	if attempts != 2 {
		t.Errorf("handler ran %d times, want 2", attempts)
	}
}

func TestWebhookRejectsWhatItCannotVerify(t *testing.T) {
	handler, called := verifyHandler(t)
	body := eventBody(t, "evt_bad", "checkout.session.completed", paidSession())

	tampered := paidSession()
	tampered["amount_total"] = 1
	tamperedBody := eventBody(t, "evt_bad", "checkout.session.completed", tampered)
	huge := bytes.Repeat([]byte("x"), int(MaxWebhookBytes)+1)

	cases := []struct {
		name    string
		request *http.Request
		want    int
	}{
		{"forged signature", webhookRequest(t, http.MethodPost, body, Sign("whsec_attacker", body, time.Now())), http.StatusBadRequest},
		{"no signature", webhookRequest(t, http.MethodPost, body, ""), http.StatusBadRequest},
		{"replayed outside the tolerance", webhookRequest(t, http.MethodPost, body, Sign(testWebhookSecret, body, time.Now().Add(-2*DefaultTolerance))), http.StatusBadRequest},
		{"body swapped under a valid signature", webhookRequest(t, http.MethodPost, tamperedBody, Sign(testWebhookSecret, body, time.Now())), http.StatusBadRequest},
		{"oversized body", webhookRequest(t, http.MethodPost, huge, Sign(testWebhookSecret, huge, time.Now())), http.StatusBadRequest},
		{"not a POST", webhookRequest(t, http.MethodGet, nil, ""), http.StatusMethodNotAllowed},
	}

	for _, tc := range cases {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, tc.request)
		if recorder.Code != tc.want {
			t.Errorf("%s: status %d, want %d", tc.name, recorder.Code, tc.want)
		}
	}

	if *called {
		t.Error("an unverified delivery reached the handler")
	}
}

// verifyHandler builds a webhook handler that records whether a handler ran.
func verifyHandler(t *testing.T) (http.Handler, *bool) {
	t.Helper()
	called := false
	handler, err := webhookClient(t).Webhook(Hooks{
		Store:  NewMemoryStore(),
		OnPaid: func(context.Context, Payment) error { called = true; return nil },
	})
	if err != nil {
		t.Fatalf("Webhook: %v", err)
	}
	return handler, &called
}

func webhookRequest(t *testing.T, method string, body []byte, signature string) *http.Request {
	t.Helper()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request := httptest.NewRequest(method, "/", reader)
	if signature != "" {
		request.Header.Set("Stripe-Signature", signature)
	}
	return request
}

// An unpaid session is Stripe saying "the customer submitted, the money has not
// moved". Fulfilling here is how a SEPA order ships before it is funded.
