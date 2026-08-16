package caisse

import (
	"maps"
	"strings"

	stripe "github.com/stripe/stripe-go/v86"
)

// buildParams turns a validated request into the params Stripe will see.
func buildParams(request CheckoutRequest) *stripe.CheckoutSessionCreateParams {
	mode := resolveMode(request.Mode)

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
		params.LineItems = append(params.LineItems, buildLineItem(line, currency))
	}
	return params
}

func buildLineItem(line Line, currency string) *stripe.CheckoutSessionCreateLineItemParams {
	quantity := line.Quantity
	if quantity == 0 {
		quantity = 1
	}
	item := &stripe.CheckoutSessionCreateLineItemParams{Quantity: stripe.Int64(quantity)}

	if !line.adHoc() {
		item.Price = stripe.String(line.PriceID)
		return item
	}

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
	return item
}
