package caisse

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"strings"
	"time"

	"github.com/FacileStudio/tronc/errors"
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

	mode := request.Mode
	if mode == "" {
		mode = ModePayment
	}

	metadata := map[string]string{ReferenceKey: request.Reference}
	maps.Copy(metadata, request.Metadata)

	params := &stripe.CheckoutSessionCreateParams{
		ClientReferenceID: stripe.String(request.Reference),
		Mode:              stripe.String(string(mode)),
		SuccessURL:        stripe.String(request.SuccessURL),
		CancelURL:         stripe.String(request.CancelURL),
		LineItems:         make([]*stripe.CheckoutSessionCreateLineItemParams, 0, len(request.Lines)),
		Metadata:          metadata,
	}
	params.Context = ctx
	params.IdempotencyKey = stripe.String(request.idempotencyKey())

	if request.CustomerID != "" {
		params.Customer = stripe.String(request.CustomerID)
	} else if request.CustomerEmail != "" {
		params.CustomerEmail = stripe.String(request.CustomerEmail)
	}
	if request.Locale != "" {
		params.Locale = stripe.String(request.Locale)
	}

	switch mode {
	case ModeSubscription:
		params.SubscriptionData = &stripe.CheckoutSessionCreateSubscriptionDataParams{Metadata: metadata}
	default:
		params.PaymentIntentData = &stripe.CheckoutSessionCreatePaymentIntentDataParams{Metadata: metadata}
	}

	currency := strings.ToLower(request.Currency)
	for _, line := range request.Lines {
		quantity := line.Quantity
		if quantity == 0 {
			quantity = 1
		}
		item := &stripe.CheckoutSessionCreateLineItemParams{Quantity: stripe.Int64(quantity)}

		if line.adHoc() {
			product := &stripe.CheckoutSessionCreateLineItemPriceDataProductDataParams{
				Name: stripe.String(line.Label),
			}
			if line.Description != "" {
				product.Description = stripe.String(line.Description)
			}
			if line.ImageURL != "" {
				product.Images = []*string{stripe.String(line.ImageURL)}
			}
			item.PriceData = &stripe.CheckoutSessionCreateLineItemPriceDataParams{
				Currency:    stripe.String(currency),
				UnitAmount:  stripe.Int64(line.Amount),
				ProductData: product,
			}
		} else {
			item.Price = stripe.String(line.PriceID)
		}

		params.LineItems = append(params.LineItems, item)
	}

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
func (r CheckoutRequest) idempotencyKey() string {
	encoded, err := json.Marshal(r)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(encoded)
	return "caisse_" + hex.EncodeToString(sum[:])[:32]
}

func (r CheckoutRequest) validate() error {
	switch {
	case strings.TrimSpace(r.Reference) == "":
		return errors.Invalid("caisse: Reference is required")
	case len(r.Reference) > maxReference:
		return errors.Invalid(fmt.Sprintf("caisse: Reference is longer than %d characters", maxReference))
	case len(r.Lines) == 0:
		return errors.Invalid("caisse: at least one line is required")
	case len(r.Lines) > maxLines:
		return errors.Invalid(fmt.Sprintf("caisse: more than %d lines", maxLines))
	case r.CustomerID != "" && r.CustomerEmail != "":
		return errors.Invalid("caisse: set CustomerID or CustomerEmail, not both")
	}

	if err := validateRedirect("SuccessURL", r.SuccessURL); err != nil {
		return err
	}
	if err := validateRedirect("CancelURL", r.CancelURL); err != nil {
		return err
	}

	mode := r.Mode
	if mode == "" {
		mode = ModePayment
	}
	if mode != ModePayment && mode != ModeSubscription {
		return errors.Invalid(fmt.Sprintf("caisse: unknown mode %q", string(r.Mode)))
	}

	adHoc := false
	for index, line := range r.Lines {
		where := fmt.Sprintf("caisse: line %d", index)
		switch {
		case line.PriceID != "" && (line.Label != "" || line.Amount != 0):
			return errors.Invalid(where + " sets both PriceID and an ad-hoc price")
		case line.Quantity < 0:
			return errors.Invalid(where + " has a negative quantity")
		}
		if line.adHoc() {
			adHoc = true
			switch {
			case mode == ModeSubscription:
				return errors.Invalid(where + " has no PriceID, which subscription mode requires")
			case strings.TrimSpace(line.Label) == "":
				return errors.Invalid(where + " has no Label")
			case line.Amount <= 0:
				return errors.Invalid(where + " has a zero or negative Amount")
			}
		}
	}

	if adHoc && len(strings.TrimSpace(r.Currency)) != 3 {
		return errors.Invalid("caisse: Currency must be a three-letter ISO 4217 code")
	}

	return validateMetadata(r.Metadata)
}

// validateRedirect refuses anything that is not an absolute http(s) URL. A
// relative or scheme-less redirect is a configuration mistake that only shows
// up once a customer has already paid.
func validateRedirect(field, raw string) error {
	if strings.TrimSpace(raw) == "" {
		return errors.Invalid("caisse: " + field + " is required")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return errors.Invalid("caisse: " + field + " is not a valid URL")
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return errors.Invalid("caisse: " + field + " must be an absolute http or https URL")
	}
	return nil
}

func validateMetadata(metadata map[string]string) error {
	if len(metadata) > maxMetadataEntries {
		return errors.Invalid(fmt.Sprintf("caisse: more than %d metadata entries", maxMetadataEntries))
	}
	for key, value := range metadata {
		switch {
		case key == ReferenceKey:
			return errors.Invalid("caisse: metadata key " + ReferenceKey + " is reserved")
		case len(key) > maxMetadataKey:
			return errors.Invalid(fmt.Sprintf("caisse: metadata key %q is longer than %d characters", key, maxMetadataKey))
		case len(value) > maxMetadataValue:
			return errors.Invalid(fmt.Sprintf("caisse: metadata value for %q is longer than %d characters", key, maxMetadataValue))
		}
	}
	return nil
}
