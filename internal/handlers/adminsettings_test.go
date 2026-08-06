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

// TestRateValidatorRange drives every case through b.Process -> capSetRate
// (M4 fix). The previous version reimplemented the validator's condition
// verbatim in the test body, so deleting the real range check from the
// handler still left this test green — tautological. Driving the table
// through the actual wizard (the way TestSetRateWizard already does for its
// two cases) means a regression in capSetRate's validation is the only way
// to make this fail.
func TestRateValidatorRange(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		raw    string
		wantOK bool
		wantN  int
	}{
		{"23000", true, 23000},
		{"23,000", true, 23000},
		{"23.000", true, 23000},
		{"999", false, 0},
		{"200001", false, 0},
		{"1000", true, rateMin},
		{"200000", true, rateMax},
		{"abc", false, 0},
		{"-5", false, 0},
		{" ", false, 0}, // trims to "" — Process's step-6 gate only drops a
		// literally-empty msg.Text, so a whitespace-only send still reaches
		// capSetRate itself (see TestSetKiroKeyWizardNeverEchoesKey).
	}
	// sentinelRate primes usd_to_vnd to a value distinct from every wantOK
	// case above BEFORE each subtest's attempt, so the rejection branch's
	// "unchanged" assertion is meaningful rather than trivially matching
	// config's own compiled default (M5-style money-safety guarantee).
	const sentinelRate = 17000
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			b, f, cfg := newSettingsTestBot(t, "1")
			s := cfg.Settings()
			s["usd_to_vnd"] = sentinelRate
			if err := cfg.SaveSettings(s); err != nil {
				t.Fatal(err)
			}
			b.Process(ctx, callbackUpdate(1, "adm:setrate"))
			b.Process(ctx, textUpdate(1, tc.raw))
			last := lastSendMsg(f)
			if tc.wantOK {
				want := "✅ USD→VND rate set to " + commaInt(tc.wantN) + ". New orders and USDT quotes price at this rate."
				if last == nil || last.Text != want {
					t.Fatalf("raw=%q reply = %+v, want %q", tc.raw, last, want)
				}
				if cfg.USDToVND() != strconv.Itoa(tc.wantN) {
					t.Errorf("raw=%q USDToVND() = %q, want %d", tc.raw, cfg.USDToVND(), tc.wantN)
				}
			} else {
				wantPrefix := "⚠️ Enter a whole number between "
				if last == nil || !strings.HasPrefix(last.Text, wantPrefix) {
					t.Fatalf("raw=%q reply = %+v, want prefix %q", tc.raw, last, wantPrefix)
				}
				if cfg.USDToVND() != strconv.Itoa(sentinelRate) {
					t.Errorf("raw=%q: rejected input must not change the rate, USDToVND() = %q, want unchanged %q", tc.raw, cfg.USDToVND(), strconv.Itoa(sentinelRate))
				}
			}
		})
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

func TestSetRateWizard(t *testing.T) {
	ctx := context.Background()
	b, f, cfg := newSettingsTestBot(t, "1")
	before := cfg.USDToVND() // M5: prior value, asserted unchanged below

	b.Process(ctx, callbackUpdate(1, "adm:setrate"))
	b.Process(ctx, textUpdate(1, "999"))
	want := "⚠️ Enter a whole number between 1,000 and 200,000 (VND per 1 USD), e.g. 23000."
	if last := lastSendMsg(f); last == nil || last.Text != want {
		t.Fatalf("invalid reply = %+v, want %q", last, want)
	}
	// M5 fix: the rejected value must persist NOTHING — this is the
	// money-safety assertion the whole handler exists to guarantee.
	if cfg.USDToVND() != before {
		t.Errorf("rejected rate must not change USDToVND(), got %q, want unchanged %q", cfg.USDToVND(), before)
	}
	b.Process(ctx, callbackUpdate(1, "adm:setrate"))
	b.Process(ctx, textUpdate(1, "23,000"))
	wantOK := "✅ USD→VND rate set to 23,000. New orders and USDT quotes price at this rate."
	if last := lastSendMsg(f); last == nil || last.Text != wantOK {
		t.Fatalf("valid reply = %+v, want %q", last, wantOK)
	}
	if cfg.USDToVND() != "23000" {
		t.Errorf("USDToVND() = %q, want 23000", cfg.USDToVND())
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
