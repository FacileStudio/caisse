package caisse

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	stripe "github.com/stripe/stripe-go/v86"
	"github.com/stripe/stripe-go/v86/webhook"
)

// MaxWebhookBytes caps the body caisse will read from a webhook delivery.
// Stripe events are a few kilobytes; the cap is what stops an unauthenticated
// request from making the process read until it runs out of memory, since the
// body has to be buffered whole before its signature can be checked.
const MaxWebhookBytes int64 = 1 << 20

// Payment is a payment Stripe reported. It is delivered to [Hooks.OnPaid],
// [Hooks.OnFailed] and [Hooks.OnExpired].
type Payment struct {
	// EventID is Stripe's event identifier, evt_…. It is the dedupe key.
	EventID string

	// Reference is the [CheckoutRequest.Reference] this payment belongs to.
	Reference string

	// SessionID is the checkout session, cs_…. Empty when the event came from
	// the payment intent rather than the session.
	SessionID string

	// PaymentIntentID is what [Client.Refund] takes.
	PaymentIntentID string

	// SubscriptionID is set in subscription mode.
	SubscriptionID string

	// CustomerID is what [Client.PortalURL] takes.
	CustomerID string

	// CustomerEmail is the address the customer paid with, when Stripe has one.
	CustomerEmail string

	// Amount is the total in the currency's smallest unit.
	Amount int64

	// Currency is the ISO 4217 code, lowercased.
	Currency string

	// FailureMessage explains a failed payment, when Stripe gave a reason.
	FailureMessage string

	// Metadata is what you sent on the checkout request.
	Metadata map[string]string

	// Livemode distinguishes real money from test money.
	Livemode bool

	// OccurredAt is when Stripe created the event.
	OccurredAt time.Time
}

// Subscription is a subscription lifecycle change, delivered to
// [Hooks.OnSubscription]. Status carries the meaning: "active", "past_due",
// "canceled", "trialing", and the rest of Stripe's set.
type Subscription struct {
	EventID           string
	ID                string
	Reference         string
	CustomerID        string
	Status            string
	CancelAtPeriodEnd bool
	Metadata          map[string]string
	Livemode          bool
	OccurredAt        time.Time
}

// Hooks is what an app does with the events caisse verified.
//
// Every handler is optional; a nil one makes its events a no-op that still gets
// acknowledged. A handler returning an error answers Stripe with 500, which
// makes Stripe retry the delivery later — so return an error only for failures
// that retrying could fix, and swallow the ones it cannot.
//
// Handlers must be idempotent anyway. The store keeps Stripe's retries from
// reaching a handler twice, but nothing keeps a handler from being interrupted
// halfway through by a deploy.
type Hooks struct {
	// Store is required. Without it there is no dedupe, so it is not defaulted.
	Store EventStore

	// OnPaid fires once money has actually moved: checkout.session.completed
	// with a settled payment, or checkout.session.async_payment_succeeded for
	// the delayed methods like SEPA debit.
	OnPaid func(context.Context, Payment) error

	// OnFailed fires on a payment that will not settle.
	OnFailed func(context.Context, Payment) error

	// OnExpired fires when a customer opened a checkout and never finished it.
	// Releasing reserved stock belongs here.
	OnExpired func(context.Context, Payment) error

	// OnRefunded fires on charge.refunded, whether the refund came from
	// [Client.Refund] or from somebody clicking refund in the Stripe dashboard.
	OnRefunded func(context.Context, Refund) error

	// OnSubscription fires on the customer.subscription.* lifecycle.
	OnSubscription func(context.Context, Subscription) error
}

