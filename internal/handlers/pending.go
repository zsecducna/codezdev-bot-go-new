// Package handlers is the Go port of telegram_bot.py's dispatch/UI layer: the
// per-update router (process() @16916-18720), the command router
// (handle_command @11627), the callback router (handle_callback @12221), the
// reply/inline keyboards, and the ~60 per-chat pending-state dicts that drive
// the multi-step dialogs.
//
// handlers-go.md §1: this is ONE package, split by FILE not by package —
// admin-is-also-a-customer, captures reference each other's slots, and the
// mass-clears span both sides, so an admin/customer package split would import-
// cycle through the shared ChatState + render helpers. Nothing imports
// handlers; handlers import everything (tg, store, config, i18n, …). No
// package-level mutable state: every global that was Python module state hangs
// off *Bot so tests construct isolated instances.
package handlers

import (
	"sync"
	"time"

	"codezdev-bot/internal/stock"
)

// States is the per-chat state registry — the Go replacement for the ~60
// module-level `pending_*` dicts (declared @1583-3869, @6942). One ChatState
// per chat, created lazily. States were in-memory only in Python (a restart
// drops in-flight dialogs by design, analysis §1.17); payment pendings/orders
// persist in the DB instead.
type States struct {
	mu sync.Mutex
	m  map[int64]*ChatState
}

// NewStates builds an empty registry.
func NewStates() *States { return &States{m: map[int64]*ChatState{}} }

// Get returns chat's state, creating it on first use (mirrors Python's
// `dict.setdefault`-style lazy population across the pending_* dicts).
func (s *States) Get(chat int64) *ChatState {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.m[chat]
	if st == nil {
		st = &ChatState{}
		s.m[chat] = st
	}
	return st
}

