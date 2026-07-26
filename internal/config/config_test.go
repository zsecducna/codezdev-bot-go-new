package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// .env parsing (parseEnvLines) — table tests mirroring load_env's per-line
// rules (telegram_bot.py:6558-6572).
// ---------------------------------------------------------------------------

func TestParseEnvLines(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  []envPair
	}{
		{
			name:  "basic key value",
			input: "FOO=bar\n",
			want:  []envPair{{"FOO", "bar"}},
		},
		{
			name:  "blank and comment lines skipped",
			input: "\n# a comment\nFOO=bar\n   \n#TELEGRAM_BOT_TOKEN=nope\n",
			want:  []envPair{{"FOO", "bar"}},
		},
		{
			name:  "line without equals sign skipped",
			input: "NOTANASSIGNMENT\nFOO=bar\n",
			want:  []envPair{{"FOO", "bar"}},
		},
		{
			name:  "export prefix tolerated",
			input: "export FOO=bar\n",
			want:  []envPair{{"FOO", "bar"}},
		},
		{
			name:  "double quotes stripped",
			input: `FOO="bar"` + "\n",
			want:  []envPair{{"FOO", "bar"}},
		},
		{
			name:  "single quotes stripped",
			input: "FOO='bar'\n",
			want:  []envPair{{"FOO", "bar"}},
		},
		{
			name:  "double then single quote strip is sequential not paired",
			input: `FOO="'bar'"` + "\n",
			want:  []envPair{{"FOO", "bar"}},
		},
		{
			name:  "surrounding whitespace trimmed from key and value",
			input: "  FOO  =  bar  \n",
			want:  []envPair{{"FOO", "bar"}},
		},
		{
			name:  "value may itself contain an equals sign",
			input: "FOO=bar=baz\n",
			want:  []envPair{{"FOO", "bar=baz"}},
		},
		{
			name:  "empty value is valid",
			input: "FOO=\n",
			want:  []envPair{{"FOO", ""}},
		},
		{
			name:  "duplicate keys both preserved in file order",
			input: "FOO=first\nFOO=second\n",
			want:  []envPair{{"FOO", "first"}, {"FOO", "second"}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseEnvLines(strings.NewReader(tc.input))
			if err != nil {
				t.Fatalf("parseEnvLines() error = %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("parseEnvLines() = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("pair %d = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Load() precedence: ambient env wins over .env; .env fills unset keys;
// duplicate .env keys keep the first occurrence.
// ---------------------------------------------------------------------------

func TestLoadEnvPrecedence(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".env"), strings.Join([]string{
		"AMBIENT_WINS=fromenv",
		"ENV_ONLY=fromenv",
		"DUPLICATE=first",
		"DUPLICATE=second",
		"",
	}, "\n"))

	t.Setenv("AMBIENT_WINS", "fromambient")

	c, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if got := c.Env("AMBIENT_WINS"); got != "fromambient" {
		t.Errorf("AMBIENT_WINS = %q, want ambient value %q", got, "fromambient")
	}
	if got := c.Env("ENV_ONLY"); got != "fromenv" {
		t.Errorf("ENV_ONLY = %q, want %q", got, "fromenv")
	}
	if got := c.Env("DUPLICATE"); got != "first" {
		t.Errorf("DUPLICATE = %q, want first .env occurrence %q", got, "first")
	}
	if got := c.Env("UNSET_ANYWHERE"); got != "" {
		t.Errorf("UNSET_ANYWHERE = %q, want empty", got)
	}
}

func TestLoadMissingEnvFileIsNotAnError(t *testing.T) {
	dir := t.TempDir() // no .env written
	c, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() error = %v, want nil for a missing .env file", err)
	}
	if got := c.Env("ANYTHING"); got != "" {
		t.Errorf("Env(ANYTHING) = %q, want empty", got)
	}
}

func TestEnvOr(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".env"), "SET_KEY=value\n")
	c, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := c.EnvOr("SET_KEY", "default"); got != "value" {
		t.Errorf("EnvOr(SET_KEY) = %q, want %q", got, "value")
	}
	if got := c.EnvOr("MISSING_KEY", "default"); got != "default" {
		t.Errorf("EnvOr(MISSING_KEY) = %q, want %q", got, "default")
	}
}

// ---------------------------------------------------------------------------
// settings.json: defaults merge, round-trip of unknown keys, atomic save.
// ---------------------------------------------------------------------------

func TestSettingsDefaultsWhenFileAbsent(t *testing.T) {
	dir := t.TempDir()
	c, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	s := c.Settings()
	if v, ok := boolValue(s, "pay_balance"); !ok || !v {
		t.Errorf("pay_balance default = %v (ok=%v), want true", v, ok)
	}
	if v := intValue(s, "max_qty"); v != 0 {
		t.Errorf("max_qty default = %d, want 0", v)
	}
}

func TestSettingsMergesFileOverDefaults(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "settings.json"), `{"pay_balance": false, "max_qty": 5}`)
	c, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	s := c.Settings()
	if v, ok := boolValue(s, "pay_balance"); !ok || v {
		t.Errorf("pay_balance = %v (ok=%v), want false (file overrides default)", v, ok)
	}
	// Untouched default key must still be present.
	if v, ok := boolValue(s, "pay_qr"); !ok || !v {
		t.Errorf("pay_qr = %v (ok=%v), want true (default preserved)", v, ok)
	}
	if v := intValue(s, "max_qty"); v != 5 {
		t.Errorf("max_qty = %d, want 5", v)
	}
}

