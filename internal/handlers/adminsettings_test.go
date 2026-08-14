package handlers

// adminsettings_test.go — tests for the ⚙️ SETTINGS + 💳 PAYMENT METHODS
// callbacks (adminsettings.go). Uses newTestBot/callbackUpdate/textUpdate/
// lastSendMsg/buttonData from fake_test.go and admin_test.go; a local
// newSettingsTestBot builds a *config.Config-backed Bot (newTestBot's Config
// is always nil) so toggles/setters have somewhere real to persist to.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"codezdev-bot/internal/config"
	"codezdev-bot/internal/pay/bep20"
	"codezdev-bot/internal/pay/tron"
	"codezdev-bot/internal/store"
	"codezdev-bot/internal/tg"
)

// newSettingsTestBot builds a Bot wired to a fresh fakeTG AND a real
// *config.Config rooted at a temp dir, so SaveSettings actually persists
// (newTestBot's Config is nil, which is fine for pure dispatch tests but
// would panic/no-op every toggle in this file).
func newSettingsTestBot(t *testing.T, adminID string) (*Bot, *fakeTG, *config.Config) {
	t.Helper()
	dir := t.TempDir()
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatalf("config load: %v", err)
	}
	f := &fakeTG{}
	b := New(Deps{TG: f, Config: cfg, AdminID: adminID})
	t.Cleanup(b.Shutdown)
	return b, f, cfg
}

// photoUpdate builds a photo message update — the shape capBroadcastPhoto
// (adminsettings.go) reads: Photo + optional Caption, no Text.
func photoUpdate(chatID int64, fileID, caption string) tg.Update {
	return tg.Update{Message: &tg.Message{
		MessageID: 2,
		Chat:      tg.Chat{ID: chatID, Type: "private"},
		From:      &tg.User{ID: chatID},
		Photo:     []tg.PhotoSize{{FileID: fileID}},
		Caption:   caption,
	}}
}

// ---- validators -------------------------------------------------------------