// ChatState holds every per-chat pending slot + runtime flag for one chat. A
// single struct replaces the 57 module dicts so sibling captures live together
// and the mass-clears can pop them under one lock (handlers-go.md §3).
//
// Locking: st.mu guards EVERY field below. Per handlers-go.md §8, the lock is
// never held across network I/O — callers mutate flags via short helper methods
// and release before any Telegram call (Python's rule @8589-8592).
type ChatState struct {
	mu sync.Mutex // guards every field below

	// ---- customer slots (cleared by ClearCustomerPending) ----
	Purchase   *PurchaseCtx  // pending_purchase @1583 {product, tier}
	OrderCode  *OrderCodeCtx // pending_ordercode @1587 {des}
	Deposit    *DepositCtx   // pending_deposit @3447 {method}
	Code       *OTPCtx       // pending_code @3488
	CustReset  *CustResetCtx // pending_custreset @3495
	CustDelete *CustDelCtx   // pending_custdelete @3500
	APIKey     *APIKeyCtx    // pending_apikey @3844 {saleClass}
	APIKeyRC   *APIKeyRCCtx  // pending_apikeyrc @3848 {targetOrderID}

	// ---- admin slots (cleared by ClearAdminPending) ----
	Admin             *AdminAction          // pending_admin @1592 {action, step, fields, photo}
	Entra             *EntraState           // pending_entra @1620 {action, step, …}
	EntraDomains      []string              // _entra_domain_cache @1629
	EntraVerifyCSV    *VerifyCSV            // _entra_verifycsv_cache @1632
	Price             *PriceCtx             // pending_price @3460 {product, code}
	Tier              string                // pending_tier @3478 product name
	Hold              *HoldArm              // pending_hold (py_stock_spec.md §3.3) {product,action}
	Upstock           *UpstockArm           // pending_upstock (py_stock_spec.md §1.3) {product,type}
	UpstockConfirm    *stock.UpstockPreview // pending_upstock_confirm (§1.7) — the de-duped, ready-to-commit snapshot
	UpstockAcc        *stock.UpstockBatch   // pending_upstock_acc (§1.7) — the running accumulator, also guarded by st.mu (Python: a global _upstock_lock; here the per-chat st.mu gives the same single-writer guarantee at finer granularity)
	UpstockPreviewMsg int64                 // upstock_preview_msg (§1.7) — message id of the live preview, edited/deleted in place
	UpstockStoreSubs  *StoreSubCache        // _store_sub_cache (§2.3) — upstockstoreprod:/upstockstoresub: cross-tap cache
	Reset             bool                  // pending_reset @3506
	GetOrd            bool                  // pending_getord @3511
	Refund            bool                  // pending_refund @3516
	Discount          bool                  // pending_discount @3464
	Promo             bool                  // pending_promo @3468
	ResellerDiscount  string                // pending_reseller_discount @3473 handle
	CustLookup        bool                  // pending_custlookup @3521
	AddAdmin          bool                  // pending_addadmin @3527
	AddReseller       bool                  // pending_addreseller @3532
	AddWL             bool                  // pending_addwl @3535
	RmAdmin           bool                  // pending_rmadmin @3538
	SetBep20          bool                  // pending_setbep20 @3546
	SetTron           bool                  // pending_settron @3550
	SetMaxQty         bool                  // pending_setmaxqty @3554
	RefundReject      *int64                // pending_refundreject @3572 request id
	NewProd           bool                  // pending_newprod @3576
	RenProd           string                // pending_renprod @3580 product name
	SetNotice         string                // pending_setnotice @3584 product name
	SetDelivery       string                // pending_setdelivery @3588 product name
	EditFile          *EditFileCtx          // pending_editfile @3593
	RmProd            string                // pending_rmprod @3599 product name
	SetApiPrice       bool                  // pending_setapiprice @3851
	SetApiMin         bool                  // pending_setapimin @3852
	SetApiMax         bool                  // pending_setapimax @3853
	SetApiCap         bool                  // pending_setapicap @3854
	// SetApiName / SetKiroName arm the Go-only SKU display-rename wizards
	// (adm:apiname / adm:kironame, adminpricing.go). No Python line to cite —
	// the original has no rename feature (same Go-only precedent as
	// captures.go's "provpool").
	SetApiName  bool
	SetKiroName bool
	// SetModelPrice arms the Go-only Fixed Pricing per-model flat-price wizard
	// (adm:setmodelprice, adminpricing.go). No Python line to cite — the
	// "fixed" sale class postdates the original entirely (same Go-only
	// precedent as the two rename flags above and captures.go's "provpool").
	SetModelPrice bool
	SetKiroPrice  bool // pending_setkiroprice @3857
	SetKiroMin    bool // pending_setkiromin @3858
	SetKiroMax    bool // pending_setkiromax @3859
	// SetFixedPrice / SetFixedMin / SetFixedMax / SetFixedName arm the
	// Go-only Fixed Pricing SKU wizards (adm:fixedprice / adm:fixedmin /
	// adm:fixedmax / adm:fixedname, adminpricing.go) — structural twins of
	// the SetKiro* trio plus the rename flag above. No Python line to cite:
	// the "fixed" sale class postdates the original entirely.
	SetFixedPrice bool
	SetFixedMin   bool
	SetFixedMax   bool
	SetFixedName  bool
	SetKiroKey    bool             // pending_setkirokey @3860
	SetKiroURL    bool             // pending_setkirourl @3861
	SetKiroAcct   bool             // pending_setkiroacct @3865
	ProvPool      bool             // pending_provpool @3869
	RefundDelete  *RefundDeleteCtx // pending_entra{"action":"refund_delete"|"refund_delete_single",...} @2929/@12830 — the admrefdel bulk sweep / refdelone: per-order Entra-deprovisioning confirm gate (adminorders.go). Its own slot, not the generic Entra field above: pending_entra in Python is a free-form dict shared by many unrelated Entra flows (login, genusers, cleanup, …), none of which have landed in Go yet — reusing the thin {Action,Step string} EntraState here would either collide with those future flows' shapes or force this one to squeeze emails/orders into two string fields it can't hold.

	// ---- ad-hoc runtime flags (handlers-go.md §8; NOT in any clear list) ----
	KBVersionSent    string // _kb_version_sent @6942 (in-memory; restart re-pushes once)
	UpstockRendering bool   // acc["rendering"] @8591 — exactly-one preview renderer
	// UpstockDirty is acc["dirty"] (§1.7): set true whenever Add() appends new
	// items to UpstockAcc; the render loop clears it each pass and stops once
	// it finds it false (no new items arrived since the last render). Not in
	// any clear list, same rationale as UpstockRendering: both are transient
	// render-loop bookkeeping the loop itself resets to a safe state (acc==nil
	// -> rendering=false) within one throttle tick even if ClearAdminPending
	// fires mid-render.
	UpstockDirty         bool
	OTPSession           *OTPSession // _otp_session_* @8166-8228
	LastMenu             int64       // last_menu — menu msg deleted on next callback @12280
	ProductClicks        []time.Time // _product_click_allowed @12191 sliding window
	ProductCooldownUntil time.Time   // _product_click_allowed @12191 cooldown
}

