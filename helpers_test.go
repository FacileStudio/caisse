package caisse

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

type stripeCall struct {
	path   string
	form   url.Values
	header http.Header
}

// fakeStripe stands in for the Stripe API so a test can assert what was sent on
// the wire, which is the only place the form encoding is observable.
func fakeStripe(t *testing.T, status int, body string) (*Client, *[]stripeCall) {
	t.Helper()
	calls := make([]stripeCall, 0, 2)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("stripe request body is not a form: %v", err)
		}
		calls = append(calls, stripeCall{path: r.URL.Path, form: r.PostForm, header: r.Header.Clone()})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if _, err := w.Write([]byte(body)); err != nil {
			t.Errorf("write fake stripe response: %v", err)
		}
	}))
	t.Cleanup(server.Close)

	client, err := New(Config{SecretKey: "sk_test_123", BaseURL: server.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return client, &calls
}

func validCheckout() CheckoutRequest {
	return CheckoutRequest{
		Reference:  "ORD-1",
		Currency:   "eur",
		Lines:      []Line{{Label: "T-shirt", Amount: 3500, Quantity: 2}},
		SuccessURL: "https://shop.test/ok",
		CancelURL:  "https://shop.test/ko",
	}
}
