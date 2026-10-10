# Production migration runbook — pool keys → llmgate keys

One-time cutover for the codezdev shop: move every ACTIVE pool-backend
customer key onto a fresh llmgate key (credits carried 1:1, 30-day validity),
deploy the new bot, and DM every migrated customer their new key.

Three phases: **rehearsal (test rig, your chat only) → prod cutover →
notify**. Nothing below touches prod until phase 2, and phase 2 starts only
on an explicit GO.

## Inventory (from the last dry-run against prod data)

- 26 keys would migrate, Σ ≈ 1,014,510 credits — including several keys
  owned by the admin chat (999,796 / 9,997 / …); decide at run time whether
  to carry your own keys 1:1 or exclude them (see DECISIONS below).
- 64 exhausted keys skip (nothing to carry).
- 16 keys skip on pool-stats failures (deleted pool keys) — review the list
  in the dry-run output before applying.

## DECISIONS to confirm before phase 2

1. **Credit carry 1:1.** Pool credits were a MARKED-UP currency (pool rate
   card = llmgate cost × markup). Carrying 1:1 into llmgate credits (1 cr =
   $1 list-price usage) is GENEROUS to old keys. Confirm, or give a divisor.
2. **Your own keys.** The admin chat owns the biggest balances. Either carry
   them like everyone else's or exclude (migrate later by hand / let them
   ride the pool until retirement).
3. **30-day validity on migrated keys.** The tool mints with 30-day expiry
   (same as new sales); recharge extends. Customers who never recharge lose
   carried credits after 30 days — the notify message says so explicitly.

## Phase 1 — Rehearsal on the test rig (chat 7302701741 only)

Goal: run the REAL migration flow against the REAL prod data COPY and the
REAL llmgate account, scoped to the operator's telegram chat, then verify
everything from the test bot.

```bash
# on the server, as root
systemctl stop kiro-go-test                       # bot must not touch the db mid-migration
cp /root/kiro-go-prod/store.db /root/kiro-go-test/store.db   # fresh copy of prod data
cd /root/kiro-go-test

# env the tool needs (pool at loopback; llmgate via the dev host)
export KIRO_GO_BASE_URL=http://127.0.0.1:8081
export KIRO_GO_ADMIN_KEY=<from /root/kiro-go-prod/settings.json>
export LLMGATE_BASE_URL=https://dev.codezdev-shop.uk
export LLMGATE_USERNAME=zsecducna
export LLMGATE_PASSWORD=<llmgate password>

# 1) dry-run scoped to the operator's chat — prints the plan, writes NOTHING
./migrate-llmgate -db store.db -chat 7302701741

# 2) apply for real (creates REAL llmgate keys, journal at store.db.migrate-journal)
./migrate-llmgate -db store.db -chat 7302701741 -apply

# 3) restart the bot and verify FROM TELEGRAM as 7302701741:
systemctl start kiro-go-test
#   - Self-Service → My API Keys: the key now shows the NEW sk-llmgate-… value,
#     live stats (llmgate numbers), 30-day expiry
#   - 👁 reveal → delivery message with dev base URL + /customer /docs /models links
#   - recharge flow: headroom math against llmgate remaining; after payment the
#     key's credit_limit rises AND expiry extends +30d
#   - the OLD pool key still works (call the pool endpoint with it)
#   - dev.codezdev-shop.uk/customer accepts the new key (quota, logs)

# 4) notify rehearsal — the DM the customers will get, sent to YOU only:
export TELEGRAM_BOT_TOKEN=<test bot token from .env>
export LLMGATE_API_BASE=https://dev.codezdev-shop.uk
export LLMGATE_PUBLIC_URL=https://dev.codezdev-shop.uk
./migrate-notify -db store.db            # dry-run: prints recipients (your chat only)
./migrate-notify -db store.db -apply     # sends the real DM to your chat
```

### Rehearsal cleanup (before prod, so nothing is minted twice)

The rehearsal minted REAL llmgate keys for your chat's rows. Prod's rows are
still `backend=''`, so the prod run would mint those keys AGAIN. Either:

- **a) keep them** and skip your chat at prod time (`-chat` cannot exclude;
  instead note the des ids and accept a duplicate for your own keys —
  harmless, they're your credits), or
- **b) clean them up:** delete the llmgate keys listed in
  `store.db.migrate-journal` (COMMITTED lines carry `llmkey=<id>`), then
  reset the test db: `curl -X DELETE -H "Authorization: Bearer $LG_TOKEN"
  https://dev.codezdev-shop.uk/api/v1/apikey/delete/<id>` for each, and
  `cp /root/kiro-go-prod/store.db /root/kiro-go-test/store.db` again.

## Phase 2 — Prod cutover (explicit GO only)

Order matters: pages first (so delivery links resolve), then bot, then keys.

```bash
# 0) backups
systemctl stop kiro-go-prod
cd /root/kiro-go-prod
cp store.db store.db.backup-$(date +%Y%m%d-%H%M%S)-pre-cutover
cp codezdev-bot codezdev-bot.prev-$(date +%Y%m%d-%H%M%S)-pre-cutover

# 1) shop-web live on the prod domain (nginx): /customer /docs /models
#    /shop-api/* /model-status/* → 127.0.0.1:8090; everything else stays on
#    the pool (old keys keep calling /v1/* through it). Install the shop-web
#    unit with API_BASE=https://api.codezdev-shop.uk and
#    LLMGATE_USERNAME/PASSWORD for the hourly status refresh.

# 2) new bot binary + env
install -m 0755 codezdev-bot.staged-llmgate-30d codezdev-bot
cat >> .env <<'EOF'
LLMGATE_BASE_URL=https://api.codezdev-shop.uk
LLMGATE_USERNAME=zsecducna
LLMGATE_PASSWORD=<llmgate password>
LLMGATE_API_BASE=https://api.codezdev-shop.uk
LLMGATE_PUBLIC_URL=https://codezdev-shop.uk
LLMGATE_TOKEN_STORE=/root/kiro-go-prod/.llmgate-token.json
EOF

# 3) migrate: dry-run first, READ IT, then apply
export KIRO_GO_BASE_URL=http://127.0.0.1:8081
export KIRO_GO_ADMIN_KEY=<from settings.json>
export LLMGATE_BASE_URL=https://api.codezdev-shop.uk
export LLMGATE_USERNAME=zsecducna
export LLMGATE_PASSWORD=<llmgate password>
./migrate-llmgate -db store.db            # plan only
./migrate-llmgate -db store.db -apply     # real keys, journal written

# 4) bot back up
systemctl start kiro-go-prod

# 5) verify: buy a fresh key end-to-end, recharge it, check a migrated key's
#    My-Keys view, confirm the pool still serves OLD keys.

# rollback (if anything is wrong):
systemctl stop kiro-go-prod
cp store.db.backup-<ts>-pre-cutover store.db
cp codezdev-bot.prev-<ts>-pre-cutover codezdev-bot
systemctl start kiro-go-prod
# (llmgate keys minted by the aborted run stay live — delete via the journal
#  list, same as rehearsal cleanup)
```

## Phase 3 — Notify all customers

```bash
cd /root/kiro-go-prod
export TELEGRAM_BOT_TOKEN=<prod bot token>
export LLMGATE_API_BASE=https://api.codezdev-shop.uk
export LLMGATE_PUBLIC_URL=https://codezdev-shop.uk
./migrate-notify -db store.db          # dry-run: recipient list, no keys printed
./migrate-notify -db store.db -apply   # DMs every migrated customer their new key
```

- Idempotent: rows are stamped `migrated_notified_ts` on success; re-run to
  retry only the failures.
- 1 send/second; 429/5xx retried 3×; failures exit code 1 and stay retryable.
- Message (EN/VI/ZH per the customer's order language) carries the new key,
  base URL, 30-day-validity + recharge-extends note, and links to
  /customer /docs.

## Aftercare

- Watch llmgate's key list (52-model status page + apikey list) for a few hours.
- Keep Pool-Go running for old keys until you decide to retire it; the pool
  key of every MIGRATED row is preserved in `api_keys.pool_key_id/pool_api_key`
  and is auto-revoked if that customer ever refunds.
- Stragglers who never read the DM: their old key keeps working; the bot's
  My-Keys reveal already shows the new key.