func TestSettingsCorruptFileFallsBackToDefaults(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "settings.json"), `{not valid json`)
	c, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	s := c.Settings()
	if v, ok := boolValue(s, "pay_balance"); !ok || !v {
		t.Errorf("pay_balance = %v (ok=%v), want true (defaults on corrupt file)", v, ok)
	}
}

func TestSaveSettingsRoundTripsUnknownKeys(t *testing.T) {
	dir := t.TempDir()
	c, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	s := c.Settings()
	s["some_future_admin_field"] = "keep me"
	s["max_qty"] = 3
	if err := c.SaveSettings(s); err != nil {
		t.Fatalf("SaveSettings() error = %v", err)
	}

	// No leftover .tmp file after the atomic rename.
	if _, err := os.Stat(filepath.Join(dir, "settings.json.tmp")); !os.IsNotExist(err) {
		t.Errorf("settings.json.tmp still exists after SaveSettings, err = %v", err)
	}

	// A fresh Config (simulating a process restart) must see the same data.
	c2, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	got := c2.Settings()
	if got["some_future_admin_field"] != "keep me" {
		t.Errorf("some_future_admin_field = %v, want %q to round-trip", got["some_future_admin_field"], "keep me")
	}
	if v := intValue(got, "max_qty"); v != 3 {
		t.Errorf("max_qty = %d, want 3", v)
	}
}

// TestSettingsMTimeCache proves Settings() decides staleness from
// settings.json's mtime, not by re-parsing on every call (load_settings,
// telegram_bot.py:5112-5126): overwriting the file's bytes while pinning its
// mtime must NOT be observed until the mtime actually changes.
func TestSettingsMTimeCache(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	writeFile(t, path, `{"max_qty": 1}`)

	c, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if v := intValue(c.Settings(), "max_qty"); v != 1 {
		t.Fatalf("initial max_qty = %d, want 1", v)
	}
	pinned, ok := statMTime(path)
	if !ok {
		t.Fatalf("statMTime() reported missing file right after writing it")
	}

	// Rewrite the content but pin the mtime back to its original value —
	// the cache must keep serving the stale value.
	writeFile(t, path, `{"max_qty": 2}`)
	if err := os.Chtimes(path, pinned, pinned); err != nil {
		t.Fatalf("os.Chtimes() error = %v", err)
	}
	if v := intValue(c.Settings(), "max_qty"); v != 1 {
		t.Errorf("max_qty after same-mtime rewrite = %d, want stale 1 (cache should not have re-read)", v)
	}

	// Now bump the mtime forward — the new content must become visible.
	bumped := pinned.Add(2 * time.Second)
	if err := os.Chtimes(path, bumped, bumped); err != nil {
		t.Fatalf("os.Chtimes() error = %v", err)
	}
	if v := intValue(c.Settings(), "max_qty"); v != 2 {
		t.Errorf("max_qty after mtime bump = %d, want fresh 2", v)
	}
}

// TestSettingsNestedValuesAreDeepCopied guards the concurrency contract of
// the settings cache under the 16-worker pool: the "suspended" map is read
// on every message's maintenance check while the admin suspend flow mutates
// its own Settings() copy before SaveSettings. If nested values were shared
// with the cache (Python's GIL-safe `dict(cache)` shallow copy), that
// mutation would be a fatal concurrent map read/write in Go — so both cache
// boundaries must deep-copy.
func TestSettingsNestedValuesAreDeepCopied(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "settings.json"), `{"suspended": {"prodA": 1}}`)
	c := mustLoad(t, dir)

	s1 := c.Settings()
	sus, ok := s1["suspended"].(map[string]any)
	if !ok {
		t.Fatalf("suspended = %T, want map[string]any", s1["suspended"])
	}
	// Caller mutates its own copy (the admin suspend flow's natural shape).
	sus["prodB"] = true

	s2 := c.Settings()
	got := s2["suspended"].(map[string]any)
	if _, leaked := got["prodB"]; leaked {
		t.Error("nested map returned by Settings() is shared with the cache (caller mutation leaked)")
	}
	if _, kept := got["prodA"]; !kept {
		t.Error("existing nested key lost by the deep copy")
	}

	// Same isolation on the save side: after SaveSettings, further caller
	// mutations of the passed-in map must not reach the refreshed cache.
	if err := c.SaveSettings(s1); err != nil {
		t.Fatalf("SaveSettings() error = %v", err)
	}
	sus["prodC"] = true
	s3 := c.Settings()
	if _, leaked := s3["suspended"].(map[string]any)["prodC"]; leaked {
		t.Error("SaveSettings stored the caller's map by reference (post-save mutation leaked into cache)")
	}
	if _, kept := s3["suspended"].(map[string]any)["prodB"]; !kept {
		t.Error("saved nested key prodB missing after SaveSettings round-trip")
	}
}

