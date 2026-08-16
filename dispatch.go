package caisse

import (
	"context"
	"encoding/json"
	"fmt"

	stripe "github.com/stripe/stripe-go/v86"
)

func (c *Client) dispatch(ctx context.Context, hooks Hooks, event stripe.Event) error {
	switch event.Type {
	case stripe.EventTypeCheckoutSessionCompleted, stripe.EventTypeCheckoutSessionAsyncPaymentSucceeded:
		return c.dispatchSession(ctx, hooks.OnPaid, event, true)

	case stripe.EventTypeCheckoutSessionAsyncPaymentFailed:
		return c.dispatchSession(ctx, hooks.OnFailed, event, false)

	case stripe.EventTypeCheckoutSessionExpired:
		return c.dispatchSession(ctx, hooks.OnExpired, event, false)

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
		return c.dispatchSubscription(ctx, hooks.OnSubscription, event)
	}

	return nil
}

// dispatchSubscription routes one customer.subscription.* event.
func (c *Client) dispatchSubscription(ctx context.Context, handler func(context.Context, Subscription) error, event stripe.Event) error {
	subscription, err := decode[stripe.Subscription](event)
	if err != nil {
		return err
	}
	return call(ctx, handler, subscriptionFrom(event, subscription))
}

// dispatchSession routes one checkout.session event. skipUnpaid drops an
// unpaid completed session — money has not moved yet, so there is nothing to
// fulfil.
func (c *Client) dispatchSession(ctx context.Context, handler func(context.Context, Payment) error, event stripe.Event, skipUnpaid bool) error {
	session, err := decode[stripe.CheckoutSession](event)
	if err != nil {
		return err
	}
	if skipUnpaid && session.PaymentStatus == stripe.CheckoutSessionPaymentStatusUnpaid {
		return nil
	}
	return call(ctx, handler, paymentFromSession(event, session))
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
