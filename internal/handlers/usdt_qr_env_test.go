package handlers

// usdt_qr_env_test.go — task #41: pins that the on-chain deposit screen's QR
// image is no longer silently dead in production. sendUSDTScreen used to
// gate the QR on strings.EqualFold(effectiveAddr, bep20.Address()) /
// addr == tron.Address() — bep20.Address()/tron.Address() are a bare
// os.Getenv, blind to dir/.env in production (no systemd EnvironmentFile,
// same root cause as task #42), so that comparison was always false and the
// QR was unconditionally dropped, forever, regardless of whether the
// operator had rotated the address or not.
//
// The discriminator that matters here: a t.Setenv-based test sets the
// AMBIENT process environment, which config.Load's snapshot seeds itself
// from FIRST (config.go:42-53) — so a t.Setenv value is visible to BOTH the
// old raw os.Getenv read AND the new cfg.Env/cfg.EnvOr read, and cannot tell
// them apart. Only a value that lives in the .env FILE and NOT in ambient
// exercises the actual production gap, so every test below writes a real
// .env file via newQREnvTestBot instead.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codezdev-bot/internal/config"
	"codezdev-bot/internal/i18n"
)

// newQREnvTestBot builds a Bot wired to a config.Config loaded from a temp
// dir whose .env FILE (not the ambient process environment) carries
// envLines. It asserts every key is ABSENT from the ambient environment
// first — a stray export in the test process would let the OLD, broken
// (raw os.Getenv) code path see the value too, making a red-first check
// pass for the wrong reason.
func newQREnvTestBot(t *testing.T, envLines map[string]string) (*Bot, *fakeTG) {
	t.Helper()
	for k := range envLines {
		if _, ok := os.LookupEnv(k); ok {
			t.Fatalf("%s is set in the ambient environment — this test needs it ABSENT so only the "+
				".env file supplies it (a stray export would make the test pass for the wrong reason)", k)
		}
	}
	dir := t.TempDir()
	var sb strings.Builder
	for k, v := range envLines {
		sb.WriteString(k)
		sb.WriteByte('=')
		sb.WriteString(v)
		sb.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(sb.String()), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatalf("config load: %v", err)
	}
	f := &fakeTG{}
	b := New(Deps{TG: f, Config: cfg, AdminID: "1"})
	t.Cleanup(b.Shutdown)
	return b, f
}

// TestQREnvSnapshotShowsMatchingQR is pin (a): a BEP20_ADDRESS and
// BEP20_QR_URL that exist ONLY in the .env snapshot (no settings.json
// override, no ambient env) must still show the QR — the address the QR
// was generated against is unrotated, so it is exactly as trustworthy as it
// always was. Before the fix, bep20.Address() read ambient env directly,
// saw nothing (these keys are .env-file-only here, exactly like a
// production host with no EnvironmentFile), and unconditionally dropped the
// QR — this test is RED against that code and GREEN against b.envAddrStr/
// b.envQRStr, which both resolve through cfg's own .env parse.
func TestQREnvSnapshotShowsMatchingQR(t *testing.T) {
	ctx := context.Background()
	addr := testSellerBSC
	qrURL := "https://example.com/bep20-env.png"
	b, f := newQREnvTestBot(t, map[string]string{
		"BEP20_ADDRESS": addr,
		"BEP20_QR_URL":  qrURL,
	})

	b.sendUSDTScreen(ctx, 1, i18n.Lang("en"), chainBSC, "test-des", "5.23")

	if len(f.Photos) != 1 {
		t.Fatalf("Photos = %d, want exactly 1 — an unrotated .env-snapshot address must still show its QR", len(f.Photos))
	}
	if f.Photos[0].PhotoURL != qrURL {
		t.Errorf("QR URL = %q, want %q", f.Photos[0].PhotoURL, qrURL)
	}
	if !strings.Contains(f.Photos[0].Caption, addr) {
		t.Errorf("QR caption = %q, want it to contain %q", f.Photos[0].Caption, addr)
	}
}

// TestQREnvSnapshotShowsMatchingQRTron is the TRON twin of pin (a), through
// the exact-compare branch (addr == b.envAddrStr("TRON_ADDRESS")) instead of
// EqualFold.
func TestQREnvSnapshotShowsMatchingQRTron(t *testing.T) {
	ctx := context.Background()
	addr := testSellerTRX
	qrURL := "https://example.com/tron-env.png"
	b, f := newQREnvTestBot(t, map[string]string{
		"TRON_ADDRESS": addr,
		"TRON_QR_URL":  qrURL,
	})

	b.sendUSDTScreen(ctx, 1, i18n.Lang("en"), chainTRX, "test-des", "5.23")

	if len(f.Photos) != 1 {
		t.Fatalf("Photos = %d, want exactly 1 — an unrotated .env-snapshot TRON address must still show its QR", len(f.Photos))
	}
	if f.Photos[0].PhotoURL != qrURL {
		t.Errorf("QR URL = %q, want %q", f.Photos[0].PhotoURL, qrURL)
	}
	if !strings.Contains(f.Photos[0].Caption, addr) {
		t.Errorf("QR caption = %q, want it to contain %q", f.Photos[0].Caption, addr)
	}
}

// TestQREnvSnapshotDropsQROnRotation is pin (b): after a REAL settings.json
// wizard rotation makes the effective address differ from the .env-snapshot
// one the QR was generated against, the QR must still be DROPPED (the
// drop-on-mismatch safety property this task must NOT delete) and the text
// caption must carry the NEW address, never the stale one.
func TestQREnvSnapshotDropsQROnRotation(t *testing.T) {
	ctx := context.Background()
	oldAddr := testSellerBSC
	newAddr := "0x" + strings.Repeat("cd", 20)
	qrURL := "https://example.com/bep20-env.png"
	b, f := newQREnvTestBot(t, map[string]string{
		"BEP20_ADDRESS": oldAddr,
		"BEP20_QR_URL":  qrURL,
	})

	b.Process(ctx, callbackUpdate(1, "adm:setbep20"))
	b.Process(ctx, textUpdate(1, newAddr))

	b.sendUSDTScreen(ctx, 1, i18n.Lang("en"), chainBSC, "test-des", "5.23")

	if len(f.Photos) != 0 {
		t.Fatalf("Photos = %d, want 0 — a stale-address QR must never be sent after a wizard rotation (photos = %+v)", len(f.Photos), f.Photos)
	}
	screen := lastSendMsg(f)
	if screen == nil {
		t.Fatal("no text fallback sent after the QR was dropped")
	}
	if !strings.Contains(screen.Text, newAddr) {
		t.Errorf("fallback caption = %q, want it to contain the NEW address %q", screen.Text, newAddr)
	}
	if strings.Contains(screen.Text, oldAddr) {
		t.Errorf("fallback caption = %q, still contains the STALE address %q", screen.Text, oldAddr)
	}
}
