// Command migrate-llmgate's migration engine. main.go is the CLI wrapper
// (flags, env fallbacks, client wiring, exit codes); this file holds the
// per-row algorithm: read the row's live pool remaining credits from
// Kiro-Go, mint an llmgate.app key carrying exactly those credits, verify
// the mint against llmgate, and commit the row's migration through
// store.MigrateAPIKeyToLlmgate — the store's idempotent, race-safe commit
// point.
//
// The run is two-phase on purpose: a PLAN phase (reads only — pool stats +
// classification + a printed plan) followed, in -apply mode only, by an ACT
// phase (mint → verify → commit, per row). Sharing the plan phase between
// dry run and apply means a dry run prints exactly what an apply would do,
// from the same stats read, with zero second implementation to drift.
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"codezdev-bot/internal/kirogo"
	"codezdev-bot/internal/llmgate"
	"codezdev-bot/internal/store"
)

// Migrator bundles the collaborators the per-row algorithm needs. Zero
// magic: every field is wired explicitly by main (tests wire fakes/short
// delays instead), so the algorithm's inputs are all visible at the call
// site.
type Migrator struct {
	// Store is the read-write handle to store.db; only
	// ListAPIKeysToMigrate / CountAPIKeysToMigrate / MigrateAPIKeyToLlmgate
	// are called on it.
	Store *store.Store
	// Pool is the Kiro-Go admin client used for the live remaining-credit
	// read (Stats). Pool keys are NEVER revoked by this tool.
	Pool *kirogo.Client
	// Gate is the llmgate.app client used to mint (CreateKey) and verify
	// (KeyStats) the replacement keys.
	Gate *llmgate.Client
	// Log receives operational warnings (stderr via slog); the operator-
	// facing plan/progress/summary lines go to Out instead, so a piped run
	// keeps its human-readable transcript on stdout.
	Log *slog.Logger
	// Out receives the plan, per-row progress lines, and the summary.
	Out io.Writer
	// AbortDelay is the pause between printing the plan and minting the
	// first key in apply mode (3s in production; tests pass 0).
	AbortDelay time.Duration
	// PreCommitHook, when non-nil, runs immediately BEFORE each row's
	// MigrateAPIKeyToLlmgate call. Test-only seam for the lost-race path
	// (the store's WHERE backend='' guard cannot be raced deterministically
	// from a test without a second writer interleaved exactly there);
	// main.go never sets it. Precedent: migrate-usdt's pendingCountsTestHook.
	PreCommitHook func(row *store.APIKey)
	// JournalPath is the append-only trace file (<db-path>.migrate-journal
	// in production) recording every mint attempt and its terminal outcome,
	// so a hard crash between CreateKey and the commit leaves a DURABLE
	// pointer to the possibly-orphaned live key instead of zero trace (the
	// next apply-mode start scans it and warns loudly). Empty disables
	// journaling (unit tests that don't exercise the journal).
	JournalPath string
}

// Counts is the run's tally — every row lands in exactly one bucket, and
// the end-of-run summary + exit code derive from it. The skip buckets are
// decided in the PLAN phase (they need only the pool stats read); the
// failure buckets only exist in the ACT phase.
type Counts struct {
	// Eligible is the store's count of migratable rows at run start (the
	// "total eligible" headline of the plan) — informational only.
	Eligible int64
	// Planned / PlannedCredits are how many rows the plan classified as
	// mintable, and the Σ credits they would carry. In apply mode Planned
	// is also exactly the number of ACT attempts made.
	Planned        int
	PlannedCredits float64
	// Migrated: rows whose commit returned true — the only success bucket.
	Migrated int
	// SkippedStats: pool stats unreadable (error / no key matched /
	// non-finite value) — fail-closed, row left untouched.
	SkippedStats int
	// SkippedExhausted: remaining <= 0, or so small it rounds below the
	// 0.01 mint floor — nothing worth carrying to llmgate.
	SkippedExhausted int
	// SkippedUnlimited: remaining == -1 — unlimited keys must be
	// hand-migrated; never mint an unlimited llmgate key by tool.
	SkippedUnlimited int
	// FailedMint: llmgate.CreateKey errored — no llmgate key exists for
	// this row (nothing orphaned), tool continued.
	FailedMint int
	// FailedVerify: a key WAS minted but KeyStats could not confirm it —
	// an UNRECORDED llmgate key now exists; CRITICAL-logged with its id.
	FailedVerify int
	// SkippedRace: commit lost to a concurrent migration (returned false) —
	// row is migrated by someone else; the just-minted key is ORPHANED and
	// CRITICAL-logged with its id.
	SkippedRace int
}

