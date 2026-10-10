# llmgate cutover — runbook

The BEDROCK API-key sale class was cut over from the self-hosted Kiro-Go/Pool
gateway to DIRECT llmgate.app keys (client: `internal/llmgate`, config:
`internal/config/settings.go`). The kiro/fixed SKUs stay on the pool; every
key already sold keeps working on whichever gateway minted it, because the
per-row routing is keyed on `api_keys.backend` (`""` = pool, `"llmgate"` =
direct mint).

## Environment variables

All four follow the standard precedence: `settings.json` key (same name,
snake_case) overrides `.env`/ambient env; two carry a compiled default.

| Env var | settings.json key | Default | What it is |
| --- | --- | --- | --- |
| `LLMGATE_BASE_URL` | `llmgate_base_url` | `https://api.codezdev-shop.uk` | CONTROL-PLANE base the bot logs into (login, create/update/delete). |
| `LLMGATE_USERNAME` | `llmgate_username` | — (unset) | llmgate panel username. |
| `LLMGATE_PASSWORD` | `llmgate_password` | — (unset) | llmgate panel password. NEVER log it. |
| `LLMGATE_API_BASE` | `llmgate_api_base` | `https://api.codezdev-shop.uk` | PUBLIC completions base shown to CUSTOMERS in the delivery message (`{apibase}`). |

The bedrock product HIDES (rather than selling and auto-refunding) until the
base URL + username + password triple is complete. The pool settings
(`KIRO_GO_*`) are untouched by this gate: they still gate the kiro/fixed SKUs.

## Wire facts the code leans on (verified live)

- `credit_limit` is in credits, 1 credit = $1. `UpdateKey` SETS it
  ABSOLUTELY — a recharge is a read-modify-write, serialized per key by a
  bot-side mutex so two concurrent top-ups of one key cannot lose a write.
- `UpdateKey` must carry the key's current `name` or llmgate CLEARS it; the
  name is re-read from `KeyStats().Info.Name` inside the lock.
- Create structurally omits `expires_at` → keys never expire.
- Usage stats lag ~30-60s (async aggregation). `remaining = credit_limit −
  metered costs`. `credit_limit` 0 = unlimited (render ∞, refuse recharges).
- Customer-realm auth: `KeyStats` sends the customer's own key as Bearer; a
  401 there means the key itself is bad (never re-login-retried).

## Delivery message

llmgate-backend keys deliver (and re-reveal, Self-Service → 🔑 My API Keys)
the `DeliveredLlmgate` template, which carries BOTH endpoints:

- `{apibase}` — `LLMGATE_API_BASE`, the recommended completions base;
- `{base}` — the shop's public web (`kiroGoPublicURL()`), the legacy base
  that also works and still serves `/customer`, `/docs`, `/models`.

Old pool keys keep their original `Delivered`/`KiroDelivered`/`FixedDelivered`
wording byte-for-byte — the template is picked per row
(`apikeyDeliveredTemplate(c, class, backend)`), so delivery and 👁 reveal of
the same key can never disagree.

## What moved, what stayed

| Path | Pool (backend `""`) | llmgate (backend `"llmgate"`) |
| --- | --- | --- |
| Mint bedrock | — | `CreateKey` (`tg-<chat>-order-<des>`, credits, no expiry) |
| Mint kiro/fixed | `provision_customer` | — |
| Recharge | `recharge_api_key` (additive) | `UpdateKey` absolute RMW, per-key mutex |
| Stats (gate/My-Keys/refund) | `/admin/stats` batched | per-key `GET /api/v1/apikey/stats` (customer realm; My-Keys fetches ≤4 in parallel) |
| Revoke | `delete_api_key` (+ upstream-still-live alert) | `DELETE /api/v1/apikey/delete/{id}` (404 = already gone) |
| Refund maths | unchanged — both backends collapse into `keyLiveStats` | same |

There is no upstream-binding concept on the llmgate branch (the llmgate key
IS the upstream), so the pool's upstream-status logging and
"sold without upstream binding" owner alerts do not apply there.

## Migration of existing keys

Old pool keys and their recharges keep hitting the pool until retired. To
move a live pool bedrock key onto a direct llmgate key (preserving the pool
uuid + cleartext for a later pool-side revoke), run:

```
go run ./cmd/migrate-llmgate
```

See `cmd/migrate-llmgate` for its flags; it stamps `backend='llmgate'` and
preserves the original pool values in `pool_key_id`/`pool_api_key`, which is
what makes the move reversible by hand. Re-running is safe (already-migrated
rows are excluded by the `backend=''` filter).