// ---- placeholder context types ------------------------------------------
//
// These carry only the fields M2.1 references (mostly none — the dispatch
// skeleton just needs the SLOTS to exist for the clear lists + golden order).
// Each later slice fleshes out its own type's fields with the cited Python
// dict shape; the identifiers are declared here so ChatState compiles and the
// clear-list reflection test can allocate/zero every slot today.

type PurchaseCtx struct{ Product, Tier string } // pending_purchase @1583

// PriceCtx is the pending pricetier: edit-existing-tier wizard — pending_price
// (telegram_bot.py:3460, a {"product","code"} dict). Product+Code together
// pin down EXACTLY which price_<CODE>_VND file the admin's next typed line
// overwrites (adminpricing.go's capPrice); unlike Tier above (armed by
// tieradd:, which only ever needs the product — the code is typed as part of
// the message), pricetier: needs both, so this can't reuse a bare string.
type PriceCtx struct{ Product, Code string }
type OrderCodeCtx struct{ Des string }  // pending_ordercode @1587
type DepositCtx struct{ Method string } // pending_deposit @3447

// OTPCtx is the AWAIT phase of a login-code session — pending_code
// (telegram_bot.py:3488), a bare `pending_code[chat_id] = True` flag in Python:
// "the next plain-text line is one or more usernames to fetch OTPs for". It
// carries no data (the usernames arrive in the captured text), so the slot's
// presence IS the whole state; it stays a struct rather than a bool so it lives
// in the same nil-pointer clear list as every other customer capture.
type OTPCtx struct{}                            // pending_code @3488
type CustResetCtx struct{}                      // pending_custreset @3495
type CustDelCtx struct{}                        // pending_custdelete @3500
type APIKeyCtx struct{ SaleClass string }       // pending_apikey @3844
type APIKeyRCCtx struct{ TargetOrderID string } // pending_apikeyrc @3848
type AdminAction struct {                       // pending_admin @1592
	Action, Step string
	// Message / PhotoID carry the broadcast wizard's single field
	// (adminsettings.go, py_settings_spec.md §5) — the Go analogue of
	// Python's generic pending_admin["values"] dict, specialized since only
	// "broadcast" and "addbal" ADMIN_ACTIONS members are ported so far.
	// PhotoID is a Telegram file_id, never a secret, so it carries no
	// logging restriction.
	Message string
	PhotoID string
	// Target / Amount carry adm:addbal's two-field collector (adminusers.go,
	// py_customers_spec.md §2: admin_start_action/admin_collect_input's
	// {"chat_id","amount"} values dict). M3/M4 fix: addbal used to live in
	// its OWN ChatState slot (AddBalCtx) with its OWN admdo:addbalconfirm/
	// admdo:addbalcancel callback strings, specifically because an earlier
	// task's file-ownership split forbade extending this shared executor —
	// that constraint no longer applies, so addbal now rides the SAME
	// pending_admin-equivalent slot and Step machine every other
	// ADMIN_ACTIONS member uses (Step walks "collecting_chatid" ->
	// "collecting_amount" -> "confirm"), and admdo:confirm/admdo:cancel are
	// Python's LITERAL callback strings again. This also fixes two bugs the
	// separate slot had: (M3) a stray admin text typed while some OTHER
	// wizard is armed could no longer land in this slot's capture by
	// accident, since there is only ONE admin wizard slot now, matching
	// Python's own single pending_admin dict; (M4) capAddBal used to go
	// inert (return false, falling through to later captures) while
	// awaiting Confirm/Cancel — Python's admin_collect_input instead
	// swallows stray text at that point (telegram_bot.py:17252-17254); the
	// shared executor's entry gate (step=="confirm" -> not "collecting",
	// return false only when armed is nil) now matches that.
	Target string
	Amount string
	// ID is addbal's dedupe key for store.AdminCreditBalance (adminusers.go),
	// minted ONCE at arm time (cbAddBal) rather than at exec time, so it stays
	// stable across the whole collecting_chatid -> collecting_amount ->
	// confirm walk and genuinely identifies "this one armed wizard instance" —
	// an id minted fresh inside execAddBal itself can never collide with
	// anything, which made the dedupe gate structurally unable to fire
	// (review finding on the execAddBal idempotency commit). A fresh adm:addbal
	// arm mints a fresh ID (new AdminAction), matching execAddBal's own
	// documented stance that a manual re-arm is a genuinely new action, not a
	// replay. Unused by "broadcast" actions.
	ID string
}
type EntraState struct{ Action, Step string } // pending_entra @1620
type VerifyCSV struct{ Rows []string }        // _entra_verifycsv_cache @1632

