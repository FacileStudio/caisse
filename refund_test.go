package caisse

import (
	"context"
	stderrors "errors"
	"net/http"
	"testing"

	"github.com/FacileStudio/tronc/errors"
	stripe "github.com/stripe/stripe-go/v86"
)

func TestRefundSendsWhatStripeExpects(t *testing.T) {
	client, calls := fakeStripe(t, http.StatusOK,
		`{"id":"re_1","object":"refund","amount":2500,"currency":"eur","status":"succeeded","charge":{"id":"ch_1","object":"charge"}}`)

	refund, err := client.Refund(context.Background(), RefundRequest{
		PaymentIntentID: "pi_1",
		Amount:          2500,
		Reason:          ReasonRequested,
	})
	if err != nil {
		t.Fatalf("Refund: %v", err)
	}
	if refund.ID != "re_1" || refund.Amount != 2500 || refund.ChargeID != "ch_1" || refund.Status != "succeeded" {
		t.Errorf("refund = %+v", refund)
	}

	call := (*calls)[0]
	if call.path != "/v1/refunds" {
		t.Errorf("posted to %s", call.path)
	}
	for key, want := range map[string]string{
		"payment_intent": "pi_1",
		"amount":         "2500",
		"reason":         ReasonRequested,
	} {
		if got := call.form.Get(key); got != want {
			t.Errorf("form[%s] = %q, want %q", key, got, want)
		}
	}
	if call.header.Get("Idempotency-Key") == "" {
		t.Error("a refund went out with no Idempotency-Key")
	}
}

// A zero amount means "everything still refundable" — sending amount=0 to Stripe
// would instead refund nothing.
func TestRefundOmitsAZeroAmount(t *testing.T) {
	client, calls := fakeStripe(t, http.StatusOK, `{"id":"re_1","object":"refund"}`)

	if _, err := client.Refund(context.Background(), RefundRequest{PaymentIntentID: "pi_1"}); err != nil {
		t.Fatalf("Refund: %v", err)
	}
	if (*calls)[0].form.Has("amount") {
		t.Error("a full refund sent an explicit amount")
	}
}

func TestRefundRejectsBadRequests(t *testing.T) {
	client, calls := fakeStripe(t, http.StatusOK, `{"id":"re_1"}`)

	cases := map[string]RefundRequest{
		"no payment intent": {},
		"a session id":      {PaymentIntentID: "cs_1"},
		"a charge id":       {PaymentIntentID: "ch_1"},
		"negative amount":   {PaymentIntentID: "pi_1", Amount: -1},
		"invented reason":   {PaymentIntentID: "pi_1", Reason: "because"},
		"reserved metadata": {PaymentIntentID: "pi_1", Metadata: map[string]string{ReferenceKey: "x"}},
	}
	for name, request := range cases {
		if _, err := client.Refund(context.Background(), request); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if len(*calls) != 0 {
		t.Errorf("a rejected refund still reached Stripe %d times", len(*calls))
	}
}

func TestRefundIdempotencyKeyTracksTheRequest(t *testing.T) {
	full := RefundRequest{PaymentIntentID: "pi_1"}
	partial := RefundRequest{PaymentIntentID: "pi_1", Amount: 100}

	if full.idempotencyKey() != (RefundRequest{PaymentIntentID: "pi_1"}).idempotencyKey() {
		t.Error("identical refunds produced different keys")
	}
	if full.idempotencyKey() == partial.idempotencyKey() {
		t.Error("a partial refund collided with the full one")
	}
}

func TestPortalURLRejectsBadInput(t *testing.T) {
	client, _ := fakeStripe(t, http.StatusOK, `{"id":"bps_1","url":"https://billing.stripe.com/p/1"}`)

	if _, err := client.PortalURL(context.Background(), "user_1", "https://app.test/billing"); err == nil {
		t.Error("accepted a customer id that is not a cus_")
	}
	if _, err := client.PortalURL(context.Background(), "cus_1", "/billing"); err == nil {
		t.Error("accepted a relative return URL")
	}

	url, err := client.PortalURL(context.Background(), "cus_1", "https://app.test/billing")
	if err != nil {
		t.Fatalf("PortalURL: %v", err)
	}
	if url != "https://billing.stripe.com/p/1" {
		t.Errorf("url = %q", url)
	}
}

// Card declines are written for the cardholder and go out as-is. Everything
// else can name internal identifiers and must not reach a client response.
func TestWrapKeepsStripeInternalsOutOfClientResponses(t *testing.T) {
	cases := map[string]struct {
		err         error
		wantCode    string
		wantMessage string
	}{
		"card declined": {
			&stripe.Error{Type: stripe.ErrorTypeCard, Msg: "Your card was declined.", HTTPStatusCode: 402},
			"invalid_argument", "Your card was declined.",
		},
		"bad parameter": {
			&stripe.Error{Type: stripe.ErrorTypeInvalidRequest, Msg: "No such price: 'price_secret'", HTTPStatusCode: 400},
			"internal", "payment request rejected",
		},
		"bad api key": {
			&stripe.Error{Type: stripe.ErrorTypeInvalidRequest, Msg: "Invalid API Key provided: sk_live_****", HTTPStatusCode: 401},
			"internal", "payment request rejected",
		},
		"rate limited": {
			&stripe.Error{Type: stripe.ErrorTypeInvalidRequest, Msg: "Too many requests", HTTPStatusCode: 429},
			"rate_limited", "payment provider is rate limiting",
		},
		"stripe is down": {
			&stripe.Error{Type: stripe.ErrorTypeAPI, Msg: "internal", HTTPStatusCode: 503},
			"unavailable", "payment provider unavailable",
		},
		"network failure": {
			stderrors.New("dial tcp: i/o timeout"),
			"unavailable", "payment provider unreachable",
		},
	}

	for name, testCase := range cases {
		wrapped := wrap("open checkout", testCase.err)
		var appErr *errors.Error
		if !stderrors.As(wrapped, &appErr) {
			t.Errorf("%s: %v is not a suite error", name, wrapped)
			continue
		}
		if appErr.Code != testCase.wantCode || appErr.Message != testCase.wantMessage {
			t.Errorf("%s: got %s/%q, want %s/%q", name, appErr.Code, appErr.Message, testCase.wantCode, testCase.wantMessage)
		}
		if !stderrors.Is(wrapped, testCase.err) {
			t.Errorf("%s: the cause was lost, so nothing reaches the logs", name)
		}
	}

	if wrap("noop", nil) != nil {
		t.Error("wrap invented an error out of nil")
	}
}
