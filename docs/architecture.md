# caisse — Architecture

How a payment travels through `caisse`, why the webhook path looks the way it does, and what
the package deliberately refuses to own.

## Topology

```
Customer ──▶ your API ──▶ caisse.Checkout ──▶ Stripe
                                                 │
                                     hosted checkout page
                                                 │
                            customer pays ───────┤
                                                 ▼
Stripe ──▶ POST /api/stripe/webhook ──▶ caisse webhook handler
                                             │
                              ┌──────────────┼──────────────┐
                              ▼              ▼              ▼
                        verify signature  EventStore    your Hooks
                        (HMAC + window)   (Postgres)    (fulfilment)
```

The customer's browser never carries a payment result you can trust. `SuccessURL` is a page
anyone can navigate to directly; the webhook is the only statement Stripe signs.

## Components

| Piece | Job |
|---|---|
| `Client` | Wraps `stripe.Client`. Validates locally, then calls one of four Stripe endpoints |
| `Webhook` | Verifies, dedupes, dispatches, and answers Stripe with a status it can act on |
| `EventStore` | Remembers which events were handled. Implemented by `pg` and `MemoryStore` |
| `Hooks` | The app's fulfilment code. Everything caisse does not know about |

## Payment lifecycle

1. `Checkout` validates the request before any network call — reference, lines, amounts,
   currency, absolute redirect URLs, metadata limits. A malformed request never costs an API
   call and never reaches Stripe with a message only Stripe can explain.
2. It derives an idempotency key by hashing the request, and sends it as `Idempotency-Key`.
   An identical retry returns the session Stripe already opened; a different request gets a
   new one. Neither can produce Stripe's `idempotency_error`.
3. Your `Reference` goes out three ways: as `client_reference_id`, in session metadata, and in
   payment intent (or subscription) metadata under `caisse_reference`.
4. The customer pays on Stripe's page. Stripe posts an event.
5. The handler verifies, claims, dispatches, confirms.

### Why the reference is written three times

A `charge.refunded` event describes a charge. It has no `client_reference_id` — that field only
exists on the checkout session. Without the reference copied onto the payment intent, a refund
issued from the Stripe dashboard arrives with nothing that points back at an order, and the app
has to keep its own Stripe-id-to-order map to survive. Copying it removes that table.

## The webhook path, step by step

```
POST /api/stripe/webhook
  │
  ├─ not POST?                        → 405
  ├─ body over MaxWebhookBytes?       → 400
  ├─ signature invalid or too old?    → 400   (handler never runs)
  ├─ Store.Begin says already seen?   → 200   (handler never runs)
  ├─ Store.Begin errored?             → 500   (Stripe retries)
  ├─ handler returned an error?       → 500   after Store.Fail   (Stripe retries)
  ├─ handler panicked?                → 500   after Store.Fail   (Stripe retries)
  └─ otherwise                        → 200   after Store.Done
```

Three decisions in there are worth stating out loud.

**A 500 is a request to retry.** Stripe re-delivers a failed webhook for about three days with
a widening backoff. Returning 500 for a transient failure is how an order still gets fulfilled
after the database comes back. Returning 500 for a permanent one is how an endpoint stays red
for three days — swallow those and log instead.

**The claim is released on failure and on panic.** If a failed attempt left the event claimed,
every retry would find it taken and do nothing. The order would silently never ship, and no
error would ever be raised again. The panic case is the same failure wearing a disguise, which
is why the dispatch runs behind a `recover`.

**The handler context outlives the request.** Once an event is claimed, the work runs under
`context.WithoutCancel` plus a timeout. If Stripe hangs up at its own 30-second limit, the
handler still finishes and still records the event — and Stripe's retry then finds it already
done. Cancelling instead would leave half-finished work behind a released claim.

## The dedupe ledger

`EventStore` is three methods bracketing one delivery: `Begin` claims, `Done` confirms, `Fail`
releases. The `pg` implementation is one table:

```
caisse_events
  id          text primary key    Stripe's evt_… identifier
  status      text                'pending' while a handler runs, 'done' after
  claimed_at  timestamptz         when the claim was taken
  done_at     timestamptz         when it was confirmed
```

`Begin` is a single statement — an insert with `ON CONFLICT DO UPDATE … WHERE … RETURNING` —
so two replicas receiving the same delivery cannot both win; PostgreSQL settles it and the
loser gets no row back.

The `WHERE` clause is also what reclaims a stale claim. A process killed between `Begin` and
`Done` leaves `status = 'pending'` forever, and without reclamation every Stripe retry would
bounce off that row: the order never ships and nothing ever complains. A claim older than
`StaleAfter` (15 minutes by default) is therefore takeable again. Keep that window comfortably
longer than the slowest handler, or a retry starts work that is still running.

`Fail` deletes rather than flags: the row's only job is to say "somebody has this, or somebody
finished it", and a failed attempt means neither.

Handled rows are worth keeping until Stripe's retry window closes, and worth nothing after.
`Purge` drops them; run it from a daily job.

## What caisse does not own

- **Your data.** No schema beyond the event ledger, no migration, no ORM.
- **Fulfilment semantics.** `Reference` is a string. caisse never dereferences it.
- **Idempotent handlers.** The store keeps Stripe's retries from reaching a handler twice.
  Nothing keeps a handler from being interrupted halfway by a deploy, so handlers must still
  be safe to re-run.
- **The catalogue.** Prices live in your database; Stripe gets amounts or a `price_…` you
  already created. Stripe is not the source of truth for what you sell.

## API version drift

stripe-go pins the API version it was generated against and, by default, rejects any event
carrying a different one. `caisse` overrides that. An endpoint created in the Stripe dashboard
takes the account's API version, which drifts from the SDK's on its own schedule, and failing
closed there means answering 400 to every delivery — no order ever fulfilled, over a version
string. The signature is still valid and the fields caisse reads have been stable for years, so
it processes the event and logs one warning naming both versions.