// HoldArm is the armed hold/restore wizard slot — pending_hold
// (py_stock_spec.md §3.3), a {"product","action"} dict; Action is "hold" or
// "restore". Unlike every other pending_* text capture in this admin
// category, a bad (non-numeric) quantity RE-ARMS this slot instead of
// aborting it (py_stock_spec.md §3.4/§11) — capHold must re-set (not clear)
// this pointer on a failed parse.
type HoldArm struct{ Product, Action string }

// UpstockArm is the armed upstock wizard slot — pending_upstock
// (py_stock_spec.md §1.3), a {"product","type"} dict. Kind is clamped to one
// of the four stock.UpstockKind constants by the type-picker callback BEFORE
// arming (never persisted to the product's stamped account-type file until a
// CONFIRMED commit — §1.9 — so an abandoned upstock never mutates a live
// product's format).
type UpstockArm struct {
	Product string
	Kind    stock.UpstockKind
}

// StoreSubCache is the upstockstoreprod:/upstockstoresub: cross-tap cache —
// Python's _store_sub_cache[chat_id] (py_stock_spec.md §2.3). Subscription
// folder NAMES can be long/arbitrary, unlike every other per-product picker
// in this category (which safely re-resolves an INDEX against a fresh
// listProducts() at tap time) — so the {product, subs} pair chosen by the
// FIRST tap is cached here to survive to the SECOND tap, rather than being
// round-tripped through callback_data.
type StoreSubCache struct {
	Product string
	Subs    []stock.StoreSub
}

// EditFileCtx is the armed config/.env editor slot — pending_editfile
// (py_stock_spec.md §4.7), a {"product","file"} dict; File is "config" or
// ".env". Unlike newprod/renprod/setnotice/setdelivery (silent abort on a
// nav/command escape), this ONE aborts WITH an explicit "↩️ Edit
// cancelled…" message (§11) — capEditFile must send it before falling
// through.
type EditFileCtx struct{ Product, File string }

// RefundDeleteCtx is the admrefdel / refdelone: Entra-deprovisioning confirm
// state (adminorders.go) — pending_entra's {"action": "refund_delete" |
// "refund_delete_single", "emails": [...], "orders": [...], "des": ...,
// "confirm_ready": true} shape (telegram_bot.py:2929, 12830). ConfirmReady is
// implicit here (the pointer's non-nil presence IS "confirm_ready": true —
// nothing arms this struct in a not-yet-ready state, unlike Python's dict
// which could in theory be built without that key).
type RefundDeleteCtx struct {
	Single bool     // true = the per-order refdelone:/refdeloneok: variant; false = the bulk admrefdel sweep
	Des    string   // only set when Single — the one order this confirm covers
	Orders []string // order ids in this batch (bulk variant; len==1 for Single, mirroring Des)
	Emails []string // de-duped @-emails to soft-delete
}

