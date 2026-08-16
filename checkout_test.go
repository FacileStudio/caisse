package caisse

import (
	"context"
	stderrors "errors"
	"net/http"
	"strings"
	"testing"

	"github.com/FacileStudio/tronc/errors"
)

// checkoutWireForm is the exact form encoding a valid checkout must produce.
var checkoutWireForm = map[string]string{
	"client_reference_id":                    "ORD-1",
	"mode":                                   "payment",
	"success_url":                            "https://shop.test/ok",
	"cancel_url":                             "https://shop.test/ko",
	"customer_email":                         "buyer@shop.test",
	"line_items[0][price_data][currency]":    "eur",
	"line_items[0][price_data][unit_amount]": "3500",
	"line_items[0][price_data][product_data][name]": "T-shirt",
	"line_items[0][quantity]":                       "2",
	"metadata[" + ReferenceKey + "]":                "ORD-1",
	"metadata[campaign]":                            "spring",
}

func TestCheckoutSendsWhatStripeExpects(t *testing.T) {
	client, calls := fakeStripe(t, http.StatusOK,
		`{"id":"cs_test_1","object":"checkout.session","url":"https://checkout.stripe.com/c/1","expires_at":1754000000,"livemode":false}`)

	request := validCheckout()
	request.Metadata = map[string]string{"campaign": "spring"}
	request.CustomerEmail = "buyer@shop.test"

	session, err := client.Checkout(context.Background(), request)
	if err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	if session.ID != "cs_test_1" || session.URL != "https://checkout.stripe.com/c/1" {
		t.Fatalf("unexpected session %+v", session)
	}
	if len(*calls) != 1 {
		t.Fatalf("made %d calls, want 1", len(*calls))
	}

	call := (*calls)[0]
	if call.path != "/v1/checkout/sessions" {
		t.Errorf("posted to %s", call.path)
	}
	assertForm(t, call, checkoutWireForm)

	if call.header.Get("Idempotency-Key") == "" {
		t.Error("no Idempotency-Key header")
	}
}

// The reference has to reach the payment intent, or charge.refunded — which
// carries no client_reference_id — can never be tied back to an order.
func TestCheckoutCopiesTheReferenceOntoThePaymentIntent(t *testing.T) {
	client, calls := fakeStripe(t, http.StatusOK, `{"id":"cs_1","object":"checkout.session"}`)

	if _, err := client.Checkout(context.Background(), validCheckout()); err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	if got := (*calls)[0].form.Get("payment_intent_data[metadata][" + ReferenceKey + "]"); got != "ORD-1" {
		t.Errorf("payment intent metadata reference = %q, want ORD-1", got)
	}
}

func TestCheckoutSubscriptionCopiesTheReferenceOntoTheSubscription(t *testing.T) {
	client, calls := fakeStripe(t, http.StatusOK, `{"id":"cs_1","object":"checkout.session"}`)

	request := validCheckout()
	request.Mode = ModeSubscription
	request.Lines = []Line{{PriceID: "price_123", Quantity: 1}}

	if _, err := client.Checkout(context.Background(), request); err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	call := (*calls)[0]
	if got := call.form.Get("subscription_data[metadata][" + ReferenceKey + "]"); got != "ORD-1" {
		t.Errorf("subscription metadata reference = %q, want ORD-1", got)
	}
	if got := call.form.Get("line_items[0][price]"); got != "price_123" {
		t.Errorf("line price = %q, want price_123", got)
	}
	if call.form.Has("payment_intent_data[metadata][" + ReferenceKey + "]") {
		t.Error("subscription mode sent payment_intent_data, which Stripe rejects")
	}
}

