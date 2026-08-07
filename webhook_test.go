package caisse

import (
	"bytes"
	"context"
	"encoding/json"
	stderrors "errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	stripe "github.com/stripe/stripe-go/v86"
)

const testWebhookSecret = "whsec_testsecret"

func webhookClient(t *testing.T) *Client {
	t.Helper()
	client, err := New(Config{
		SecretKey:     "sk_test_123",
		WebhookSecret: testWebhookSecret,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return client
}

func eventBody(t *testing.T, id, eventType string, object any) []byte {
	t.Helper()
	raw, err := json.Marshal(object)
	if err != nil {
		t.Fatalf("marshal event object: %v", err)
	}
	body, err := json.Marshal(map[string]any{
		"id":          id,
		"object":      "event",
		"api_version": stripe.APIVersion,
		"created":     time.Now().Unix(),
		"livemode":    false,
		"type":        eventType,
		"data":        map[string]any{"object": json.RawMessage(raw)},
	})
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	return body
}

func deliver(t *testing.T, handler http.Handler, body []byte, at time.Time) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/stripe/webhook", bytes.NewReader(body))
	request.Header.Set("Stripe-Signature", Sign(testWebhookSecret, body, at))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func paidSession() map[string]any {
	return map[string]any{
		"id":                  "cs_test_1",
		"object":              "checkout.session",
		"client_reference_id": "ORD-42",
		"payment_status":      "paid",
		"amount_total":        7000,
		"currency":            "eur",
		"mode":                "payment",
		"payment_intent":      map[string]any{"id": "pi_test_1", "object": "payment_intent"},
		"customer":            map[string]any{"id": "cus_test_1", "object": "customer"},
		"customer_details":    map[string]any{"email": "buyer@shop.test"},
		"metadata":            map[string]string{ReferenceKey: "ORD-42", "campaign": "spring"},
	}
}

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
	client := webhookClient(t)
	called := false
	handler, err := client.Webhook(Hooks{
		Store:  NewMemoryStore(),
		OnPaid: func(context.Context, Payment) error { called = true; return nil },
	})
	if err != nil {
		t.Fatalf("Webhook: %v", err)
	}
	body := eventBody(t, "evt_bad", "checkout.session.completed", paidSession())

	t.Run("forged signature", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
		request.Header.Set("Stripe-Signature", Sign("whsec_attacker", body, time.Now()))
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("status %d, want 400", recorder.Code)
		}
	})

	t.Run("no signature", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("status %d, want 400", recorder.Code)
		}
	})

	t.Run("replayed outside the tolerance", func(t *testing.T) {
		response := deliver(t, handler, body, time.Now().Add(-2*DefaultTolerance))
		if response.Code != http.StatusBadRequest {
			t.Errorf("status %d, want 400", response.Code)
		}
	})

	t.Run("body swapped under a valid signature", func(t *testing.T) {
		inflated := paidSession()
		inflated["amount_total"] = 1
		tampered := eventBody(t, "evt_bad", "checkout.session.completed", inflated)
		request := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(tampered))
		request.Header.Set("Stripe-Signature", Sign(testWebhookSecret, body, time.Now()))
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("status %d, want 400", recorder.Code)
		}
	})

	t.Run("oversized body", func(t *testing.T) {
		huge := bytes.Repeat([]byte("x"), int(MaxWebhookBytes)+1)
		request := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(huge))
		request.Header.Set("Stripe-Signature", Sign(testWebhookSecret, huge, time.Now()))
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("status %d, want 400", recorder.Code)
		}
	})

	t.Run("not a POST", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodGet, "/", nil)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusMethodNotAllowed {
			t.Errorf("status %d, want 405", recorder.Code)
		}
	})

	if called {
		t.Error("an unverified delivery reached the handler")
	}
}

// An unpaid session is Stripe saying "the customer submitted, the money has not
// moved". Fulfilling here is how a SEPA order ships before it is funded.
func TestWebhookIgnoresAnUnpaidSession(t *testing.T) {
	client := webhookClient(t)
	paid, failed := 0, 0
	handler, err := client.Webhook(Hooks{
		Store:    NewMemoryStore(),
		OnPaid:   func(context.Context, Payment) error { paid++; return nil },
		OnFailed: func(context.Context, Payment) error { failed++; return nil },
	})
	if err != nil {
		t.Fatalf("Webhook: %v", err)
	}

	session := paidSession()
	session["payment_status"] = "unpaid"
	if response := deliver(t, handler, eventBody(t, "evt_unpaid", "checkout.session.completed", session), time.Now()); response.Code != http.StatusOK {
		t.Fatalf("status %d", response.Code)
	}
	if paid != 0 || failed != 0 {
		t.Errorf("unpaid session fired OnPaid=%d OnFailed=%d, want 0 and 0", paid, failed)
	}

	session["payment_status"] = "paid"
	if response := deliver(t, handler, eventBody(t, "evt_async_ok", "checkout.session.async_payment_succeeded", session), time.Now()); response.Code != http.StatusOK {
		t.Fatalf("status %d", response.Code)
	}
	if paid != 1 {
		t.Errorf("async success fired OnPaid=%d, want 1", paid)
	}
}