// Webhook returns the handler for a Stripe webhook endpoint. Mount it on a
// route that skips any authentication middleware — Stripe authenticates with a
// signature, not a session.
//
// The handler verifies the signature, rejects a body older than the configured
// tolerance, and refuses to run a handler for an event the store has already
// seen. It answers 400 for anything it cannot verify, 500 for a handler that
// failed, and 200 otherwise.
//
// It reports an error rather than returning a handler when it has no signing
// secret or no store, because both failures are invisible in production: an
// endpoint that skips verification accepts a forged "order paid" from anyone
// who knows the URL, and one without a store fulfils twice.
func (c *Client) Webhook(hooks Hooks) (http.Handler, error) {
	if c.webhookSecret == "" {
		return nil, fmt.Errorf("caisse: Webhook needs Config.WebhookSecret")
	}
	if hooks.Store == nil {
		return nil, fmt.Errorf("caisse: Webhook needs Hooks.Store")
	}

	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost {
			writer.Header().Set("Allow", http.MethodPost)
			http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		body, err := io.ReadAll(http.MaxBytesReader(writer, request.Body, MaxWebhookBytes))
		if err != nil {
			c.logger.Warn("caisse: unreadable webhook body", "error", err)
			http.Error(writer, "bad request", http.StatusBadRequest)
			return
		}

		event, err := webhook.ConstructEventWithOptions(
			body,
			request.Header.Get("Stripe-Signature"),
			c.webhookSecret,
			webhook.ConstructEventOptions{Tolerance: c.tolerance, IgnoreAPIVersionMismatch: true},
		)
		if err != nil {
			c.logger.Warn("caisse: rejected webhook signature", "error", err, "remote", request.RemoteAddr)
			http.Error(writer, "signature verification failed", http.StatusBadRequest)
			return
		}
		c.warnAPIVersion(event.APIVersion)

		ctx, cancel := context.WithTimeout(context.WithoutCancel(request.Context()), c.handlerTimeout)
		defer cancel()

		fresh, err := hooks.Store.Begin(ctx, event.ID)
		if err != nil {
			c.logger.Error("caisse: event store unavailable", "error", err, "event", event.ID)
			http.Error(writer, "event store unavailable", http.StatusInternalServerError)
			return
		}
		if !fresh {
			writer.WriteHeader(http.StatusOK)
			return
		}

		if err := c.run(ctx, hooks, event); err != nil {
			c.logger.Error("caisse: webhook handler failed", "error", err, "event", event.ID, "type", event.Type)
			if releaseErr := hooks.Store.Fail(ctx, event.ID); releaseErr != nil {
				c.logger.Error("caisse: could not release event claim", "error", releaseErr, "event", event.ID)
			}
			http.Error(writer, "handler failed", http.StatusInternalServerError)
			return
		}

		if err := hooks.Store.Done(ctx, event.ID); err != nil {
			c.logger.Error("caisse: handled event but could not record it", "error", err, "event", event.ID)
		}
		writer.WriteHeader(http.StatusOK)
	}), nil
}

// warnAPIVersion reports, once, that the endpoint's Stripe API version is not
// the one this SDK was generated against.
//
// stripe-go rejects such an event outright. caisse does not: an endpoint
// created in the dashboard takes the account's API version, which drifts from
// the SDK's on its own schedule, and failing closed there means answering 400
// to every delivery — no order ever fulfilled, for a version string. The
// signature was still valid and the fields caisse reads have been stable for
// years, so it processes the event and says so loudly instead.
func (c *Client) warnAPIVersion(eventVersion string) {
	if eventVersion == "" || eventVersion == stripe.APIVersion {
		return
	}
	c.apiVersionWarning.Do(func() {
		c.logger.Warn("caisse: webhook API version differs from the SDK",
			"event_api_version", eventVersion, "sdk_api_version", stripe.APIVersion)
	})
}

// run isolates a panicking handler so the claim is still released. Without it a
// panic leaves the event claimed, and Stripe's retry finds it already taken and
// silently does nothing.
func (c *Client) run(ctx context.Context, hooks Hooks, event stripe.Event) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("caisse: handler panicked: %v", recovered)
		}
	}()
	return c.dispatch(ctx, hooks, event)
}

func (c *Client) dispatch(ctx context.Context, hooks Hooks, event stripe.Event) error {
	switch event.Type {
	case stripe.EventTypeCheckoutSessionCompleted, stripe.EventTypeCheckoutSessionAsyncPaymentSucceeded:
		session, err := decode[stripe.CheckoutSession](event)
		if err != nil {
			return err
		}
		if session.PaymentStatus == stripe.CheckoutSessionPaymentStatusUnpaid {
			return nil
		}
		return call(ctx, hooks.OnPaid, paymentFromSession(event, session))

	case stripe.EventTypeCheckoutSessionAsyncPaymentFailed:
		session, err := decode[stripe.CheckoutSession](event)
		if err != nil {
			return err
		}
		return call(ctx, hooks.OnFailed, paymentFromSession(event, session))

	case stripe.EventTypeCheckoutSessionExpired:
		session, err := decode[stripe.CheckoutSession](event)
		if err != nil {
			return err
		}
		return call(ctx, hooks.OnExpired, paymentFromSession(event, session))

	case stripe.EventTypePaymentIntentPaymentFailed:
		intent, err := decode[stripe.PaymentIntent](event)
		if err != nil {
			return err
		}
		return call(ctx, hooks.OnFailed, paymentFromIntent(event, intent))

	case stripe.EventTypeChargeRefunded:
		charge, err := decode[stripe.Charge](event)
		if err != nil {
			return err
		}
		return call(ctx, hooks.OnRefunded, refundFromCharge(event, charge))

	case stripe.EventTypeCustomerSubscriptionCreated,
		stripe.EventTypeCustomerSubscriptionUpdated,
		stripe.EventTypeCustomerSubscriptionDeleted,
		stripe.EventTypeCustomerSubscriptionPaused,
		stripe.EventTypeCustomerSubscriptionResumed:
		subscription, err := decode[stripe.Subscription](event)
		if err != nil {
			return err
		}
		return call(ctx, hooks.OnSubscription, subscriptionFrom(event, subscription))
	}

	return nil
}