var badCheckouts = map[string]func(*CheckoutRequest){
	"no reference":          func(r *CheckoutRequest) { r.Reference = "" },
	"no lines":              func(r *CheckoutRequest) { r.Lines = nil },
	"no label":              func(r *CheckoutRequest) { r.Lines[0].Label = "" },
	"zero amount":           func(r *CheckoutRequest) { r.Lines[0].Amount = 0 },
	"negative amount":       func(r *CheckoutRequest) { r.Lines[0].Amount = -1 },
	"negative quantity":     func(r *CheckoutRequest) { r.Lines[0].Quantity = -1 },
	"price and amount":      func(r *CheckoutRequest) { r.Lines[0].PriceID = "price_1" },
	"no currency":           func(r *CheckoutRequest) { r.Currency = "" },
	"short currency":        func(r *CheckoutRequest) { r.Currency = "eu" },
	"relative success url":  func(r *CheckoutRequest) { r.SuccessURL = "/ok" },
	"schemeless cancel url": func(r *CheckoutRequest) { r.CancelURL = "shop.test/ko" },
	"ftp success url":       func(r *CheckoutRequest) { r.SuccessURL = "ftp://shop.test/ok" },
	"no success url":        func(r *CheckoutRequest) { r.SuccessURL = "" },
	"unknown mode":          func(r *CheckoutRequest) { r.Mode = "donation" },
	"customer id and email": func(r *CheckoutRequest) { r.CustomerID = "cus_1"; r.CustomerEmail = "a@b.test" },
	"long reference":        func(r *CheckoutRequest) { r.Reference = strings.Repeat("x", maxReference+1) },
	"reserved metadata":     func(r *CheckoutRequest) { r.Metadata = map[string]string{ReferenceKey: "x"} },
	"long metadata key":     func(r *CheckoutRequest) { r.Metadata = map[string]string{strings.Repeat("k", 41): "v"} },
	"long metadata value":   func(r *CheckoutRequest) { r.Metadata = map[string]string{"k": strings.Repeat("v", 501)} },
	"subscription ad hoc":   func(r *CheckoutRequest) { r.Mode = ModeSubscription },
}

func TestCheckoutRejectsBadRequests(t *testing.T) {
	client, calls := fakeStripe(t, http.StatusOK, `{"id":"cs_1"}`)
	for name, mutate := range badCheckouts {
		request := validCheckout()
		mutate(&request)
		_, err := client.Checkout(context.Background(), request)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		var appErr *errors.Error
		if !stderrors.As(err, &appErr) || appErr.Code != "invalid_argument" {
			t.Errorf("%s: got %v, want an invalid_argument error", name, err)
		}
	}
	if len(*calls) != 0 {
		t.Errorf("a rejected request still reached Stripe %d times", len(*calls))
	}
}

// A retry of the same request must reuse Stripe's session rather than open a
// second one, and a different request must never collide with it.
func TestIdempotencyKeyTracksTheRequest(t *testing.T) {
	base := validCheckout()
	same := validCheckout()
	if base.idempotencyKey() != same.idempotencyKey() {
		t.Error("identical requests produced different keys")
	}

	changed := validCheckout()
	changed.Lines[0].Amount = 3600
	if base.idempotencyKey() == changed.idempotencyKey() {
		t.Error("a different amount produced the same key")
	}
	if !strings.HasPrefix(base.idempotencyKey(), "caisse_") {
		t.Errorf("key %q is not namespaced", base.idempotencyKey())
	}
}

func TestCheckoutQuantityDefaultsToOne(t *testing.T) {
	client, calls := fakeStripe(t, http.StatusOK, `{"id":"cs_1"}`)
	request := validCheckout()
	request.Lines[0].Quantity = 0

	if _, err := client.Checkout(context.Background(), request); err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	if got := (*calls)[0].form.Get("line_items[0][quantity]"); got != "1" {
		t.Errorf("quantity = %q, want 1", got)
	}
}

func TestNewValidatesKeys(t *testing.T) {
	cases := map[string]Config{
		"empty":             {SecretKey: ""},
		"publishable key":   {SecretKey: "pk_test_123"},
		"nonsense key":      {SecretKey: "hunter2"},
		"bad hook secret":   {SecretKey: "sk_test_123", WebhookSecret: "not-a-secret"},
		"session id as key": {SecretKey: "cs_test_123"},
	}
	for name, cfg := range cases {
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	client, err := New(Config{SecretKey: "sk_live_123", WebhookSecret: "whsec_abc"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !client.Live() {
		t.Error("a sk_live_ key did not report Live()")
	}
}

// Two requests that produce the same Stripe session must hash the same, or a
// retry that spells the defaults differently opens a second checkout.
func TestIdempotencyKeyIgnoresEquivalentSpellings(t *testing.T) {
	explicit := validCheckout()
	explicit.Mode = ModePayment
	explicit.Currency = "eur"
	explicit.Lines[0].Quantity = 1

	implicit := validCheckout()
	implicit.Mode = ""
	implicit.Currency = "EUR"
	implicit.Lines[0].Quantity = 0

	if explicit.idempotencyKey() != implicit.idempotencyKey() {
		t.Error("the same effective request hashed to two keys, so a retry would open a second session")
	}
}

// assertForm checks that the form Stripe received carries every expected key
// with its exact value.
func assertForm(t *testing.T, call stripeCall, want map[string]string) {
	t.Helper()
	for key, value := range want {
		if got := call.form.Get(key); got != value {
			t.Errorf("form[%s] = %q, want %q", key, got, value)
		}
	}
}
