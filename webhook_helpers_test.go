package caisse

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	stripe "github.com/stripe/stripe-go/v86"
)

const testWebhookSecret = "whsec_testsecret"

func webhookClient(t *testing.T) *Client {
	t.Helper()
	client, err := New(Config{
		SecretKey:     "sk_test_123",
		WebhookSecret: testWebhookSecret,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return client
}

func eventBody(t *testing.T, id, eventType string, object any) []byte {
	t.Helper()
	raw, err := json.Marshal(object)
	if err != nil {
		t.Fatalf("marshal event object: %v", err)
	}
	body, err := json.Marshal(map[string]any{
		"id":          id,
		"object":      "event",
		"api_version": stripe.APIVersion,
		"created":     time.Now().Unix(),
		"livemode":    false,
		"type":        eventType,
		"data":        map[string]any{"object": json.RawMessage(raw)},
	})
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	return body
}

func deliver(t *testing.T, handler http.Handler, body []byte, at time.Time) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/stripe/webhook", bytes.NewReader(body))
	request.Header.Set("Stripe-Signature", Sign(testWebhookSecret, body, at))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func paidSession() map[string]any {
	return map[string]any{
		"id":                  "cs_test_1",
		"object":              "checkout.session",
		"client_reference_id": "ORD-42",
		"payment_status":      "paid",
		"amount_total":        7000,
		"currency":            "eur",
		"mode":                "payment",
		"payment_intent":      map[string]any{"id": "pi_test_1", "object": "payment_intent"},
		"customer":            map[string]any{"id": "cus_test_1", "object": "customer"},
		"customer_details":    map[string]any{"email": "buyer@shop.test"},
		"metadata":            map[string]string{ReferenceKey: "ORD-42", "campaign": "spring"},
	}
}
