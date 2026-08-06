// Handles directory, audit trail, bans, and admin aggregate queries — port of
// store.py:522-663 (audit/handles/bans) and store.py:872-994 (admin queries).
package store

import (
	"database/sql"
	"log"
	"sort"
	"strings"
	"time"
)

// ---- audit ----------------------------------------------------------------

// AuditRow mirrors one `audit` row (store.py:77-83): append-only trail of
// destructive actions (account delete / password reset / tenant cleanup).
type AuditRow struct {
	TS        float64
	ActorChat string
	ActorKind string // 'customer' | 'admin' | 'reseller'
	Action    string
	Target    string
	Outcome   string
	Detail    string
}

// LogAction appends one destructive-action row to the audit trail
// (store.py:522-533). Best-effort: a failed audit write is logged and
// swallowed, never returned to the caller — recording an action must not abort
// the action itself. Field clipping mirrors Python: target[:4000],
// outcome[:500], detail[:2000] (rune-safe).
func (s *Store) LogAction(actorChat, actorKind, action, target, outcome, detail string) {
	_, err := s.db.Exec(
		"INSERT INTO audit(ts, actor_chat, actor_kind, action, target, outcome, detail) "+
			"VALUES (?,?,?,?,?,?,?)",
		now(), actorChat, actorKind, action,
		truncRunes(target, 4000), truncRunes(outcome, 500), truncRunes(detail, 2000))
	if err != nil {
		log.Println("audit LogAction failed:", err)
	}
}

// RecentAudit returns recent audit rows, newest first (store.py:536-550).
// query: optional case-insensitive substring filter over actor_chat / target /
// action / detail / actor_kind / outcome ("" = no filter).
func (s *Store) RecentAudit(limit int, query string) ([]AuditRow, error) {
	q := "SELECT ts, actor_chat, actor_kind, action, target, outcome, detail FROM audit"
	var args []any
	if query != "" {
		like := "%" + query + "%"
		q += " WHERE actor_chat LIKE ? OR target LIKE ? OR action LIKE ? " +
			"OR detail LIKE ? OR actor_kind LIKE ? OR outcome LIKE ?"
		args = append(args, like, like, like, like, like, like)
	}
	q += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditRow
	for rows.Next() {
		var (
			a                                              AuditRow
			ts                                             sql.NullFloat64
			chat, kind, action, target, outcome, detailCol sql.NullString
		)
		if err := rows.Scan(&ts, &chat, &kind, &action, &target, &outcome, &detailCol); err != nil {
			return nil, err
		}
		a.TS = ts.Float64
		a.ActorChat, a.ActorKind, a.Action = chat.String, kind.String, action.String
		a.Target, a.Outcome, a.Detail = target.String, outcome.String, detailCol.String
		out = append(out, a)
	}
	return out, rows.Err()
}

// ---- handles (chat_id <-> @username directory) ----------------------------

// SetHandle persists the latest @handle seen for a chat_id (store.py:553-568).
// Upsert keyed by chat_id so a username change just overwrites the row.
// Empty/non-'@'-prefixed handles are ignored so we never blank out a known one
// and never store dead, unmatchable display names.
func (s *Store) SetHandle(chatID, handle string) error {
	if handle == "" || !strings.HasPrefix(handle, "@") {
		return nil
	}
	_, err := s.db.Exec(
		"INSERT INTO handles(chat_id, handle, ts) VALUES (?,?,?) "+
			"ON CONFLICT(chat_id) DO UPDATE SET handle=excluded.handle, ts=excluded.ts",
		chatID, handle, now())
	return err
}

// MarkHandleChecked records that a chat_id's handle was looked up, even if it
// had no username (store.py:571-581). Inserts a NULL-handle marker row (never
// overwrites a real handle) so the one-time getChat backfill skips this chat
// on the next restart. HandleForChat returns "" for the NULL marker, and
// ChatIDForHandle never matches it.
func (s *Store) MarkHandleChecked(chatID string) error {
	_, err := s.db.Exec("INSERT OR IGNORE INTO handles(chat_id, handle, ts) VALUES (?,?,?)",
		chatID, nil, now())
	return err
}