func TestSettingsFileRemovedInvalidatesCache(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	writeFile(t, path, `{"max_qty": 9}`)

	c, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if v := intValue(c.Settings(), "max_qty"); v != 9 {
		t.Fatalf("initial max_qty = %d, want 9", v)
	}

	if err := os.Remove(path); err != nil {
		t.Fatalf("os.Remove() error = %v", err)
	}
	if v := intValue(c.Settings(), "max_qty"); v != 0 {
		t.Errorf("max_qty after file removal = %d, want default 0", v)
	}
}

// ---------------------------------------------------------------------------
// Typed getter precedence: settings.json > ambient env > .env > defaults
// (architecture-go.md §7).
// ---------------------------------------------------------------------------

func TestUSDToVNDPrecedence(t *testing.T) {
	t.Run("compiled default when nothing set", func(t *testing.T) {
		c := mustLoad(t, t.TempDir())
		if got := c.USDToVND(); got != DefaultUSDToVND {
			t.Errorf("USDToVND() = %q, want default %q", got, DefaultUSDToVND)
		}
	})

	t.Run("env overrides default", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, ".env"), "USD_TO_VND=26000\n")
		c := mustLoad(t, dir)
		if got := c.USDToVND(); got != "26000" {
			t.Errorf("USDToVND() = %q, want %q", got, "26000")
		}
	})

	t.Run("settings overrides env", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, ".env"), "USD_TO_VND=26000\n")
		writeFile(t, filepath.Join(dir, "settings.json"), `{"usd_to_vnd": 27500.9}`)
		c := mustLoad(t, dir)
		if got := c.USDToVND(); got != "27500" {
			t.Errorf("USDToVND() = %q, want int-truncated settings value %q", got, "27500")
		}
	})

	t.Run("zero settings value falls through to env", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, ".env"), "USD_TO_VND=26000\n")
		writeFile(t, filepath.Join(dir, "settings.json"), `{"usd_to_vnd": 0}`)
		c := mustLoad(t, dir)
		if got := c.USDToVND(); got != "26000" {
			t.Errorf("USDToVND() = %q, want env value %q (falsy settings value ignored)", got, "26000")
		}
	})
}

func TestBEP20AddressPrecedence(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".env"), "BEP20_ADDRESS=0xenv\n")
	writeFile(t, filepath.Join(dir, "settings.json"), `{"bep20_address": "0xsettings"}`)
	c := mustLoad(t, dir)
	if got := c.BEP20Address(); got != "0xsettings" {
		t.Errorf("BEP20Address() = %q, want settings override %q", got, "0xsettings")
	}
}

func TestTRONAddressFallsBackToEnvWhenSettingsUnset(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".env"), "TRON_ADDRESS=Tenv\n")
	c := mustLoad(t, dir)
	if got := c.TRONAddress(); got != "Tenv" {
		t.Errorf("TRONAddress() = %q, want env value %q", got, "Tenv")
	}
}

func TestKiroGoSettingsOverrideEnv(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".env"), strings.Join([]string{
		"KIRO_GO_ADMIN_KEY=envkey",
		"KIRO_GO_BASE_URL=https://env.example",
		"KIRO_GO_ACCOUNT_ID=envacct",
		"",
	}, "\n"))
	writeFile(t, filepath.Join(dir, "settings.json"), `{
		"kiro_go_admin_key": "setkey",
		"kiro_go_base_url": "https://set.example",
		"kiro_go_account_id": "setacct"
	}`)
	c := mustLoad(t, dir)

	if got := c.KiroGoAdminKey(); got != "setkey" {
		t.Errorf("KiroGoAdminKey() = %q, want %q", got, "setkey")
	}
	if got := c.KiroGoBaseURL(); got != "https://set.example" {
		t.Errorf("KiroGoBaseURL() = %q, want %q", got, "https://set.example")
	}
	if got := c.KiroGoAccountID(); got != "setacct" {
		t.Errorf("KiroGoAccountID() = %q, want %q", got, "setacct")
	}
}

