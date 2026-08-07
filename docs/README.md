# caisse — Documentation

| Page | What's in it |
|---|---|
| [Architecture](architecture.md) | Payment lifecycle, the webhook path, the dedupe ledger, what caisse refuses to own |
| [Configuration](configuration.md) | Every environment variable and `Config` field, and their traps |
| [Development](development.md) | Local setup, the quality gate, testing webhooks, versioning |
| [API](api.md) | Every exported symbol, package by package |

`caisse` is a library. It ships no service and has no deployment of its own — it is deployed by
whatever app imports it, which is why there is no `deployment.md` here.

Release history lives in [CHANGELOG.md](../CHANGELOG.md), at the repo root.

Back to the [README](../README.md).
