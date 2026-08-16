package caisse

import (
	"fmt"
	"strconv"
	"time"

	"github.com/stripe/stripe-go/v86/webhook"
)

// Sign produces the Stripe-Signature header for a payload, so a test can drive
// a webhook handler without Stripe and without the Stripe CLI.
//
// It is the only reason a test needs to know how Stripe signs anything.
func Sign(secret string, payload []byte, at time.Time) string {
	signature := webhook.ComputeSignature(at, payload, secret)
	return "t=" + strconv.FormatInt(at.Unix(), 10) + ",v1=" + fmt.Sprintf("%x", signature)
}
