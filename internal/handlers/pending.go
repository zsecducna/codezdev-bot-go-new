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
	Txid       *TxidCtx      // pending_txid @3455 {chain, des, startTs}
	Code       *OTPCtx       // pending_code @3488
	CustReset  *CustResetCtx // pending_custreset @3495
	CustDelete *CustDelCtx   // pending_custdelete @3500
	Sell       *SellState    // pending_sell @3560 {step, fields…}
	APIKey     *APIKeyCtx    // pending_apikey @3844 {saleClass}
	APIKeyRC   *APIKeyRCCtx  // pending_apikeyrc @3848 {targetOrderID}

	// ---- admin slots (cleared by ClearAdminPending) ----
	Admin             *AdminAction  // pending_admin @1592 {action, step, fields, photo}
	Entra             *EntraState   // pending_entra @1620 {action, step, …}
	EntraDomains      []string      // _entra_domain_cache @1629
	EntraVerifyCSV    *VerifyCSV    // _entra_verifycsv_cache @1632
	Price             string        // pending_price @3460 product name
	Tier              string        // pending_tier @3478 product name
	Hold              string        // pending_hold @3483 product name
	Upstock           string        // pending_upstock @3655 product name
	UpstockConfirm    *UpstockBatch // pending_upstock_confirm @3659
	UpstockAcc        *UpstockAcc   // pending_upstock_acc @3666 (also under Bot.upstockMu @3670)
	UpstockPreviewMsg int64         // upstock_preview_msg @3669
	Reset             bool          // pending_reset @3506
	GetOrd            bool          // pending_getord @3511
	Refund            bool          // pending_refund @3516
	Discount          bool          // pending_discount @3464
	Promo             bool          // pending_promo @3468
	ResellerDiscount  string        // pending_reseller_discount @3473 handle
	CustLookup        bool          // pending_custlookup @3521
	AddAdmin          bool          // pending_addadmin @3527
	AddReseller       bool          // pending_addreseller @3532
	AddWL             bool          // pending_addwl @3535
	RmAdmin           bool          // pending_rmadmin @3538
	SetRate           bool          // pending_setrate @3542
	SetBep20          bool          // pending_setbep20 @3546
	SetTron           bool          // pending_settron @3550
	SetMaxQty         bool          // pending_setmaxqty @3554
	SetAwsPrice       *int64        // pending_setawsprice @3564 sale id
	AwsReject         *int64        // pending_awsreject @3568 sale id
	RefundReject      *int64        // pending_refundreject @3572 request id
	NewProd           bool          // pending_newprod @3576
	RenProd           string        // pending_renprod @3580 product name
	SetNotice         string        // pending_setnotice @3584 product name
	SetDelivery       string        // pending_setdelivery @3588 product name
	EditFile          *EditFileCtx  // pending_editfile @3593
	RmProd            string        // pending_rmprod @3599 product name
	SetApiPrice       bool          // pending_setapiprice @3851
	SetApiMin         bool          // pending_setapimin @3852
	SetApiMax         bool          // pending_setapimax @3853
	SetApiCap         bool          // pending_setapicap @3854
	SetKiroPrice      bool          // pending_setkiroprice @3857
	SetKiroMin        bool          // pending_setkiromin @3858
	SetKiroMax        bool          // pending_setkiromax @3859
	SetKiroKey        bool          // pending_setkirokey @3860
	SetKiroURL        bool          // pending_setkirourl @3861
	SetKiroAcct       bool          // pending_setkiroacct @3865
	ProvPool          bool          // pending_provpool @3869

	// ---- ad-hoc runtime flags (handlers-go.md §8; NOT in any clear list) ----
	KBVersionSent        string      // _kb_version_sent @6942 (in-memory; restart re-pushes once)
	UpstockRendering     bool        // acc["rendering"] @8591 — exactly-one preview renderer
	USDTPolling          bool        // _usdt_polling @9850 — one confirm-poll per chat
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
type OrderCodeCtx struct{ Des string }          // pending_ordercode @1587
type DepositCtx struct{ Method string }         // pending_deposit @3447
type TxidCtx struct {                           // pending_txid @3455
	Chain, Des string
	StartTS    float64
}
// OTPCtx is the AWAIT phase of a login-code session — pending_code
// (telegram_bot.py:3488), a bare `pending_code[chat_id] = True` flag in Python:
// "the next plain-text line is one or more usernames to fetch OTPs for". It
// carries no data (the usernames arrive in the captured text), so the slot's
// presence IS the whole state; it stays a struct rather than a bool so it lives
// in the same nil-pointer clear list as every other customer capture.
type OTPCtx struct{} // pending_code @3488
type CustResetCtx struct{} // pending_custreset @3495
type CustDelCtx struct{}   // pending_custdelete @3500
// SellState — pending_sell @3560 (step + language today; M2.6 adds the fields).
type SellState struct {
	Step int
	Lang string
}
type APIKeyCtx struct{ SaleClass string }       // pending_apikey @3844
type APIKeyRCCtx struct{ TargetOrderID string } // pending_apikeyrc @3848
type AdminAction struct {                       // pending_admin @1592
	Action, Step string
}
type EntraState struct{ Action, Step string } // pending_entra @1620
type VerifyCSV struct{ Rows []string }        // _entra_verifycsv_cache @1632
type UpstockBatch struct{ Product string }    // pending_upstock_confirm @3659
type UpstockAcc struct{ Product string }      // pending_upstock_acc @3666
type EditFileCtx struct{ Path string }        // pending_editfile @3593
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
// buy-quantity, deposit-amount, order-code, login-code and sell flows each arm
// a "next plain text is for me" slot checked in a FIXED priority order; calling
// this on entry to any one of them ensures only the flow just started stays
// live, so a stale sibling can't silently eat the next number. Exactly the 10
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
	st.Sell = nil
	st.CustReset = nil
	st.CustDelete = nil
	st.Txid = nil
}

// ClearAdminPending drops every half-finished ADMIN input flow for this chat —
// port of _clear_admin_pending (telegram_bot.py:3602-3624). Called when an
// admin (re)enters the panel or starts a new admin command so a stale flag
// (e.g. an abandoned "add admin" prompt) can't consume their next plain text.
// Exactly the 46 slots the Python loop pops (handlers-go.md §3); a unit test
// asserts this cleared set matches that list by reflection.
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
	st.Upstock = ""
	st.UpstockConfirm = nil
	st.UpstockAcc = nil
	st.UpstockPreviewMsg = 0
	st.Price = ""
	st.Tier = ""
	st.Hold = ""
	st.NewProd = false
	st.RenProd = ""
	st.SetNotice = ""
	st.SetDelivery = ""
	st.EditFile = nil
	st.RmProd = ""
	st.Entra = nil
	st.SetRate = false
	st.SetBep20 = false
	st.SetTron = false
	st.SetApiPrice = false
	st.SetApiMin = false
	st.SetApiMax = false
	st.SetKiroPrice = false
	st.SetKiroMin = false
	st.SetKiroMax = false
	st.SetApiCap = false
	st.SetKiroKey = false
	st.SetKiroURL = false
	st.SetKiroAcct = false
	st.ProvPool = false
	st.SetMaxQty = false
	st.SetAwsPrice = nil
	st.AwsReject = nil
	st.RefundReject = nil
	st.EntraDomains = nil
	st.EntraVerifyCSV = nil
}