func TestKiroURLValidator(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"http://localhost:8080", true},
		{"https://kiro.example.com/api", true},
		{"ftp://bad", false},
		{"not a url", false},
		{"http://has space.com", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := kiroURLRE.MatchString(tc.in); got != tc.want {
			t.Errorf("kiroURLRE(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestKiroAcctUUIDValidator(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"22bb9057-183d-4ff6-89c4-7f230cb98b8e", true},
		{"22BB9057-183D-4FF6-89C4-7F230CB98B8E", true},
		{"not-a-uuid", false},
		{"22bb9057-183d-4ff6-89c4", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := kiroAcctUUIDRE.MatchString(tc.in); got != tc.want {
			t.Errorf("kiroAcctUUIDRE(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestMaxQtyValidator drives every case through b.Process -> capSetMaxQty
// (M4 fix, same rationale as TestRateValidatorRange above).
func TestMaxQtyValidator(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		raw    string
		wantOK bool
		wantN  int
	}{
		{"10", true, 10},
		{"0", true, 0},
		{"10,000", true, 10000},
		{"-1", false, 0},
		{"abc", false, 0},
		{" ", false, 0},
	}
	// sentinelMaxQty primes max_qty to a value distinct from every wantOK
	// case above (including the valid "0" = unlimited case) so the rejection
	// branch's "unchanged" assertion can't trivially pass against config's
	// own zero-value default.
	const sentinelMaxQty = 77
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			b, f, cfg := newSettingsTestBot(t, "1")
			s := cfg.Settings()
			s["max_qty"] = sentinelMaxQty
			if err := cfg.SaveSettings(s); err != nil {
				t.Fatal(err)
			}
			b.Process(ctx, callbackUpdate(1, "adm:setmaxqty"))
			b.Process(ctx, textUpdate(1, tc.raw))
			last := lastSendMsg(f)
			if tc.wantOK {
				want := "✅ Max bulk buy set to unlimited."
				if tc.wantN != 0 {
					want = "✅ Max bulk buy set to " + itoa(tc.wantN) + " accounts per order."
				}
				if last == nil || last.Text != want {
					t.Fatalf("raw=%q reply = %+v, want %q", tc.raw, last, want)
				}
				if cfg.MaxBulk() != tc.wantN {
					t.Errorf("raw=%q MaxBulk() = %d, want %d", tc.raw, cfg.MaxBulk(), tc.wantN)
				}
			} else {
				want := "⚠️ Enter a whole number (0 = unlimited), e.g. 10."
				if last == nil || last.Text != want {
					t.Fatalf("raw=%q reply = %+v, want %q", tc.raw, last, want)
				}
				if cfg.MaxBulk() != sentinelMaxQty {
					t.Errorf("raw=%q: rejected input must not change max_qty, MaxBulk() = %d, want unchanged %d", tc.raw, cfg.MaxBulk(), sentinelMaxQty)
				}
			}
		})
	}
}

func TestBep20TronAddressValidators(t *testing.T) {
	validBep20 := "0x1234567890123456789012345678901234567890"
	if !bep20.ValidAddress(validBep20) {
		t.Errorf("valid BEP20 address rejected: %q", validBep20)
	}
	if bep20.ValidAddress("not-an-address") {
		t.Error("invalid BEP20 address accepted")
	}
	validTron := "T" + strings.Repeat("1", 33)
	if !tron.ValidAddress(validTron) {
		t.Errorf("valid TRON address rejected: %q", validTron)
	}
	if tron.ValidAddress("bad") {
		t.Error("invalid TRON address accepted")
	}
}

// ---- permission split ---------------------------------------------------

// TestSettingsPermissionSplit is the spec's central permission assertion
// (py_settings_spec.md §3): of all 22 callbacks, only togglesales and
// toggleprivate are reachable by a standard (granted, non-super) admin —
// everything else is refused.
//
// H2 fix (superseded by H1 below, kept for history): this used to assert the
// refusal via a SECOND answerCallbackQuery alert (f.Answers), which the
// fake-Telegram harness recorded unconditionally regardless of Telegram's
// real one-answer-per-callback_query_id rule — so the old assertion passed
// even though a real Telegram server would have dropped that second alert
// silently. settingsSuperAdminOnly's chat-message refusal fixed THAT
// specific problem.
//
// H1 fix (admincap.go): none of the superAdminOnly actions below are in
// stdAdminCbCap's table (none are grantable to a standard admin, matching
// Python's _std_admin_cb_cap — only togglesales/toggleprivate/wladd/rmwl are
// toggle_sales-capability), so command.go's central chokepoint now denies
// them all via a pre-ack ALERT before settingsSuperAdminOnly's chat message
// ever gets sent. That gate fires BEFORE the unconditional ack (H2's
// "second answer" problem doesn't apply — this is the FIRST and ONLY
// answer for the update), so f.Sends is empty for every denied action here;
// the assertion moves to wantAdminCapDenied (admin_test.go) accordingly.
func TestSettingsPermissionSplit(t *testing.T) {
	ctx := context.Background()
	superAdminOnly := []string{
		"togglebcast", "togglestockbcast", "togglesalenotify", "toggledepnotify",
		"togglemaint", "togglereferral", "setmaxqty", "setrate", "setkirourl",
		"setkirokey", "setkiroacct", "setbep20", "settron", "broadcast",
		"togglepay:balance", "togglepay:qr", "togglepay:paypal", "togglepay:binance",
		"togglepay:bep20", "togglepay:tron",
	}
	exempt := map[string]bool{"togglesales": true, "toggleprivate": true}

	run := func(action string) {
		b, f := newTestBot("999", nil) // super admin id "999"
		b.adminsMu.Lock()
		b.admins["2"] = true // chat 2: standard, GRANTED admin — not super
		b.adminsMu.Unlock()
		b.Process(ctx, callbackUpdate(2, "adm:"+action))
		if exempt[action] {
			// togglesales/toggleprivate are toggle_sales-capability and must
			// NOT be denied at all — the gate passes them straight through.
			if last := lastSendMsg(f); last == nil || last.Text == settingsPermissionDenied {
				t.Errorf("action %q: should be reachable by a standard admin, got %+v", action, last)
			}
			return
		}
		wantAdminCapDenied(t, f, "adm:"+action)
	}
	for _, a := range superAdminOnly {
		run(a)
	}
	for a := range exempt {
		run(a)
	}
}

// ---- toggleSetting: nil-cfg vs save-failure must not collapse -------------
//
// Task #29 continued: toggleSetting (adminsettings.go, shared by cbToggleSimple
// and cbToggleMaint) used to return (false, false) for BOTH "no config
// wired" and "config wired but the write failed" — the two callers could not
// tell the difference and always sent settingsNotConfigured, the exact bug
// already fixed at four direct call sites above (TestToggleSalesSendsSaveFailedOnWriteFailure
// and its siblings) but one frame further down, inside this shared helper —
// invisible to a grep for "settingsNotConfigured right after a SaveSettings
// err check", since the SaveSettings call and the string choice are no
// longer in the same function. toggleSetting now returns a distinguishable
// error (errToggleSettingNotConfigured for the nil-cfg case, the raw
// SaveSettings error otherwise) and each caller picks the reply text off
// that. Both directions are pinned per caller below — a test asserting only
// the failure-text case would still pass if the two strings were swapped at
// the call site.

// TestToggleSimpleSendsSaveFailedOnWriteFailure drives togglebcast, one of
// cbToggleSimple's four callers (togglebcast/togglestockbcast/
// togglesalenotify/toggledepnotify — cbToggleSimple's own doc comment). The
// other three share this exact function body by construction, so they are
// not separately driven here.
func TestToggleSimpleSendsSaveFailedOnWriteFailure(t *testing.T) {
	ctx := context.Background()
	b, f, _ := newReadOnlyCfgAuditBot(t)
	b.Process(ctx, callbackUpdate(1, "adm:togglebcast"))
	if last := lastSendMsg(f); last == nil || last.Text != settingsSaveFailed {
		t.Fatalf("reply on save failure = %+v, want %q", last, settingsSaveFailed)
	}
}

// TestToggleSimpleSendsNotConfiguredWhenCfgNil is the OTHER direction for
// cbToggleSimple: no Config wired at all (newTestBot, same nil-cfg shape
// TestExecBroadcastNilStoreNoPanic already uses for its own nil dependency)
// must still get settingsNotConfigured, not settingsSaveFailed.
func TestToggleSimpleSendsNotConfiguredWhenCfgNil(t *testing.T) {
	ctx := context.Background()
	b, f := newTestBot("1", nil)
	defer b.Shutdown()
	b.Process(ctx, callbackUpdate(1, "adm:togglebcast"))
	if last := lastSendMsg(f); last == nil || last.Text != settingsNotConfigured {
		t.Fatalf("reply with nil cfg = %+v, want %q", last, settingsNotConfigured)
	}
}

func TestToggleMaintSendsSaveFailedOnWriteFailure(t *testing.T) {
	ctx := context.Background()
	b, f, _ := newReadOnlyCfgAuditBot(t)
	b.Process(ctx, callbackUpdate(1, "adm:togglemaint"))
	if last := lastSendMsg(f); last == nil || last.Text != settingsSaveFailed {
		t.Fatalf("reply on save failure = %+v, want %q", last, settingsSaveFailed)
	}
}

func TestToggleMaintSendsNotConfiguredWhenCfgNil(t *testing.T) {
	ctx := context.Background()
	b, f := newTestBot("1", nil)
	defer b.Shutdown()
	b.Process(ctx, callbackUpdate(1, "adm:togglemaint"))
	if last := lastSendMsg(f); last == nil || last.Text != settingsNotConfigured {
		t.Fatalf("reply with nil cfg = %+v, want %q", last, settingsNotConfigured)
	}
}

// ---- simple bell/maintenance/referral toggles -------------------------------

func TestSimpleToggles(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		action, key     string
		def             bool
		onText, offText string
	}{
		{"togglebcast", "sale_broadcast", true,
			"Sale broadcast on delivery is now ON 🔔.", "Sale broadcast on delivery is now OFF 🔕."},
		{"togglestockbcast", "auto_stock_broadcast", true,
			"Auto restock alert is now ON 🔔.", "Auto restock alert is now OFF 🔕."},
		{"togglesalenotify", "admin_sale_notify", true,
			"New-sale admin alerts are now ON 🔔.", "New-sale admin alerts are now OFF 🔕."},
		{"toggledepnotify", "admin_deposit_notify", true,
			"New-deposit admin alerts are now ON 🔔.", "New-deposit admin alerts are now OFF 🔕."},
		{"togglemaint", "maintenance", false,
			"Maintenance mode is now ON 🛠️ (customers blocked).", "Maintenance mode is now OFF 🟢 (normal)."},
		{"togglereferral", "referral_enabled", false,
			"Referral program is now ON 👥 (customers can refer).", "Referral program is now OFF 🚫."},
	}
	for _, tc := range cases {
		t.Run(tc.action, func(t *testing.T) {
			b, f, cfg := newSettingsTestBot(t, "1")

			b.Process(ctx, callbackUpdate(1, "adm:"+tc.action))
			next1, _ := cfg.Settings()[tc.key].(bool)
			if next1 != !tc.def {
				t.Fatalf("%s: after 1st toggle, %s = %v, want %v", tc.action, tc.key, next1, !tc.def)
			}
			want1 := tc.offText
			if next1 {
				want1 = tc.onText
			}
			if last := lastSendMsg(f); last == nil || last.Text != want1 {
				t.Fatalf("%s: reply = %+v, want text %q", tc.action, last, want1)
			}

			b.Process(ctx, callbackUpdate(1, "adm:"+tc.action))
			next2, _ := cfg.Settings()[tc.key].(bool)
			if next2 != tc.def {
				t.Fatalf("%s: after 2nd toggle, %s = %v, want %v", tc.action, tc.key, next2, tc.def)
			}
			want2 := tc.offText
			if next2 {
				want2 = tc.onText
			}
			if last := lastSendMsg(f); last == nil || last.Text != want2 {
				t.Fatalf("%s: 2nd reply = %+v, want text %q", tc.action, last, want2)
			}
		})
	}
}

// ---- bespoke togglesales / toggleprivate ------------------------------------

func TestToggleSalesBespoke(t *testing.T) {
	ctx := context.Background()
	b, f, cfg := newSettingsTestBot(t, "1")

	s := cfg.Settings()
	s["private_sale"] = true
	s["sales_paused"] = false
	if err := cfg.SaveSettings(s); err != nil {
		t.Fatal(err)
	}

	// Pause: must NOT touch private_sale.
	b.Process(ctx, callbackUpdate(1, "adm:togglesales"))
	s = cfg.Settings()
	if v, _ := s["sales_paused"].(bool); !v {
		t.Fatalf("sales_paused = %v, want true after pausing", v)
	}
	if v, _ := s["private_sale"].(bool); !v {
		t.Fatalf("pausing must not touch private_sale, got %v want true (untouched)", v)
	}
	if last := lastSendMsg(f); last == nil || last.Text != "Sales are now PAUSED ⏸️ (no buying)." {
		t.Fatalf("reply = %+v", last)
	}

	// Unpause: must clear private_sale too.
	b.Process(ctx, callbackUpdate(1, "adm:togglesales"))
	s = cfg.Settings()
	if v, _ := s["sales_paused"].(bool); v {
		t.Fatalf("sales_paused = %v, want false after unpausing", v)
	}
	if v, ok := s["private_sale"].(bool); ok && v {
		t.Fatalf("unpausing must clear private_sale, got %v", v)
	}
	if last := lastSendMsg(f); last == nil || last.Text != "Sales are now ON 🟢 (buying allowed)." {
		t.Fatalf("reply = %+v", last)
	}
}

func TestTogglePrivateBespoke(t *testing.T) {
	ctx := context.Background()
	b, f, cfg := newSettingsTestBot(t, "1")

	b.Process(ctx, callbackUpdate(1, "adm:toggleprivate"))
	s := cfg.Settings()
	if v, _ := s["private_sale"].(bool); !v {
		t.Fatalf("private_sale = %v, want true", v)
	}
	if v, _ := s["sales_paused"].(bool); !v {
		t.Fatalf("enabling private sale must force sales_paused=true, got %v", v)
	}
	wantOn := "Private sale is now ON 🔒 — public sale auto-paused. Resellers + 0 whitelisted customer(s) can buy."
	if last := lastSendMsg(f); last == nil || last.Text != wantOn {
		t.Fatalf("reply = %+v, want %q", last, wantOn)
	}

	b.Process(ctx, callbackUpdate(1, "adm:toggleprivate"))
	s = cfg.Settings()
	if v, _ := s["private_sale"].(bool); v {
		t.Fatalf("private_sale = %v, want false", v)
	}
	if v, _ := s["sales_paused"].(bool); !v {
		t.Fatalf("disabling private sale must leave sales_paused untouched, got %v want true", v)
	}
	wantOff := "Private sale is now OFF 🔓 — back to the normal Sale on/off rule."
	if last := lastSendMsg(f); last == nil || last.Text != wantOff {
		t.Fatalf("reply = %+v, want %q", last, wantOff)
	}
}

// ---- toggle handlers: settingsSaveFailed on a genuine write failure -------
//
// Task #29 (Opus review of c4d4970): cbToggleSales, cbTogglePrivate, and
// cbToggleReferral each catch their own b.cfg.SaveSettings error but replied
// with settingsNotConfigured — "Settings not configured on this instance."
// — instead of settingsSaveFailed. Settings ARE configured on this path (a
// genuine nil-b.cfg reply is a separate, earlier branch in the same
// handlers, untouched by this fix); only the WRITE failed. The wrong text
// sent an admin off to check a config that was fine, at the exact moment
// the real fault — a failed disk write — was what needed their attention.
// newReadOnlyCfgAuditBot (adminpricing_test.go) forces every SaveSettings
// call to fail by loading config from a chmod 0500 directory — exactly the
// fault these sites need injected, so it is reused here rather than
// duplicating the read-only-dir setup TestSetBep20WizardNoAuditOnSaveFailure
// and TestSetTronWizardNoAuditOnSaveFailure above already inline by hand.

func TestToggleSalesSendsSaveFailedOnWriteFailure(t *testing.T) {
	ctx := context.Background()
	b, f, _ := newReadOnlyCfgAuditBot(t)
	b.Process(ctx, callbackUpdate(1, "adm:togglesales"))
	if last := lastSendMsg(f); last == nil || last.Text != settingsSaveFailed {
		t.Fatalf("reply on save failure = %+v, want %q", last, settingsSaveFailed)
	}
}

func TestTogglePrivateSendsSaveFailedOnWriteFailure(t *testing.T) {
	ctx := context.Background()
	b, f, _ := newReadOnlyCfgAuditBot(t)
	b.Process(ctx, callbackUpdate(1, "adm:toggleprivate"))
	if last := lastSendMsg(f); last == nil || last.Text != settingsSaveFailed {
		t.Fatalf("reply on save failure = %+v, want %q", last, settingsSaveFailed)
	}
}

func TestToggleReferralSendsSaveFailedOnWriteFailure(t *testing.T) {
	ctx := context.Background()
	b, f, _ := newReadOnlyCfgAuditBot(t)
	b.Process(ctx, callbackUpdate(1, "adm:togglereferral"))
	if last := lastSendMsg(f); last == nil || last.Text != settingsSaveFailed {
		t.Fatalf("reply on save failure = %+v, want %q", last, settingsSaveFailed)
	}
}

// ---- payments submenu re-render vs terminal-message split -------------------

func TestPaymentsToggleRerendersMenu(t *testing.T) {
	ctx := context.Background()
	b, f, _ := newSettingsTestBot(t, "1")
	b.Process(ctx, callbackUpdate(1, "adm:togglepay:bep20"))
	last := lastSendMsg(f)
	if last == nil {
		t.Fatal("no message sent")
	}
	if last.Text != "🔑 💳 PAYMENT METHODS" {
		t.Errorf("payments toggle must re-render the submenu with title %q, got %q", "🔑 💳 PAYMENT METHODS", last.Text)
	}
	data := buttonData(last.ReplyMarkup)
	found := false
	for _, d := range data {
		if strings.HasPrefix(d, "adm:togglepay:") {
			found = true
		}
	}
	if !found {
		t.Errorf("re-rendered menu missing togglepay buttons: %v", data)
	}
}

func TestNonPaymentToggleSendsTerminalMessage(t *testing.T) {
	ctx := context.Background()
	b, f, _ := newSettingsTestBot(t, "1")
	b.Process(ctx, callbackUpdate(1, "adm:togglebcast"))
	last := lastSendMsg(f)
	if last == nil {
		t.Fatal("no message sent")
	}
	if last.Text == "🔑 💳 PAYMENT METHODS" {
		t.Error("a non-payments toggle must not re-render the payments menu")
	}
	data := buttonData(last.ReplyMarkup)
	if len(data) != 1 || data[0] != "admcat:settings" {
		t.Errorf("non-payments toggle must send a terminal message with a lone Back button, got %v", data)
	}
}

// TestTogglePayRefusesLastDepositRail proves the deposit-fundability guard:
// with bep20 the only WORKING deposit rail (enabled AND configured; tron
// off, balance irrelevant), disabling bep20 must be refused — otherwise no
// customer can put new money into the shop at all (Fix 2, teammate review of
// 1c83611). Mutating cbTogglePay's guard back to
// slices.Contains(payMethodNames, ...) — the pre-fix "any of 3 methods"
// check — makes this fail, because "balance" alone used to satisfy it.
func TestTogglePayRefusesLastDepositRail(t *testing.T) {
	ctx := context.Background()
	t.Setenv("BEP20_ADDRESS", testSellerBSC) // bep20 must be Configured() too, or the guard never engages
	t.Setenv("TRON_ADDRESS", "")
	b, f, cfg := newSettingsTestBot(t, "1")
	s := cfg.Settings()
	s["pay_balance"] = true
	s["pay_bep20"] = true
	s["pay_tron"] = false
	if err := cfg.SaveSettings(s); err != nil {
		t.Fatal(err)
	}

	b.Process(ctx, callbackUpdate(1, "adm:togglepay:bep20"))
	want := "⚠️ Can't disable the last deposit method — at least one must stay on so customers can fund their wallet."
	if last := lastSendMsg(f); last == nil || last.Text != want {
		t.Fatalf("reply = %+v, want %q", last, want)
	}
	s2 := cfg.Settings()
	if v, _ := s2["pay_bep20"].(bool); !v {
		t.Error("the last deposit rail must not actually be disabled")
	}
}

// TestTogglePayRefusesLastConfiguredDepositRail reproduces the seam bug the
// 2nd review round caught: a TRON-only shop (the common single-chain setup)
// — BEP20_ADDRESS never set, so pay_bep20 is absent from settings.json and
// reads ENABLED (Fix 1's new default), but bep20 is NOT Configured() and
// therefore invisible to every customer. TRON is both enabled and
// configured — it is the ONLY rail that actually works. Disabling tron must
// still be refused: the old (1st-round) guard checked payEnabled(bep20)
// alone, saw it "on", and would have waved this through, going dark with a
// success confirmation. Mutating cbTogglePay to drop the railConfigured[m]()
// check from the "other rail" loop reproduces exactly that and makes this
// fail.
func TestTogglePayRefusesLastConfiguredDepositRail(t *testing.T) {
	ctx := context.Background()
	t.Setenv("TRON_ADDRESS", testSellerTRX)
	t.Setenv("BEP20_ADDRESS", "") // bep20 stays enabled-by-default but unconfigured
	b, f, cfg := newSettingsTestBot(t, "1")
	s := cfg.Settings()
	s["pay_tron"] = true
	// pay_bep20 intentionally absent — mirrors a real pre-migration
	// settings.json with no pay_bep20 key at all.
	if err := cfg.SaveSettings(s); err != nil {
		t.Fatal(err)
	}

	b.Process(ctx, callbackUpdate(1, "adm:togglepay:tron"))
	want := "⚠️ Can't disable the last deposit method — at least one must stay on so customers can fund their wallet."
	if last := lastSendMsg(f); last == nil || last.Text != want {
		t.Fatalf("reply = %+v, want %q — an unconfigured bep20 must not count as a working deposit rail", last, want)
	}
	s2 := cfg.Settings()
	if v, _ := s2["pay_tron"].(bool); !v {
		t.Error("the last WORKING deposit rail must not actually be disabled")
	}
}

// TestTogglePayAllowsDisablingUnconfiguredDepositRail proves the deliberate
// edge-case decision documented on cbTogglePay: when the method being
// toggled is itself enabled-but-UNCONFIGURED, the guard does not fire, even
// if it is the only enabled deposit method. The shop is already dark on that
// rail regardless of the setting, so blocking the toggle would only stop an
// admin from cleaning up a half-configured instance — it protects nothing.
func TestTogglePayAllowsDisablingUnconfiguredDepositRail(t *testing.T) {
	ctx := context.Background()
	t.Setenv("BEP20_ADDRESS", "")
	t.Setenv("TRON_ADDRESS", "")
	b, f, cfg := newSettingsTestBot(t, "1")
	s := cfg.Settings()
	s["pay_bep20"] = true // enabled, but unconfigured — not a working rail
	s["pay_tron"] = false
	if err := cfg.SaveSettings(s); err != nil {
		t.Fatal(err)
	}

	b.Process(ctx, callbackUpdate(1, "adm:togglepay:bep20"))
	if last := lastSendMsg(f); last == nil || last.Text == "⚠️ Can't disable the last deposit method — at least one must stay on so customers can fund their wallet." {
		t.Fatalf("disabling an unconfigured deposit rail was blocked, want it to succeed: %+v", last)
	}
	s2 := cfg.Settings()
	if v, _ := s2["pay_bep20"].(bool); v {
		t.Error("pay_bep20 should now be false")
	}
}

// TestTogglePayAllowsDisablingLastBalance proves "balance" is never subject
// to the last-method guard: it spends money already on deposit, it cannot
// fund the shop, so counting it toward "a payment method survives" is
// exactly the bug that let an admin zero out both real deposit rails while
// the guard still reported success (Fix 2). Here bep20/tron are already
// off; disabling the sole remaining "balance" must go through cleanly.
func TestTogglePayAllowsDisablingLastBalance(t *testing.T) {
	ctx := context.Background()
	b, f, cfg := newSettingsTestBot(t, "1")
	s := cfg.Settings()
	s["pay_balance"] = true
	s["pay_bep20"] = false
	s["pay_tron"] = false
	if err := cfg.SaveSettings(s); err != nil {
		t.Fatal(err)
	}

	b.Process(ctx, callbackUpdate(1, "adm:togglepay:balance"))
	if last := lastSendMsg(f); last == nil || last.Text == "⚠️ Can't disable the last deposit method — at least one must stay on so customers can fund their wallet." {
		t.Fatalf("disabling the last balance method was blocked, want it to succeed: %+v", last)
	}
	s2 := cfg.Settings()
	if v, _ := s2["pay_balance"].(bool); v {
		t.Error("pay_balance should now be false")
	}
}

// TestTogglePayStaleMethodAnswers proves a stale adm:togglepay:paypal (or
// :binance) tap — left over from a menu rendered before the USDT migration
// removed those rails — gets an explicit answer instead of silently doing
// nothing after the tapped message was already deleted (Fix 3, same defect
// class as cbDep's dep: fall-through fix in deposit.go).
func TestTogglePayStaleMethodAnswers(t *testing.T) {
	ctx := context.Background()
	b, f, _ := newSettingsTestBot(t, "1")
	b.Process(ctx, callbackUpdate(1, "adm:togglepay:paypal"))
	last := lastSendMsg(f)
	if last == nil {
		t.Fatal("a stale adm:togglepay:paypal tap must not be silently dropped")
	}
	if last.Text != "⚠️ That payment method no longer exists." {
		t.Errorf("reply = %q, want the removed-method notice", last.Text)
	}
}

// TestTogglePaySendsSaveFailedOnWriteFailure is cbTogglePay's counterpart to
// the togglesales/toggleprivate/togglereferral trio above (task #29,
// adminsettings.go:935). "balance" is used deliberately: it is the one
// payMethodNames entry NOT in depositMethodNames, so it never engages
// cbTogglePay's last-working-deposit-rail guard (see the EDGE CASE note on
// cbTogglePay) — the save-failure branch under test is reached directly,
// with no other guard to satisfy first.
func TestTogglePaySendsSaveFailedOnWriteFailure(t *testing.T) {
	ctx := context.Background()
	b, f, _ := newReadOnlyCfgAuditBot(t)
	b.Process(ctx, callbackUpdate(1, "adm:togglepay:balance"))
	if last := lastSendMsg(f); last == nil || last.Text != settingsSaveFailed {
		t.Fatalf("reply on save failure = %+v, want %q", last, settingsSaveFailed)
	}
}

// ---- text wizards ------------------------------------------------------

func TestSetMaxQtyWizard(t *testing.T) {
	ctx := context.Background()
	b, f, cfg := newSettingsTestBot(t, "1")

	b.Process(ctx, callbackUpdate(1, "adm:setmaxqty"))
	if last := lastSendMsg(f); last == nil || !strings.Contains(last.Text, "Current max bulk buy: unlimited") {
		t.Fatalf("prompt = %+v", last)
	}
	b.Process(ctx, textUpdate(1, "abc"))
	if last := lastSendMsg(f); last == nil || last.Text != "⚠️ Enter a whole number (0 = unlimited), e.g. 10." {
		t.Fatalf("invalid reply = %+v", last)
	}
	// pop semantics: must retap the button to retry.
	b.Process(ctx, callbackUpdate(1, "adm:setmaxqty"))
	b.Process(ctx, textUpdate(1, "10"))
	if last := lastSendMsg(f); last == nil || last.Text != "✅ Max bulk buy set to 10 accounts per order." {
		t.Fatalf("valid reply = %+v", last)
	}
	if cfg.MaxBulk() != 10 {
		t.Errorf("MaxBulk() = %d, want 10", cfg.MaxBulk())
	}
}

func TestSetBep20Wizard(t *testing.T) {
	ctx := context.Background()
	b, f, cfg := newSettingsTestBot(t, "1")
	before := cfg.BEP20Address() // M5: prior value, asserted unchanged below

	b.Process(ctx, callbackUpdate(1, "adm:setbep20"))
	b.Process(ctx, textUpdate(1, "not-an-address"))
	if last := lastSendMsg(f); last == nil || !strings.Contains(last.Text, "Not a valid BSC/BEP20 address") {
		t.Fatalf("invalid reply = %+v", last)
	}
	if cfg.BEP20Address() != before {
		t.Errorf("rejected address must not change BEP20Address(), got %q, want unchanged %q", cfg.BEP20Address(), before)
	}
	b.Process(ctx, callbackUpdate(1, "adm:setbep20"))
	addr := "0x1234567890123456789012345678901234567890"
	b.Process(ctx, textUpdate(1, addr))
	if last := lastSendMsg(f); last == nil || !strings.HasPrefix(last.Text, "✅ USDT BEP20 address set:\n"+addr) {
		t.Fatalf("valid reply = %+v", last)
	}
	if cfg.BEP20Address() != addr {
		t.Errorf("BEP20Address() = %q, want %q", cfg.BEP20Address(), addr)
	}
}

func TestSetTronWizard(t *testing.T) {
	ctx := context.Background()
	b, f, cfg := newSettingsTestBot(t, "1")
	before := cfg.TRONAddress() // M5: prior value, asserted unchanged below

	b.Process(ctx, callbackUpdate(1, "adm:settron"))
	b.Process(ctx, textUpdate(1, "bad"))
	if last := lastSendMsg(f); last == nil || !strings.Contains(last.Text, "Not a valid TRON/TRC20 address") {
		t.Fatalf("invalid reply = %+v", last)
	}
	if cfg.TRONAddress() != before {
		t.Errorf("rejected address must not change TRONAddress(), got %q, want unchanged %q", cfg.TRONAddress(), before)
	}
	b.Process(ctx, callbackUpdate(1, "adm:settron"))
	addr := "T" + strings.Repeat("1", 33)
	b.Process(ctx, textUpdate(1, addr))
	if last := lastSendMsg(f); last == nil || !strings.HasPrefix(last.Text, "✅ USDT TRC20 (TRON) address set:\n"+addr) {
		t.Fatalf("valid reply = %+v", last)
	}
	if cfg.TRONAddress() != addr {
		t.Errorf("TRONAddress() = %q, want %q", cfg.TRONAddress(), addr)
	}
}

// ---- task #13 Tier 1: payout-address audit logging -------------------------
// .omc/artifacts/config-audit-spec.md — capSetBep20/capSetTron are the two
// sites where an unaudited change silently redirects 100% of incoming funds.

// TestAuditOldNewClipsOversizedOld pins the review-found failure case: old
// is an UNVALIDATED passthrough of BEP20_ADDRESS/TRON_ADDRESS (env, or a
// past admin's raw settings.json value), unlike newVal which always passed
// bep20.ValidAddress/tron.ValidAddress before reaching auditOldNew. Before
// this fix, an oversized old pushed the formatted "old → new" past
// LogAction's silent 500-rune outcome clip and could cut off the arrow and
// the new address entirely — a row with no visible destination on the one
// field where that matters most. This asserts the arrow and the full new
// address always survive, and the whole result stays comfortably inside the
// 500-rune clip regardless of how large old is.
func TestAuditOldNewClipsOversizedOld(t *testing.T) {
	longOld := strings.Repeat("A", 600) // larger than any realistic address/URL
	newVal := "0x1111111111111111111111111111111111111111"

	got := auditOldNew(longOld, newVal)

	if !strings.HasSuffix(got, " → "+newVal) {
		t.Fatalf("clipped result must still end in %q, got %q", " → "+newVal, got)
	}
	wantOldPrefix := strings.Repeat("A", auditOldClipRunes) + "…"
	if !strings.HasPrefix(got, wantOldPrefix) {
		t.Fatalf("old side must clip to %d runes + an ellipsis, got %q", auditOldClipRunes, got)
	}
	if n := len([]rune(got)); n > 500 {
		t.Fatalf("clipped result is %d runes, still over LogAction's 500-rune outcome clip", n)
	}
}

// newAuditTestBot builds a Bot wired to BOTH a real *config.Config (so
// SaveSettings persists) AND a real *store.Store (so LogAction/RecentAudit
// actually round-trip) — newSettingsTestBot only wires the former.
func newAuditTestBot(t *testing.T, adminID string) (*Bot, *fakeTG, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatalf("config load: %v", err)
	}
	st, err := store.Open(filepath.Join(dir, "store.db"))
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	f := &fakeTG{}
	b := New(Deps{TG: f, Store: st, Config: cfg, AdminID: adminID})
	t.Cleanup(b.Shutdown)
	return b, f, st
}

