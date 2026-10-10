// Command migrate-llmgate is the one-shot data step of the Kiro-Go/Pool →
// llmgate.app key cutover: for every eligible pool-backend api_keys row it
// mints a direct llmgate.app key carrying the row's REMAINING pool credits,
// verifies the mint, and commits the row's migration (pool values preserved
// in pool_key_id/pool_api_key, key_id/api_key repointed at the llmgate key,
// backend='llmgate'). Old pool keys are NOT revoked here — customers keep
// them working until the pool is retired; revocation of a healthy key is a
// later, separate decision needing the preserved pool_key_id/pool_api_key
// columns. ONE exception (business rule 2026-10-10): EXHAUSTED keys
// (remaining <= 0) are DELETED from the pool under -apply — there is no
// customer value left to protect, and the pool must not accumulate dead
// keys. Those deletes are best-effort: a 404 is fine, any other failure is
// counted (failed-exhausted-delete) and the run continues.
//
// Run OFFLINE, with the bot process stopped (same posture as migrate-usdt):
// this tool does not gate against a live writer beyond SQLite's own file
// locking, and a concurrent bot settle could race the per-row commit (the
// store's WHERE backend=” guard makes that race SAFE — the loser's llmgate
// key is orphaned and CRITICAL-logged — but a quiet database is still the
// only sane way to run a one-shot migration).
//
// Usage:
//
//	migrate-llmgate -db store.db                        # dry run: reads pool stats, prints the plan (incl. DELETE-EXHAUSTED lines), writes/mints/deletes nothing
//	migrate-llmgate -db store.db -apply                 # the real migration, after a 3s Ctrl-C-to-abort window; exhausted pool keys are also deleted
//	migrate-llmgate -db store.db -chat 12345 -apply     # REHEARSAL: same flow, scoped to ONE telegram chat only
//
// Both upstream credentials come from flags or their env fallbacks:
// -pool-base/-pool-key (KIRO_GO_BASE_URL / KIRO_GO_ADMIN_KEY) address the
// Kiro-Go pool gateway for live remaining-credit stats; -llmgate-base/
// -llmgate-user/-llmgate-pass (LLMGATE_BASE_URL / LLMGATE_USERNAME /
// LLMGATE_PASSWORD) address llmgate.app for the mints. If either upstream is
// not fully configured the tool exits 2 BEFORE touching the database.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"time"

	"codezdev-bot/internal/kirogo"
	"codezdev-bot/internal/llmgate"
	"codezdev-bot/internal/store"
)

// defaultLlmgateBase is the llmgate base URL used when neither -llmgate-base
// nor LLMGATE_BASE_URL says otherwise — the shop's own llmgate deployment,
// taken from the llmgate package's own constant so the two can never drift.
const defaultLlmgateBase = llmgate.DefaultBaseURL

// abortDelay is the pause between printing the migration plan and starting
// to mint in -apply mode: the operator's last chance to Ctrl-C before the
// first real key is created. A field on Migrator (not a const here) purely
// so tests can shrink it to zero; production always passes 3s.
const abortDelay = 3 * time.Second