// FailureCount is the exit-code input: the three buckets that leave either
// an unmigrated row or an orphaned/unrecorded llmgate key behind. Zero
// means every row ended migrated, deliberately skipped, or fail-closed
// BEFORE anything was minted.
func (c Counts) FailureCount() int {
	return c.FailedMint + c.FailedVerify + c.SkippedRace
}

// skipReason buckets a plan-phase skip; the empty string means "plannable".
type skipReason string

const (
	skipNone      skipReason = ""
	skipStats     skipReason = "stats"
	skipExhausted skipReason = "exhausted"
	skipUnlimited skipReason = "unlimited"
)

// planRow is one row's classified plan: the store row, the customer-
// favorable 2-decimal remaining credits to carry (valid only when Skip is
// skipNone), and, for skips, a human-readable detail for the plan line.
type planRow struct {
	Row       *store.APIKey
	Remaining float64
	Skip      skipReason
	Detail    string
}

// Run executes the whole migration: plan phase (always), then — only under
// apply — the abort window and the act phase. It returns the Counts tally;
// a non-nil error means the run could not complete (store failure after
// listing, or a commit error) and main must exit non-zero regardless of the
// counts. Per-row upstream failures NEVER abort the run; they only bump
// their bucket — one bad row must not strand the other hundreds.
func (m *Migrator) Run(ctx context.Context, apply bool, limit int) (Counts, error) {
	var c Counts

	// The plan headline: the store's own count of eligible rows (independent
	// of any -limit paging), so the operator sees the true backlog even when
	// slicing the run with -limit.
	eligible, err := m.Store.CountAPIKeysToMigrate()
	if err != nil {
		return c, fmt.Errorf("count eligible rows: %w", err)
	}
	c.Eligible = eligible

	// Step 0 — list the rows this run will consider, oldest first. limit<=0
	// means "all", but SQL "LIMIT 0" returns NOTHING, so 0 must become -1
	// (SQLite's no-limit sentinel) — the one translation this call needs.
	sqlLimit := limit
	if sqlLimit <= 0 {
		sqlLimit = -1
	}
	rows, err := m.Store.ListAPIKeysToMigrate(sqlLimit)
	if err != nil {
		return c, fmt.Errorf("list rows to migrate: %w", err)
	}

	// ---- PLAN PHASE (reads only; identical for dry run and apply) --------
	fmt.Fprintf(m.Out, "plan: %d eligible key(s) to migrate", eligible)
	if limit > 0 {
		fmt.Fprintf(m.Out, " (limit=%d applies)", limit)
	}
	fmt.Fprintln(m.Out)

	plans := make([]planRow, 0, len(rows))
	for _, row := range rows {
		pr := m.planRow(ctx, row)
		plans = append(plans, pr)
		// Bucket the skip NOW (not in the summary printer) so the counts are
		// the tally the plan phase actually produced — the same loop that
		// classifies is the loop that counts.
		switch pr.Skip {
		case skipNone:
			c.Planned++
			c.PlannedCredits += pr.Remaining
		case skipStats:
			c.SkippedStats++
		case skipExhausted:
			c.SkippedExhausted++
		case skipUnlimited:
			c.SkippedUnlimited++
		}
	}
	// ---- end plan phase ---------------------------------------------------

	if !apply {
		// DRY RUN: nothing beyond this point — no llmgate CreateKey call was
		// made (the act phase below is the only place that mints), no row
		// changed, and the store was only read.
		m.printSummary(c)
		fmt.Fprintf(m.Out, "DRY RUN — nothing was written; %d keys would be migrated, Σ credits %.2f\n",
			c.Planned, c.PlannedCredits)
		return c, nil
	}

	// ---- ABORT WINDOW: the last Ctrl-C point before the first mint -------
	fmt.Fprintf(m.Out, "proceeding with -apply in %s — Ctrl-C to abort\n", m.AbortDelay)
	if err := sleepCtx(ctx, m.AbortDelay); err != nil {
		return c, fmt.Errorf("aborted before apply: %w", err)
	}
	// ---- end abort window -------------------------------------------------

	// ---- DURABLE JOURNAL: the crash-window trace ---------------------------
	// Opened ONLY here (apply mode): a dry run never mints, so it must also
	// never write journal lines. At open, scan for MINTING entries from
	// earlier runs that never reached a terminal line — each one is a key
	// this tool asked llmgate to mint and then lost track of (hard crash,
	// kill -9, power loss). They are possible live billable orphans; the
	// operator gets one LOUD warning naming them before this run adds more.
	var j *journal
	if m.JournalPath != "" {
		j, err = openJournal(m.JournalPath)
		if err != nil {
			return c, fmt.Errorf("open journal %s: %w", m.JournalPath, err)
		}
		defer j.Close()
		interrupted, serr := j.scanInterrupted()
		if serr != nil {
			return c, fmt.Errorf("scan journal %s: %w", m.JournalPath, serr)
		}
		if len(interrupted) > 0 {
			fmt.Fprintf(m.Out, "⚠️ WARNING: %d journal MINTING entry(ies) with NO terminal line — possible orphaned llmgate key from an interrupted run; check llmgate for these names:\n",
				len(interrupted))
			for _, line := range interrupted {
				fmt.Fprintf(m.Out, "  %s\n", line)
			}
			m.Log.Error("migrate-llmgate: interrupted MINTING entries in journal — possible orphaned llmgate keys", "count", len(interrupted))
		}
	}
	// ---- end journal open ---------------------------------------------------

	// ---- ACT PHASE: mint → verify → commit, per plannable row ------------
	for _, pr := range plans {
		if pr.Skip != skipNone {
			continue // already bucketed + printed by the plan phase
		}
		// A non-nil error here is a STORE-level commit failure — fatal to the
		// run, with the summary still printed so the operator sees exactly
		// how far it got. Upstream (pool/llmgate) failures never reach this
		// return; they are counted and the run continues.
		if err := m.migrateRow(ctx, pr, &c, j); err != nil {
			m.printSummary(c)
			return c, err
		}
	}
	// ---- end act phase ----------------------------------------------------

	m.printSummary(c)
	return c, nil
}