// ChatIDsMissingHandle returns customer chat_ids (from orders+balances) with
// no handle row yet — powers the one-time getChat backfill (store.py:584-594).
// Python queried handles first, released the lock, then called all_chat_ids();
// with the 1-connection pool there is no re-entrancy hazard, so we just run
// both reads sequentially.
func (s *Store) ChatIDsMissingHandle() ([]string, error) {
	rows, err := s.db.Query("SELECT chat_id FROM handles")
	if err != nil {
		return nil, err
	}
	have := map[string]bool{}
	for rows.Next() {
		var cid sql.NullString
		if err := rows.Scan(&cid); err != nil {
			rows.Close()
			return nil, err
		}
		have[cid.String] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	all, err := s.AllChatIDs()
	if err != nil {
		return nil, err
	}
	var missing []string
	for _, cid := range all {
		if !have[cid] {
			missing = append(missing, cid)
		}
	}
	return missing, nil
}

// HandleForChat returns the stored @handle for a chat_id, or "" (absent OR the
// NULL checked-marker, store.py:597-602).
func (s *Store) HandleForChat(chatID string) (string, error) {
	var handle sql.NullString
	err := s.db.QueryRow("SELECT handle FROM handles WHERE chat_id=?",
		chatID).Scan(&handle)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return handle.String, err
}

// ChatIDForHandle resolves an @username to a chat_id via the handles
// directory, or "" (store.py:605-620). Case-insensitive; a leading '@' is
// optional in the query. If several chats ever shared the handle (username
// reassigned), the most-recently-seen wins.
func (s *Store) ChatIDForHandle(handle string) (string, error) {
	if handle == "" {
		return "", nil
	}
	h := strings.TrimSpace(handle)
	if !strings.HasPrefix(h, "@") {
		h = "@" + h
	}
	var chatID string
	err := s.db.QueryRow(
		"SELECT chat_id FROM handles WHERE handle=? COLLATE NOCASE "+
			"ORDER BY ts DESC LIMIT 1", h).Scan(&chatID)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return chatID, err
}

// ---- bans -----------------------------------------------------------------

// BannedRow mirrors one `banned` row (store.py:149-150).
type BannedRow struct {
	ChatID string
	Reason string
	ByChat string
	TS     float64
}

// BanUser blocks a chat_id from the bot (idempotent upsert — re-banning
// refreshes the reason/timestamp, store.py:625-632). The in-memory gate set is
// the hot path; this is the persistent backing so a ban survives restarts.
// Reason is clipped to 500 runes like Python's str(reason)[:500].
func (s *Store) BanUser(chatID, reason, byChat string) error {
	_, err := s.db.Exec("INSERT OR REPLACE INTO banned(chat_id, reason, by_chat, ts) "+
		"VALUES (?,?,?,?)", chatID, truncRunes(reason, 500), byChat, now())
	return err
}

// UnbanUser lifts a ban. Returns true if the chat was actually banned
// (store.py:635-639).
func (s *Store) UnbanUser(chatID string) (bool, error) {
	res, err := s.db.Exec("DELETE FROM banned WHERE chat_id=?", chatID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// IsBanned reports whether the chat_id is currently banned — the DB is the
// authoritative source behind the bot's in-memory gate set (store.py:642-647).
func (s *Store) IsBanned(chatID string) (bool, error) {
	var one int
	err := s.db.QueryRow("SELECT 1 FROM banned WHERE chat_id=?", chatID).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

// BannedChatIDs returns the set of all banned chat_ids, loaded once at startup
// into the bot's in-memory gate set (store.py:650-654).
func (s *Store) BannedChatIDs() (map[string]bool, error) {
	rows, err := s.db.Query("SELECT chat_id FROM banned")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var cid sql.NullString
		if err := rows.Scan(&cid); err != nil {
			return nil, err
		}
		out[cid.String] = true
	}
	return out, rows.Err()
}

// BannedList returns banned chats, newest first, for the admin viewer
// (store.py:657-663).
func (s *Store) BannedList(limit int) ([]BannedRow, error) {
	rows, err := s.db.Query("SELECT chat_id, reason, by_chat, ts FROM banned "+
		"ORDER BY ts DESC LIMIT ?", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BannedRow
	for rows.Next() {
		var (
			b                   BannedRow
			chatID, reason, byC sql.NullString
			ts                  sql.NullFloat64
		)
		if err := rows.Scan(&chatID, &reason, &byC, &ts); err != nil {
			return nil, err
		}
		b.ChatID, b.Reason, b.ByChat, b.TS = chatID.String, reason.String, byC.String, ts.Float64
		out = append(out, b)
	}
	return out, rows.Err()
}

// ---- admin queries --------------------------------------------------------

// AllChatIDs returns every chat_id we've ever seen (orders + balances),
// de-duped and sorted (store.py:892-902). Empty ids are skipped like Python's
// `if cid:` truthiness check.
func (s *Store) AllChatIDs() ([]string, error) {
	ids := map[string]bool{}
	for _, q := range []string{
		"SELECT DISTINCT chat_id FROM orders",
		"SELECT DISTINCT chat_id FROM balances",
	} {
		rows, err := s.db.Query(q)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var cid sql.NullString
			if err := rows.Scan(&cid); err != nil {
				rows.Close()
				return nil, err
			}
			if cid.String != "" {
				ids[cid.String] = true
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	out := make([]string, 0, len(ids))
	for cid := range ids {
		out = append(out, cid)
	}
	sort.Strings(out)
	return out, nil
}

// RevenueSummary aggregates revenue figures for the admin dashboard
// (store.py:905-932). Revenue counts only DELIVERED account orders (money in
// + goods out) and excludes wallet top-ups so a balance-funded purchase isn't
// double-counted.
type RevenueSummary struct {
	DeliveredCount   int64
	DeliveredRevenue int64
	TodayCount       int64
	TodayRevenue     int64
	// AwaitingPaymentCount — 'reserved', non-deposit: a customer opened a
	// checkout and wandered off. Harmless, expires by itself, normally large.
	AwaitingPaymentCount int64
	// PaidUndeliveredCount — 'paid', non-deposit: MONEY IS IN AND GOODS ARE
	// NOT OUT. Every one of these is a customer who has paid and received
	// nothing. Never sum this into AwaitingPaymentCount: the whole defect was
	// that a routinely-large number taught admins to ignore a field that also
	// carried this one.
	PaidUndeliveredCount int64
	// PaidUndeliveredOldestTS is the epoch-seconds ts of the OLDEST such order,
	// or 0 when there are none. The render needs it because "2" invites
	// triage-later while "oldest 3h ago" does not. Formatting is the handler's
	// job; the store returns the raw stamp.
	PaidUndeliveredOldestTS float64
	// RefundedCount — REAL customer refunds ONLY (refund_kind IN
	// ('customer','admin_cancel')), stats-fixes-spec.md §5.2 (finding A2). The
	// old query summed status IN ('refunded','expired') with no deposit
	// filter, so a rescued late top-up (late_settled) and hundreds of
	// abandoned checkouts inflated a number that could report refunds a shop
	// with ZERO real refunds never had. late_settled, mint_failed and
	// out_of_stock are deliberately excluded from this field — see the
	// sibling fields below, which give each its own visible line instead of
	// folding it in or dropping it silently.
	RefundedCount int64
	// RefundedRevenue — Σ total of the SAME genuine-refund set RefundedCount
	// counts (stats-fixes-spec.md §5.5, finding A7). The A7 numerator: Gross =
	// DeliveredRevenue + RefundedRevenue, Net = DeliveredRevenue unchanged. See
	// GetRevenueSummary's query comment for the caveat on partial refunds.
	RefundedRevenue int64
	// ExpiredCount — abandoned checkouts (status='expired'), split out of the
	// old combined "Refunded/expired" line.
	ExpiredCount int64
	// MintFailedCount — OUR own Kiro-Go mint failing (apikey_deliver.go). An
	// outage signal, never a customer refund.
	MintFailedCount int64
	// OutOfStockCount — customer paid, the reserved folder-stock rows were
	// gone (purchase.go). OUR fulfilment failure on the main product path and
	// a restocking signal — invisible on every surface before this fix, so it
	// is rendered even at zero (a persistent zero is the healthy reading).
	OutOfStockCount int64
	// UnclassifiedCount — refunded rows with refund_kind IS NULL. These
	// predate the taxonomy and CANNOT be retroactively classified (spec
	// §1.2 — there is no backfill; the distinguishing information was never
	// recorded). Reported in its own bucket, NEVER folded into RefundedCount
	// — doing so would recreate finding A2 inside the fix for A2.
	UnclassifiedCount int64
	Customers         int64
	WalletTotal       int64
}

// GetRevenueSummary computes the dashboard aggregate (store.py:905-932).
// depositProduct "" defaults to DepositProduct (Python default "__deposit__").
func (s *Store) GetRevenueSummary(depositProduct string) (*RevenueSummary, error) {
	if depositProduct == "" {
		depositProduct = DepositProduct
	}
	dayStart := now() - 86400 // "today" = trailing 24h, store.py:915
	var r RevenueSummary
	// COALESCE where Python tolerated SUM() returning None (store.py:917-919).
	//
	// The deposit filter is spelled COALESCE-first rather than a bare
	// `product!=?` (stats-fixes-spec.md §5.1 / audit A1) — `NULL != x` is NULL,
	// not TRUE, so a bare filter would silently DROP a delivered row whose
	// product is NULL out of the WHERE clause entirely, DEFLATING both the
	// count and the SUM (review finding on this slice: an earlier draft of
	// this comment had the direction backwards — "inflate" — which sends a
	// future reader hunting for an over-count when the actual symptom is an
	// under-count; PaidUndelivered's doc comment below states the correct
	// direction, "would silently vanish"). COALESCE-first is what puts that
	// row BACK into both figures. This is now more than cosmetic:
	// DeliveredCount feeds the refund-rate denominator (admin.go's
	// adminDashboardText), so a dropped NULL-product row would also quietly
	// inflate a displayed percentage (fewer orders in the denominator raises
	// the rate). The exact fragment:
	//
	//	COALESCE(product,'')!=?
	if err := s.db.QueryRow("SELECT COUNT(*), COALESCE(SUM(total),0) FROM orders "+
		"WHERE status='delivered' AND COALESCE(product,'')!=?", depositProduct).
		Scan(&r.DeliveredCount, &r.DeliveredRevenue); err != nil {
		return nil, err
	}
	// ts is COALESCE-guarded too (review finding on this slice): ts has no
	// NOT NULL constraint (store.go's schema; PaidUndelivered's own doc
	// comment and admin.go's PaidUndeliveredOldestTS>0 guard both already
	// treat a NULL ts as a live-data possibility one function away from this
	// one), and `NULL>=x` is NULL, not TRUE — the identical trap this fix
	// just closed for `product`. Not reachable via CreateOrder today (always
	// writes now()), same as the product case above.
	//
	// THIS GUARD CHANGES NOTHING TODAY, and that is worth stating plainly so
	// nobody reads it as behaviour or writes a test that only appears to cover
	// it. dayStart is a real epoch second, so COALESCE(ts,0)>=dayStart is
	// 0>=1.7e9 — false — while the bare NULL>=dayStart is NULL, also not true.
	// A NULL-ts row is excluded from Today either way. The two forms diverge
	// only at a threshold of zero or below, which dayStart cannot reach, so no
	// test can distinguish this line from its absence and one written to try
	// would pass against the unfixed code too.
	//
	// That "no test can distinguish it" rests on a premise worth naming,
	// because a routine refactor would falsify it: store.now is a plain func
	// (store.go), NOT an injectable var — unlike handlers.Bot.now, which is.
	// Make store.now injectable and a test could drive dayStart to zero and
	// pin the divergence, at which point this paragraph is wrong and the line
	// deserves a test.
	//
	// It stays for a reason better than documentation: a bare `ts>=` sitting
	// beside COALESCE(product,'') in the SAME WHERE clause forces every future
	// reader to work out why one column is guarded and the other is not.
	// Uniformity across one clause is a maintenance property, and it survives
	// even if the reasoning above is someday falsified.
	if err := s.db.QueryRow("SELECT COUNT(*), COALESCE(SUM(total),0) FROM orders "+
		"WHERE status='delivered' AND COALESCE(product,'')!=? AND COALESCE(ts,0)>=?",
		depositProduct, dayStart).Scan(&r.TodayCount, &r.TodayRevenue); err != nil {
		return nil, err
	}
	// The split that matters (spec §5.3). Both exclude wallet top-ups, for the
	// same reason DeliveredCount does: a deposit is not a product sale, and a
	// deposit sitting in 'paid' is not a customer waiting on goods. Omitting
	// this filter would be A1's bug reappearing here and would make the alarm
	// cry wolf.
	if err := s.db.QueryRow("SELECT COUNT(*) FROM orders "+
		"WHERE status='reserved' AND COALESCE(product,'')!=?", depositProduct).
		Scan(&r.AwaitingPaymentCount); err != nil {
		return nil, err
	}
	paidN, paidOldest, err := s.PaidUndelivered(depositProduct)
	if err != nil {
		return nil, err
	}
	r.PaidUndeliveredCount, r.PaidUndeliveredOldestTS = paidN, paidOldest
	// The A2 split (stats-fixes-spec.md §5.2): five queries, five disjoint
	// buckets over status='refunded'/'expired', each with the SAME
	// COALESCE(product,'')!=? deposit filter as PaidUndelivered above — a bare
	// `product != ?` would silently drop a NULL-product row from every bucket
	// (`NULL != x` is NULL, not TRUE in SQL), the exact trap already caught in
	// A4. late_settled is deliberately in none of these: it is not a refund
	// (§2.2) and, being a __deposit__-only kind, would need its own line if
	// ever surfaced — not conflated with a product refund bucket.
	// RefundedRevenue rides the same query as RefundedCount (one row, two
	// columns) — it is money actually credited back on GENUINE refunds only
	// (customer + admin_cancel), the numerator A7's Refunded line needs
	// (stats-fixes-spec.md §5.5). CAVEAT, not fixable here, cuts BOTH ways —
	// review finding on this slice: orders.total is the ORIGINAL charge and is
	// never rewritten on a partial refund. adminorders.go's refundAmount
	// pro-rates day/hour warranty tiers down from o.Total when an admin issues
	// a non-forceFull refund; only forceFull sets amount=o.Total. The refunded
	// order still leaves status='delivered' ENTIRELY, so DeliveredRevenue (the
	// admin-facing Net line) loses the row's FULL total, not just the prorated
	// amount actually credited. Net result on a partial refund: Refunded
	// OVERSTATES cash actually returned (SUM(total) is an upper bound), and
	// Net UNDERSTATES cash actually retained, by the exact same residue
	// (total-amount) — Gross is unaffected either way. The actual credited
	// amount is not persisted anywhere queryable, so neither side can be
	// corrected from this table. Recording it would need a new column (same
	// shape as list_total/code_discount, spec §3) and is out of this slice's
	// scope.
	if err := s.db.QueryRow("SELECT COUNT(*), COALESCE(SUM(total),0) FROM orders "+
		"WHERE status='refunded' AND COALESCE(product,'')!=? AND refund_kind IN (?,?)",
		depositProduct, RefundKindCustomer, RefundKindAdminCancel).
		Scan(&r.RefundedCount, &r.RefundedRevenue); err != nil {
		return nil, err
	}
	if err := s.db.QueryRow("SELECT COUNT(*) FROM orders "+
		"WHERE status='refunded' AND COALESCE(product,'')!=? AND refund_kind=?",
		depositProduct, RefundKindMintFailed).
		Scan(&r.MintFailedCount); err != nil {
		return nil, err
	}
	if err := s.db.QueryRow("SELECT COUNT(*) FROM orders "+
		"WHERE status='refunded' AND COALESCE(product,'')!=? AND refund_kind=?",
		depositProduct, RefundKindOutOfStock).
		Scan(&r.OutOfStockCount); err != nil {
		return nil, err
	}
	if err := s.db.QueryRow("SELECT COUNT(*) FROM orders "+
		"WHERE status='refunded' AND COALESCE(product,'')!=? AND refund_kind IS NULL",
		depositProduct).
		Scan(&r.UnclassifiedCount); err != nil {
		return nil, err
	}
	if err := s.db.QueryRow("SELECT COUNT(*) FROM orders "+
		"WHERE status='expired' AND COALESCE(product,'')!=?", depositProduct).
		Scan(&r.ExpiredCount); err != nil {
		return nil, err
	}
	// Customers (stats-fixes-spec.md §5.1, audit A1 — the highest-ranked
	// finding). This query had NO product filter at all, unlike its siblings
	// above: every chat that has ever topped up a wallet reads as a "delivered
	// order" customer, so a shop with 10 real buyers and 40 deposit-only chats
	// rendered "Customers: 50" and looked like a catastrophic-conversion shop
	// that does not exist. COALESCE-first for the same NULL-product reason as
	// every filter above.
	if err := s.db.QueryRow("SELECT COUNT(DISTINCT chat_id) FROM orders "+
		"WHERE status='delivered' AND COALESCE(product,'')!=?", depositProduct).
		Scan(&r.Customers); err != nil {
		return nil, err
	}
	if err := s.db.QueryRow("SELECT COALESCE(SUM(amount),0) FROM balances").
		Scan(&r.WalletTotal); err != nil {
		return nil, err
	}
	return &r, nil
}

// PaidUndelivered counts the orders where MONEY IS IN AND GOODS ARE NOT OUT —
// status 'paid', wallet top-ups excluded — and returns the epoch-seconds ts of
// the OLDEST one (0 when the count is 0). Audit finding A4
// (.omc/artifacts/stats-audit.md), spec §5.3.
//
// Why this is its own method rather than another line in GetRevenueSummary:
// every one of these rows is a customer who has paid and received nothing, so
// the figure is worth checking on its own (including straight against prod)
// without computing an entire dashboard. The dashboard consumes it too.
//
// The deposit exclusion is load-bearing, not cosmetic. A __deposit__ order in
// 'paid' is a top-up mid-settle, not a customer waiting on goods; counting it
// would be the A1 defect (a missing `AND product!=?`) reappearing inside A4's
// fix and would make an alarm that must stay believable cry wolf. The SAME
// filter is applied to MIN(ts) — dropping it only there still yields a correct
// count with a wrong, overstated age.
//
// The filter is spelled COALESCE-first rather than a bare `product!=?` —
//
//	COALESCE(product,'') rather than a bare `product!=?`
//
// — because orders.product has no NOT NULL constraint (store.go's schema),
// and in SQL `NULL != x` is NULL, not TRUE — a 'paid' row with a NULL product
// would silently vanish from BOTH the count and MIN(ts) instead of being
// caught by the deposit filter, which is the opposite failure direction of
// what this filter exists to guarantee (Opus review finding on this slice).
// CreateOrder always writes a non-NULL product today, so this guards a
// live-data edge, not a currently-reachable application path.
//
// (The SQL fragment above is deliberately an INDENTED doc-comment line, not
// inline prose: gofmt's doc-comment formatter silently turns two adjacent
// single-quote characters into one typographic close-quote when they appear
// in flowing text — verified reproducible against the stock Go toolchain,
// not a project quirk — which would corrupt this exact SQL fragment (an
// empty-string literal) on the next `gofmt -w` of this file. An indented line
// renders as a preformatted code block and is exempt.)
//
// NOTE, deliberately not folded in: a __deposit__ order stuck in 'paid' means a
// customer's money arrived and their wallet was never credited. That is also
// real harm, but it is a DIFFERENT incident class with a different fix, and
// merging it here would rebuild the very conflation this split exists to undo.
// It deserves its own counter — recorded in the audit's gap list, not silently
// swept in.
//
// depositProduct "" defaults to DepositProduct.
func (s *Store) PaidUndelivered(depositProduct string) (count int64, oldestTS float64, err error) {
	if depositProduct == "" {
		depositProduct = DepositProduct
	}
	// MIN() over an empty set is NULL, hence the NullFloat64 rather than
	// COALESCE(...,0): "no such order" and "an order stamped 0" are different
	// facts, and only the former may report a zero age.
	var oldest sql.NullFloat64
	err = s.db.QueryRow(
		"SELECT COUNT(*), MIN(ts) FROM orders WHERE status='paid' AND COALESCE(product,'')!=?",
		depositProduct).Scan(&count, &oldest)
	if err != nil {
		return 0, 0, err
	}
	if oldest.Valid {
		oldestTS = oldest.Float64
	}
	return count, oldestTS, nil
}

// RevenueDay is one calendar-day row of the revenue matrix (store.py:944-950).
type RevenueDay struct {
	Date  string           // "YYYY-MM-DD", LOCAL time
	Cells map[string]int64 // product -> revenue that day
	Total int64
	Count int64
}

// RevenueMatrix is delivered revenue bucketed by (calendar day × product) for
// the admin Revenue view (store.py:935-994).
type RevenueMatrix struct {
	Products      []string         // columns, ordered by total desc
	Rows          []RevenueDay     // one per active day, MOST-RECENT first, capped
	ProductTotals map[string]int64 // column totals (full range)
	ProductCounts map[string]int64
	GrandTotal    int64
	GrandCount    int64
	Truncated     bool // true if older days were dropped by maxRows
}

// GetRevenueMatrix ports revenue_matrix (store.py:935-994). Counts only
// DELIVERED orders, excludes wallet top-ups. sinceTS (epoch seconds, nil = all
// history) lower-bounds orders by ts. Days are bucketed in LOCAL time so a
// "day" matches what the admin sees on the clock. Aggregation happens in Go
// (like Python did in-process) so we don't depend on SQLite date functions.
func (s *Store) GetRevenueMatrix(depositProduct string, sinceTS *float64, maxRows int) (*RevenueMatrix, error) {
	if depositProduct == "" {
		depositProduct = DepositProduct
	}
	if maxRows <= 0 {
		maxRows = 60 // Python default, store.py:935
	}
	// COALESCE-first for the same NULL-product reason as GetRevenueSummary
	// above (audit A1's trap, stats-fixes-spec.md §5.1): a bare `product!=?`
	// would silently DROP a NULL-product delivered row (`NULL != x` is NULL,
	// not TRUE), which would make this matrix's GrandTotal/GrandCount
	// disagree with the dashboard's DeliveredRevenue/DeliveredCount on the
	// exact same range — a real inconsistency between the two revenue
	// surfaces, not a cosmetic gap.
	q := "SELECT ts, product, total FROM orders WHERE status='delivered' AND COALESCE(product,'')!=?"
	args := []any{depositProduct}
	if sinceTS != nil {
		q += " AND ts>=?"
		args = append(args, *sinceTS)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// Aggregate exactly like store.py:964-981: plain maps, bucketed by local
	// calendar day.
	dayProd := map[string]map[string]int64{}
	dayCount := map[string]int64{}
	prodTotal := map[string]int64{}
	prodCount := map[string]int64{}
	var grandTotal, grandCount int64
	for rows.Next() {
		var (
			ts      sql.NullFloat64
			product sql.NullString
			total   sql.NullInt64
		)
		if err := rows.Scan(&ts, &product, &total); err != nil {
			return nil, err
		}
		// Local-time day bucket — mirrors time.strftime("%Y-%m-%d",
		// time.localtime(ts or 0)) (store.py:973).
		day := time.Unix(int64(ts.Float64), 0).Local().Format("2006-01-02")
		amt := total.Int64
		cell := dayProd[day]
		if cell == nil {
			cell = map[string]int64{}
			dayProd[day] = cell
		}
		cell[product.String] += amt
		dayCount[day]++
		prodTotal[product.String] += amt
		prodCount[product.String]++
		grandTotal += amt
		grandCount++
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// products sorted by total revenue desc (store.py:983).
	products := make([]string, 0, len(prodTotal))
	for p := range prodTotal {
		products = append(products, p)
	}
	sort.Slice(products, func(i, j int) bool {
		if prodTotal[products[i]] != prodTotal[products[j]] {
			return prodTotal[products[i]] > prodTotal[products[j]]
		}
		return products[i] < products[j] // stable tie-break
	})

	// Days most-recent first, capped to maxRows (store.py:984-990).
	allDays := make([]string, 0, len(dayProd))
	for d := range dayProd {
		allDays = append(allDays, d)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(allDays)))
	truncated := len(allDays) > maxRows
	if truncated {
		allDays = allDays[:maxRows]
	}
	outRows := make([]RevenueDay, 0, len(allDays))
	for _, day := range allDays {
		cells := map[string]int64{}
		var total int64
		for _, p := range products {
			cells[p] = dayProd[day][p]
			total += cells[p]
		}
		outRows = append(outRows, RevenueDay{
			Date: day, Cells: cells, Total: total, Count: dayCount[day]})
	}
	return &RevenueMatrix{
		Products: products, Rows: outRows,
		ProductTotals: prodTotal, ProductCounts: prodCount,
		GrandTotal: grandTotal, GrandCount: grandCount,
		Truncated: truncated,
	}, nil
}
