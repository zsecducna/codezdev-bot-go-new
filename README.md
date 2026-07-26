# codezdev_bot-go

Go rewrite of the Kiro Telegram shop bot (Python original: `/Users/z/Desktop/Kiro`).
Drop-in replacement: runs against the existing `store.db`, `accounts/` folder tree,
`settings.json`, `.env` — zero data migration.

## Layout

- `cmd/bot` — entry point: config load, boot guards, long-poll loop, workers.
- `internal/tg` — hand-rolled Telegram Bot API client (stdlib only).
- `internal/store`, `internal/refund` — SQLite (modernc.org/sqlite): balances,
  orders, api_keys, referrals, refund requests.
- `internal/stock` — folder-state stock machine (`active/ → reserved/ → sold/`).
- `internal/pay/{sepay,paypal,binance,tron,bep20}` — payment providers.
- `internal/kirogo` — Kiro-Go admin/customer API client.
- `internal/provision`, `internal/subproc` — account provisioning via child
  processes (browser logins stay Python/Node).

## Develop

```bash
go build ./...
go vet ./...
go test ./...
```

Local test: put `TELEGRAM_BOT_TOKEN` in `.env` (gitignored). Never deploy to
production without an explicit ask.
