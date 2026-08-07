package caisse

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"github.com/FacileStudio/tronc/errors"
	stripe "github.com/stripe/stripe-go/v86"
)

// Refund reasons Stripe accepts. Anything else is rejected before the call.
const (
	ReasonDuplicate  = "duplicate"
	ReasonFraudulent = "fraudulent"
	ReasonRequested  = "requested_by_customer"
)

// RefundRequest gives money back.
type RefundRequest struct {
	// PaymentIntentID is the intent to refund, pi_…. Take it from
	// [Payment.PaymentIntentID].
	PaymentIntentID string `json:"payment_intent_id"`

	// Amount is how much to give back, in the currency's smallest unit. Zero
	// refunds everything still refundable.
	Amount int64 `json:"amount,omitempty"`

	// Reason is one of [ReasonDuplicate], [ReasonFraudulent] or
	// [ReasonRequested]. Empty is allowed.
	Reason string `json:"reason,omitempty"`

	// Metadata is attached to the refund.
	Metadata map[string]string `json:"metadata,omitempty"`
}

// Refund is money given back — either the result of [Client.Refund], or a
// refund reported by Stripe through [Hooks.OnRefunded].
//
// EventID, OccurredAt and Livemode are only set on the webhook path. ID is only
// set on the API path: a charge.refunded event describes the charge, not the
// individual refund.
type Refund struct {
	EventID         string
	ID              string
	ChargeID        string
	PaymentIntentID string
	Reference       string
	Amount          int64
	Currency        string
	Status          string
	Metadata        map[string]string
	OccurredAt      time.Time
	Livemode        bool
}

// Refund gives money back for a payment.
//
// The call carries an idempotency key derived from the request, so a retry
// after a timeout cannot refund twice — the single most expensive mistake this
// package exists to prevent.
func (c *Client) Refund(ctx context.Context, request RefundRequest) (Refund, error) {
	if err := request.validate(); err != nil {
		return Refund{}, err
	}

	params := &stripe.RefundCreateParams{
		PaymentIntent: stripe.String(request.PaymentIntentID),
		Metadata:      request.Metadata,
	}
	params.Context = ctx
	params.IdempotencyKey = stripe.String(request.idempotencyKey())
	if request.Amount > 0 {
		params.Amount = stripe.Int64(request.Amount)
	}
	if request.Reason != "" {
		params.Reason = stripe.String(request.Reason)
	}

	refund, err := c.api.V1Refunds.Create(ctx, params)
	if err != nil {
		return Refund{}, wrap("refund", err)
	}

	result := Refund{
		ID:              refund.ID,
		PaymentIntentID: request.PaymentIntentID,
		Amount:          refund.Amount,
		Currency:        string(refund.Currency),
		Status:          string(refund.Status),
		Metadata:        refund.Metadata,
		Reference:       refund.Metadata[ReferenceKey],
	}
	if refund.Charge != nil {
		result.ChargeID = refund.Charge.ID
	}
	return result, nil
}

func (r RefundRequest) idempotencyKey() string {
	encoded, err := json.Marshal(r)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(encoded)
	return "caisse_refund_" + hex.EncodeToString(sum[:])[:32]
}

func (r RefundRequest) validate() error {
	switch {
	case !strings.HasPrefix(r.PaymentIntentID, "pi_"):
		return errors.Invalid("caisse: PaymentIntentID must be a Stripe payment intent (pi_…)")
	case r.Amount < 0:
		return errors.Invalid("caisse: Amount cannot be negative")
	}
	switch r.Reason {
	case "", ReasonDuplicate, ReasonFraudulent, ReasonRequested:
	default:
		return errors.Invalid("caisse: Reason must be empty, " + ReasonDuplicate + ", " + ReasonFraudulent + " or " + ReasonRequested)
	}
	return validateMetadata(r.Metadata)
}
