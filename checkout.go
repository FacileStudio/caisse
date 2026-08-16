package caisse

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	stripe "github.com/stripe/stripe-go/v86"
)

// Mode selects what a checkout collects.
type Mode string

const (
	// ModePayment takes a single payment. Lines may price themselves.
	ModePayment Mode = "payment"

	// ModeSubscription starts a recurring subscription. Every line must name a
	// recurring PriceID; Stripe has nowhere to put an ad-hoc recurring amount.
	ModeSubscription Mode = "subscription"
)

// Stripe's own limits, enforced here so a bad request fails locally with a
// readable message instead of as a 400 from an API call you paid for.
const (
	maxLines           = 100
	maxMetadataEntries = 50
	maxMetadataKey     = 40
	maxMetadataValue   = 500
	maxReference       = 200
)

// Line is one row of the checkout.
//
// Set either PriceID, for a price already defined in Stripe, or Label and
// Amount for an ad-hoc one. Setting both is an error rather than a silent
// precedence rule, because the two disagree about what the customer pays.
type Line struct {
	// PriceID names an existing Stripe price, price_….
	PriceID string `json:"price_id,omitempty"`

	// Label is the product name shown on the checkout page.
	Label string `json:"label,omitempty"`

	// Description is an optional second line under Label.
	Description string `json:"description,omitempty"`

	// Amount is the unit price in the currency's smallest unit.
	Amount int64 `json:"amount,omitempty"`

	// ImageURL is an optional absolute https image for the line.
	ImageURL string `json:"image_url,omitempty"`

	// Quantity defaults to 1.
	Quantity int64 `json:"quantity,omitempty"`
}

func (l Line) adHoc() bool { return l.PriceID == "" }

// CheckoutRequest opens a Stripe Checkout session.
type CheckoutRequest struct {
	// Reference is your identifier for whatever is being paid for — an order
	// id, an invoice number. It is opaque to caisse, comes back on every event
	// as [Payment.Reference], and is what you reconcile against.
	Reference string `json:"reference"`

	// Lines is what the customer is buying. At least one, at most 100.
	Lines []Line `json:"lines"`

	// Currency is the ISO 4217 code, lowercased. Required for ad-hoc lines and
	// ignored when every line names a PriceID.
	Currency string `json:"currency,omitempty"`

	// Mode defaults to [ModePayment].
	Mode Mode `json:"mode,omitempty"`

	// SuccessURL is where Stripe sends the customer after payment. It may
	// contain Stripe's {CHECKOUT_SESSION_ID} template.
	//
	// Landing here is not proof of payment — the customer can navigate to it
	// directly. Only a webhook is.
	SuccessURL string `json:"success_url"`

	// CancelURL is where Stripe sends a customer who backs out.
	CancelURL string `json:"cancel_url"`

	// CustomerEmail prefills the email field. Mutually exclusive with CustomerID.
	CustomerEmail string `json:"customer_email,omitempty"`

	// CustomerID attaches the session to an existing Stripe customer, cus_….
	CustomerID string `json:"customer_id,omitempty"`

	// Locale forces the checkout language, "fr" or "en". Empty means auto.
	Locale string `json:"locale,omitempty"`

	// Metadata is copied onto the session and onto the payment intent or
	// subscription. [ReferenceKey] is reserved.
	Metadata map[string]string `json:"metadata,omitempty"`
}

// Session is an opened checkout. Send the customer to URL.
type Session struct {
	ID        string
	URL       string
	ExpiresAt time.Time
	Livemode  bool
}

// Checkout opens a Stripe Checkout session.
//
// The request is validated before any network call, and carries an idempotency
// key derived from its own contents: retrying an identical request returns the
// session already opened rather than a second one, while a genuinely different
// request always gets a new session. Neither case can produce Stripe's
// idempotency_error.
func (c *Client) Checkout(ctx context.Context, request CheckoutRequest) (Session, error) {
	if err := request.validate(); err != nil {
		return Session{}, err
	}

	params := buildParams(request)
	params.Context = ctx
	params.IdempotencyKey = stripe.String(request.idempotencyKey())

	session, err := c.api.V1CheckoutSessions.Create(ctx, params)
	if err != nil {
		return Session{}, wrap("open checkout", err)
	}

	return Session{
		ID:        session.ID,
		URL:       session.URL,
		ExpiresAt: time.Unix(session.ExpiresAt, 0).UTC(),
		Livemode:  session.Livemode,
	}, nil
}

// idempotencyKey hashes the request so an identical retry reuses the session
// Stripe already opened. Adding a field to CheckoutRequest changes every key,
// which costs one duplicated session per in-flight retry across a deploy.
//
// The request is normalised the same way Checkout normalises it before sending,
// so two requests that produce the same session hash the same. Without that,
// Quantity 0 and Quantity 1 — which mean the same thing — would open two.
func (r CheckoutRequest) idempotencyKey() string {
	normalised := r
	normalised.Currency = strings.ToLower(strings.TrimSpace(r.Currency))
	if normalised.Mode == "" {
		normalised.Mode = ModePayment
	}
	normalised.Lines = make([]Line, len(r.Lines))
	copy(normalised.Lines, r.Lines)
	for index := range normalised.Lines {
		if normalised.Lines[index].Quantity == 0 {
			normalised.Lines[index].Quantity = 1
		}
	}

	encoded, err := json.Marshal(normalised)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(encoded)
	return "caisse_" + hex.EncodeToString(sum[:])[:32]
}