// latestAuditRow returns the newest RecentAudit row with the given action, or
// nil. RecentAudit orders newest-first (store/misc.go), so the first match
// walking the slice is the most recent one. Passing action itself as
// RecentAudit's substring query (rather than "" + a Go-side filter) matters
// for the NoAudit tests below: an unfiltered LIMIT 20 could let a real
// regression row scroll out of the window and read as "absent" — filtering
// server-side removes that blind spot regardless of how many other rows
// exist.
func latestAuditRow(t *testing.T, st *store.Store, action string) *store.AuditRow {
	t.Helper()
	rows, err := st.RecentAudit(20, action)
	if err != nil {
		t.Fatalf("RecentAudit: %v", err)
	}
	for i := range rows {
		if rows[i].Action == action {
			return &rows[i]
		}
	}
	return nil
}

// TestSetBep20WizardAuditsAddressChange proves capSetBep20 writes a row shaped
// per spec §4.1/§4.4: action="config_set_bep20_address", target="bep20_address",
// actor = the admin who made the change, and outcome="<old> → <new>" with the
// never-set case rendered "(unset)" rather than an empty string. The SECOND
// assertion (old value on a follow-up change) is the one that actually
// falsifies a new-value-only regression: a row that only ever recorded the
// new address would still satisfy "a row with this action exists" but would
// fail here, because its "old" side would be the wrong (or missing) address.
func TestSetBep20WizardAuditsAddressChange(t *testing.T) {
	ctx := context.Background()
	b, f, st := newAuditTestBot(t, "1")

	// First-ever set: no prior settings.json/env value, so old side = "(unset)".
	b.Process(ctx, callbackUpdate(1, "adm:setbep20"))
	addr1 := "0x1111111111111111111111111111111111111111"
	b.Process(ctx, textUpdate(1, addr1))
	if last := lastSendMsg(f); last == nil || !strings.HasPrefix(last.Text, "✅ USDT BEP20 address set:\n"+addr1) {
		t.Fatalf("valid reply = %+v", last)
	}
	row1 := latestAuditRow(t, st, "config_set_bep20_address")
	if row1 == nil {
		t.Fatal("no audit row written for first bep20_address change")
	}
	if row1.Target != "bep20_address" {
		t.Errorf("Target = %q, want bep20_address", row1.Target)
	}
	if row1.ActorChat != "1" || row1.ActorKind != "admin" {
		t.Errorf("actor = %q/%q, want 1/admin", row1.ActorChat, row1.ActorKind)
	}
	if want := "(unset) → " + addr1; row1.Outcome != want {
		t.Errorf("Outcome = %q, want %q", row1.Outcome, want)
	}

	// Second change: the outcome's OLD side must be addr1, proving the row
	// actually captured the pre-mutation value rather than re-deriving "old"
	// from thin air (or from the new value, which would make old==new).
	b.Process(ctx, callbackUpdate(1, "adm:setbep20"))
	addr2 := "0x2222222222222222222222222222222222222222"
	b.Process(ctx, textUpdate(1, addr2))
	row2 := latestAuditRow(t, st, "config_set_bep20_address")
	if row2 == nil {
		t.Fatal("no audit row written for second bep20_address change")
	}
	if want := addr1 + " → " + addr2; row2.Outcome != want {
		t.Errorf("second Outcome = %q, want %q", row2.Outcome, want)
	}
}

