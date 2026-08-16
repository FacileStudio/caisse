package caisse

import (
	"context"
	"net/http"
	"testing"
	"time"
)

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
// Stripe sends related objects as bare id strings unless you expand them, so
// the production payload looks nothing like an expanded fixture. These are the
// shapes that actually arrive on the endpoint.
var unexpandedSession = map[string]any{
	"id": "cs_test_1", "object": "checkout.session", "client_reference_id": "ORD-42",
	"payment_status": "paid", "amount_total": 7000, "currency": "eur", "mode": "payment",
	"payment_intent": "pi_test_1",
	"customer":       "cus_test_1",
	"subscription":   "sub_test_1",
}

var unexpandedCharge = map[string]any{
	"id": "ch_test_1", "object": "charge", "amount_refunded": 2500, "currency": "eur",
	"payment_intent": "pi_test_1",
	"metadata":       map[string]string{ReferenceKey: "ORD-42"},
}

var unexpandedSubscription = map[string]any{
	"id": "sub_test_1", "object": "subscription", "status": "active",
	"customer": "cus_test_1",
}

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

	deliverOK(t, handler, "evt_flat_paid", "checkout.session.completed", unexpandedSession)
	if paid.PaymentIntentID != "pi_test_1" || paid.CustomerID != "cus_test_1" || paid.SubscriptionID != "sub_test_1" {
		t.Errorf("unexpanded ids lost: %+v", paid)
	}

	deliverOK(t, handler, "evt_flat_refund", "charge.refunded", unexpandedCharge)
	if refunded.PaymentIntentID != "pi_test_1" {
		t.Errorf("unexpanded payment intent lost on refund: %+v", refunded)
	}

	deliverOK(t, handler, "evt_flat_sub", "customer.subscription.created", unexpandedSubscription)
	if subscription.CustomerID != "cus_test_1" {
		t.Errorf("unexpanded customer lost on subscription: %+v", subscription)
	}
}

func deliverOK(t *testing.T, handler http.Handler, id, eventType string, object map[string]any) {
	t.Helper()
	response := deliver(t, handler, eventBody(t, id, eventType, object), time.Now())
	if response.Code != http.StatusOK {
		t.Fatalf("%s: status %d, body %q", eventType, response.Code, response.Body.String())
	}
}