// main wires flags → env fallbacks → store + both upstream clients → Run,
// and owns process exit codes: 0 only when the run finished with zero
// failed-mint/failed-verify/skipped-race/failed-exhausted-delete rows,
// 1 on any error or failure count, 2 when an upstream client is not fully
// configured.
func main() {
	dbPath := flag.String("db", "", "path to store.db — REQUIRED")
	apply := flag.Bool("apply", false, "actually mint llmgate keys and migrate rows, and DELETE exhausted pool keys (remaining <= 0, best-effort). Without this flag: dry run — reads and a plan printout only, nothing is written, minted, or deleted")
	limit := flag.Int("limit", 0, "migrate at most N rows this run (oldest first); 0 = all eligible rows")
	// REHEARSAL SCOPE: -chat restricts BOTH the eligible count and the listed
	// rows to one telegram chat_id, so the operator can run the ENTIRE
	// mint→verify→commit flow against their own account before pointing it at
	// every customer. 0 (unset) keeps the unscoped behavior — the flag is a
	// pure filter on the same WHERE, never a different code path.
	chat := flag.Int64("chat", 0, "rehearsal scope: only migrate rows belonging to this telegram chat id (0 = all eligible rows)")
	poolBase := flag.String("pool-base", "", "Kiro-Go pool admin base URL (fallback env KIRO_GO_BASE_URL)")
	poolKey := flag.String("pool-key", "", "Kiro-Go admin key (fallback env KIRO_GO_ADMIN_KEY)")
	lgBase := flag.String("llmgate-base", "", "llmgate.app base URL (fallback env LLMGATE_BASE_URL, default "+defaultLlmgateBase+")")
	lgUser := flag.String("llmgate-user", "", "llmgate username (fallback env LLMGATE_USERNAME)")
	lgPass := flag.String("llmgate-pass", "", "llmgate password (fallback env LLMGATE_PASSWORD)")
	flag.Parse()

	// -db is REQUIRED with no default: a bare run must never silently pick a
	// store.db (or worse, CREATE one) next to wherever the operator happens
	// to be. store.Open would happily create a fresh empty database, and an
	// empty "successful" migration of 0 rows is exactly the silent foot-gun
	// this check exists to stop.
	if *dbPath == "" {
		fmt.Fprintln(os.Stderr, "migrate-llmgate: -db is required (path to store.db)")
		os.Exit(1)
	}

	// Flag → env → default resolution, as a plain function so tests can
	// exercise the fallback table directly without mutating os.Environ.
	cfg := resolveSettings(settingsInput{
		poolBase: *poolBase, poolKey: *poolKey,
		lgBase: *lgBase, lgUser: *lgUser, lgPass: *lgPass,
	}, os.Getenv)

	logger := slog.Default()

	// The store is opened READ-WRITE even for a dry run (store.Open runs the
	// idempotent schema migrations, which the llmgate-cutover columns need if
	// this binary is ever first to touch an older prod DB). "Dry run writes
	// nothing" refers to the MIGRATION's own writes — rows minted/committed —
	// not the schema bring-up every store consumer performs at open.
	st, err := store.Open(*dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "migrate-llmgate: open store: %v\n", err)
		os.Exit(1)
	}
	defer st.Close()

	// Kiro-Go client: Stats is an ADMIN call, so it needs only the base URL +
	// admin key (ConfiguredBase) — no AccountID exists for this tool to send,
	// and Stats never reads one. Requiring the full bedrock-sale Configured()
	// here would demand a credential this tool has no flag for.
	pool := kirogo.New(kirogo.Config{BaseURL: cfg.poolBase, AdminKey: cfg.poolKey}, logger)

	// llmgate client per the fixed internal/llmgate contract: New(Config{
	// BaseURL, Username, Password }, logger).
	gate := llmgate.New(llmgate.Config{
		BaseURL:  cfg.lgBase,
		Username: cfg.lgUser,
		Password: cfg.lgPass,
	}, logger)

	// GUARD: both upstreams must be usable BEFORE the plan is even printed —
	// discovering a missing llmgate password after the first mint is the
	// failure shape this exit-2 gate exists to make impossible. For the pool
	// side, ConfiguredBase (base+admin key) is the exact capability Stats
	// needs; for llmgate, Configured (base+username+password) is the login
	// pair CreateKey needs.
	if !pool.ConfiguredBase() || !gate.Configured() {
		fmt.Fprintln(os.Stderr, "migrate-llmgate: upstreams not configured —")
		if !pool.ConfiguredBase() {
			fmt.Fprintln(os.Stderr, "  pool (Kiro-Go): set -pool-base/-pool-key or KIRO_GO_BASE_URL/KIRO_GO_ADMIN_KEY")
		}
		if !gate.Configured() {
			fmt.Fprintln(os.Stderr, "  llmgate: set -llmgate-base/-llmgate-user/-llmgate-pass or LLMGATE_BASE_URL/LLMGATE_USERNAME/LLMGATE_PASSWORD")
		}
		os.Exit(2)
	}

	// Ctrl-C aborts in-flight HTTP calls (and the pre-apply window) cleanly
	// instead of leaving them to a hard kill mid-mint.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	m := &Migrator{
		Store: st,
		Pool:  pool,
		Gate:  gate,
		Log:   logger,
		Out:   os.Stdout,
		// The durable mint trace lives beside the database it talks about:
		// <db-path>.migrate-journal. Read by the next apply-mode start to
		// warn about MINTINGs an interrupted run never closed.
		JournalPath:   *dbPath + ".migrate-journal",
		AbortDelay:    abortDelay,
		PreCommitHook: nil, // test-only seam; production never sets it
	}
	counts, err := m.Run(ctx, *apply, *limit, *chat)
	if err != nil {
		fmt.Fprintf(os.Stderr, "migrate-llmgate: %v\n", err)
		os.Exit(1)
	}
	// Exit 0 only when nothing needs an operator's follow-up: a failed mint,
	// a failed verify, a lost race, or a failed exhausted-key delete each
	// leave either a row unmigrated, an orphaned llmgate key, or an
	// exhausted pool key that should be gone behind them, and must read as
	// a failed run even though the tool did its best per-row.
	if counts.FailureCount() > 0 {
		os.Exit(1)
	}
}

// settingsInput is the flag-layer half of the settings table passed to
// resolveSettings — kept as a struct (not five loose params) so the
// flag→env→default precedence for one knob travels together.
type settingsInput struct {
	poolBase, poolKey      string
	lgBase, lgUser, lgPass string
}

// settings is the fully resolved upstream configuration after flag → env →
// default fallback.
type settings struct {
	poolBase, poolKey      string
	lgBase, lgUser, lgPass string
}

// resolveSettings applies the flag → env → default precedence for every
// upstream knob: an explicitly-passed flag always wins, an env var fills an
// unset flag, and only the llmgate base URL has a default (the shop's own
// deployment) — the pool base/key and the llmgate credentials have NO
// defaults on purpose, so a half-configured run trips the Configured() gate
// (exit 2) instead of minting against a guessed endpoint.
func resolveSettings(in settingsInput, getenv func(string) string) settings {
	// envOr: per-knob fallback helper — flag value when set, else the env var.
	envOr := func(flagVal, envName string) string {
		if flagVal != "" {
			return flagVal
		}
		return getenv(envName)
	}
	s := settings{
		poolBase: envOr(in.poolBase, "KIRO_GO_BASE_URL"),
		poolKey:  envOr(in.poolKey, "KIRO_GO_ADMIN_KEY"),
		lgBase:   envOr(in.lgBase, "LLMGATE_BASE_URL"),
		lgUser:   envOr(in.lgUser, "LLMGATE_USERNAME"),
		lgPass:   envOr(in.lgPass, "LLMGATE_PASSWORD"),
	}
	// The llmgate base is the one knob with a safe default: minting keys on
	// the shop's own llmgate deployment is the entire purpose of this tool,
	// and that endpoint is not a secret the way the credentials are.
	if s.lgBase == "" {
		s.lgBase = defaultLlmgateBase
	}
	return s
}
