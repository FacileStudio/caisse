# caisse — Development

Local setup, the quality gate, how to test a webhook without Stripe, and how the module is
versioned.

## Prerequisites

- Go 1.24 or newer. The `go.mod` floor is 1.24 and CI builds on exactly that, so a 1.25-only
  construct fails there rather than in a consumer still on the floor.
- PostgreSQL 14 or newer, for the `pg` tests. Optional locally; they skip themselves.
- `golangci-lint` v2.12.2, optional. The gate skips the lint pass when it is not installed.

## Setup

```sh
git clone https://github.com/FacileStudio/caisse
cd caisse
mise run hooks
go test ./...
```

`mise run hooks` points `core.hooksPath` at `.githooks`, which runs the gate before every push.

## The quality gate

```sh
mise run check                  # gofmt, vet, test -race, lint
sh scripts/check.sh --no-lint   # skip the lint pass
sh scripts/check.sh --format    # rewrite Go sources in place
```

`scripts/check.sh` deliberately depends on nothing but a `go`, and is not invoked through mise:
`mise run` resolves every tool in the merged config before running any task body, so an
unrelated broken tool in a global config would take the gate down with it.

## Tests

Everything except `pg` runs with no external service. The Stripe API is stood up as an
`httptest` server, which is the only place the wire format is observable — asserting on the
form encoding is how the nested `line_items[0][price_data][unit_amount]` shape stays correct.

```sh
go test -race ./...
```

### The pg tests

They need a real PostgreSQL and skip themselves when `CAISSE_TEST_DATABASE_URL` is unset,
which is why CI sets it unconditionally: the dedupe SQL is the one piece that cannot be
verified any other way, and a silent skip would let it ship untested.

```sh
export CAISSE_TEST_DATABASE_URL='postgres://caisse:caisse@localhost:5432/caisse_test?sslmode=disable'
go test -race ./pg/
```

They truncate `caisse_events` on every run. Point them at a scratch database.

### Testing a webhook

`caisse.Sign` produces the `Stripe-Signature` header, so a handler can be driven end to end
with `httptest` and no Stripe account:

```go
body := []byte(`{"id":"evt_1","object":"event","type":"checkout.session.completed", …}`)
request := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
request.Header.Set("Stripe-Signature", caisse.Sign(secret, body, time.Now()))
```

Against a real account, `stripe listen --forward-to localhost:8080/api/stripe/webhook` prints a
`whsec_…` to use as `STRIPE_WEBHOOK_SECRET` for that session. It is a different secret from the
dashboard endpoint's; mixing them up produces a 400 on every delivery.

## CI

One job on Go 1.24 with a PostgreSQL 16 service: `gofmt -l`, `go vet`, `go test -race` with
coverage, then `golangci-lint`.

## Versioning

Semver, tagged from `main`. While on `v0`, a breaking change bumps the minor — the same rule
`tronc` follows. `pg` is a package of the root module, not a module of its own, so it shares
the root's tags.

The stripe-go major is part of the public contract only by implication: `caisse` hides every
Stripe type behind its own, so a consumer never imports stripe-go and a major bump here is not
a major bump there.

Record every change in [CHANGELOG.md](../CHANGELOG.md) before tagging.