// planRow (the method) classifies one store row into a planRow: it reads
// the row's live pool stats, applies the skip rules, computes the customer-
// favorable remaining-credit figure, and prints the row's plan line. This
// is steps 1-4 of the per-row algorithm; everything here is a READ.
func (m *Migrator) planRow(ctx context.Context, row *store.APIKey) planRow {
	// STEP 1 — read the pool's live stats for this row's key: by key uuid
	// (row.KeyID), falling back to the cleartext key (row.APIKey) when the
	// uuid lookup errors or matches nothing. Fail-closed: if neither
	// identifier yields exactly the key, the row is skipped rather than
	// guessed at — minting against a stale local credits figure would hand
	// the customer the wrong quota with no way to detect it later.
	stat, why := m.readPoolStats(ctx, row)
	if stat == nil {
		m.planSkipLine(row, skipStats, why)
		return planRow{Row: row, Skip: skipStats, Detail: why}
	}

	// STEP 2 — unlimited keys are a hard, loud stop: -1 means the pool
	// imposes no credit cap, and no llmgate key must ever be minted with an
	// unlimited quota by a tool. These need a human to decide the quota.
	if stat.CreditsRemaining.IsUnlimited() {
		why := "unlimited key — hand-migrate (never mint an unlimited llmgate key)"
		m.Log.Error("migrate-llmgate: unlimited pool key must be hand-migrated",
			"des", row.OrderID, "chat", row.ChatID, "pool", row.KeyID)
		m.planSkipLine(row, skipUnlimited, why)
		return planRow{Row: row, Skip: skipUnlimited, Detail: why}
	}

	// STEP 3 — exhausted (or negative, which the pool never sends but the
	// maths below should never silently honor): nothing to carry.
	remaining := float64(stat.CreditsRemaining)
	if remaining <= 0 {
		why := fmt.Sprintf("exhausted (remaining=%.2f)", remaining)
		m.planSkipLine(row, skipExhausted, why)
		return planRow{Row: row, Skip: skipExhausted, Detail: why}
	}

	// Fail-closed on non-finite stats: a NaN/Inf creditsRemaining decodes
	// cleanly from JSON and then poisons every comparison above (NaN fails
	// them all) straight into a NaN-credit mint. Bucketed with the other
	// "we could not read this row's real quota" skips, not exhausted — the
	// key may be fine; the DATA is not.
	if math.IsNaN(remaining) || math.IsInf(remaining, 0) {
		why := fmt.Sprintf("non-finite creditsRemaining (%v) — pool data corrupt", stat.CreditsRemaining)
		m.Log.Error("migrate-llmgate: non-finite pool remaining", "des", row.OrderID, "chat", row.ChatID, "pool", row.KeyID)
		m.planSkipLine(row, skipStats, why)
		return planRow{Row: row, Skip: skipStats, Detail: why}
	}

	// STEP 4 — round UP to 2 decimals, customer-favorable: the customer
	// bought N credits, the pool metered fractions against them, and the
	// carried quota must never round the residue away in the shop's favor
	// (see roundUp2; e.g. 1999.95 stays 1999.95, 10.001 becomes 10.01).
	// The <0.01 floor below is the mint-side dust guard: llmgate keys
	// worth less than one cent are noise — skipped as exhausted
	// (unreachable for any remaining>0, since ceil(r*100)>=1 then; it
	// exists to make a zero/near-zero result unrepresentable in the act
	// phase even if the rounding maths ever changes).
	remaining = roundUp2(remaining)
	if remaining < 0.01 {
		why := fmt.Sprintf("remaining %.4f rounds below the 0.01 mint floor", float64(stat.CreditsRemaining))
		m.planSkipLine(row, skipExhausted, why)
		return planRow{Row: row, Skip: skipExhausted, Detail: why}
	}

	// Plannable: print what an apply would do for this row, in the act
	// phase's own vocabulary (same name, same figure) so the plan IS the
	// preview of the mint.
	fmt.Fprintf(m.Out, "  plan des=%s chat=%d pool=%s remaining=%.2f → llmgate name=%s credits=%.2f\n",
		row.OrderID, row.ChatID, row.KeyID, remaining, llmKeyName(row), remaining)
	return planRow{Row: row, Remaining: remaining, Skip: skipNone}
}