// OTPSession is the STREAMING phase of a login-code session plus the per-chat
// cooldown stamp — the Go home of Python's four module dicts
// (_otp_active / _otp_epoch / _otp_started @8166-8168 and _otp_last @645), all
// keyed by chat and all guarded there by _otp_lock. Here they are fields of one
// per-chat struct guarded by st.mu, so "leave the await phase and open the
// streaming phase" (_otp_session_start @8194) is ONE critical section together
// with the pending_code pop — "session live" can never flicker false in between.
//
// The struct is allocated on first use and then NEVER set back to nil: Epoch and
// LastAccepted must outlive an individual session (an epoch that restarted at 0
// would let a stopped fetch's stale result be delivered, and a dropped cooldown
// stamp would reopen the anti-spam window). Session liveness is Active > 0, not
// the pointer's presence.
type OTPSession struct {
	// Active is the number of unfinished fetches in the current batch —
	// _otp_active[chat_id] (@8167). 0 means no batch is streaming.
	Active int
	// Epoch is the ever-increasing batch token — _otp_epoch[chat_id] (@8168).
	// Each fetch goroutine captures the value it started under; a stop (or a
	// superseding batch) bumps it so those goroutines discard their in-flight
	// result instead of DMing a stale code.
	Epoch int64
	// StartedAt is the current batch's start time — _otp_started[chat_id]
	// (@8169), read by the OTP_SESSION_MAX_S stale-session watchdog.
	StartedAt time.Time
	// LastAccepted is the time of the last ACCEPTED /otp request — _otp_last
	// (@645), the OTP_COOLDOWN_S anti-spam stamp. Kept here (not in a clear
	// list) because a rate limit a customer could reset by cancelling would be
	// no rate limit at all.
	LastAccepted time.Time
}

// ---- mass-clears --------------------------------------------------------

// ClearCustomerPending drops every half-finished CUSTOMER text-capture flow for
// this chat — port of _clear_customer_pending (telegram_bot.py:3627-3645). The
// buy-quantity, deposit-amount, order-code and login-code flows each arm
// a "next plain text is for me" slot checked in a FIXED priority order; calling
// this on entry to any one of them ensures only the flow just started stays
// live, so a stale sibling can't silently eat the next number. Exactly the 9
// slots the Python loop pops (handlers-go.md §3).
func (st *ChatState) ClearCustomerPending() {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.Purchase = nil
	st.Deposit = nil
	st.APIKey = nil
	st.APIKeyRC = nil
	st.OrderCode = nil
	st.Code = nil
	st.CustReset = nil
	st.CustDelete = nil
}

// ClearAdminPending drops every half-finished ADMIN input flow for this chat —
// port of _clear_admin_pending (telegram_bot.py:3602-3624). Called when an
// admin (re)enters the panel or starts a new admin command so a stale flag
// (e.g. an abandoned "add admin" prompt) can't consume their next plain text.
// The Python loop's slots (handlers-go.md §3) PLUS the Go-only additions that
// have no Python counterpart (SetApiName / SetKiroName, the SKU
// display-rename wizards; SetModelPrice, the Fixed Pricing per-model
// flat-price wizard; and SetFixedPrice / SetFixedMin / SetFixedMax /
// SetFixedName, the Fixed Pricing SKU's own price/bounds/name wizards); a
// unit test asserts this cleared set matches that list by reflection.
func (st *ChatState) ClearAdminPending() {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.Admin = nil
	st.CustLookup = false
	st.AddAdmin = false
	st.AddReseller = false
	st.AddWL = false
	st.RmAdmin = false
	st.Reset = false
	st.GetOrd = false
	st.Refund = false
	st.Discount = false
	st.Promo = false
	st.ResellerDiscount = ""
	st.Upstock = nil
	st.UpstockConfirm = nil
	st.UpstockAcc = nil
	st.UpstockPreviewMsg = 0
	st.UpstockStoreSubs = nil
	st.Price = nil
	st.Tier = ""
	st.Hold = nil
	st.NewProd = false
	st.RenProd = ""
	st.SetNotice = ""
	st.SetDelivery = ""
	st.EditFile = nil
	st.RmProd = ""
	st.Entra = nil
	st.SetBep20 = false
	st.SetTron = false
	st.SetApiPrice = false
	st.SetApiMin = false
	st.SetApiMax = false
	st.SetKiroPrice = false
	st.SetKiroMin = false
	st.SetKiroMax = false
	st.SetFixedPrice = false
	st.SetFixedMin = false
	st.SetFixedMax = false
	st.SetFixedName = false
	st.SetApiCap = false
	st.SetApiName = false
	st.SetKiroName = false
	st.SetModelPrice = false
	st.SetKiroKey = false
	st.SetKiroURL = false
	st.SetKiroAcct = false
	st.ProvPool = false
	st.SetMaxQty = false
	st.RefundDelete = nil
	st.RefundReject = nil
	st.EntraDomains = nil
	st.EntraVerifyCSV = nil
}
