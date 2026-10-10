# llmgate cutover — runbook

The BEDROCK API-key sale class was cut over from the self-hosted Kiro-Go/Pool
gateway to DIRECT llmgate.app keys (client: `internal/llmgate`, config:
`internal/config/settings.go`). The kiro/fixed SKUs stay on the pool; every
key already sold keeps working on whichever gateway minted it, because the
per-row routing is keyed on `api_keys.backend` (`""` = pool, `"llmgate"` =
direct mint).

## Environment variables

All five follow the standard precedence: `settings.json` key (same name,
snake_case) overrides `.env`/ambient env; two carry a compiled default and
`LLMGATE_PUBLIC_URL` falls back to the pool's public-web chain (see its
table row).

| Env var | settings.json key | Default | What it is |
| --- | --- | --- | --- |
| `LLMGATE_BASE_URL` | `llmgate_base_url` | `https://api.codezdev-shop.uk` | CONTROL-PLANE base the bot logs into (login, create/update/delete). |
| `LLMGATE_USERNAME` | `llmgate_username` | — (unset) | llmgate panel username. |
| `LLMGATE_PASSWORD` | `llmgate_password` | — (unset) | llmgate panel password. NEVER log it. |
| `LLMGATE_API_BASE` | `llmgate_api_base` | `https://api.codezdev-shop.uk` | PUBLIC completions base shown to CUSTOMERS in the delivery message (`{apibase}`). |
| `LLMGATE_PUBLIC_URL` | `llmgate_public_url` | `KIRO_GO_PUBLIC_URL` → `KIRO_GO_BASE_URL` → the `LLMGATE_API_BASE` value | Host of the SHOP'S WEB PAGES (shop-web: dashboard, `/docs`, `/models`) linked as `{pubbase}` in the delivery message. Its own knob because on the dev rig the web pages live on a different host than the API. |

The bedrock product HIDES (rather than selling and auto-refunding) until the
base URL + username + password triple is complete. The pool settings
(`KIRO_GO_*`) are untouched by this gate: they still gate the kiro/fixed SKUs.

## Wire facts the code leans on (verified live)

- `credit_limit` is in credits, 1 credit = $1. `UpdateKey` SETS it
  ABSOLUTELY — a recharge is a read-modify-write, serialized per key by a
  bot-side mutex so two concurrent top-ups of one key cannot lose a write.
- `UpdateKey` must carry the key's current `name` or llmgate CLEARS it; the
  name is re-read from `KeyStats().Info.Name` inside the lock.
- Usage stats lag ~30-60s (async aggregation). `remaining = credit_limit −
  metered costs`. `credit_limit` 0 = unlimited (render ∞, refuse recharges).
- Customer-realm auth: `KeyStats` sends the customer's own key as Bearer; a
  401 there means the key itself is bad (never re-login-retried).

## Expiry rules (business rules 2026-10-10)

- **Mint = 30 days.** Every freshly minted llmgate key is created with
  `expires_at = time.Now().Add(llmgate.DefaultValidity)` — a flat 30-day
  window (`30 * 24 * time.Hour`, NOT a calendar month; revised 2026-10-10).
  The constant lives once in `internal/llmgate` and is shared by the mint,
  the recharge renewal and the migration tool so the three cannot drift.
  The legacy never-expire mint is gone; only an explicit zero time can
  still request it (nothing does).
- **Recharge RENEWS.** The read-modify-write's `UpdateKey` sends
  `expires_at = now+30 days` alongside the absolute new limit, so topping up
  an ACTIVE key extends its validity — the delivery message's
  "Recharging before expiry adds credits AND extends validity by another
  30 days" line is this write.
- **Expired keys cannot be recharged.** Refused at order time by the RC gate
  (`rcGate` `Err:"expired"` → the `RCExpired` notice: buy a new key) and
  again at delivery time (pre-check + in-lock re-check → auto-refund). The
  renewal is never a resurrection. `KeyUsage.Status()` gains the verdict
  `expired` (precedence: disabled > expired > exhausted > active; an expired
  UNLIMITED key is still just expired), and My-Keys renders it as
  `📅 expired`. Pool keys never expire and can never produce the verdict.
- `expires_at` rides the key-realm stats `info` ONLY when the key has one
  (verified live); `Key.Expiry()` parses the raw string (RFC3339, fractional
  seconds tolerated) and returns `false` for absent/unparsable.
- `cmd/migrate-llmgate` mints migrated keys with the same 30-day expiry,
  so migrated owners recharge-to-renew like everyone else.

## Delivery message

llmgate-backend keys deliver (and re-reveal, Self-Service → 🔑 My API Keys)
the `DeliveredLlmgate` template. Since the 2026-10-10 expiry rules it carries
exactly ONE endpoint plus the web-page links:

- `{apibase}` — `LLMGATE_API_BASE`, the completions base (the only Base URL
  shown; the legacy-endpoint line was REMOVED);
- `{pubbase}` — `LLMGATE_PUBLIC_URL`, the shop's WEB-PAGES host serving
  `/`, `/docs`, `/models` (dashboard, docs, pricing).

The template also states the 30-day validity and the recharge-to-renew
rule (see Expiry rules above).

Old pool keys keep their original `Delivered`/`KiroDelivered`/`FixedDelivered`
wording byte-for-byte — the template is picked per row
(`apikeyDeliveredTemplate(c, class, backend)`), so delivery and 👁 reveal of
the same key can never disagree.

## What moved, what stayed

| Path | Pool (backend `""`) | llmgate (backend `"llmgate"`) |
| --- | --- | --- |
| Mint bedrock | — | `CreateKey` (`tg-<chat>-order-<des>`, credits, now+30d expiry) |
| Mint kiro/fixed | `provision_customer` | — |
| Recharge | `recharge_api_key` (additive) | `UpdateKey` absolute RMW (limit + renewal), per-key mutex; expired keys refused |
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
