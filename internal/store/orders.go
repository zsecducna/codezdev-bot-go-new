// Orders — port of the "---- orders ----" section of store.py (store.py:197-309)
// plus the admin/sweeper order queries (store.py:503-519, 817-835, 874-889) and
// the delivered-history aggregates (store.py:485-500, 1150-1157).
package store

import (
	"database/sql"
	"fmt"
)

// Order mirrors one `orders` row (schema store.py:68-71 + migrated columns
// store.py:106-117). Nullable columns are pointers so a NULL round-trips
// exactly like Python's None-carrying dicts.
type Order struct {
	Des            string
	ChatID         string
	Lang           string
	Product        string
	Tier           string
	Qty            int64
	Total          int64
	Status         string
	PaypalOrderID  *string  // NULL until set_paypal_order (store.py:216-220)
	TS             float64  // creation epoch seconds
	Code           *string  // applied promo/referral code (store.py:106-112)
	CodeKind       *string  // 'promo' | 'referral'
	ReferrerChat   *string  // who to pay on delivery (referral only)
	EntraDeletedTS *float64 // refund-delete sweep stamp (store.py:113-117)
	// RefundKind classifies WHY this order is 'refunded' (stats-fixes-spec.md
	// §2.1). nil on every non-refunded order — that is the invariant
	// ClaimOrderKind enforces.
	//
	// nil on a REFUNDED row means UNCLASSIFIED. The step-3 handler migration
	// has landed — all seven forward-to-'refunded' sites in internal/handlers
	// call ClaimOrderKind with an explicit kind, and
	// internal/handlers.TestNoBareClaimOrderToRefunded fails the build if a
	// future eighth site slips back onto the plain wrapper. So today a nil
	// here means a genuine pre-existing row (no backfill possible, spec §1.2).
	// Reports must still bucket nil separately and must NOT relabel it
	// "pre-migration": that guard test covers the shape it can see, not a
	// correctness proof, so nil staying UNCLASSIFIED (rather than assumed
	// historical) is the conservative reading if it is ever wrong.
	RefundKind *string
}

// orderCols is the explicit column list every order SELECT uses — explicit
// (never SELECT *) so scanning is immune to physical column order differences
// between a freshly-created DB and an ALTER-migrated old one.
const orderCols = "des, chat_id, lang, product, tier, qty, total, status, " +
	"paypal_order_id, ts, code, code_kind, referrer_chat, entra_deleted_ts, " +
	"refund_kind"

// rowScanner abstracts *sql.Row / *sql.Rows for the shared scan helper.
type rowScanner interface{ Scan(dest ...any) error }

// scanOrder scans one row selected with orderCols into an Order, converting
// SQL NULLs the same way sqlite3.Row -> dict did (None -> nil pointer;
// NULL text/int in non-null-by-convention columns -> zero value).
func scanOrder(r rowScanner) (*Order, error) {
	var (
		o                            Order
		lang, product, tier, status  sql.NullString
		qty, total                   sql.NullInt64
		paypal, code, kind, referrer sql.NullString
		ts, entraTS                  sql.NullFloat64
		chatID                       sql.NullString
		refundKind                   sql.NullString
	)
	if err := r.Scan(&o.Des, &chatID, &lang, &product, &tier, &qty, &total,
		&status, &paypal, &ts, &code, &kind, &referrer, &entraTS,
		&refundKind); err != nil {
		return nil, err
	}
	o.ChatID = chatID.String
	o.Lang, o.Product, o.Tier, o.Status = lang.String, product.String, tier.String, status.String
	o.Qty, o.Total, o.TS = qty.Int64, total.Int64, ts.Float64
	if paypal.Valid {
		v := paypal.String
		o.PaypalOrderID = &v
	}
	if code.Valid {
		v := code.String
		o.Code = &v
	}
	if kind.Valid {
		v := kind.String
		o.CodeKind = &v
	}
	if referrer.Valid {
		v := referrer.String
		o.ReferrerChat = &v
	}
	if entraTS.Valid {
		v := entraTS.Float64
		o.EntraDeletedTS = &v
	}
	if refundKind.Valid {
		v := refundKind.String
		o.RefundKind = &v
	}
	return &o, nil
}

