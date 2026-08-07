# Changelog

All notable changes to `caisse` are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow semver —
while on `v0`, a breaking change bumps the minor.

## [0.1.0] — 2026-08-07

First release. `caisse` is the suite's Stripe adapter: `Checkout`, `Refund`, `PortalURL` and
`Webhook`, over stripe-go v86 and `tronc` and nothing else.

### Added

- `Client.Checkout` — Stripe Checkout sessions in payment and subscription mode. The request is
  validated before any network call, so a missing reference, a negative amount or a relative
  redirect URL fails locally with a readable message instead of as a 400 from an API call you
  already paid for.

- `Client.Webhook` — signature verification, timestamp tolerance, a 1 MiB body cap, and dedupe
  through an `EventStore`. It returns an error rather than a handler when it has no signing
  secret or no store: both failures are invisible in production, and an endpoint that skips
  verification accepts a forged "order paid" from anyone who knows the URL.

- `Client.Refund` and `Client.PortalURL`.

- `EventStore`, `MemoryStore`, and the `pg` package — a PostgreSQL ledger importing only
  `database/sql`, so adopting it needs no driver caisse chose. `Begin` is a single
  `INSERT … ON CONFLICT DO UPDATE … WHERE … RETURNING`, which is what makes two replicas
  receiving the same delivery unable to both win, and what lets a claim left behind by a killed
  process be reclaimed after `StaleAfter`. Without that reclamation, every Stripe retry bounces
  off the stale row and the order silently never ships.

- `Sign`, which produces a `Stripe-Signature` header so a test can drive a webhook handler
  end to end without Stripe and without the Stripe CLI.

### Decided

- **Idempotency keys are derived, not supplied.** Both `Checkout` and `Refund` hash their own
  request into the key. An identical retry reuses Stripe's result; a different request always
  gets a new one; neither can produce Stripe's `idempotency_error`. A double refund is the most
  expensive mistake this package exists to prevent, and leaving the key to the caller is how it
  happens.

- **The reference is copied onto the payment intent** under `caisse_reference`. Stripe carries
  `client_reference_id` on the checkout session only, so a `charge.refunded` event — a refund
  issued from the dashboard, say — would otherwise arrive with nothing pointing back at an
  order, and every consumer would need its own Stripe-id-to-order table.

- **A failed or panicking handler releases its claim.** A claim left behind would make every
  Stripe retry find the event taken and do nothing: the order never ships and nothing ever
  raises an error again.

- **The handler context outlives the request**, under `context.WithoutCancel` plus a timeout.
  When Stripe hangs up at its own 30-second limit, the handler still finishes and still records
  the event, and the retry then finds it done. Cancelling instead leaves half-finished work
  behind a released claim.

- **An API version mismatch is a warning, not a rejection.** stripe-go rejects such an event by
  default; caisse overrides that. An endpoint created in the dashboard takes the account's API
  version, which drifts from the SDK's on its own schedule, and failing closed there means
  answering 400 to every delivery — no order ever fulfilled, over a version string.

- **Stripe messages do not leave the package**, except card declines. Those are written for the
  cardholder; everything else can name internal identifiers and becomes a generic `internal`
  error with the cause kept for the logs.

- **The Go floor is 1.24**, matching `tronc`. The `pg` tests pin pgx to v5.8.0 for it: v5.9
  raised its own floor to 1.25, and a test-only dependency has no business dictating what a
  consumer must run.
