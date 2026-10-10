// Command migrate-notify is the customer-facing second half of the
// Kiro-Go/Pool → llmgate.app key cutover: AFTER cmd/migrate-llmgate -apply
// has moved the rows, this tool DMs every migrated customer their NEW
// llmgate key through the bot's own Telegram token — the same bot identity
// they already talk to — and stamps api_keys.migrated_notified_ts so a
// re-run skips everyone already served (the stamp IS the delivery record;
// a customer whose DM failed stays unstamped and is retried next run).
//
// Run ONLINE (unlike migrate-llmgate, this tool only talks to Telegram and
// its own store, and per-row failures never abort the fan-out), but still
// with care: it sends REAL messages to REAL customers.
//
// Usage:
//
//	migrate-notify -db store.db                     # dry run: prints one line per recipient (chat, lang, credits, order — NO key), sends nothing
//	migrate-notify -db store.db -apply              # the real fan-out: DM + stamp, per recipient
//	migrate-notify -db store.db -apply -limit 50    # slice the backlog
//
// Configuration (env only, no flags for secrets):
//   - TELEGRAM_BOT_TOKEN — the bot's own token; REQUIRED for -apply, unused
//     by a dry run.
//   - LLMGATE_API_BASE — the API endpoint the DM tells customers to call
//     (default https://api.codezdev-shop.uk).
//   - LLMGATE_PUBLIC_URL — the human-facing site the DM links for the
//     quota/docs pages; falls back to the same base when unset.
//
// Exit codes: 0 when every recipient was served (or it was a dry run);
// 1 when any DM failed (or the run itself errored); 2 on configuration
// errors — missing -db, or -apply without TELEGRAM_BOT_TOKEN — detected
// BEFORE anything is sent.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"codezdev-bot/internal/store"
)

// defaultShopAPIBase is where the DM points customers' API traffic when
// neither LLMGATE_API_BASE says otherwise: the shop's own gateway front door.
const defaultShopAPIBase = "https://api.codezdev-shop.uk"

// production pacing constants: 1s idle between sends keeps the fan-out under
// Telegram's per-bot flood limits; 3s is the backoff when a 429 does not
// document a retry_after (and for 5xx/transport retries). Fields on the
// Notifier so tests can shrink both to ~zero.
const (
	prodSendPause = 1 * time.Second
	prodRetryWait = 3 * time.Second
)

// config is the fully resolved configuration after the env fallbacks.
type config struct {
	dbPath  string
	apply   bool
	limit   int
	token   string // the bot's Telegram token (empty is legal only for a dry run)
	apiBase string
	pubBase string
}

// errConfig marks a configuration problem — main maps it to exit 2 before
// any store is opened or any message queued.
var errConfig = errors.New("configuration error")

// resolveConfig applies the env fallback table as a plain function (tests
// drive it with a fake getenv, never os.Environ):
//   - -db is REQUIRED outright — same foot-gun guard as migrate-llmgate: a
//     bare run must never silently pick (or CREATE) a store.db beside
//     wherever the operator happens to be;
//   - TELEGRAM_BOT_TOKEN is required only for -apply — a dry run must work
//     on a machine with no token at all;
//   - LLMGATE_API_BASE defaults to the shop's gateway; LLMGATE_PUBLIC_URL
//     falls back to whatever apiBase resolved to (one deployment serving
//     both roles is the common shape).
func resolveConfig(dbPath string, apply bool, limit int, getenv func(string) string) (config, error) {
	cfg := config{
		dbPath:  dbPath,
		apply:   apply,
		limit:   limit,
		token:   getenv("TELEGRAM_BOT_TOKEN"),
		apiBase: getenv("LLMGATE_API_BASE"),
		pubBase: getenv("LLMGATE_PUBLIC_URL"),
	}
	if cfg.dbPath == "" {
		return cfg, fmt.Errorf("%w: -db is required (path to store.db)", errConfig)
	}
	if cfg.apiBase == "" {
		cfg.apiBase = defaultShopAPIBase
	}
	if cfg.pubBase == "" {
		cfg.pubBase = cfg.apiBase
	}
	// The token gate: apply mode cannot send without it; a dry run never
	// needs one, so the same missing env is fine there.
	if cfg.apply && cfg.token == "" {
		return cfg, fmt.Errorf("%w: -apply requires TELEGRAM_BOT_TOKEN to be set", errConfig)
	}
	return cfg, nil
}

// main wires flags → env fallbacks → store + notifier → Run, and owns the
// process exit codes: 0 only when the whole backlog was served (or a dry
// run printed cleanly), 1 on any failed DM or run error, 2 on configuration
// errors caught before anything was sent.
func main() {
	dbPath := flag.String("db", "", "path to store.db — REQUIRED")
	apply := flag.Bool("apply", false, "actually DM each migrated customer their new key and stamp the row. Without this flag: dry run — prints one line per recipient (chat, lang, credits, order; never the key) and sends nothing")
	limit := flag.Int("limit", 0, "notify at most N recipients this run (oldest first); 0 = all unnotified")
	flag.Parse()

	// Resolve BEFORE opening anything: a config problem must exit 2 with no
	// side effects, and the token gate inside fires only in apply mode.
	cfg, err := resolveConfig(*dbPath, *apply, *limit, os.Getenv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "migrate-notify: %v\n", err)
		os.Exit(2)
	}

	logger := slog.Default()

	// The store is opened READ-WRITE even for a dry run, exactly like
	// migrate-llmgate: store.Open performs the idempotent schema bring-up
	// (the migrated_notified_ts column must exist for this binary to even
	// list the queue), and "dry run writes nothing" refers to the tool's own
	// writes — stamps — not the schema bring-up every consumer performs.
	st, err := store.Open(cfg.dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "migrate-notify: open store: %v\n", err)
		os.Exit(1)
	}
	defer st.Close()

	n := &Notifier{
		Store:     st,
		Log:       logger,
		Out:       os.Stdout,
		HTTP:      &http.Client{Timeout: 30 * time.Second},
		TgBase:    telegramAPIBase,
		Token:     cfg.token,
		APIBase:   cfg.apiBase,
		PubBase:   cfg.pubBase,
		SendPause: prodSendPause,
		RetryWait: prodRetryWait,
	}
	counts, err := n.Run(context.Background(), cfg.apply, cfg.limit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "migrate-notify: %v\n", err)
		os.Exit(1)
	}
	// Exit 1 when any recipient was missed: the backlog semantics make a
	// re-run safe (failed rows are still unstamped), but THIS run's contract
	// — everyone served — was not met.
	if counts.Failed > 0 {
		fmt.Fprintf(os.Stderr, "migrate-notify: %d recipient(s) failed — re-run to retry them\n", counts.Failed)
		os.Exit(1)
	}
}