// TestSetBep20WizardNoAuditOnSaveFailure proves the audit row is emitted only
// AFTER SaveSettings succeeds (spec §4.3): a read-only settings dir forces
// every SaveSettings call to fail, and no "config_set_bep20_address" row may
// appear despite the admin having sent a fully valid address.
func TestSetBep20WizardNoAuditOnSaveFailure(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	roDir := filepath.Join(dir, "ro")
	if err := os.Mkdir(roDir, 0o500); err != nil { // read+execute, no write
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(roDir, 0o700) }) // let TempDir cleanup remove it

	cfg, err := config.Load(roDir)
	if err != nil {
		t.Fatalf("config load: %v", err)
	}
	st, err := store.Open(filepath.Join(dir, "store.db")) // store.db lives OUTSIDE roDir
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	f := &fakeTG{}
	b := New(Deps{TG: f, Store: st, Config: cfg, AdminID: "1"})
	t.Cleanup(b.Shutdown)

	b.Process(ctx, callbackUpdate(1, "adm:setbep20"))
	b.Process(ctx, textUpdate(1, "0x3333333333333333333333333333333333333333"))
	if last := lastSendMsg(f); last == nil || last.Text != settingsSaveFailed {
		t.Fatalf("reply on save failure = %+v, want %q", last, settingsSaveFailed)
	}
	if row := latestAuditRow(t, st, "config_set_bep20_address"); row != nil {
		t.Fatalf("audit row written despite failed SaveSettings: %+v", *row)
	}
}

// TestSetTronWizardAuditsAddressChange is capSetTron's counterpart to
// TestSetBep20WizardAuditsAddressChange above — same row shape, same
// old-value-on-second-change falsification.
func TestSetTronWizardAuditsAddressChange(t *testing.T) {
	ctx := context.Background()
	b, f, st := newAuditTestBot(t, "1")

	addr1 := "T" + strings.Repeat("1", 33)
	b.Process(ctx, callbackUpdate(1, "adm:settron"))
	b.Process(ctx, textUpdate(1, addr1))
	if last := lastSendMsg(f); last == nil || !strings.HasPrefix(last.Text, "✅ USDT TRC20 (TRON) address set:\n"+addr1) {
		t.Fatalf("valid reply = %+v", last)
	}
	row1 := latestAuditRow(t, st, "config_set_tron_address")
	if row1 == nil {
		t.Fatal("no audit row written for first tron_address change")
	}
	if row1.Target != "tron_address" {
		t.Errorf("Target = %q, want tron_address", row1.Target)
	}
	if row1.ActorChat != "1" || row1.ActorKind != "admin" {
		t.Errorf("actor = %q/%q, want 1/admin", row1.ActorChat, row1.ActorKind)
	}
	if want := "(unset) → " + addr1; row1.Outcome != want {
		t.Errorf("Outcome = %q, want %q", row1.Outcome, want)
	}

	addr2 := "T" + strings.Repeat("2", 33)
	b.Process(ctx, callbackUpdate(1, "adm:settron"))
	b.Process(ctx, textUpdate(1, addr2))
	row2 := latestAuditRow(t, st, "config_set_tron_address")
	if row2 == nil {
		t.Fatal("no audit row written for second tron_address change")
	}
	if want := addr1 + " → " + addr2; row2.Outcome != want {
		t.Errorf("second Outcome = %q, want %q", row2.Outcome, want)
	}
}

// TestSetTronWizardNoAuditOnSaveFailure is capSetTron's counterpart to
// TestSetBep20WizardNoAuditOnSaveFailure above.
func TestSetTronWizardNoAuditOnSaveFailure(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	roDir := filepath.Join(dir, "ro")
	if err := os.Mkdir(roDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(roDir, 0o700) })

	cfg, err := config.Load(roDir)
	if err != nil {
		t.Fatalf("config load: %v", err)
	}
	st, err := store.Open(filepath.Join(dir, "store.db"))
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	f := &fakeTG{}
	b := New(Deps{TG: f, Store: st, Config: cfg, AdminID: "1"})
	t.Cleanup(b.Shutdown)

	b.Process(ctx, callbackUpdate(1, "adm:settron"))
	b.Process(ctx, textUpdate(1, "T"+strings.Repeat("9", 33)))
	if last := lastSendMsg(f); last == nil || last.Text != settingsSaveFailed {
		t.Fatalf("reply on save failure = %+v, want %q", last, settingsSaveFailed)
	}
	if row := latestAuditRow(t, st, "config_set_tron_address"); row != nil {
		t.Fatalf("audit row written despite failed SaveSettings: %+v", *row)
	}
}

// TestSetBep20WizardAuditOldValueUsesEnvFallback pins the one deliberate
// deviation from spec §4.4's literal instruction ("read old from the s map
// already in hand"): capSetBep20 captures oldAddr via cfg.BEP20Address(),
// which layers settings.json over an env fallback, not a bare settings-map
// read. Without an ambient BEP20_ADDRESS the two approaches are
// indistinguishable — every other test in this file would pass unchanged
// under either implementation. This test sets the env var BEFORE
// config.Load (Config.Env is a Load-time-frozen snapshot, config.go:46-52,
// so setting it after Load would have no effect) so the two approaches
// diverge: a bare map read would still render "(unset)" here, which would be
// a wrong "old" value for a shop whose address was only ever supplied via
// env.
func TestSetBep20WizardAuditOldValueUsesEnvFallback(t *testing.T) {
	ctx := context.Background()
	envAddr := "0x9999999999999999999999999999999999999999"
	t.Setenv("BEP20_ADDRESS", envAddr)
	b, f, st := newAuditTestBot(t, "1")

	b.Process(ctx, callbackUpdate(1, "adm:setbep20"))
	newAddr := "0x1111111111111111111111111111111111111111"
	b.Process(ctx, textUpdate(1, newAddr))
	if last := lastSendMsg(f); last == nil || !strings.HasPrefix(last.Text, "✅ USDT BEP20 address set:\n"+newAddr) {
		t.Fatalf("valid reply = %+v", last)
	}
	row := latestAuditRow(t, st, "config_set_bep20_address")
	if row == nil {
		t.Fatal("no audit row written")
	}
	if want := envAddr + " → " + newAddr; row.Outcome != want {
		t.Errorf(`Outcome = %q, want %q (old side must be the env-configured address, not "(unset)")`, row.Outcome, want)
	}
}

// TestSetTronWizardAuditOldValueUsesEnvFallback is capSetTron's counterpart
// to TestSetBep20WizardAuditOldValueUsesEnvFallback above.
func TestSetTronWizardAuditOldValueUsesEnvFallback(t *testing.T) {
	ctx := context.Background()
	envAddr := "T" + strings.Repeat("9", 33)
	t.Setenv("TRON_ADDRESS", envAddr)
	b, f, st := newAuditTestBot(t, "1")

	b.Process(ctx, callbackUpdate(1, "adm:settron"))
	newAddr := "T" + strings.Repeat("1", 33)
	b.Process(ctx, textUpdate(1, newAddr))
	if last := lastSendMsg(f); last == nil || !strings.HasPrefix(last.Text, "✅ USDT TRC20 (TRON) address set:\n"+newAddr) {
		t.Fatalf("valid reply = %+v", last)
	}
	row := latestAuditRow(t, st, "config_set_tron_address")
	if row == nil {
		t.Fatal("no audit row written")
	}
	if want := envAddr + " → " + newAddr; row.Outcome != want {
		t.Errorf(`Outcome = %q, want %q (old side must be the env-configured address, not "(unset)")`, row.Outcome, want)
	}
}

// ---- Task #13 Tier 2-5: toggle audit pinning -------------------------------

