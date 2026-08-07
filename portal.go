package caisse

import (
	"context"
	"strings"

	"github.com/FacileStudio/tronc/errors"
	stripe "github.com/stripe/stripe-go/v86"
)

// PortalURL opens a Stripe Billing Portal session and returns the URL to send
// the customer to.
//
// The portal is where a subscriber updates their card, downloads invoices and
// cancels — every screen you would otherwise build and keep in sync with
// Stripe. The URL is single-use and short-lived, so mint one per visit and
// never store it.
func (c *Client) PortalURL(ctx context.Context, customerID, returnURL string) (string, error) {
	if !strings.HasPrefix(customerID, "cus_") {
		return "", errors.Invalid("caisse: customerID must be a Stripe customer (cus_…)")
	}
	if err := validateRedirect("returnURL", returnURL); err != nil {
		return "", err
	}

	params := &stripe.BillingPortalSessionCreateParams{
		Customer:  stripe.String(customerID),
		ReturnURL: stripe.String(returnURL),
	}
	params.Context = ctx

	session, err := c.api.V1BillingPortalSessions.Create(ctx, params)
	if err != nil {
		return "", wrap("open billing portal", err)
	}
	return session.URL, nil
}
