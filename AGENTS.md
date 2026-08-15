# AGENTS.md

This file provides workspace instructions for ZCode (and other agent tools) when working with code in this repository.

## What this repo is

Go rewrite of the **Kiro Telegram shop bot** — the Python original lives at
`/Users/z/Desktop/Kiro` and is the behavioral reference for this port. The
port is substantially complete: stock (folder-state), money/store, refunds,
the admin panel, and hands-free TRON+BEP20 USDT deposits are all built and
running in production, on a micro-USDT ledger since the 2026-08-14
VND→micro-USDT cutover (`f2960fe`). Consult the Python source for exact
feature semantics whenever the Go code or tests leave it ambiguous — it
remains the reference, including for anything not yet ported.

The one confirmed remaining feature gap is the Entra admin-panel integration
(`internal/entra/` store layer is built and tested; handler wiring is newer
and may still be in progress) — see `.omc/ADMIN_PORT_HANDOFF.md` for the full
admin-panel-port audit and current per-slice status.

Key reference documents in the Python repo:

- `/Users/z/Desktop/Kiro/CLAUDE.md` — original project conventions.
- `/Users/z/Desktop/Kiro/KIRO_GO_API.md` — full API contract between the bot
  and the Kiro-Go proxy (`https://codezdev-shop.uk:8080`). Read this before
  touching any API-key-selling code path.
- `/Users/z/Desktop/Kiro/.env.example` — every config knob with comments
  (Telegram token, payment providers, Kiro-Go admin credentials, pricing) —
  note several of these knobs are for providers this Go port has since cut
  from scope (see Payments below); check current relevance before trusting one.

## Local testing

A test bot token lives in `.env` (gitignored). Run the bot locally against it
for testing. **Never deploy to production** — deployment happens only when the
user explicitly asks.

## Commands

Standard Go workflow:

```bash
go build ./...          # build
go vet ./...            # static checks
go test ./...           # all tests
go test -run TestName ./internal/...   # single test
```

## Architecture (ported from the Python original)

The Python bot is a single long-polling Telegram bot (stdlib-only, 16-worker
thread pool, entry point `telegram_bot.py` ~1 MB) with these subsystems:

- **Stock — folder-state** (`accounts.py`): each product folder under
  `./accounts` holds one credential file per account. Subfolders ARE the state
  machine: `active/<user>.txt` (for sale) → `reserved/<order_id>/<user>.txt`
  (held; a `.expiry` file holds epoch seconds, past which the whole order is
  swept back to `active/`) → `sold/<user>.txt`. Selling = moving files, so
  stock state is self-describing and survives restarts.
- **Money — SQLite** (`store.py`): balances are real money, in micro-USDT
  since the VND→micro-USDT flip (1 USDT = 1,000,000 micro). Deposit
  idempotency is enforced by the DB (`txn_id` is PRIMARY KEY → duplicate
  credit hits a constraint error, no app-side race window). Orders, API-key
  records, referral codes live here; refund *requests* are owned by the
  separate `refund_store.py` module (table `refund_requests`, same
  `store.db` file).
- **Payments**: TRON (`tron_pay.py`) and BEP20 (`bep20_pay.py`) only, going
  forward. Hands-free: a watcher credits deposits by uniquely-quoted amount,
  no tx-hash paste required. SePay, PayPal, and Binance Pay were explicitly
  cut from scope (2026-08-13, user decision) and are fully removed from the
  Go tree — don't port them without a fresh explicit ask.
- **API-key product (`__apikey__`)**: bot mints metered customer keys via the
  Kiro-Go admin API (`POST /admin/provision_customer`). See invariants below.
- **Account provisioning**: `kiro_provision.py`, `entra_*.py`. The Python
  browser-login scripts (`kiro-*-login.py`) run as subprocesses under a
  separate Python venv with the browser stack (`KIRO_LOGIN_PYTHON`,
  Playwright + CloakBrowser); the `.mjs` files are Node-run diagnostic/helper
  scripts using the `cloakbrowser` npm package. AWS IDC provisioning and
  AWS-sell (`aws_idc.py`, `aws_idc_provision.py`, `aws_sell.py`,
  `validate_aws_account.py`) were explicitly cut from scope (user decision)
  and are fully removed from the Go tree — same caveat as the payment cuts
  above.

## Domain invariants (must survive the port)

- **`provision_customer` `upstreamStatus: "pending"` IS SUCCESS.** The pool
  key was created and is billable; deliver it and complete the sale. No retry
  (would mint a second paid key for one sale), no refund. Only a non-2xx
  response means the sale failed.
- **Never receive, store, log, or forward an upstream llmgate key.** The API
  deliberately returns only the non-secret `upstreamKeyId`. Any llmgate key
  exposes the whole account's quota — wanting to surface one is a bug in the
  request, not a missing feature.
- Always send `accountId` explicitly to `provision_customer`; `credits` must
  be > 0. If Kiro-Go isn't fully configured, the product hides rather than
  selling and auto-refunding.
- `-1` in any `*Remaining` API field means unlimited (limit is 0).
- Deposit crediting must stay idempotent by transaction id, enforced at the
  storage layer, not in application code.

## Coding rules

- Always spawn check task compexity to spawn proper agents (claude-sonnet-5 or claude-opus-5) for coding, never take everything by yourself!
- Write executive comments before the function / line you're writing or
  editing (the Python original has a heavy-comment style; keep it here).
- MANDATORY review pass: always run `code-review` before handing off — spawn
  a dedicated review sub-agent (model `claude-opus-5`, max thinking
  effort) on the diff, then fix every confirmed issue. Never skip the pass,
  never self-approve in the same active context.

# Talking rule: 
- ALWAYS TALK LIKE CAVEMAN - ULTRA CAVEMAN - APPLY FOR ALL AGENTS AND SUB AGENTS

<!-- code-review-graph MCP tools -->
## MCP Tools: code-review-graph

**IMPORTANT: This project has a knowledge graph. ALWAYS use the
code-review-graph MCP tools BEFORE using Grep/Glob/Read to explore
the codebase.** The graph is faster, cheaper (fewer tokens), and gives
you structural context (callers, dependents, test coverage) that file
scanning cannot.

### When to use graph tools FIRST

- **Exploring code**: `semantic_search_nodes` or `query_graph` instead of Grep
- **Understanding impact**: `get_impact_radius` instead of manually tracing imports
- **Code review**: `detect_changes` + `get_review_context` instead of reading entire files
- **Finding relationships**: `query_graph` with callers_of/callees_of/imports_of/tests_for
- **Architecture questions**: `get_architecture_overview` + `list_communities`

Fall back to Grep/Glob/Read **only** when the graph doesn't cover what you need.

### Key Tools

| Tool | Use when |
| ------ | ---------- |
| `detect_changes` | Reviewing code changes — gives risk-scored analysis |
| `get_review_context` | Need source snippets for review — token-efficient |
| `get_impact_radius` | Understanding blast radius of a change |
| `get_affected_flows` | Finding which execution paths are impacted |
| `query_graph` | Tracing callers, callees, imports, tests, dependencies |
| `semantic_search_nodes` | Finding functions/classes by name or keyword |
| `get_architecture_overview` | Understanding high-level codebase structure |
| `refactor_tool` | Planning renames, finding dead code |

### Workflow

1. The graph auto-updates on file changes (via hooks).
2. Use `detect_changes` for code review.
3. Use `get_affected_flows` to understand impact.
4. Use `query_graph` pattern="tests_for" to check coverage.