// TestToggleSalesAuditsOneRowNamingBothTransitions pins the sharpest gap an
// independent review found in this file: cbToggleSales's own comment says a
// two-row emit "would misrepresent one admin tap as two independent
// actions", but nothing enforced that ONE tap actually produces one row.
// This drives the handler through a scenario where BOTH keys genuinely
// change value — private_sale force-pauses sales_paused as a side effect,
// then unpausing force-clears private_sale back — and asserts the audit
// trail grows by exactly one row for the tap under test, and that row names
// both transitions.
//
// The row-count delta (not a filtered latestAuditRow lookup) is the
// assertion that actually catches a two-row regression regardless of what
// action string the second row would use.
//
// Falsification: reverting cbToggleSales to write two LogAction calls (one
// per key) instead of one still passes every other test in this file, but
// fails HERE — the row-count delta becomes 2, not 1. Verified by mutation
// (temporarily splitting the emit, confirming this test fails, then
// restoring the single call).
func TestToggleSalesAuditsOneRowNamingBothTransitions(t *testing.T) {
	ctx := context.Background()
	b, _, st := newAuditTestBot(t, "1")

	// Arm private_sale ON first: its own side effect force-pauses
	// sales_paused (private_sale: false→true; sales_paused: false→true), so
	// both keys start the tap under test already TRUE.
	b.Process(ctx, callbackUpdate(1, "adm:toggleprivate"))
	before, err := st.RecentAudit(20, "")
	if err != nil {
		t.Fatalf("RecentAudit (before): %v", err)
	}

	// The tap under test: unpausing sales_paused ALSO clears private_sale as
	// a side effect (sales_paused: true→false; private_sale: true→false) —
	// two real transitions from one admin tap.
	b.Process(ctx, callbackUpdate(1, "adm:togglesales"))
	after, err := st.RecentAudit(20, "")
	if err != nil {
		t.Fatalf("RecentAudit (after): %v", err)
	}

	if got, want := len(after)-len(before), 1; got != want {
		t.Fatalf("audit rows added by one adm:togglesales tap = %d, want %d (rows: %+v)",
			got, want, after)
	}
	row := after[0] // RecentAudit orders newest-first
	if row.Action != "config_toggle_sales_paused" {
		t.Errorf("Action = %q, want config_toggle_sales_paused", row.Action)
	}
	wantOutcome := "sales_paused: true → false; private_sale: true → false"
	if row.Outcome != wantOutcome {
		t.Errorf("Outcome = %q, want %q (must name BOTH key transitions in the ONE row)",
			row.Outcome, wantOutcome)
	}
}

// TestTogglePrivateAuditsOneRowNamingBothTransitions is cbTogglePrivate's
// counterpart to TestToggleSalesAuditsOneRowNamingBothTransitions above —
// same one-row invariant, same row-count-delta assertion, the OTHER two-key
// site in this file.
//
// Falsification: same shape as the sales test — splitting cbTogglePrivate's
// single LogAction call into two makes the row-count delta 2, not 1.
// Verified by mutation (temporarily splitting the emit, confirming this test
// fails, then restoring the single call).
func TestTogglePrivateAuditsOneRowNamingBothTransitions(t *testing.T) {
	ctx := context.Background()
	b, _, st := newAuditTestBot(t, "1")

	before, err := st.RecentAudit(20, "")
	if err != nil {
		t.Fatalf("RecentAudit (before): %v", err)
	}

	// Fresh store: sales_paused and private_sale both start false. Enabling
	// private_sale force-pauses sales_paused as a side effect — two real
	// transitions from one admin tap.
	b.Process(ctx, callbackUpdate(1, "adm:toggleprivate"))
	after, err := st.RecentAudit(20, "")
	if err != nil {
		t.Fatalf("RecentAudit (after): %v", err)
	}

	if got, want := len(after)-len(before), 1; got != want {
		t.Fatalf("audit rows added by one adm:toggleprivate tap = %d, want %d (rows: %+v)",
			got, want, after)
	}
	row := after[0]
	if row.Action != "config_toggle_private_sale" {
		t.Errorf("Action = %q, want config_toggle_private_sale", row.Action)
	}
	wantOutcome := "private_sale: false → true; sales_paused: false → true"
	if row.Outcome != wantOutcome {
		t.Errorf("Outcome = %q, want %q (must name BOTH key transitions in the ONE row)",
			row.Outcome, wantOutcome)
	}
}

// TestToggleDepNotifyAuditsViaToggleSetting pins Task #13 Tier 5:
// toggleSetting is the SINGLE instrumentation point cbToggleSimple's four
// bell toggles and cbToggleMaint all reduce to (adminsettings.go's own doc
// comment on toggleSetting). admin_deposit_notify is the toggle picked here
// over the other three bell toggles because it is the one tied directly to
// money-movement visibility — a super-admin silently losing deposit alerts
// is a real-money blind spot, not just a cosmetic notification miss — and it
// also exercises the shared cbToggleSimple wrapper that 4 of the 5
// toggleSetting callers route through (cbToggleMaint is the one bespoke
// caller). The row shape asserted below is generic across all five callers,
// since every one of them shares this exact code path with only
// (key, default, prefix) varying.
//
// admin_deposit_notify defaults to true (unset reads as ON), so a fresh
// config's first tap flips it OFF — old=true, new=false.
//
// Falsification: reverting toggleSetting to skip its auditConfigSet call
// fails this test with "no audit row written" — verified by mutation
// (temporarily commenting out the auditConfigSet call inside toggleSetting,
// confirming this test fails, then restoring it).
func TestToggleDepNotifyAuditsViaToggleSetting(t *testing.T) {
	ctx := context.Background()
	b, f, st := newAuditTestBot(t, "1")

	b.Process(ctx, callbackUpdate(1, "adm:toggledepnotify"))
	if last := lastSendMsg(f); last == nil || !strings.Contains(last.Text, "New-deposit admin alerts are now OFF") {
		t.Fatalf("toggle reply = %+v", last)
	}

	row := latestAuditRow(t, st, "config_toggle_admin_deposit_notify")
	if row == nil {
		t.Fatal("no audit row written for admin_deposit_notify toggle")
	}
	if row.Target != "admin_deposit_notify" {
		t.Errorf("Target = %q, want admin_deposit_notify", row.Target)
	}
	if row.ActorChat != "1" || row.ActorKind != "admin" {
		t.Errorf("actor = %q/%q, want 1/admin", row.ActorChat, row.ActorKind)
	}
	if want := "true → false"; row.Outcome != want {
		t.Errorf("Outcome = %q, want %q", row.Outcome, want)
	}
}

// TestToggleMaintAuditsViaToggleSetting pins the THIRD of toggleSetting's
// three call PATHS — not the same thing as "every toggle", which is why this
// test exists separately from TestToggleDepNotifyAuditsViaToggleSetting
// above. cbToggleMaint calls toggleSetting directly; it does NOT go through
// the shared cbToggleSimple wrapper the four bell toggles share. Pinning
// dep-notify alone proves the cbToggleSimple path is wired; it says nothing
// about cbToggleMaint's own direct call, which is a separate call site that
// could regress independently (e.g. a future edit that inlines
// Settings()/SaveSettings directly into cbToggleMaint for a maintenance-mode
// special case, bypassing toggleSetting and its audit call, while every
// other test in this file — none of which touch maintenance — stays green).
//
// Deliberately NOT adding this for togglebcast/togglestockbcast/
// togglesalenotify: those three are the literal same cbToggleSimple call
// with a different key string already covered by dep-notify's pin of that
// shared path — testing them too would exercise the payMethodNames-style map
// literal, not new code. One test per PATH, not per toggle.
//
// Falsification: rewriting cbToggleMaint to save the setting directly
// instead of calling b.toggleSetting fails this test with "no audit row
// written for maintenance toggle" — verified by mutation (temporarily
// inlining the save and dropping the toggleSetting/auditConfigSet call,
// confirming this test fails, then restoring it).
func TestToggleMaintAuditsViaToggleSetting(t *testing.T) {
	ctx := context.Background()
	b, f, st := newAuditTestBot(t, "1")

	b.Process(ctx, callbackUpdate(1, "adm:togglemaint"))
	if last := lastSendMsg(f); last == nil || !strings.Contains(last.Text, "Maintenance mode is now ON") {
		t.Fatalf("toggle reply = %+v", last)
	}

	row := latestAuditRow(t, st, "config_toggle_maintenance")
	if row == nil {
		t.Fatal("no audit row written for maintenance toggle")
	}
	if row.Target != "maintenance" {
		t.Errorf("Target = %q, want maintenance", row.Target)
	}
	if row.ActorChat != "1" || row.ActorKind != "admin" {
		t.Errorf("actor = %q/%q, want 1/admin", row.ActorChat, row.ActorKind)
	}
	if want := "false → true"; row.Outcome != want {
		t.Errorf("Outcome = %q, want %q", row.Outcome, want)
	}
}

func TestSetKiroURLWizard(t *testing.T) {
	ctx := context.Background()
	b, f, cfg := newSettingsTestBot(t, "1")
	before := cfg.KiroGoBaseURL() // M5: prior value, asserted unchanged below

	b.Process(ctx, callbackUpdate(1, "adm:setkirourl"))
	b.Process(ctx, textUpdate(1, "not a url"))
	if last := lastSendMsg(f); last == nil || !strings.Contains(last.Text, "Not a valid URL") {
		t.Fatalf("invalid reply = %+v", last)
	}
	if cfg.KiroGoBaseURL() != before {
		t.Errorf("rejected URL must not change KiroGoBaseURL(), got %q, want unchanged %q", cfg.KiroGoBaseURL(), before)
	}
	b.Process(ctx, callbackUpdate(1, "adm:setkirourl"))
	b.Process(ctx, textUpdate(1, "http://localhost:8080/"))
	last := lastSendMsg(f)
	if last == nil || !strings.HasPrefix(last.Text, "✅ Kiro-Go server URL set to http://localhost:8080\n") {
		t.Fatalf("valid reply = %+v", last)
	}
	if !strings.Contains(last.Text, "admin key not set yet") {
		t.Errorf("probe text should report admin key not set, got %q", last.Text)
	}
	if cfg.KiroGoBaseURL() != "http://localhost:8080" {
		t.Errorf("KiroGoBaseURL() = %q", cfg.KiroGoBaseURL())
	}
}

// TestSetKiroKeyWizardNeverEchoesKey also exercises the empty-key rejection
// (via a whitespace-only message that trims to "" — Process trims Text
// before the capture chain sees it, so a truly-empty msg.Text can't reach
// here at all: Process's step 6 gate returns early on that).
func TestSetKiroKeyWizardNeverEchoesKey(t *testing.T) {
	ctx := context.Background()
	b, f, cfg := newSettingsTestBot(t, "1")

	b.Process(ctx, callbackUpdate(1, "adm:setkirokey"))
	b.Process(ctx, textUpdate(1, " "))
	if last := lastSendMsg(f); last == nil || !strings.Contains(last.Text, "Empty key ignored") {
		t.Fatalf("empty reply = %+v", last)
	}

	b.Process(ctx, callbackUpdate(1, "adm:setkirokey"))
	secret := "super-secret-kirogo-password"
	b.Process(ctx, textUpdate(1, secret))
	last := lastSendMsg(f)
	if last == nil || !strings.HasPrefix(last.Text, "✅ Kiro-Go admin password saved.") {
		t.Fatalf("valid reply = %+v", last)
	}
	if strings.Contains(last.Text, secret) {
		t.Fatal("the admin key must NEVER be echoed back in the success message")
	}
	for _, s := range f.Sends {
		if strings.Contains(s.Text, secret) {
			t.Fatal("the admin key must NEVER appear in any outbound message")
		}
	}
	if cfg.KiroGoAdminKey() != secret {
		t.Errorf("KiroGoAdminKey() = %q, want %q", cfg.KiroGoAdminKey(), secret)
	}
}

