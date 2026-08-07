# caisse — API

Every exported symbol, package by package. Generated from the source, not from memory.

## caisse

### Constructing

```go
func New(cfg Config) (*Client, error)
func FromEnv() (*Client, error)
func (c *Client) Live() bool
```

`New` rejects a key that is not `sk_…` or `rk_…`, and a webhook secret that is not `whsec_…`.
`FromEnv` reads `STRIPE_SECRET_KEY` and `STRIPE_WEBHOOK_SECRET`.

### Constants

| Name | Value | What it is |
|---|---|---|
| `ReferenceKey` | `caisse_reference` | Metadata key carrying your reference. Reserved |
| `DefaultTolerance` | `5m` | Maximum webhook signature age |
| `DefaultHandlerTimeout` | `20s` | Bound on one handler run |
| `MaxWebhookBytes` | `1 MiB` | Cap on a webhook body |
| `ModePayment`, `ModeSubscription` | `payment`, `subscription` | Checkout modes |
| `ReasonDuplicate`, `ReasonFraudulent`, `ReasonRequested` | Stripe's refund reasons | |

### Checkout

```go
func (c *Client) Checkout(ctx context.Context, request CheckoutRequest) (Session, error)
```

| `CheckoutRequest` field | Type | Notes |
|---|---|---|
| `Reference` | `string` | Required, max 200 chars. Your order id |
| `Lines` | `[]Line` | Required, 1 to 100 |
| `Currency` | `string` | ISO 4217, required when any line prices itself |
| `Mode` | `Mode` | Defaults to `ModePayment` |
| `SuccessURL`, `CancelURL` | `string` | Required, absolute http(s) |
| `CustomerEmail` | `string` | Mutually exclusive with `CustomerID` |
| `CustomerID` | `string` | `cus_…` |
| `Locale` | `string` | `fr`, `en`, … Empty means auto |
| `Metadata` | `map[string]string` | Max 50 entries, keys 40 chars, values 500 |

| `Line` field | Type | Notes |
|---|---|---|
| `PriceID` | `string` | `price_…`. Excludes `Label` and `Amount` |
| `Label` | `string` | Required for an ad-hoc line |
| `Description` | `string` | Optional second line |
| `Amount` | `int64` | Unit price in the smallest currency unit. Must be positive |
| `ImageURL` | `string` | Optional |
| `Quantity` | `int64` | Defaults to 1 |

`Session` carries `ID`, `URL`, `ExpiresAt` and `Livemode`. Send the customer to `URL`.

Subscription mode requires every line to name a `PriceID`; Stripe has nowhere to put an ad-hoc
recurring amount.

### Refunds and the billing portal

```go
func (c *Client) Refund(ctx context.Context, request RefundRequest) (Refund, error)
func (c *Client) PortalURL(ctx context.Context, customerID, returnURL string) (string, error)
```

`RefundRequest` takes `PaymentIntentID` (`pi_…`, required), `Amount` (zero refunds everything
still refundable), `Reason` and `Metadata`.

`Refund` carries `EventID`, `ID`, `ChargeID`, `PaymentIntentID`, `Reference`, `Amount`,
`Currency`, `Status`, `Metadata`, `OccurredAt` and `Livemode`. `EventID`, `OccurredAt` and
`Livemode` are set only on the webhook path; `ID` only on the API path, because a
`charge.refunded` event describes the charge rather than the individual refund.

`PortalURL` returns a single-use, short-lived URL. Mint one per visit; never store it.

### Webhook

```go
func (c *Client) Webhook(hooks Hooks) (http.Handler, error)
func Sign(secret string, payload []byte, at time.Time) string
```

`Webhook` errors when it has no signing secret or no store. `Sign` produces a
`Stripe-Signature` header so tests can drive the handler without Stripe.

| `Hooks` field | Signature | Fires on |
|---|---|---|
| `Store` | `EventStore` | Required |
| `OnPaid` | `func(context.Context, Payment) error` | `checkout.session.completed` (settled), `…async_payment_succeeded` |
| `OnFailed` | same | `…async_payment_failed`, `payment_intent.payment_failed` |
| `OnExpired` | same | `checkout.session.expired` |
| `OnRefunded` | `func(context.Context, Refund) error` | `charge.refunded` |
| `OnSubscription` | `func(context.Context, Subscription) error` | `customer.subscription.*` |

A nil handler makes its events a no-op that is still acknowledged. A handler returning an error
answers Stripe with 500, which makes Stripe retry — return one only for failures a retry could
fix.

`Payment` carries `EventID`, `Reference`, `SessionID`, `PaymentIntentID`, `SubscriptionID`,
`CustomerID`, `CustomerEmail`, `Amount`, `Currency`, `FailureMessage`, `Metadata`, `Livemode`
and `OccurredAt`.

`Subscription` carries `EventID`, `ID`, `Reference`, `CustomerID`, `Status`,
`CancelAtPeriodEnd`, `Metadata`, `Livemode` and `OccurredAt`.

### Event store

```go
type EventStore interface {
	Begin(ctx context.Context, eventID string) (bool, error)
	Done(ctx context.Context, eventID string) error
	Fail(ctx context.Context, eventID string) error
}

func NewMemoryStore() *MemoryStore
```

`MemoryStore` is for tests and local development. In production it forgets every deploy and
dedupes per replica, so a redelivery after a restart fulfils the order a second time.

### Errors

Every failure is a `*tronc/errors.Error`, so `httpjson.WriteError` maps it to the right status.

| Stripe failure | Code | Message |
|---|---|---|
| Card declined | `invalid_argument` | Stripe's own, written for the cardholder |
| HTTP 429 | `rate_limited` | `payment provider is rate limiting` |
| HTTP 5xx or `api_error` | `unavailable` | `payment provider unavailable` |
| No response at all | `unavailable` | `payment provider unreachable` |
| Anything else | `internal` | `payment request rejected` |

Only card errors carry their Stripe message outward. Everything else can name internal
identifiers, so it becomes a generic message with the cause preserved for the logs.

## caisse/pg

```go
const Schema = "…"
const DefaultStaleAfter = 15 * time.Minute

func New(db *sql.DB, staleAfter time.Duration) *Store
func EnsureSchema(ctx context.Context, db *sql.DB) error

func (s *Store) Begin(ctx context.Context, eventID string) (bool, error)
func (s *Store) Done(ctx context.Context, eventID string) error
func (s *Store) Fail(ctx context.Context, eventID string) error
func (s *Store) Purge(ctx context.Context, olderThan time.Duration) (int64, error)
```

`staleAfter` of zero means `DefaultStaleAfter`. The table is `caisse_events` and its name is
fixed. Apply `Schema` through the app's migrations; `EnsureSchema` is for tests and local
development. Run `Purge` from a daily job — handled rows are worth keeping until Stripe's
retry window closes, about three days, and worth nothing after.