// readPoolStats performs plan step 1's lookup: Stats by the row's pool uuid
// first, then — only if the uuid lookup errored or matched no key — Stats
// by the row's cleartext pool key. Returns the matched KeyStat, or nil plus
// a human reason covering every attempt (for the skip line and log).
func (m *Migrator) readPoolStats(ctx context.Context, row *store.APIKey) (*kirogo.KeyStat, string) {
	// Primary attempt: by uuid. A row with no stored uuid starts directly
	// at the cleartext fallback so the reason names what was actually tried.
	if row.KeyID != "" {
		stat, why := m.statsAttempt(ctx, kirogo.StatsRequest{ID: row.KeyID})
		if stat != nil {
			return stat, ""
		}
		if row.APIKey == "" {
			return nil, why
		}
		// FALLBACK: uuid unknown to the pool (or the call failed) — try the
		// cleartext key before declaring the row unreadable.
		m.Log.Warn("migrate-llmgate: pool stats by id failed, falling back to apiKey",
			"des", row.OrderID, "pool", row.KeyID, "reason", why)
	}
	if row.APIKey != "" {
		if stat, why := m.statsAttempt(ctx, kirogo.StatsRequest{APIKey: row.APIKey}); stat != nil {
			return stat, ""
		} else {
			return nil, why
		}
	}
	// No identifier at all: a live pool-backend row with neither uuid nor
	// cleartext key cannot be looked up on the pool by anything.
	return nil, "row has neither pool key id nor api key"
}

// statsAttempt is one Stats call and the fail-closed interpretation of its
// answer: a transport/API error, or a 2xx that matches ZERO keys, both mean
// "no data" (nil + reason). Multiple matches cannot happen for a unique-id
// filter, but the first entry is used rather than trusted — the pool
// filtered, we read.
func (m *Migrator) statsAttempt(ctx context.Context, req kirogo.StatsRequest) (*kirogo.KeyStat, string) {
	res, err := m.Pool.Stats(ctx, req)
	if err != nil {
		return nil, fmt.Sprintf("stats error: %v", err)
	}
	if len(res.Keys) == 0 {
		return nil, "no keys matched"
	}
	return &res.Keys[0], ""
}