func TestSetKiroAcctWizard(t *testing.T) {
	ctx := context.Background()
	b, f, cfg := newSettingsTestBot(t, "1")
	before := cfg.KiroGoAccountID() // M5: prior value, asserted unchanged below

	b.Process(ctx, callbackUpdate(1, "adm:setkiroacct"))
	b.Process(ctx, textUpdate(1, "not-a-uuid"))
	if last := lastSendMsg(f); last == nil || !strings.Contains(last.Text, "Not a valid account id") {
		t.Fatalf("invalid reply = %+v", last)
	}
	if cfg.KiroGoAccountID() != before {
		t.Errorf("rejected account id must not change KiroGoAccountID(), got %q, want unchanged %q", cfg.KiroGoAccountID(), before)
	}
	b.Process(ctx, callbackUpdate(1, "adm:setkiroacct"))
	uuid := "22bb9057-183d-4ff6-89c4-7f230cb98b8e"
	b.Process(ctx, textUpdate(1, uuid))
	last := lastSendMsg(f)
	if last == nil || !strings.HasPrefix(last.Text, "✅ Kiro-Go account id set to "+uuid) {
		t.Fatalf("valid reply = %+v", last)
	}
	if cfg.KiroGoAccountID() != uuid {
		t.Errorf("KiroGoAccountID() = %q, want %q", cfg.KiroGoAccountID(), uuid)
	}
}

// ---- Task #13 Tier 2: Kiro-Go credential/redirection audit logging --------
// .omc/artifacts/config-audit-spec.md §2-3 — kiro_go_base_url ranks ABOVE the
// admin key (repointing the URL steals the key on the next call), the key
// itself is a SECRET audited as set/unset only, kiro_go_account_id is a
// plain identifier audited in full.

// TestRedactKiroURLPreservesHostAndFullyRedactsEmbeddedAt pins the two
// defects an independent review found in kiroURLUserinfoRE's first version
// (non-greedy and unbounded: `://[^\s"']*?@`), both reachable through inputs
// kiroURLRE itself already accepts (it excludes only whitespace and "/"):
//   - a query-string "@" (e.g. "?token=a@b") must NOT eat the host — the
//     non-greedy/unbounded version stopped at the FIRST "@" it saw, which
//     is the query's, consuming the host along with it. The host is the
//     entire forensic point of auditing this field (see kiroURLUserinfoRE's
//     doc comment) — losing it here defeats the reason the row exists.
//   - an admin-pasted UNENCODED "@" inside the userinfo itself (e.g.
//     "u:p@ss@host") must be FULLY redacted, not leave a password fragment
//     ("ss") behind — the old pattern's non-greedy match stopped at the
//     FIRST "@", leaving everything from the second "@" onward untouched.
func TestRedactKiroURLPreservesHostAndFullyRedactsEmbeddedAt(t *testing.T) {
	cases := []struct{ in, want string }{
		// No "/" appears before the query's "@", so the slash-bounded class
		// can't reach past the path boundary to find an "@" to redact —
		// unchanged, host fully intact.
		{"https://kiro.example.com/api?token=a@b", "https://kiro.example.com/api?token=a@b"},
		// Greedy + slash-bounded consumes through the LAST "@" in the
		// authority, so both the username and the embedded "@" in the
		// password are swallowed — no "ss" fragment survives.
		{"https://u:p@ss@host/", "https://***@host/"},
	}
	for _, tc := range cases {
		if got := redactKiroURL(tc.in); got != tc.want {
			t.Errorf("redactKiroURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestSetKiroURLWizardAuditsChangeWithRedaction pins the one field in this
// tier that needs structural redaction rather than a verbatim old->new: a
// Kiro-Go base URL can carry `user:pass@` userinfo (kiroURLRE's own character
// class permits it — it excludes only whitespace and "/"), and that userinfo
// must never reach a row the audit viewer renders or /audit can
// substring-search. Both mandatory assertions from spec §6.1 apply to the
// SECOND change (old side), not just the first, since redactKiroURL runs on
// both old and new.
func TestSetKiroURLWizardAuditsChangeWithRedaction(t *testing.T) {
	ctx := context.Background()
	b, f, st := newAuditTestBot(t, "1")

	const secret1 = "s3cr3t-pass-one"
	url1 := "http://admin:" + secret1 + "@host1.example.com:8080"
	b.Process(ctx, callbackUpdate(1, "adm:setkirourl"))
	b.Process(ctx, textUpdate(1, url1))
	if last := lastSendMsg(f); last == nil || !strings.HasPrefix(last.Text, "✅ Kiro-Go server URL set to "+url1) {
		t.Fatalf("valid reply = %+v", last)
	}
	row1 := latestAuditRow(t, st, "config_set_kiro_go_base_url")
	if row1 == nil {
		t.Fatal("no audit row written for first kiro_go_base_url change")
	}
	if row1.Target != "kiro_go_base_url" {
		t.Errorf("Target = %q, want kiro_go_base_url", row1.Target)
	}
	wantOutcome1 := "(unset) → http://***@host1.example.com:8080"
	if row1.Outcome != wantOutcome1 {
		t.Errorf("Outcome = %q, want %q", row1.Outcome, wantOutcome1)
	}
	if !strings.Contains(row1.Outcome, "host1.example.com") {
		t.Error("the host must survive redaction — it's the forensically useful part")
	}

	// Second change: the OLD side must ALSO be redacted (old is the
	// PREVIOUS url, which still carries secret1) while its host survives.
	const secret2 = "s3cr3t-pass-two"
	url2 := "http://user2:" + secret2 + "@host2.example.com"
	b.Process(ctx, callbackUpdate(1, "adm:setkirourl"))
	b.Process(ctx, textUpdate(1, url2))
	row2 := latestAuditRow(t, st, "config_set_kiro_go_base_url")
	if row2 == nil {
		t.Fatal("no audit row written for second kiro_go_base_url change")
	}
	wantOutcome2 := "http://***@host1.example.com:8080 → http://***@host2.example.com"
	if row2.Outcome != wantOutcome2 {
		t.Errorf("Outcome = %q, want %q", row2.Outcome, wantOutcome2)
	}

	// Mandatory assertion 2 (spec §6.1): sweep EVERY field of EVERY row for
	// EITHER secret — a redactor that only cleans the new side but not a
	// later row's old side would otherwise slip through.
	rows, err := st.RecentAudit(20, "")
	if err != nil {
		t.Fatalf("RecentAudit: %v", err)
	}
	for _, r := range rows {
		for _, f := range []string{r.Action, r.Target, r.Outcome, r.Detail, r.ActorChat, r.ActorKind} {
			if strings.Contains(f, secret1) || strings.Contains(f, secret2) {
				t.Fatalf("a Kiro-Go URL secret leaked into audit row field %q", f)
			}
		}
	}
}

// TestSetKiroURLWizardNoAuditOnSaveFailure mirrors
// TestSetBep20WizardNoAuditOnSaveFailure — audit only after SaveSettings
// succeeds (spec §4.3).
func TestSetKiroURLWizardNoAuditOnSaveFailure(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	roDir := filepath.Join(dir, "ro")
	if err := os.Mkdir(roDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(roDir, 0o700) })

	cfg, err := config.Load(roDir)
	if err != nil {
		t.Fatalf("config load: %v", err)
	}
	st, err := store.Open(filepath.Join(dir, "store.db"))
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	f := &fakeTG{}
	b := New(Deps{TG: f, Store: st, Config: cfg, AdminID: "1"})
	t.Cleanup(b.Shutdown)

	b.Process(ctx, callbackUpdate(1, "adm:setkirourl"))
	b.Process(ctx, textUpdate(1, "http://example.com"))
	if last := lastSendMsg(f); last == nil || last.Text != settingsSaveFailed {
		t.Fatalf("reply on save failure = %+v, want %q", last, settingsSaveFailed)
	}
	if row := latestAuditRow(t, st, "config_set_kiro_go_base_url"); row != nil {
		t.Fatalf("audit row written despite failed SaveSettings: %+v", *row)
	}
}

// TestSetKiroKeyWizardAuditsSetNeverLeaksValue proves capSetKiroKey's audit
// row (spec §3.1) records ONLY the set/unset event — never the key, its
// length, or a prefix, in EITHER direction. This is the SECRET-class site the
// spec singles out as the one field this whole tier must get right.
func TestSetKiroKeyWizardAuditsSetNeverLeaksValue(t *testing.T) {
	ctx := context.Background()
	b, f, st := newAuditTestBot(t, "1")

	const secret = "KIRO-ADMIN-VERY-SECRET-9f8e7d6c5b4a3210"
	b.Process(ctx, callbackUpdate(1, "adm:setkirokey"))
	b.Process(ctx, textUpdate(1, secret))
	if last := lastSendMsg(f); last == nil || !strings.HasPrefix(last.Text, "✅ Kiro-Go admin password saved.") {
		t.Fatalf("valid reply = %+v", last)
	}

	row := latestAuditRow(t, st, "config_set_kiro_go_admin_key")
	if row == nil {
		t.Fatal("no audit row written for kiro_go_admin_key set")
	}
	if row.Target != "kiro_go_admin_key" {
		t.Errorf("Target = %q, want kiro_go_admin_key", row.Target)
	}
	if row.Outcome != "set" {
		t.Errorf("Outcome = %q, want %q (never the key, its length, or a prefix)", row.Outcome, "set")
	}
	// Mandatory assertion 2 (spec §6.1): the secret must appear in NO field,
	// including Detail — an oracle via /audit is worse than rendering it.
	for _, r := range []struct{ name, val string }{
		{"Action", row.Action}, {"Target", row.Target}, {"Outcome", row.Outcome},
		{"Detail", row.Detail}, {"ActorChat", row.ActorChat}, {"ActorKind", row.ActorKind},
	} {
		if strings.Contains(r.val, secret) {
			t.Fatalf("kiro_go_admin_key leaked into audit row field %s: %q", r.name, r.val)
		}
	}
}

// TestSetKiroKeyWizardNoAuditOnSaveFailure mirrors the bep20/tron
// no-audit-on-failure tests for the admin-key wizard.
func TestSetKiroKeyWizardNoAuditOnSaveFailure(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	roDir := filepath.Join(dir, "ro")
	if err := os.Mkdir(roDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(roDir, 0o700) })

	cfg, err := config.Load(roDir)
	if err != nil {
		t.Fatalf("config load: %v", err)
	}
	st, err := store.Open(filepath.Join(dir, "store.db"))
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	f := &fakeTG{}
	b := New(Deps{TG: f, Store: st, Config: cfg, AdminID: "1"})
	t.Cleanup(b.Shutdown)

	b.Process(ctx, callbackUpdate(1, "adm:setkirokey"))
	b.Process(ctx, textUpdate(1, "some-admin-key"))
	if last := lastSendMsg(f); last == nil || last.Text != settingsSaveFailed {
		t.Fatalf("reply on save failure = %+v, want %q", last, settingsSaveFailed)
	}
	if row := latestAuditRow(t, st, "config_set_kiro_go_admin_key"); row != nil {
		t.Fatalf("audit row written despite failed SaveSettings: %+v", *row)
	}
}

// TestSetKiroAcctWizardAuditsChange proves kiro_go_account_id is audited in
// full (spec §3: an identifier, not a credential) — same old-value-on-
// second-change falsification as the bep20/tron tests above.
func TestSetKiroAcctWizardAuditsChange(t *testing.T) {
	ctx := context.Background()
	b, f, st := newAuditTestBot(t, "1")

	uuid1 := "11111111-1111-4111-8111-111111111111"
	b.Process(ctx, callbackUpdate(1, "adm:setkiroacct"))
	b.Process(ctx, textUpdate(1, uuid1))
	if last := lastSendMsg(f); last == nil || !strings.HasPrefix(last.Text, "✅ Kiro-Go account id set to "+uuid1) {
		t.Fatalf("valid reply = %+v", last)
	}
	row1 := latestAuditRow(t, st, "config_set_kiro_go_account_id")
	if row1 == nil {
		t.Fatal("no audit row written for first kiro_go_account_id change")
	}
	if want := "(unset) → " + uuid1; row1.Outcome != want {
		t.Errorf("Outcome = %q, want %q", row1.Outcome, want)
	}

	uuid2 := "22222222-2222-4222-8222-222222222222"
	b.Process(ctx, callbackUpdate(1, "adm:setkiroacct"))
	b.Process(ctx, textUpdate(1, uuid2))
	row2 := latestAuditRow(t, st, "config_set_kiro_go_account_id")
	if row2 == nil {
		t.Fatal("no audit row written for second kiro_go_account_id change")
	}
	if want := uuid1 + " → " + uuid2; row2.Outcome != want {
		t.Errorf("second Outcome = %q, want %q", row2.Outcome, want)
	}
}

// TestSetKiroAcctWizardNoAuditOnSaveFailure mirrors the bep20/tron
// no-audit-on-failure tests for the account-id wizard.
func TestSetKiroAcctWizardNoAuditOnSaveFailure(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	roDir := filepath.Join(dir, "ro")
	if err := os.Mkdir(roDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(roDir, 0o700) })

	cfg, err := config.Load(roDir)
	if err != nil {
		t.Fatalf("config load: %v", err)
	}
	st, err := store.Open(filepath.Join(dir, "store.db"))
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	f := &fakeTG{}
	b := New(Deps{TG: f, Store: st, Config: cfg, AdminID: "1"})
	t.Cleanup(b.Shutdown)

	b.Process(ctx, callbackUpdate(1, "adm:setkiroacct"))
	b.Process(ctx, textUpdate(1, "33333333-3333-4333-8333-333333333333"))
	if last := lastSendMsg(f); last == nil || last.Text != settingsSaveFailed {
		t.Fatalf("reply on save failure = %+v, want %q", last, settingsSaveFailed)
	}
	if row := latestAuditRow(t, st, "config_set_kiro_go_account_id"); row != nil {
		t.Fatalf("audit row written despite failed SaveSettings: %+v", *row)
	}
}

// ---- Task #13 Tier 4: adm:setmaxqty audit logging --------------------------

// TestSetMaxQtyWizardAuditsChange proves max_qty is audited with its literal
// numeric value, including "0" — 0 means unlimited (a REAL configured value
// here, per cbSetMaxQty's own "unlimited" label), never auditOldNew's
// "(unset)" fallback (which only fires on an EMPTY old string).
func TestSetMaxQtyWizardAuditsChange(t *testing.T) {
	ctx := context.Background()
	b, f, st := newAuditTestBot(t, "1")

	b.Process(ctx, callbackUpdate(1, "adm:setmaxqty"))
	b.Process(ctx, textUpdate(1, "10"))
	if last := lastSendMsg(f); last == nil || !strings.Contains(last.Text, "10 accounts per order") {
		t.Fatalf("valid reply = %+v", last)
	}
	row1 := latestAuditRow(t, st, "config_set_max_qty")
	if row1 == nil {
		t.Fatal("no audit row written for first max_qty change")
	}
	if want := "0 → 10"; row1.Outcome != want {
		t.Errorf("Outcome = %q, want %q (fresh config starts at 0/unlimited, not \"(unset)\")", row1.Outcome, want)
	}

	// Second change, back to 0 (unlimited): old side must be "10", proving
	// the row captured the real prior value rather than re-deriving it.
	b.Process(ctx, callbackUpdate(1, "adm:setmaxqty"))
	b.Process(ctx, textUpdate(1, "0"))
	row2 := latestAuditRow(t, st, "config_set_max_qty")
	if row2 == nil {
		t.Fatal("no audit row written for second max_qty change")
	}
	if want := "10 → 0"; row2.Outcome != want {
		t.Errorf("second Outcome = %q, want %q", row2.Outcome, want)
	}
}

// TestSetMaxQtyWizardNoAuditOnSaveFailure mirrors the bep20/tron
// no-audit-on-failure tests for the max-qty wizard.
func TestSetMaxQtyWizardNoAuditOnSaveFailure(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	roDir := filepath.Join(dir, "ro")
	if err := os.Mkdir(roDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(roDir, 0o700) })

	cfg, err := config.Load(roDir)
	if err != nil {
		t.Fatalf("config load: %v", err)
	}
	st, err := store.Open(filepath.Join(dir, "store.db"))
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	f := &fakeTG{}
	b := New(Deps{TG: f, Store: st, Config: cfg, AdminID: "1"})
	t.Cleanup(b.Shutdown)

	b.Process(ctx, callbackUpdate(1, "adm:setmaxqty"))
	b.Process(ctx, textUpdate(1, "10"))
	if last := lastSendMsg(f); last == nil || last.Text != settingsSaveFailed {
		t.Fatalf("reply on save failure = %+v, want %q", last, settingsSaveFailed)
	}
	if row := latestAuditRow(t, st, "config_set_max_qty"); row != nil {
		t.Fatalf("audit row written despite failed SaveSettings: %+v", *row)
	}
}