func decode[T any](event stripe.Event) (*T, error) {
	if event.Data == nil {
		return nil, fmt.Errorf("caisse: event %s has no data", event.ID)
	}
	var decoded T
	if err := json.Unmarshal(event.Data.Raw, &decoded); err != nil {
		return nil, fmt.Errorf("caisse: event %s: %w", event.ID, err)
	}
	return &decoded, nil
}

func call[T any](ctx context.Context, handler func(context.Context, T) error, payload T) error {
	if handler == nil {
		return nil
	}
	return handler(ctx, payload)
}

func paymentFromSession(event stripe.Event, session *stripe.CheckoutSession) Payment {
	payment := Payment{
		EventID:    event.ID,
		Reference:  reference(session.ClientReferenceID, session.Metadata),
		SessionID:  session.ID,
		Amount:     session.AmountTotal,
		Currency:   string(session.Currency),
		Metadata:   session.Metadata,
		Livemode:   event.Livemode,
		OccurredAt: time.Unix(event.Created, 0).UTC(),
	}
	if session.PaymentIntent != nil {
		payment.PaymentIntentID = session.PaymentIntent.ID
	}
	if session.Subscription != nil {
		payment.SubscriptionID = session.Subscription.ID
	}
	if session.Customer != nil {
		payment.CustomerID = session.Customer.ID
	}
	payment.CustomerEmail = session.CustomerEmail
	if payment.CustomerEmail == "" && session.CustomerDetails != nil {
		payment.CustomerEmail = session.CustomerDetails.Email
	}
	return payment
}

func paymentFromIntent(event stripe.Event, intent *stripe.PaymentIntent) Payment {
	payment := Payment{
		EventID:         event.ID,
		Reference:       reference("", intent.Metadata),
		PaymentIntentID: intent.ID,
		Amount:          intent.Amount,
		Currency:        string(intent.Currency),
		CustomerEmail:   intent.ReceiptEmail,
		Metadata:        intent.Metadata,
		Livemode:        event.Livemode,
		OccurredAt:      time.Unix(event.Created, 0).UTC(),
	}
	if intent.Customer != nil {
		payment.CustomerID = intent.Customer.ID
	}
	if intent.LastPaymentError != nil {
		payment.FailureMessage = intent.LastPaymentError.Msg
	}
	return payment
}

func refundFromCharge(event stripe.Event, charge *stripe.Charge) Refund {
	refund := Refund{
		EventID:    event.ID,
		ChargeID:   charge.ID,
		Reference:  reference("", charge.Metadata),
		Amount:     charge.AmountRefunded,
		Currency:   string(charge.Currency),
		Status:     string(charge.Status),
		Metadata:   charge.Metadata,
		Livemode:   event.Livemode,
		OccurredAt: time.Unix(event.Created, 0).UTC(),
	}
	if charge.PaymentIntent != nil {
		refund.PaymentIntentID = charge.PaymentIntent.ID
	}
	return refund
}

func subscriptionFrom(event stripe.Event, subscription *stripe.Subscription) Subscription {
	result := Subscription{
		EventID:           event.ID,
		ID:                subscription.ID,
		Reference:         reference("", subscription.Metadata),
		Status:            string(subscription.Status),
		CancelAtPeriodEnd: subscription.CancelAtPeriodEnd,
		Metadata:          subscription.Metadata,
		Livemode:          event.Livemode,
		OccurredAt:        time.Unix(event.Created, 0).UTC(),
	}
	if subscription.Customer != nil {
		result.CustomerID = subscription.Customer.ID
	}
	return result
}

func reference(clientReferenceID string, metadata map[string]string) string {
	if clientReferenceID != "" {
		return clientReferenceID
	}
	return metadata[ReferenceKey]
}

// Sign produces the Stripe-Signature header for a payload, so a test can drive
// a webhook handler without Stripe and without the Stripe CLI.
//
// It is the only reason a test needs to know how Stripe signs anything.
func Sign(secret string, payload []byte, at time.Time) string {
	signature := webhook.ComputeSignature(at, payload, secret)
	return "t=" + strconv.FormatInt(at.Unix(), 10) + ",v1=" + fmt.Sprintf("%x", signature)
}