// roundUp2 rounds a positive remaining-credits figure UP to 2 decimals —
// always in the CUSTOMER's favor: the residue the pool metered away in
// fractions of a cent must never be rounded away again on the llmgate side.
// math.Ceil (not round-half): 10.001 becomes 10.01, and the float artifact
// 1999.95*100 = 199994.999... ceils back onto exactly 1999.95.
func roundUp2(r float64) float64 {
	return math.Ceil(r*100) / 100
}

// llmKeyName builds the llmgate key name for one row — the SAME convention
// the pool mint used ("tg-<chat>-order-<des>", kirogo types.go), so an
// operator staring at the llmgate console can tie every migrated key back
// to its Telegram order without a database join.
func llmKeyName(row *store.APIKey) string {
	return "tg-" + strconv.FormatInt(row.ChatID, 10) + "-order-" + row.OrderID
}

// migrateRow is the ACT phase for one plannable row: steps 5-8 of the
// per-row algorithm (mint, verify, commit, report). Every UPSTREAM failure
// inside bumps a Counts bucket and returns nil — the tool continues with
// the next row. The only non-nil returns are a store-level commit ERROR and
// a journal write ERROR, both fatal to the whole run (Run aborts on them):
// a database whose commit state is unknown must not be papered over as one
// more skipped row, and a journal we cannot append to cannot be trusted to
// carry the orphan trace it exists for. j may be nil (journaling disabled).
func (m *Migrator) migrateRow(ctx context.Context, pr planRow, c *Counts, j *journal) error {
	row := pr.Row

	// JOURNAL: mint intent FIRST, durably, before the request leaves. If the
	// process dies anywhere between here and a terminal line below, the next
	// apply-mode start finds this MINTING unmatched and warns — the key may
	// be live on llmgate with zero trace anywhere else.
	if j != nil {
		if err := j.write(fmt.Sprintf("MINTING des=%s name=%s credits=%.2f",
			row.OrderID, llmKeyName(row), pr.Remaining)); err != nil {
			return fmt.Errorf("journal minting des=%s: %w", row.OrderID, err)
		}
	}

	// STEP 5 — mint the llmgate key carrying exactly the row's remaining
	// pool credits. An error means NO llmgate key exists (nothing to clean
	// up): count failed-mint, log, move on.
	created, err := m.Gate.CreateKey(ctx, llmKeyName(row), pr.Remaining)
	if err != nil {
		c.FailedMint++
		if j != nil {
			if jerr := j.write(fmt.Sprintf("FAILED-MINT des=%s", row.OrderID)); jerr != nil {
				return fmt.Errorf("journal failed-mint des=%s: %w", row.OrderID, jerr)
			}
		}
		fmt.Fprintf(m.Out, "  failed-mint des=%s chat=%d pool=%s credits=%.2f error=%v\n",
			row.OrderID, row.ChatID, row.KeyID, pr.Remaining, err)
		m.Log.Error("migrate-llmgate: llmgate mint failed", "des", row.OrderID, "chat", row.ChatID, "error", err)
		return nil
	}

	// STEP 6 — verify the mint before committing the row to it: KeyStats on
	// the freshly created key must succeed AND report the id CreateKey
	// returned. A failure here leaves an UNRECORDED llmgate key (it exists,
	// is live, and no api_keys row points at it) — that is a CRITICAL
	// operator action item, printed with everything needed to clean it up
	// by hand, never buried in a summary count alone.
	usage, err := m.Gate.KeyStats(ctx, created.APIKey)
	// usage==nil with err==nil is outside the llmgate contract, but a nil
	// deref inside a migration tool is its own failure class — folded into
	// the same failed-verify verdict instead of panicking.
	if err != nil || usage == nil || usage.Info.ID != created.ID {
		c.FailedVerify++
		if j != nil {
			if jerr := j.write(fmt.Sprintf("FAILED-VERIFY des=%s llmkey=%d",
				row.OrderID, created.ID)); jerr != nil {
				return fmt.Errorf("journal failed-verify des=%s: %w", row.OrderID, jerr)
			}
		}
		fmt.Fprintf(m.Out, "  CRITICAL failed-verify des=%s chat=%d — an llmgate key now exists UNRECORDED: id=%d credit_limit=%.2f (delete or record it by hand)\n",
			row.OrderID, row.ChatID, created.ID, created.CreditLimit)
		m.Log.Error("migrate-llmgate: mint verify failed — llmgate key exists unrecorded",
			"des", row.OrderID, "chat", row.ChatID, "llmgate_id", created.ID,
			"credit_limit", created.CreditLimit, "stats_error", err)
		return nil
	}

	// Test-only seam (see Migrator.PreCommitHook): fires exactly between
	// verify and commit, the one window the store's backend='' guard exists
	// to protect, so the lost-race path is reachable from a test.
	if m.PreCommitHook != nil {
		m.PreCommitHook(row)
	}

	// STEP 7 — commit the row's migration: the single idempotent UPDATE
	// that preserves the pool uuid/cleartext into pool_key_id/pool_api_key
	// and repoints key_id/api_key/credits at the llmgate key. false means
	// someone else migrated this row first — the ROW is safe, but this
	// run's just-verified llmgate key is now an ORPHAN nobody references:
	// CRITICAL, with its id for manual deletion.
	moved, err := m.Store.MigrateAPIKeyToLlmgate(
		row.OrderID, row.KeyID, row.APIKey,
		strconv.FormatInt(created.ID, 10), created.APIKey, pr.Remaining)
	if err != nil {
		// A database error at the commit point is NOT a per-row skip: the
		// llmgate key exists, is verified, and the row's true state is
		// unknown. Abort the run (Run prints the summary first) — a re-run
		// is safe by construction: backend='' excludes any row this commit
		// did land on. The journal's MINTING line is deliberately left
		// WITHOUT a terminal line here: the outcome really is unresolved,
		// and the next start's interrupted-scan must keep naming it.
		fmt.Fprintf(m.Out, "  CRITICAL commit-error des=%s — llmgate key id=%d exists; row state unknown: %v\n",
			row.OrderID, created.ID, err)
		return fmt.Errorf("commit des=%s: %w", row.OrderID, err)
	}
	if !moved {
		c.SkippedRace++
		if j != nil {
			if jerr := j.write(fmt.Sprintf("RACE des=%s llmkey=%d",
				row.OrderID, created.ID)); jerr != nil {
				return fmt.Errorf("journal race des=%s: %w", row.OrderID, jerr)
			}
		}
		fmt.Fprintf(m.Out, "  CRITICAL skipped-race des=%s — row already migrated elsewhere; the just-minted llmgate key is ORPHANED: id=%d (delete it by hand)\n",
			row.OrderID, created.ID)
		m.Log.Error("migrate-llmgate: lost the commit race — llmgate key orphaned",
			"des", row.OrderID, "llmgate_id", created.ID)
		return nil
	}

	// STEP 8 — success: one line per row, in the vocabulary the whole run
	// uses (des/chat/pool on the left, llmgate id/credits on the right).
	// The journal's COMMITTED line is the terminal entry that closes this
	// row's MINTING — write it before the human-facing line so even a crash
	// between the two leaves the journal truthful.
	c.Migrated++
	if j != nil {
		if jerr := j.write(fmt.Sprintf("COMMITTED des=%s llmkey=%d", row.OrderID, created.ID)); jerr != nil {
			return fmt.Errorf("journal committed des=%s: %w", row.OrderID, jerr)
		}
	}
	fmt.Fprintf(m.Out, "migrated des=%s chat=%d pool=%s → llmgate id=%d credits=%.2f\n",
		row.OrderID, row.ChatID, row.KeyID, created.ID, pr.Remaining)
	return nil
}

