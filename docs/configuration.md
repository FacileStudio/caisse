# caisse — Configuration

Every environment variable and `Config` field the code reads, and the traps behind them.

## Environment

Read by `caisse.FromEnv` only. Building a `Client` with `caisse.New` reads nothing from the
environment.

| Variable | Required | Default | What it does |
|---|---|---|---|
| `STRIPE_SECRET_KEY` | yes | — | Stripe secret or restricted key, `sk_…` or `rk_…` |
| `STRIPE_WEBHOOK_SECRET` | no | — | Signing secret of the endpoint, `whsec_…` |

`STRIPE_WEBHOOK_SECRET` is optional to construct a client but required to build a webhook
handler. `Client.Webhook` returns an error without it rather than accepting unverified
deliveries.

## Config

| Field | Default | What it does |
|---|---|---|
| `SecretKey` | — | Required. Rejected at construction unless it starts with `sk_` or `rk_` |
| `WebhookSecret` | — | Rejected unless empty or starting with `whsec_` |
| `Tolerance` | `5m` | Maximum age of a webhook signature |
| `HandlerTimeout` | `20s` | Bounds one webhook handler run |
| `HTTPClient` | `http.DefaultClient` | The client used to reach Stripe |
| `BaseURL` | Stripe's API | Overrides the API root. For tests; leave empty in production |
| `Logger` | `slog.Default()` | Receives rejection warnings and bookkeeping errors |

### The key prefix check

A publishable key (`pk_…`) pasted into server configuration is a common mistake, and Stripe
only reports it at the first API call — in production, on a real customer. `caisse.New` rejects
it at boot instead, along with anything that is not a secret key at all.

`Client.Live()` reports whether the key is a live one. Gate a staging deployment on it if you
want certainty that it cannot take real money.

### Tolerance

The signature window is what makes a captured webhook body useless to replay later. Widening it
past a few minutes buys nothing except a longer replay window; the only reason to touch it is a
server whose clock you do not control.

### HandlerTimeout

Stripe gives an endpoint about 30 seconds before it gives up and retries. `HandlerTimeout` has
to stay under that, or Stripe starts a retry while the first attempt is still running. It also
has to stay well under `pg` `StaleAfter`, for the same reason.

## pg

| Argument | Default | What it does |
|---|---|---|
| `db` | — | Any `*sql.DB`. GORM hands one over with `db.DB()` |
| `staleAfter` | `15m` | How long a claim may sit unconfirmed before another delivery may take it |

The table is `caisse_events`, and its name is fixed. Apply `pg.Schema` through the app's own
migrations — `tronc/migrate`, goose, whatever is already there. `pg.EnsureSchema` exists for
tests and local development, not for boot.

Trap: `staleAfter` shorter than the slowest handler means a retry can start work the first
attempt has not finished. Longer than a few hours means a crashed process leaves an order
unfulfilled for that long. Fifteen minutes fits a handler measured in seconds.

## Stripe dashboard

The webhook endpoint needs these events selected, and nothing more:

| Event | Handler |
|---|---|
| `checkout.session.completed` | `OnPaid` |
| `checkout.session.async_payment_succeeded` | `OnPaid` |
| `checkout.session.async_payment_failed` | `OnFailed` |
| `checkout.session.expired` | `OnExpired` |
| `payment_intent.payment_failed` | `OnFailed` |
| `charge.refunded` | `OnRefunded` |
| `customer.subscription.created`, `.updated`, `.deleted`, `.paused`, `.resumed` | `OnSubscription` |

Any other event type is acknowledged with 200 and ignored, so selecting extras is harmless —
just noisier.