func TestKiroGoFallsBackToEnvWhenSettingsBlank(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".env"), "KIRO_GO_ADMIN_KEY=envkey\n")
	// A blank/whitespace-only settings value must not shadow the env value,
	// mirroring apply_saved_kirogo's `.strip()` truthiness check.
	writeFile(t, filepath.Join(dir, "settings.json"), `{"kiro_go_admin_key": "   "}`)
	c := mustLoad(t, dir)
	if got := c.KiroGoAdminKey(); got != "envkey" {
		t.Errorf("KiroGoAdminKey() = %q, want env fallback %q", got, "envkey")
	}
}

// ---------------------------------------------------------------------------
// Pure settings.json getters: ReferralEnabled, PayEnabled, MaxBulk.
// ---------------------------------------------------------------------------

func TestReferralEnabledLegacyFallback(t *testing.T) {
	t.Run("defaults to false", func(t *testing.T) {
		c := mustLoad(t, t.TempDir())
		if c.ReferralEnabled() {
			t.Error("ReferralEnabled() = true, want false default")
		}
	})
	t.Run("explicit referral_enabled wins", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "settings.json"), `{"referral_enabled": true, "promo_enabled": false}`)
		c := mustLoad(t, dir)
		if !c.ReferralEnabled() {
			t.Error("ReferralEnabled() = false, want true")
		}
	})
	t.Run("legacy promo_enabled used when referral_enabled absent", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "settings.json"), `{"promo_enabled": true}`)
		c := mustLoad(t, dir)
		if !c.ReferralEnabled() {
			t.Error("ReferralEnabled() = false, want true via legacy promo_enabled")
		}
	})
}

func TestPayEnabledDefaults(t *testing.T) {
	c := mustLoad(t, t.TempDir())
	if !c.PayEnabled("balance") {
		t.Error(`PayEnabled("balance") = false, want true default`)
	}
	if c.PayEnabled("bep20") {
		t.Error(`PayEnabled("bep20") = true, want false default (on-chain methods default off)`)
	}
	if c.PayEnabled("tron") {
		t.Error(`PayEnabled("tron") = true, want false default`)
	}
}

func TestPayEnabledAdminOverride(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "settings.json"), `{"pay_bep20": true, "pay_balance": false}`)
	c := mustLoad(t, dir)
	if !c.PayEnabled("bep20") {
		t.Error(`PayEnabled("bep20") = false, want true after admin override`)
	}
	if c.PayEnabled("balance") {
		t.Error(`PayEnabled("balance") = true, want false after admin override`)
	}
}

func TestMaxBulk(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "settings.json"), `{"max_qty": 12}`)
	c := mustLoad(t, dir)
	if got := c.MaxBulk(); got != 12 {
		t.Errorf("MaxBulk() = %d, want 12", got)
	}
}

// ---------------------------------------------------------------------------
// BootGuard — multi-tenant money-safety check (telegram_bot.py:18957-18971).
// ---------------------------------------------------------------------------

func TestBootGuard(t *testing.T) {
	cases := []struct {
		name       string
		envValue   string
		boundValue string
		wantErr    bool
	}{
		{"unset env is never a mismatch", "", "bound-value", false},
		{"matching values pass", "same", "same", false},
		{"mismatched values refuse to boot", "other", "bound", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.envValue != "" {
				writeFile(t, filepath.Join(dir, ".env"), "SEPAY_ACCOUNT="+tc.envValue+"\n")
			}
			c := mustLoad(t, dir)
			err := c.BootGuard("SEPAY_ACCOUNT", tc.boundValue)
			if (err != nil) != tc.wantErr {
				t.Errorf("BootGuard() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// test helpers
// ---------------------------------------------------------------------------

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writeFile(%s) error = %v", path, err)
	}
}

func mustLoad(t *testing.T, dir string) *Config {
	t.Helper()
	c, err := Load(dir)
	if err != nil {
		t.Fatalf("Load(%s) error = %v", dir, err)
	}
	return c
}

// sanity check that json.Marshal round-trips through SaveSettings without
// losing the shape callers rely on (map[string]any, not a struct-projected
// subset).
func TestSaveSettingsWritesPlainJSONObject(t *testing.T) {
	dir := t.TempDir()
	c := mustLoad(t, dir)
	if err := c.SaveSettings(map[string]any{"pay_balance": false}); err != nil {
		t.Fatalf("SaveSettings() error = %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("saved settings.json is not valid JSON: %v", err)
	}
	if v, ok := decoded["pay_balance"].(bool); !ok || v {
		t.Errorf("decoded pay_balance = %v (ok=%v), want false", v, ok)
	}
}