// planSkipLine prints one skipped row's plan line: which row, which bucket,
// why — so the dry-run plan and the apply transcript both carry the full
// skip audit, not just the summary counts.
func (m *Migrator) planSkipLine(row *store.APIKey, why skipReason, detail string) {
	fmt.Fprintf(m.Out, "  plan des=%s chat=%d pool=%s SKIP %s (%s)\n",
		row.OrderID, row.ChatID, row.KeyID, why, detail)
}

// printSummary emits the end-of-run tally — every bucket, always, in both
// modes: the operator's receipt of what the run did (apply) or would have
// done (dry run).
func (m *Migrator) printSummary(c Counts) {
	fmt.Fprintf(m.Out, "summary: migrated=%d skipped-stats=%d skipped-exhausted=%d skipped-unlimited=%d failed-mint=%d failed-verify=%d skipped-race=%d\n",
		c.Migrated, c.SkippedStats, c.SkippedExhausted, c.SkippedUnlimited,
		c.FailedMint, c.FailedVerify, c.SkippedRace)
}

// sleepCtx waits d, returning early with ctx's error if the context is
// cancelled first — so the Ctrl-C abort window (and any future delay) is
// actually interruptible, matching the abort message the operator reads.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// ---- durable journal ----------------------------------------------------------
//
// The journal is the trace the migrator's own crash window demands: between
// "llmgate has created the key" and "the api_keys row points at it" there is
// a stretch where a hard kill (Ctrl-C at the wrong instant, kill -9, power
// loss) leaves a LIVE, BILLABLE llmgate key recorded nowhere — not in the
// store (the commit never ran) and not in this process's memory (it died).
// Every mint attempt therefore appends MINTING to the journal BEFORE the
// CreateKey call, and every terminal outcome (COMMITTED / FAILED-MINT /
// FAILED-VERIFY / RACE) appends its line right after; the next apply-mode
// start scans for MINTINGs with no terminal line and warns the operator
// with the names to check in the llmgate console. Lines are append-only
// (O_APPEND|O_CREATE|O_WRONLY) and Sync()ed per line — a journal that
// silently stops recording is worse than none, so write errors are FATAL
// to the run, never swallowed.

