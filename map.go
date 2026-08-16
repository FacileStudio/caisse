package caisse

import (
	"time"

	stripe "github.com/stripe/stripe-go/v86"
)

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
