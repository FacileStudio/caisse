package caisse

import (
	stderrors "errors"
	"fmt"
	"net/http"

	"github.com/FacileStudio/tronc/errors"
	stripe "github.com/stripe/stripe-go/v86"
)

// wrap turns a Stripe failure into the suite error envelope, so an app can hand
// it straight to httpjson.WriteError and get the right status code out.
//
// Only card errors carry their Stripe message outward: those are written for
// the cardholder and are the one class of failure the customer can act on.
// Everything else — a rejected parameter, a bad API key, a reused idempotency
// key — is a defect on our side, and its message can name internal identifiers,
// so it becomes a generic internal error with the cause kept for the logs.
func wrap(operation string, err error) error {
	if err == nil {
		return nil
	}
	cause := fmt.Errorf("caisse: %s: %w", operation, err)

	var stripeErr *stripe.Error
	if !stderrors.As(err, &stripeErr) {
		return errors.New("unavailable", "payment provider unreachable", cause)
	}

	switch {
	case stripeErr.Type == stripe.ErrorTypeCard:
		return errors.New("invalid_argument", stripeErr.Msg, cause)
	case stripeErr.HTTPStatusCode == http.StatusTooManyRequests:
		return errors.New("rate_limited", "payment provider is rate limiting", cause)
	case stripeErr.HTTPStatusCode >= http.StatusInternalServerError,
		stripeErr.Type == stripe.ErrorTypeAPI:
		return errors.New("unavailable", "payment provider unavailable", cause)
	default:
		return errors.Internal("payment request rejected", cause)
	}
}