func TestWebhookRoutesEveryEventItClaimsToHandle(t *testing.T) {
	client := webhookClient(t)
	seen := map[string]int{}
	var refund Refund
	var subscription Subscription
	var failure Payment

	handler, err := client.Webhook(Hooks{
		Store:      NewMemoryStore(),
		OnPaid:     func(context.Context, Payment) error { seen["paid"]++; return nil },
		OnExpired:  func(context.Context, Payment) error { seen["expired"]++; return nil },
		OnFailed:   func(_ context.Context, p Payment) error { seen["failed"]++; failure = p; return nil },
		OnRefunded: func(_ context.Context, r Refund) error { seen["refunded"]++; refund = r; return nil },
		OnSubscription: func(_ context.Context, s Subscription) error {
			seen["subscription"]++
			subscription = s
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Webhook: %v", err)
	}

	deliveries := []struct {
		id        string
		eventType string
		object    map[string]any
	}{
		{"evt_exp", "checkout.session.expired", paidSession()},
		{"evt_afail", "checkout.session.async_payment_failed", paidSession()},
		{"evt_pifail", "payment_intent.payment_failed", map[string]any{
			"id": "pi_test_2", "object": "payment_intent", "amount": 7000, "currency": "eur",
			"metadata":           map[string]string{ReferenceKey: "ORD-42"},
			"last_payment_error": map[string]any{"message": "Your card was declined.", "type": "card_error"},
		}},
		{"evt_refund", "charge.refunded", map[string]any{
			"id": "ch_test_1", "object": "charge", "amount": 7000, "amount_refunded": 2500,
			"currency": "eur", "status": "succeeded",
			"payment_intent": map[string]any{"id": "pi_test_1", "object": "payment_intent"},
			"metadata":       map[string]string{ReferenceKey: "ORD-42"},
		}},
		{"evt_sub", "customer.subscription.updated", map[string]any{
			"id": "sub_test_1", "object": "subscription", "status": "past_due",
			"cancel_at_period_end": true,
			"customer":             map[string]any{"id": "cus_test_1", "object": "customer"},
			"metadata":             map[string]string{ReferenceKey: "PLAN-9"},
		}},
		{"evt_unknown", "invoice.upcoming", map[string]any{"id": "in_1", "object": "invoice"}},
	}

	for _, delivery := range deliveries {
		response := deliver(t, handler, eventBody(t, delivery.id, delivery.eventType, delivery.object), time.Now())
		if response.Code != http.StatusOK {
			t.Fatalf("%s: status %d, body %q", delivery.eventType, response.Code, response.Body.String())
		}
	}

	want := map[string]int{"expired": 1, "failed": 2, "refunded": 1, "subscription": 1}
	for key, count := range want {
		if seen[key] != count {
			t.Errorf("%s fired %d times, want %d", key, seen[key], count)
		}
	}
	if seen["paid"] != 0 {
		t.Errorf("OnPaid fired %d times, want 0", seen["paid"])
	}

	if failure.FailureMessage != "Your card was declined." || failure.Reference != "ORD-42" {
		t.Errorf("failed payment = %+v", failure)
	}
	if refund.Amount != 2500 || refund.Reference != "ORD-42" || refund.PaymentIntentID != "pi_test_1" || refund.ChargeID != "ch_test_1" {
		t.Errorf("refund = %+v", refund)
	}
	if subscription.ID != "sub_test_1" || subscription.Status != "past_due" || subscription.Reference != "PLAN-9" ||
		subscription.CustomerID != "cus_test_1" || !subscription.CancelAtPeriodEnd {
		t.Errorf("subscription = %+v", subscription)
	}
}

func TestWebhookAcknowledgesEventsWithNoHandler(t *testing.T) {
	client := webhookClient(t)
	handler, err := client.Webhook(Hooks{Store: NewMemoryStore()})
	if err != nil {
		t.Fatalf("Webhook: %v", err)
	}
	response := deliver(t, handler, eventBody(t, "evt_nohandler", "checkout.session.completed", paidSession()), time.Now())
	if response.Code != http.StatusOK {
		t.Errorf("status %d, want 200", response.Code)
	}
}

type brokenStore struct{}

func (brokenStore) Begin(context.Context, string) (bool, error) {
	return false, stderrors.New("connection refused")
}
func (brokenStore) Done(context.Context, string) error { return nil }
func (brokenStore) Fail(context.Context, string) error { return nil }

// If the ledger is unreachable there is no way to know whether this event was
// already handled, so the only safe answer is 500 and let Stripe retry.
func TestWebhookRefusesToGuessWhenTheStoreIsDown(t *testing.T) {
	client := webhookClient(t)
	called := false
	handler, err := client.Webhook(Hooks{
		Store:  brokenStore{},
		OnPaid: func(context.Context, Payment) error { called = true; return nil },
	})
	if err != nil {
		t.Fatalf("Webhook: %v", err)
	}

	response := deliver(t, handler, eventBody(t, "evt_nostore", "checkout.session.completed", paidSession()), time.Now())
	if response.Code != http.StatusInternalServerError {
		t.Errorf("status %d, want 500", response.Code)
	}
	if called {
		t.Error("handler ran without a working dedupe ledger")
	}
}

// A mismatched API version must not cost a payment: verify, warn, carry on.
func TestWebhookProcessesAnEventFromAnotherAPIVersion(t *testing.T) {
	client := webhookClient(t)
	called := false
	handler, err := client.Webhook(Hooks{
		Store:  NewMemoryStore(),
		OnPaid: func(context.Context, Payment) error { called = true; return nil },
	})
	if err != nil {
		t.Fatalf("Webhook: %v", err)
	}

	body := eventBody(t, "evt_oldapi", "checkout.session.completed", paidSession())
	body = []byte(strings.Replace(string(body), stripe.APIVersion, "2019-02-19", 1))

	if response := deliver(t, handler, body, time.Now()); response.Code != http.StatusOK {
		t.Fatalf("status %d, body %q", response.Code, response.Body.String())
	}
	if !called {
		t.Error("an event from an older API version was dropped")
	}
}

func TestMemoryStoreClaimsOnce(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()

	first, err := store.Begin(ctx, "evt_1")
	if err != nil || !first {
		t.Fatalf("Begin = %v, %v; want true, nil", first, err)
	}
	second, err := store.Begin(ctx, "evt_1")
	if err != nil || second {
		t.Fatalf("second Begin = %v, %v; want false, nil", second, err)
	}
	if err := store.Fail(ctx, "evt_1"); err != nil {
		t.Fatalf("Fail: %v", err)
	}
	reclaimed, err := store.Begin(ctx, "evt_1")
	if err != nil || !reclaimed {
		t.Fatalf("Begin after Fail = %v, %v; want true, nil", reclaimed, err)
	}
	if err := store.Done(ctx, "evt_1"); err != nil {
		t.Fatalf("Done: %v", err)
	}
	if err := store.Fail(ctx, "evt_1"); err != nil {
		t.Fatalf("Fail after Done: %v", err)
	}
	afterDone, err := store.Begin(ctx, "evt_1")
	if err != nil || afterDone {
		t.Fatalf("Begin after Done = %v, %v; want false, nil", afterDone, err)
	}
}

// Stripe sends related objects as bare id strings unless you expand them, so
// the production payload looks nothing like an expanded fixture. This is the
// shape that actually arrives on the endpoint.
func TestWebhookReadsUnexpandedStripeIDs(t *testing.T) {
	client := webhookClient(t)
	var paid Payment
	var refunded Refund
	var subscription Subscription

	handler, err := client.Webhook(Hooks{
		Store:          NewMemoryStore(),
		OnPaid:         func(_ context.Context, p Payment) error { paid = p; return nil },
		OnRefunded:     func(_ context.Context, r Refund) error { refunded = r; return nil },
		OnSubscription: func(_ context.Context, s Subscription) error { subscription = s; return nil },
	})
	if err != nil {
		t.Fatalf("Webhook: %v", err)
	}

	session := map[string]any{
		"id": "cs_test_1", "object": "checkout.session", "client_reference_id": "ORD-42",
		"payment_status": "paid", "amount_total": 7000, "currency": "eur", "mode": "payment",
		"payment_intent": "pi_test_1",
		"customer":       "cus_test_1",
		"subscription":   "sub_test_1",
	}
	if response := deliver(t, handler, eventBody(t, "evt_flat_paid", "checkout.session.completed", session), time.Now()); response.Code != http.StatusOK {
		t.Fatalf("status %d, body %q", response.Code, response.Body.String())
	}
	if paid.PaymentIntentID != "pi_test_1" || paid.CustomerID != "cus_test_1" || paid.SubscriptionID != "sub_test_1" {
		t.Errorf("unexpanded ids lost: %+v", paid)
	}

	charge := map[string]any{
		"id": "ch_test_1", "object": "charge", "amount_refunded": 2500, "currency": "eur",
		"payment_intent": "pi_test_1",
		"metadata":       map[string]string{ReferenceKey: "ORD-42"},
	}
	if response := deliver(t, handler, eventBody(t, "evt_flat_refund", "charge.refunded", charge), time.Now()); response.Code != http.StatusOK {
		t.Fatalf("status %d", response.Code)
	}
	if refunded.PaymentIntentID != "pi_test_1" {
		t.Errorf("unexpanded payment intent lost on refund: %+v", refunded)
	}

	subscriptionObject := map[string]any{
		"id": "sub_test_1", "object": "subscription", "status": "active",
		"customer": "cus_test_1",
	}
	if response := deliver(t, handler, eventBody(t, "evt_flat_sub", "customer.subscription.created", subscriptionObject), time.Now()); response.Code != http.StatusOK {
		t.Fatalf("status %d", response.Code)
	}
	if subscription.CustomerID != "cus_test_1" {
		t.Errorf("unexpanded customer lost on subscription: %+v", subscription)
	}
}
