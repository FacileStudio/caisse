package caisse

import (
	"context"
	stderrors "errors"
	"net/http"
	"strings"
	"testing"
	"time"

	stripe "github.com/stripe/stripe-go/v86"
)

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

// routedEvents exercises every event type the webhook claims to route, with the
// minimal payload each handler decodes from.
var routedEvents = []struct {
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

	for _, delivery := range routedEvents {
		deliverOK(t, handler, delivery.id, delivery.eventType, delivery.object)
	}

	assertRouted(t, seen, failure, refund, subscription)
}

func assertRouted(t *testing.T, seen map[string]int, failure Payment, refund Refund, subscription Subscription) {
	t.Helper()
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
