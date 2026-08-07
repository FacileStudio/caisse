# caisse

The Stripe adapter for the [Facile Suite](https://facile.studio). One way to open a checkout,
one way to receive a webhook, and no opinion about your database.

`caisse` owns nothing. No schema, no migration, no domain type — it never learns what a product
or an invoice is. It takes amounts in the currency's smallest unit and an opaque reference of
your choosing, and hands back events carrying that same reference.

## What it does

- Opens Stripe Checkout sessions for one-off payments and for subscriptions
- Verifies webhook signatures, enforces a timestamp tolerance, and caps the body it will read
- Refuses to run a handler twice for an event Stripe redelivered
- Releases its claim on an event when a handler fails or panics, so a Stripe retry can succeed
- Derives an idempotency key from every request, so a retried checkout or refund cannot double
- Copies your reference onto the payment intent, so a refund event still points at your order
- Turns Stripe failures into the suite error envelope without leaking Stripe internals outward
- Ships a PostgreSQL event ledger in [`pg/`](pg/), and an in-memory one for tests

## Stack

| Layer | Tech |
|---|---|
| Runtime | Go 1.24, [stripe-go](https://github.com/stripe/stripe-go) v86, [tronc](https://github.com/FacileStudio/tronc) — and nothing else |
| Storage | Whatever the app already has; `pg/` needs only a `*sql.DB` |

## Install

```sh
go get github.com/FacileStudio/caisse
```

Taking a payment:

```go
client, err := caisse.FromEnv()
if err != nil {
	return err
}

session, err := client.Checkout(ctx, caisse.CheckoutRequest{
	Reference:  order.ID,
	Currency:   "eur",
	Lines:      []caisse.Line{{Label: "T-shirt / M / Black", Amount: 3500, Quantity: 2}},
	SuccessURL: "https://shop.example/orders/" + order.ID,
	CancelURL:  "https://shop.example/cart",
})
if err != nil {
	return err
}
// Send the customer to session.URL.
```

Hearing about it:

```go
handler, err := client.Webhook(caisse.Hooks{
	Store: pg.New(db, 0),
	OnPaid: func(ctx context.Context, payment caisse.Payment) error {
		return orders.MarkPaid(ctx, payment.Reference, payment.PaymentIntentID)
	},
	OnExpired: func(ctx context.Context, payment caisse.Payment) error {
		return orders.ReleaseStock(ctx, payment.Reference)
	},
})
if err != nil {
	return err
}
router.Handle("/api/stripe/webhook", handler)
```

Mount that route outside any authentication middleware: Stripe authenticates with a signature,
not a session. A customer landing on `SuccessURL` is not proof of payment — only `OnPaid` is.

## Configuration

| Variable | What it does |
|---|---|
| `STRIPE_SECRET_KEY` | Stripe secret or restricted key, `sk_…` or `rk_…` |
| `STRIPE_WEBHOOK_SECRET` | Signing secret of the webhook endpoint, `whsec_…` |

Read by `caisse.FromEnv`. Everything else is set in code through `caisse.Config`.

Full reference: [docs/configuration.md](docs/configuration.md).

## Structure

```
*.go       The client: checkout, refund, billing portal, webhook, event store
pg/        PostgreSQL event ledger, importing only database/sql
docs/      Architecture, configuration, development, API
```

## Documentation

| Doc | What's in it |
|---|---|
| [Architecture](docs/architecture.md) | Payment lifecycle, the dedupe ledger, what caisse refuses to own |
| [Configuration](docs/configuration.md) | Every environment variable and `Config` field |
| [Development](docs/development.md) | Local setup, the quality gate, testing webhooks, versioning |
| [API](docs/api.md) | Every exported symbol |

Release history lives in [CHANGELOG.md](CHANGELOG.md).

---

Part of the [Facile Suite](https://facile.studio) — self-hosted tools for creative studios
and freelancers. One login, zero cloud dependency.
