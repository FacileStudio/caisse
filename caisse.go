// Package caisse is the Facile Suite's Stripe adapter: one way to open a
// checkout, one way to receive a webhook, and no opinion about your database.
//
// It deliberately owns nothing. There is no schema here, no migration, no
// domain type — caisse never learns what a product or an invoice is. It takes
// amounts in the currency's smallest unit and an opaque reference of your
// choosing, and hands back events carrying that same reference. Storage,
// fulfilment and the meaning of a reference stay in the app.
//
// The reason it exists is [Client.Webhook]. Verifying a Stripe signature,
// tolerating clock skew, and refusing to run a handler twice for a redelivered
// event are the three things every integration needs and most get wrong, and
// getting them wrong means shipping an order twice or marking an unpaid order
// paid. That logic lives here once.
//
// Amounts are always int64 in the currency's smallest unit — cents for EUR,
// yen for JPY. There is no float anywhere in this package, on purpose.
package caisse

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	stripe "github.com/stripe/stripe-go/v86"
)

// ReferenceKey is the metadata key caisse writes your [CheckoutRequest.Reference]
// under, on both the session and the payment intent it creates.
//
// Stripe only carries client_reference_id on the checkout session itself, so a
// later charge.refunded event has no way back to your order without it. Copying
// the reference into payment intent metadata is what makes refunds correlatable.
// The key is reserved: a request whose Metadata uses it is rejected.
const ReferenceKey = "caisse_reference"

// DefaultTolerance is the maximum age of a webhook signature caisse accepts.
// It matches Stripe's own recommendation.
const DefaultTolerance = 5 * time.Minute

// DefaultHandlerTimeout bounds a single webhook handler run. Stripe gives an
// endpoint 30 seconds before it gives up and retries, so finishing under that
// is what keeps a retry from finding work already half-done.
const DefaultHandlerTimeout = 20 * time.Second

// Config configures a [Client]. Only SecretKey is required; WebhookSecret is
// required only by [Client.Webhook], which reports its absence rather than
// silently accepting unverified deliveries.
type Config struct {
	// SecretKey is the Stripe secret or restricted key, sk_… or rk_….
	SecretKey string

	// WebhookSecret is the signing secret of the endpoint, whsec_….
	WebhookSecret string

	// Tolerance overrides [DefaultTolerance] for webhook signature age.
	Tolerance time.Duration

	// HandlerTimeout overrides [DefaultHandlerTimeout].
	HandlerTimeout time.Duration

	// HTTPClient overrides the client used to reach Stripe.
	HTTPClient *http.Client

	// BaseURL overrides the Stripe API root. It exists for tests; leave it
	// empty in production.
	BaseURL string

	// Logger receives one warning per rejected webhook and one error per
	// bookkeeping failure. Defaults to slog.Default().
	Logger *slog.Logger
}

// Client talks to Stripe. It is safe for concurrent use.
type Client struct {
	api            *stripe.Client
	webhookSecret  string
	tolerance      time.Duration
	handlerTimeout time.Duration
	logger         *slog.Logger
	live           bool

	apiVersionWarning sync.Once
}

// New validates cfg and returns a ready client.
//
// It rejects a key that is not a Stripe secret key, because the publishable
// key is the one that gets pasted into server config by mistake and Stripe
// would only say so at the first API call, in production.
func New(cfg Config) (*Client, error) {
	key := strings.TrimSpace(cfg.SecretKey)
	switch {
	case key == "":
		return nil, fmt.Errorf("caisse: SecretKey is required")
	case strings.HasPrefix(key, "pk_"):
		return nil, fmt.Errorf("caisse: SecretKey is a publishable key, not a secret key")
	case !strings.HasPrefix(key, "sk_") && !strings.HasPrefix(key, "rk_"):
		return nil, fmt.Errorf("caisse: SecretKey is not a Stripe secret key (want sk_… or rk_…)")
	}

	secret := strings.TrimSpace(cfg.WebhookSecret)
	if secret != "" && !strings.HasPrefix(secret, "whsec_") {
		return nil, fmt.Errorf("caisse: WebhookSecret is not a Stripe signing secret (want whsec_…)")
	}

	tolerance := cfg.Tolerance
	if tolerance <= 0 {
		tolerance = DefaultTolerance
	}
	handlerTimeout := cfg.HandlerTimeout
	if handlerTimeout <= 0 {
		handlerTimeout = DefaultHandlerTimeout
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	backendConfig := &stripe.BackendConfig{}
	if cfg.HTTPClient != nil {
		backendConfig.HTTPClient = cfg.HTTPClient
	}
	if cfg.BaseURL != "" {
		backendConfig.URL = stripe.String(strings.TrimSuffix(cfg.BaseURL, "/"))
	}

	return &Client{
		api:            stripe.NewClient(key, stripe.WithBackends(stripe.NewBackendsWithConfig(backendConfig))),
		webhookSecret:  secret,
		tolerance:      tolerance,
		handlerTimeout: handlerTimeout,
		logger:         logger,
		live:           strings.HasPrefix(key, "sk_live_") || strings.HasPrefix(key, "rk_live_"),
	}, nil
}

// FromEnv builds a client from STRIPE_SECRET_KEY and STRIPE_WEBHOOK_SECRET.
func FromEnv() (*Client, error) {
	return New(Config{
		SecretKey:     os.Getenv("STRIPE_SECRET_KEY"),
		WebhookSecret: os.Getenv("STRIPE_WEBHOOK_SECRET"),
	})
}

// Live reports whether the client is configured against live Stripe keys.
// Use it to keep a staging deployment from taking real money.
func (c *Client) Live() bool { return c.live }