// ---- broadcast --------------------------------------------------------------

// fakeBroadcastTG lets the broadcast fan-out test selectively fail specific
// recipient chat ids — fake_test.go's shared fakeTG.sendErr fails EVERY
// SendMessage call uniformly, which can't express "2 of 4 recipients
// bounced". Embedding keeps every other fakeTG behavior (recording, etc.)
// intact; only SendMessage is overridden.
type fakeBroadcastTG struct {
	*fakeTG
	failChats map[int64]bool
}

func (f *fakeBroadcastTG) SendMessage(ctx context.Context, p tg.SendMessageParams) (*tg.Message, error) {
	if f.failChats[p.ChatID] {
		return nil, errors.New("boom")
	}
	return f.fakeTG.SendMessage(ctx, p)
}

// TestBroadcastFanOut drives the full adm:broadcast -> text -> admdo:confirm
// flow and asserts the sent/failed counts in the final summary, with 2 of 4
// seeded recipients forced to fail.
func TestBroadcastFanOut(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "store.db"))
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	chatIDs := []string{"101", "102", "103", "104"}
	for i, cid := range chatIDs {
		if _, _, err := st.Credit(cid, 1000, "TXN-"+strconv.Itoa(i)); err != nil {
			t.Fatalf("seed credit: %v", err)
		}
	}

	base := &fakeTG{}
	failing := &fakeBroadcastTG{fakeTG: base, failChats: map[int64]bool{102: true, 104: true}}
	b := New(Deps{TG: failing, Store: st, AdminID: "1"})
	defer b.Shutdown()

	b.Process(ctx, callbackUpdate(1, "adm:broadcast"))
	b.Process(ctx, textUpdate(1, "hello everyone"))
	if last := lastSendMsg(base); last == nil || !strings.Contains(last.Text, "🔧 Broadcast — confirm:") {
		t.Fatalf("confirm summary = %+v", last)
	}
	b.Process(ctx, callbackUpdate(1, "admdo:confirm"))

	// The fan-out runs on a tracked worker goroutine; Shutdown blocks until it
	// (and its final summary send) completes — no sleep/poll needed.
	b.Shutdown()

	last := lastSendMsg(base)
	want := "📣 Broadcast sent to 2 chats (2 failed)."
	if last == nil || last.Text != want {
		t.Fatalf("final summary = %+v, want %q", last, want)
	}

	// L6 fix: assert the actual per-recipient sends, not just the summary
	// counts above — an empty-body bug (e.g. broadcasting "" instead of the
	// armed message) would still satisfy a count-only assertion.
	gotText := map[int64]string{}
	for _, s := range base.Sends {
		if s.ChatID == 101 || s.ChatID == 103 {
			gotText[s.ChatID] = s.Text
		}
	}
	for _, cid := range []int64{101, 103} {
		if gotText[cid] != "hello everyone" {
			t.Errorf("recipient %d: got text %q, want %q", cid, gotText[cid], "hello everyone")
		}
	}

	// Task #14: the fan-out must leave exactly one audit row behind — before
	// this fix it left none at all. This is an ABSOLUTE row count (this
	// test's store is fresh, so 1 total row after == 1 row added), not a
	// before/after delta — asserting the count at all (not just "a row with
	// the right Action exists") is still deliberate: a future edit that
	// renamed/split this into two LogAction calls would still satisfy a
	// filtered lookup while silently doubling the trail. Fragile in one way:
	// if any seeding helper this test starts sharing ever writes its own
	// audit row, this and the other two broadcast row-count assertions break
	// for a reason unrelated to broadcasting at all.
	rows, err := st.RecentAudit(10, "")
	if err != nil {
		t.Fatalf("RecentAudit: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("audit rows after broadcast = %d, want exactly 1: %+v", len(rows), rows)
	}
	row := rows[0]
	if row.Action != "broadcast_send" {
		t.Errorf("Action = %q, want broadcast_send", row.Action)
	}
	if row.ActorChat != "1" || row.ActorKind != "admin" {
		t.Errorf("actor = %q/%q, want 1/admin", row.ActorChat, row.ActorKind)
	}
	if row.Target != "all_chats" {
		t.Errorf("Target = %q, want all_chats", row.Target)
	}
	if !strings.Contains(row.Outcome, "sent=2 fail=2") {
		t.Errorf("Outcome = %q, want it to contain the sent/fail counts matching the fan-out's own return values", row.Outcome)
	}
	if !strings.Contains(row.Outcome, "hello everyone") {
		t.Errorf("Outcome = %q, want it to contain the message excerpt", row.Outcome)
	}
	if row.Detail != "hello everyone" {
		t.Errorf("Detail = %q, want the full message body", row.Detail)
	}
}