// queryOrders runs a SELECT built on orderCols and scans all rows.
func (s *Store) queryOrders(q string, args ...any) ([]*Order, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Order
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// CreateOrder persists a freshly reserved order with status='reserved'
// (store.py:199-205).
func (s *Store) CreateOrder(des, chatID, lang, product, tier string, qty, total int64) error {
	_, err := s.db.Exec(
		"INSERT INTO orders(des, chat_id, lang, product, tier, qty, "+
			"total, status, paypal_order_id, ts) "+
			"VALUES (?,?,?,?,?,?,?, 'reserved', NULL, ?)",
		des, chatID, lang, product, tier, qty, total, now())
	return err
}

// GetOrder returns the order row, or nil if absent (store.py:208-213).
func (s *Store) GetOrder(des string) (*Order, error) {
	o, err := scanOrder(s.db.QueryRow(
		"SELECT "+orderCols+" FROM orders WHERE des=?", des))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return o, err
}

// MarkEntraDeleted records that the Entra account(s) for order `des` have been
// soft-deleted, so the refund-delete admin sweep and per-order button both
// skip it from then on (store.py:223-228).
func (s *Store) MarkEntraDeleted(des string, ts float64) error {
	_, err := s.db.Exec("UPDATE orders SET entra_deleted_ts=? WHERE des=?", ts, des)
	return err
}

// UpdateOrderCode attaches (or clears) an applied promo/referral code on a
// RESERVED order and overwrites its total to the recomputed, code-adjusted
// price (store.py:231-244). code=nil clears the code. Only a reserved order is
// mutated so a paid/delivered order's recorded total can never change
// underneath delivery.
func (s *Store) UpdateOrderCode(des string, code, codeKind, referrerChat *string, total int64) error {
	_, err := s.db.Exec(
		"UPDATE orders SET code=?, code_kind=?, referrer_chat=?, total=? "+
			"WHERE des=? AND status='reserved'",
		strOrNil(code), strOrNil(codeKind), strOrNil(referrerChat), total, des)
	return err
}

// USDT migration note (2026-08-06): SetPaypalOrder (the write half of the
// paypal_order_id column) and OrderDesForPaypal (the webhook's reverse
// lookup) were removed as dead code once the PayPal rail's only callers
// (internal/handlers/paypal.go) were deleted — S2. PaypalOrderID itself stays
// readable on Order (scanOrder above) for historical order display
// (adminusers.go), and the paypal_order_id COLUMN stays in the schema per
// the migration plan — both are dropped together only in the later live-data
// migration slice (S9), not here.

// ClaimOrder atomically moves an order from `fromStatus` to `toStatus`
// (store.py:255-270). Returns the order if THIS call won the transition
// (caller proceeds), or nil if the order was missing or already past
// `fromStatus` — another path already claimed it. The
// UPDATE ... WHERE status=? with a RowsAffected()==1 gate is the single atomic
// guard against double-delivery across goroutines/events
// (architecture-go.md §5).
func (s *Store) ClaimOrder(des, fromStatus, toStatus string) (*Order, error) {
	return s.ClaimOrderKind(des, fromStatus, toStatus, "")
}

// Refund kinds — the five semantically distinct events that all collapse into
// status='refunded' today (stats-fixes-spec.md §2.1). Keeping them apart is the
// whole point: the original dashboard summed refunds, abandoned checkouts,
// failed mints and rescued late payments into one "Refunded/expired" number, so
// a shop with ZERO real refunds could report hundreds.
const (
	// RefundKindCustomer — an admin refunded a COMPLETED sale (delivered ->
	// refunded). Goods went out, money came back. A real refund.
	RefundKindCustomer = "customer"
	// RefundKindAdminCancel — an admin refunded a PAID order before delivery.
	// Money back, no goods. A real refund.
	RefundKindAdminCancel = "admin_cancel"
	// RefundKindMintFailed — our own API-key mint failed (apikey_deliver.go).
	// The customer wanted to buy and WE could not deliver: an operational
	// failure, not a customer refund.
	RefundKindMintFailed = "mint_failed"
	// RefundKindOutOfStock — the customer paid and the reserved folder-stock
	// rows were gone (purchase.go). Our fulfilment failure, on the MAIN product
	// path. Counting it as a customer refund would blame customers for our own
	// stockouts.
	RefundKindOutOfStock = "out_of_stock"
	// RefundKindLateSettled — payment landed after the checkout window and was
	// credited to the wallet. For a __deposit__ order the customer is sent
	// Deposit.OK: this is a SUCCESS wearing a refund's status, and counting it
	// as a refund is the exact defect this taxonomy exists to remove.
	//
	// CURRENTLY UNREACHABLE, kept as correct defensive code: settlePaid's
	// expired-tail branch (deposit.go) is the only writer, and settlePaid's
	// own deposit branch is bypassed entirely by usdt.go's deposit settle path
	// (which credits directly and never calls settlePaid, to avoid a double
	// credit) — so no LIVE __deposit__ order reaches this kind today. If a
	// future rail routes deposits back through settlePaid, this kind is ready;
	// until then, treat this as aspirational rather than an observed case.
	RefundKindLateSettled = "late_settled"
)

// validRefundKinds gates ClaimOrderKind. An unrecognised string is rejected
// rather than stored: a typo silently creating a sixth bucket would be the same
// class of defect as the four-way overload being fixed here.
var validRefundKinds = map[string]bool{
	RefundKindCustomer:    true,
	RefundKindAdminCancel: true,
	RefundKindMintFailed:  true,
	RefundKindOutOfStock:  true,
	RefundKindLateSettled: true,
}

// IsRealRefund reports whether a refund kind counts toward the CUSTOMER-FACING
// refund rate. Only an admin-issued refund of a paid or delivered order does.
//
// mint_failed and out_of_stock are OUR failures — folding them in makes an
// outage or a stockout look like customer dissatisfaction and hides both.
// late_settled is frequently a completed top-up. A NULL kind (pre-migration
// row) is not a real refund either: it is unknown, and unknown must be reported
// as its own bucket, never assumed (stats-fixes-spec.md §2.2).
func IsRealRefund(kind string) bool {
	return kind == RefundKindCustomer || kind == RefundKindAdminCancel
}

// IsRealRefundOrder is the nil-safe form for an Order straight out of the
// store, and is what report code should call. Order.RefundKind is a *string
// that is legitimately nil on every unclassified refunded row — both the
// pre-migration ones and (until the step-3 handler migration lands) any row
// refunded through the plain ClaimOrder wrapper. Dereferencing it to call
// IsRealRefund would panic on the very first such row, on a money-reporting
// path, so the pointer form exists to make the safe call the obvious one.
// nil is NOT a real refund: unknown must be reported as its own bucket, never
// assumed either way (stats-fixes-spec.md §1.2).
func IsRealRefundOrder(o *Order) bool {
	if o == nil || o.RefundKind == nil {
		return false
	}
	return IsRealRefund(*o.RefundKind)
}

// ClaimOrderKind is ClaimOrder plus the refund_kind write, in the SAME atomic
// UPDATE that already guards double-delivery.
//
// THE INVARIANT: refund_kind is non-NULL if and only if status='refunded'.
// It is enforced structurally rather than by convention — the one UPDATE sets
// the kind when toStatus=="refunded" and NULLs it for every other status. That
// matters because three call sites REVERT a refund
// (apikey_deliver.go:250,411 and purchase.go:343 all do
// ClaimOrder(des, "refunded", "paid")); if the kind survived such a revert, a
// status='paid' row would carry a stale kind and every GROUP BY refund_kind
// report would count an order that is not refunded at all.
//
// Because ClaimOrder delegates here with kind=="", every pre-existing call site
// clears the column for free, with no edit and no chance of anyone forgetting.
// Only the forward-to-'refunded' sites need to change. There are SEVEN of them
// (spec §2.1) — not five; the API-key rail refunds through its OWN path, since
// doRefund branches at adminorders.go:206 into doRefundApikeyGroup for any
// isApikeyFamily product and never reaches adminorders.go:233. STEP 3 HAS
// LANDED — all seven now call ClaimOrderKind (line numbers below drift with
// unrelated edits; treat them as approximate, the test below is the source of
// truth):
//
//	adminorders.go:252     customer | admin_cancel   (branch on o.Status)
//	apikey_refund.go:263   customer | admin_cancel   (branch on m.Status)
//	apikey_refund.go:284   customer | admin_cancel   (branch on o.Status)
//	apikey_deliver.go:231  mint_failed
//	apikey_deliver.go:397  mint_failed
//	purchase.go:328        out_of_stock
//	deposit.go:~285        late_settled (product==depositProduct) |
//	                       out_of_stock (any other product — the reserved
//	                       stock already released at expiry, so a late
//	                       payment on a non-deposit order has nothing to
//	                       deliver against either; tagging it late_settled
//	                       would make a paid-not-delivered event invisible on
//	                       every refund-kind bucket, review finding on this
//	                       slice)
//
// The three dynamic-`from` sites are pre-gated by refundableStatuses
// {delivered, paid} (apikey_refund.go:33), so each is a REAL refund: delivered
// -> customer, paid -> admin_cancel.
//
// Completeness is now enforced by a test, not just a checklist:
// internal/handlers.TestNoBareClaimOrderToRefunded parses the module's AST on
// every `go test` and fails the build if any call site anywhere reaches
// status='refunded' through the plain ClaimOrder wrapper — catching a future
// eighth site the moment it is written rather than at the next manual audit.
// The manual cross-check for a reviewer without a Go toolchain handy is still
// `grep -rn 'ClaimOrder(' internal/handlers/ | grep '"refunded")'` returning
// nothing. Do not trust either check alone without reading the diff — this
// list was wrong twice before step 3 landed (missed purchase.go, then both
// apikey_refund.go sites), and a THIRD miss surfaced only in review after
// step 3 "landed": deposit.go's site used one kind for two different events.
//
// refundKind must be "" or one of the RefundKind* constants; anything else is
// rejected WITHOUT moving the order. Passing a kind with a non-refunded
// toStatus is also rejected — it means the caller has confused itself.
func (s *Store) ClaimOrderKind(des, fromStatus, toStatus, refundKind string) (*Order, error) {
	if refundKind != "" && !validRefundKinds[refundKind] {
		return nil, fmt.Errorf("claim order %s: unknown refund kind %q", des, refundKind)
	}
	if refundKind != "" && toStatus != "refunded" {
		return nil, fmt.Errorf("claim order %s: refund kind %q given for status %q (only 'refunded' carries a kind)",
			des, refundKind, toStatus)
	}
	// A self-transition is the ONE sequence that could strip a kind off a row
	// that is still legitimately refunded: refunded -> refunded matches the
	// WHERE, reports RowsAffected()==1, and writes NULL. No caller does this
	// today (the dynamic-`from` sites are gated to {delivered, paid}), but that
	// guard lives in internal/handlers, so the store must not rely on it.
	// Rejecting is correct rather than COALESCE-preserving the old kind:
	// preserving would break the revert path, which is the real invariant.
	if fromStatus == toStatus {
		return nil, fmt.Errorf("claim order %s: no-op transition %q -> %q", des, fromStatus, toStatus)
	}
	// nil -> SQL NULL. Any transition away from 'refunded' therefore clears a
	// previously-set kind as part of the same guarded write.
	var kindArg any
	if toStatus == "refunded" && refundKind != "" {
		kindArg = refundKind
	}
	// One BEGIN IMMEDIATE transaction so the guarded UPDATE and the read-back
	// SELECT see a consistent row (mirror of Python's single `with c` conn).
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	res, err := tx.Exec("UPDATE orders SET status=?, refund_kind=? WHERE des=? AND status=?",
		toStatus, kindArg, des, fromStatus)
	if err != nil {
		return nil, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	if n != 1 {
		// Lost the race (or unknown des) — caller must not act (store.py:267-268).
		return nil, tx.Commit()
	}
	o, err := scanOrder(tx.QueryRow("SELECT "+orderCols+" FROM orders WHERE des=?" /**/, des))
	if err != nil {
		return nil, err
	}
	return o, tx.Commit()
}

// ReservedOrderForChat returns this chat's currently-reserved (unpaid) order,
// or nil (store.py:273-285). Used to detect a checkout still awaiting payment
// when the customer starts a new action. Wallet top-ups excluded.
func (s *Store) ReservedOrderForChat(chatID string) (*Order, error) {
	o, err := scanOrder(s.db.QueryRow(
		"SELECT "+orderCols+" FROM orders WHERE chat_id=? AND status='reserved' "+
			"AND product!=? ORDER BY ts DESC LIMIT 1", chatID, DepositProduct))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return o, err
}

// OrdersForChat returns this chat's orders, newest first (store.py:288-309).
// Powers the customer /history view. statuses: optional filter (nil/empty =
// all); sinceTS: optional epoch lower bound on ts (nil = no bound).
func (s *Store) OrdersForChat(chatID string, limit int, statuses []string, sinceTS *float64) ([]*Order, error) {
	q := "SELECT " + orderCols + " FROM orders WHERE chat_id=?"
	args := []any{chatID}
	if len(statuses) > 0 {
		q += " AND status IN (" + placeholders(len(statuses)) + ")"
		for _, st := range statuses {
			args = append(args, st)
		}
	}
	if sinceTS != nil {
		q += " AND ts>=?"
		args = append(args, *sinceTS)
	}
	q += " ORDER BY ts DESC LIMIT ?"
	args = append(args, limit)
	return s.queryOrders(q, args...)
}

// AllOrders returns every order across ALL customers, newest first
// (store.py:503-519). statuses: optional filter; nil = all. Used by admin
// maintenance such as the Entra 'Clean Up Orders' tool.
func (s *Store) AllOrders(statuses []string) ([]*Order, error) {
	q := "SELECT " + orderCols + " FROM orders"
	var args []any
	if len(statuses) > 0 {
		q += " WHERE status IN (" + placeholders(len(statuses)) + ")"
		for _, st := range statuses {
			args = append(args, st)
		}
	}
	q += " ORDER BY ts DESC"
	return s.queryOrders(q, args...)
}

// RecentOrders returns the most recent orders, newest first (store.py:874-889).
// statuses: optional filter; nil = all statuses.
func (s *Store) RecentOrders(limit int, statuses []string) ([]*Order, error) {
	q := "SELECT " + orderCols + " FROM orders"
	var args []any
	if len(statuses) > 0 {
		q += " WHERE status IN (" + placeholders(len(statuses)) + ")"
		for _, st := range statuses {
			args = append(args, st)
		}
	}
	q += " ORDER BY ts DESC LIMIT ?"
	args = append(args, limit)
	return s.queryOrders(q, args...)
}

// ReservedOrdersBefore returns reserved orders created before `tsCutoff`
// (store.py:817-824). Powers the restart-safe expiry backstop that expires
// checkouts whose per-order timer goroutine died.
func (s *Store) ReservedOrdersBefore(tsCutoff float64) ([]*Order, error) {
	return s.queryOrders(
		"SELECT "+orderCols+" FROM orders WHERE status='reserved' AND ts<?",
		tsCutoff)
}

// PaidOrders returns orders stuck in 'paid' (money in, delivery never
// completed), newest first (store.py:827-835). Surfaced by the cron reconcile
// for manual review — re-driving them moves real funds, so they are
// intentionally never auto-handled.
func (s *Store) PaidOrders() ([]*Order, error) {
	return s.queryOrders(
		"SELECT " + orderCols + " FROM orders WHERE status='paid' ORDER BY ts DESC")
}

// HasDeliveredOrder reports whether this chat has any DELIVERED account order
// (i.e. is not a first-time buyer). Wallet top-ups are excluded
// (store.py:1150-1157).
func (s *Store) HasDeliveredOrder(chatID string) (bool, error) {
	var one int
	err := s.db.QueryRow(
		"SELECT 1 FROM orders WHERE chat_id=? AND status='delivered' "+
			"AND product!=? LIMIT 1", chatID, DepositProduct).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

// ProductSummary is one row of the delivered-per-product aggregate
// (store.py:485-500).
type ProductSummary struct {
	Product  string
	Accounts int64 // Σ qty
	Paid     int64 // Σ total, VND
	Orders   int64
}

// DeliveredProductSummary aggregates a chat's DELIVERED orders per product for
// the /history summary (store.py:485-500). Aggregates in SQL over ALL
// delivered orders (no row cap), excluding wallet top-ups; ordered by amount
// paid desc.
func (s *Store) DeliveredProductSummary(chatID string) ([]ProductSummary, error) {
	rows, err := s.db.Query(
		"SELECT product, COALESCE(SUM(qty),0) AS accounts, "+
			"COALESCE(SUM(total),0) AS paid, COUNT(*) AS orders "+
			"FROM orders WHERE chat_id=? AND status='delivered' AND product!=? "+
			"GROUP BY product ORDER BY paid DESC", chatID, DepositProduct)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProductSummary
	for rows.Next() {
		var (
			p  sql.NullString
			ps ProductSummary
		)
		if err := rows.Scan(&p, &ps.Accounts, &ps.Paid, &ps.Orders); err != nil {
			return nil, err
		}
		ps.Product = p.String
		out = append(out, ps)
	}
	return out, rows.Err()
}

// ReservedApikeyCredits returns the total CREDITS locked in currently-RESERVED
// (unpaid, not-yet-minted) API-key orders across ALL customers
// (store.py:367-383). For an API-key order the `qty` column holds the credit
// amount, so this is SUM(qty) over reserved apikey orders. Counts BOTH new-key
// sales and recharge top-ups. nil products = the Python default
// ("__apikey__", "__apikeyrc__").
func (s *Store) ReservedApikeyCredits(products []string) (int64, error) {
	if len(products) == 0 {
		products = []string{"__apikey__", "__apikeyrc__"}
	}
	q := fmt.Sprintf("SELECT COALESCE(SUM(qty),0) FROM orders "+
		"WHERE product IN (%s) AND status='reserved'", placeholders(len(products)))
	args := make([]any, len(products))
	for i, p := range products {
		args[i] = p
	}
	var total int64
	err := s.db.QueryRow(q, args...).Scan(&total)
	return total, err
}