// journal is the append-only mint trace. Not safe for concurrent use — the
// migrator is single-threaded by construction (one row at a time, same
// goroutine), which is exactly the access pattern Run enforces.
type journal struct {
	f *os.File
}

// openJournal opens (or creates) the journal for appending.
func openJournal(path string) (*journal, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	return &journal{f: f}, nil
}

// write appends one timestamped line and pushes it to disk. The unix-nano
// prefix orders entries across runs; the line itself is the fixed
// "<VERB> des=<order> [name=<name>] [llmkey=<id>] [credits=<x>]" shape the
// startup scan parses.
func (j *journal) write(line string) error {
	if _, err := fmt.Fprintf(j.f, "%d %s\n", time.Now().UnixNano(), line); err != nil {
		return err
	}
	return j.f.Sync()
}

// Close releases the underlying file.
func (j *journal) Close() error { return j.f.Close() }

// scanInterrupted returns the raw journal lines of every MINTING entry
// whose des never reached a terminal line (COMMITTED, FAILED-MINT,
// FAILED-VERIFY or RACE) — the possible orphaned keys from interrupted
// runs, in first-attempted order. A commit-ERROR abort deliberately leaves
// its MINTING open (the outcome is genuinely unresolved), so it keeps
// being reported until an operator resolves the row by hand.
func (j *journal) scanInterrupted() ([]string, error) {
	// The journal file is opened WRITE-only for appends; read it back by
	// path instead. Missing file = nothing was ever attempted = clean.
	data, err := os.ReadFile(j.f.Name())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	pending := map[string]string{}
	var order []string
	for _, raw := range strings.Split(string(data), "\n") {
		fields := strings.Fields(raw)
		if len(fields) < 3 {
			continue // blank/short line — never a journal entry
		}
		verb := fields[1]
		des := journalField(fields, "des")
		if des == "" {
			continue
		}
		switch verb {
		case "MINTING":
			if _, seen := pending[des]; !seen {
				order = append(order, des)
			}
			pending[des] = raw
		case "COMMITTED", "FAILED-MINT", "FAILED-VERIFY", "RACE":
			delete(pending, des)
		}
	}
	out := make([]string, 0, len(pending))
	for _, des := range order {
		if line, still := pending[des]; still {
			out = append(out, line)
		}
	}
	return out, nil
}

// journalField extracts the value of the first "key=" token in a
// whitespace-split journal line, "" when absent.
func journalField(fields []string, key string) string {
	prefix := key + "="
	for _, f := range fields {
		if strings.HasPrefix(f, prefix) {
			return strings.TrimPrefix(f, prefix)
		}
	}
	return ""
}