// TestBroadcastPhotoFanOut is the L6 fix's photo-path counterpart to
// TestBroadcastFanOut above — execBroadcast's photo branch (runBroadcast,
// SendPhotoURLOpts) had no fan-out test at all before this. Asserts every
// seeded recipient actually received the photo's file_id + caption.
func TestBroadcastPhotoFanOut(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "store.db"))
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	chatIDs := []string{"201", "202", "203"}
	for i, cid := range chatIDs {
		if _, _, err := st.Credit(cid, 1000, "PTXN-"+strconv.Itoa(i)); err != nil {
			t.Fatalf("seed credit: %v", err)
		}
	}

	f := &fakeTG{}
	b := New(Deps{TG: f, Store: st, AdminID: "1"})
	defer b.Shutdown()

	b.Process(ctx, callbackUpdate(1, "adm:broadcast"))
	b.Process(ctx, photoUpdate(1, "file999", "sale ends soon"))
	if last := lastSendMsg(f); last == nil || !strings.Contains(last.Text, "🔧 Broadcast — confirm:") {
		t.Fatalf("confirm summary = %+v", last)
	}
	b.Process(ctx, callbackUpdate(1, "admdo:confirm"))

	// The fan-out runs on a tracked worker goroutine; Shutdown blocks until it
	// (and its final summary send) completes — no sleep/poll needed.
	b.Shutdown()

	last := lastSendMsg(f)
	want := "📣 Broadcast sent to 3 chats (0 failed)."
	if last == nil || last.Text != want {
		t.Fatalf("final summary = %+v, want %q", last, want)
	}

	gotPhoto := map[int64]photoSend{}
	for _, p := range f.Photos {
		gotPhoto[p.ChatID] = p
	}
	for _, cidStr := range chatIDs {
		cid, _ := strconv.ParseInt(cidStr, 10, 64)
		p, ok := gotPhoto[cid]
		if !ok {
			t.Errorf("recipient %d: no photo sent", cid)
			continue
		}
		if p.PhotoURL != "file999" {
			t.Errorf("recipient %d: PhotoURL (file_id) = %q, want %q", cid, p.PhotoURL, "file999")
		}
		if p.Caption != "sale ends soon" {
			t.Errorf("recipient %d: Caption = %q, want %q", cid, p.Caption, "sale ends soon")
		}
	}

	// Task #14, photo path: same one-row assertion as TestBroadcastFanOut,
	// plus the "photo" marker so a caption-less photo broadcast (no message
	// excerpt at all) still leaves a distinguishable outcome.
	rows, err := st.RecentAudit(10, "")
	if err != nil {
		t.Fatalf("RecentAudit: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("audit rows after broadcast = %d, want exactly 1: %+v", len(rows), rows)
	}
	row := rows[0]
	if row.Action != "broadcast_send" {
		t.Errorf("Action = %q, want broadcast_send", row.Action)
	}
	if !strings.Contains(row.Outcome, "sent=3 fail=0") {
		t.Errorf("Outcome = %q, want it to contain the sent/fail counts", row.Outcome)
	}
	if !strings.Contains(row.Outcome, "photo") {
		t.Errorf("Outcome = %q, want a photo marker", row.Outcome)
	}
	if !strings.Contains(row.Outcome, "sale ends soon") {
		t.Errorf("Outcome = %q, want the caption excerpt", row.Outcome)
	}
	if row.Detail != "sale ends soon" {
		t.Errorf("Detail = %q, want the full caption body", row.Detail)
	}
}

// TestBroadcastAuditRowNoRawExcerptOverflow pins the excerpt-vs-full-body
// split itself (task #14): outcome (displayed by sendAuditLog) must stay
// bounded to broadcastOutcomeExcerptRunes even when the admin's message is
// long, while detail (stored, not rendered by the current viewer) must keep
// the complete body up to LogAction's own 2000-rune clip.
//
// The probe text is built from a multi-byte Vietnamese vowel, not ASCII —
// this shop's own customer-facing copy is Vietnamese, so multi-byte
// truncation is the live case, not a theoretical one. "ế" encodes as 3
// UTF-8 bytes, and 200 (the cap) is not a multiple of 3, so slicing the
// excerpt by BYTE index instead of rune index is guaranteed to cut inside a
// character's byte sequence rather than on a boundary, producing a
// corrupted excerpt that cannot match the exact rune-safe prefix asserted
// below. Falsification: change auditBroadcast's `[]rune(excerpt)` cap to a
// byte slice (`excerpt[:200]`) and this test reddens on the excerpt
// comparison; a same-length pure-ASCII probe would not have caught that.
func TestBroadcastAuditRowNoRawExcerptOverflow(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "store.db"))
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	if _, _, err := st.Credit("301", 1000, "LTXN-0"); err != nil {
		t.Fatalf("seed credit: %v", err)
	}

	f := &fakeTG{}
	b := New(Deps{TG: f, Store: st, AdminID: "1"})
	defer b.Shutdown()

	// 250 runes: past the 200-rune outcome excerpt cap, comfortably under
	// LogAction's 2000-rune detail clip.
	const vnChar = "ế"
	longMsg := strings.Repeat(vnChar, 250)

	b.Process(ctx, callbackUpdate(1, "adm:broadcast"))
	b.Process(ctx, textUpdate(1, longMsg))
	if last := lastSendMsg(f); last == nil || !strings.Contains(last.Text, "🔧 Broadcast — confirm:") {
		t.Fatalf("confirm summary = %+v", last)
	}
	b.Process(ctx, callbackUpdate(1, "admdo:confirm"))
	b.Shutdown()

	rows, err := st.RecentAudit(10, "")
	if err != nil {
		t.Fatalf("RecentAudit: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("audit rows after broadcast = %d, want exactly 1: %+v", len(rows), rows)
	}
	row := rows[0]

	if strings.Contains(row.Outcome, longMsg) {
		t.Errorf("Outcome contains the FULL 250-rune message uncapped, want it truncated to %d runes", broadcastOutcomeExcerptRunes)
	}

	// Assert the excerpt on RUNE count against the exact expected prefix —
	// a substring/byte-length check would still pass a byte-sliced
	// (rune-unsafe) excerpt here, since a corrupted 200-byte cut is still
	// "roughly the right size"; only an exact-string comparison against the
	// correct 200-rune prefix catches the mid-character cut.
	parts := strings.SplitN(row.Outcome, " · ", 2)
	if len(parts) != 2 {
		t.Fatalf("Outcome = %q, want a ` · <excerpt>` suffix", row.Outcome)
	}
	excerpt := parts[1]
	wantExcerpt := strings.Repeat(vnChar, broadcastOutcomeExcerptRunes) + "…"
	if excerpt != wantExcerpt {
		t.Errorf("excerpt = %q (%d runes), want the exact %d-rune prefix %q (%d runes)",
			excerpt, len([]rune(excerpt)), broadcastOutcomeExcerptRunes, wantExcerpt, len([]rune(wantExcerpt)))
	}
	if row.Detail != longMsg {
		t.Errorf("Detail = %q (%d runes), want the FULL uncapped %d-rune body", row.Detail, len([]rune(row.Detail)), len([]rune(longMsg)))
	}
}

// TestBroadcastCancel exercises admdo:cancel — the wizard is abandoned and
// the reply routes all the way to admroot (not admcat:settings).
func TestBroadcastCancel(t *testing.T) {
	ctx := context.Background()
	b, f, _ := newSettingsTestBot(t, "1")
	b.Process(ctx, callbackUpdate(1, "adm:broadcast"))
	b.Process(ctx, textUpdate(1, "never mind"))
	b.Process(ctx, callbackUpdate(1, "admdo:cancel"))
	last := lastSendMsg(f)
	if last == nil || last.Text != "❌ Cancelled." {
		t.Fatalf("reply = %+v", last)
	}
	data := buttonData(last.ReplyMarkup)
	if len(data) != 1 || data[0] != "admroot" {
		t.Errorf("cancel must route to admroot, got %v", data)
	}
	st := b.states.Get(1)
	st.mu.Lock()
	armed := st.Admin
	st.mu.Unlock()
	if armed != nil {
		t.Error("cancel must clear the armed wizard")
	}
}

// TestExecBroadcastNilStoreNoPanic drives execBroadcast's OWN b.store==nil
// guard (adminsettings.go:~1372) — a unit-test-only shape (newTestBot wires
// no Store at all). Without that guard, execBroadcast's very next line
// (b.store.AllChatIDs()) would nil-pointer-panic, so this pins that the
// guard stays in place and the reply still reports "0 chats". It does NOT
// reach auditBroadcast: execBroadcast returns before the fan-out worker
// (and therefore auditBroadcast) ever runs. auditBroadcast's own
// b.store==nil guard was unreachable on every call path — execBroadcast
// always returns first — so it has been removed rather than left as dead
// code with a misleading test claiming coverage of it.
func TestExecBroadcastNilStoreNoPanic(t *testing.T) {
	ctx := context.Background()
	b, f := newTestBot("1", nil)
	defer b.Shutdown()

	b.Process(ctx, callbackUpdate(1, "adm:broadcast"))
	b.Process(ctx, textUpdate(1, "hello"))
	if last := lastSendMsg(f); last == nil || !strings.Contains(last.Text, "🔧 Broadcast — confirm:") {
		t.Fatalf("confirm summary = %+v", last)
	}
	b.Process(ctx, callbackUpdate(1, "admdo:confirm"))

	last := lastSendMsg(f)
	want := "📣 Broadcast sent to 0 chats (0 failed)."
	if last == nil || last.Text != want {
		t.Fatalf("nil-store broadcast reply = %+v, want %q", last, want)
	}
}

// TestBroadcastPhotoSuperAdminOnly is the explicit re-check the photo path
// needs since it skips the callback/admin-text gate entirely — a non-super
// admin's photo must be refused, and the wizard must stay armed so they can
// retry with text.
func TestBroadcastPhotoSuperAdminOnly(t *testing.T) {
	ctx := context.Background()
	b, f := newTestBot("999", nil) // chat 2 is neither super admin nor granted
	st := b.states.Get(2)
	st.mu.Lock()
	st.Admin = &AdminAction{Action: "broadcast", Step: "collecting"}
	st.mu.Unlock()

	b.Process(ctx, photoUpdate(2, "file123", "caption"))
	if last := lastSendMsg(f); last == nil || last.Text != "⚠️ Image broadcast is super-admin only — send text instead." {
		t.Fatalf("photo refusal = %+v", last)
	}
	st.mu.Lock()
	stillArmed := st.Admin != nil && st.Admin.Step == "collecting"
	st.mu.Unlock()
	if !stillArmed {
		t.Error("wizard must remain armed after the photo refusal")
	}
}

// TestBroadcastPhotoSuperAdmin confirms the happy path: a super admin's photo
// (with caption) arms the confirm step with the photo summary.
func TestBroadcastPhotoSuperAdmin(t *testing.T) {
	ctx := context.Background()
	b, f, _ := newSettingsTestBot(t, "1")
	b.Process(ctx, callbackUpdate(1, "adm:broadcast"))
	b.Process(ctx, photoUpdate(1, "file123", "check this out"))
	last := lastSendMsg(f)
	want := "🔧 Broadcast — confirm:\n• photo: attached ✅\n• caption: check this out"
	if last == nil || last.Text != want {
		t.Fatalf("confirm summary = %+v, want %q", last, want)
	}
}

// TestBroadcastConfirmNoRace is the M3 fix's regression test: capAdminInput
// (text) and capBroadcastPhoto (photo) both write Message/PhotoID/Step on the
// SAME chat's *AdminAction under st.mu, then (before the fix) called
// sendBroadcastConfirm with the raw pointer, which read those fields back
// with NO lock — a data race between one goroutine's locked write and
// another's unlocked read. Firing both capture paths concurrently, many
// times, on one chat gives `go test -race` a real chance to catch that
// exact race if it ever regresses; with the fix (snapshot under lock, plain
// string params into sendBroadcastConfirm) this must run clean.
func TestBroadcastConfirmNoRace(t *testing.T) {
	ctx := context.Background()
	b, _, _ := newSettingsTestBot(t, "1")
	b.Process(ctx, callbackUpdate(1, "adm:broadcast"))

	const n = 40
	var wg sync.WaitGroup
	wg.Add(2 * n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			b.Process(ctx, textUpdate(1, "concurrent msg "+strconv.Itoa(i)))
		}(i)
		go func(i int) {
			defer wg.Done()
			b.Process(ctx, photoUpdate(1, "file"+strconv.Itoa(i), "concurrent caption"))
		}(i)
	}
	wg.Wait()
}
