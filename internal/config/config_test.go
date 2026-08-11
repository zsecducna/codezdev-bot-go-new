package config

import (
	"bytes"
	"encoding/json"
	"log"
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
	if v, ok := boolValue(s, "sale_broadcast"); !ok || !v {
		t.Errorf("sale_broadcast = %v (ok=%v), want true (default preserved)", v, ok)
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
		// Task #40: 27500 (whole number, in [RateMin, RateMax]) is a shape
		// capSetRate could actually have written — must still win over env.
		writeFile(t, filepath.Join(dir, "settings.json"), `{"usd_to_vnd": 27500}`)
		c := mustLoad(t, dir)
		if got := c.USDToVND(); got != "27500" {
			t.Errorf("USDToVND() = %q, want settings value %q", got, "27500")
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

	// Task #40 note: this used to be "27500.9 truncates to 27500" — that
	// truncation is exactly the shape-2 poison-value bug (0.5 -> "0"), just
	// with a less alarming example. 27500.9 is not a whole number, so
	// capSetRate (internal/handlers/adminsettings.go) could never have
	// written it; it must now be rejected and fall through, not truncated.
	t.Run("fractional settings value is rejected, not truncated, falls through to env", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, ".env"), "USD_TO_VND=26000\n")
		writeFile(t, filepath.Join(dir, "settings.json"), `{"usd_to_vnd": 27500.9}`)
		c := mustLoad(t, dir)
		if got := c.USDToVND(); got != "26000" {
			t.Errorf("USDToVND() = %q, want env fallback %q (fractional rate is not admin-settable)", got, "26000")
		}
	})

	// TestUSDToVNDPrecedence/whitespace-padded ambient rate is trimmed: the
	// structural half of task #25's follow-up. Load's env snapshot copies the
	// AMBIENT process environment verbatim (config.go:42-53) — only .env-FILE
	// lines go through parseEnvLines's own trim — so an operator-exported
	// `USD_TO_VND=" 25000 "` used to reach every caller of USDToVND() with its
	// whitespace intact, and every caller parses the result with
	// strconv.ParseFloat, which rejects surrounding whitespace outright.
	// t.Setenv (not a .env-file fixture) is deliberate: the defect lives
	// specifically in the un-trimmed AMBIENT path.
	t.Run("whitespace-padded ambient rate is trimmed", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("USD_TO_VND", " 25000 ")
		c := mustLoad(t, dir)
		if got := c.USDToVND(); got != "25000" {
			t.Errorf("USDToVND() = %q, want trimmed %q", got, "25000")
		}
	})
}

// TestUSDToVNDRejectsHandEditedRate is task #40: a hand-edited settings.json
// "usd_to_vnd" can carry a value no admin action (capSetRate,
// internal/handlers/adminsettings.go) could ever have produced. Every case
// here asserts the RESOLVED rate string USDToVND() hands the rest of the
// app — not truthyFloat/validAdminRate directly — so the test pins the
// thing that actually matters: what price the shop uses.
//
// Each malicious/malformed subtest pairs a settings.json value with an env
// fallback that is deliberately DIFFERENT from both the settings value and
// DefaultUSDToVND, so a pass can only mean "fell through to env", never
// "coincidentally landed on the right number a different way" — the trap
// called out in the task: shapes 2 and 3 are the ones where the OLD code
// does NOT fall through (0.5 truncates to poison "0"; true resolves to a
// silently-valid "1"), so a test that lets the old code succeed by
// accident would prove nothing.
func TestUSDToVNDRejectsHandEditedRate(t *testing.T) {
	const envFallback = "26000"

	cases := []struct {
		name string
		json string // settings.json body
		want string // resolved USDToVND()
	}{
		// ---- healthy control -------------------------------------------
		{"healthy in-range integer wins over env", `{"usd_to_vnd": 23000}`, "23000"},

		// ---- shape 1: non-numeric string --------------------------------
		// truthyFloat("abc") = 0 -> already fell through under the old
		// code too; still asserted so a regression here is caught.
		{"non-numeric string falls through to env", `{"usd_to_vnd": "abc"}`, envFallback},

		// ---- shape 2: 0 < x < 1 (the poison-value shape) -----------------
		// OLD code: truthyFloat(0.5)=0.5 (non-zero) -> strconv.Itoa(int(0.5))
		// = "0" -> every downstream `rate <= 0` gate fires. NEW code must
		// reject 0.5 outright (not admin-settable: out of [RateMin,RateMax]
		// AND fractional) and fall through to env — never produce "0".
		{"0.5 does not truncate to poison zero, falls through to env", `{"usd_to_vnd": 0.5}`, envFallback},

		// ---- shape 3: JSON boolean true (silent mispricing shape) -------
		// OLD code: truthyFloat(true)=1 -> strconv.Itoa(1) = "1", a
		// "valid" but catastrophic rate. NEW code must reject it (1 is
		// outside [RateMin, RateMax]) and fall through to env — never "1".
		{"boolean true does not resolve to a silent rate of 1, falls through to env", `{"usd_to_vnd": true}`, envFallback},
		{"boolean false falls through to env", `{"usd_to_vnd": false}`, envFallback},

		// ---- capSetRate boundary — exactly in range is honored -----------
		{"exactly RateMin is honored", `{"usd_to_vnd": 1000}`, "1000"},
		{"exactly RateMax is honored", `{"usd_to_vnd": 200000}`, "200000"},

		// ---- capSetRate boundary — just outside is rejected --------------
		{"one below RateMin falls through to env", `{"usd_to_vnd": 999}`, envFallback},
		{"one above RateMax falls through to env", `{"usd_to_vnd": 200001}`, envFallback},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, filepath.Join(dir, ".env"), "USD_TO_VND="+envFallback+"\n")
			writeFile(t, filepath.Join(dir, "settings.json"), tc.json)
			c := mustLoad(t, dir)
			if got := c.USDToVND(); got != tc.want {
				t.Errorf("USDToVND() with settings.json %s = %q, want %q", tc.json, got, tc.want)
			}
		})
	}
}

// TestUSDToVNDRejectionIsLoggedOncePerLoad is the "must LOG that it did so,
// loudly and once per load, naming the key and the offending value" half of
// task #40 — an admin who never sees this log has no way to know the rate
// they believe is in effect is not.
func TestUSDToVNDRejectionIsLoggedOncePerLoad(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "settings.json"), `{"usd_to_vnd": "abc"}`)
	c := mustLoad(t, dir)

	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(orig)

	// Two calls, same entry point: the mtime-cache must not re-validate
	// (and thus not re-log) on the second call, since settings.json never
	// changed on disk between them.
	_ = c.USDToVND()
	_ = c.USDToVND()

	out := buf.String()
	if !strings.Contains(out, "usd_to_vnd") {
		t.Errorf("log output = %q, want it to name the offending key %q", out, "usd_to_vnd")
	}
	if !strings.Contains(out, "abc") {
		t.Errorf("log output = %q, want it to name the offending value %q", out, "abc")
	}
	if n := strings.Count(out, "usd_to_vnd"); n != 1 {
		t.Errorf("log fired %d times across two USDToVND() calls with no settings.json change, want exactly 1 (once per load)", n)
	}
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

// TestTRONAPIKeyReadsEnvSnapshotNotAmbient pins task #16's fix for
// tron.go:225: a value that exists ONLY in dir/.env — never touching
// os.Setenv, so a raw os.Getenv("TRON_API_KEY") in this same process would
// find nothing — must still reach TRONAPIKey() through the .env snapshot
// Load() built. No settings.json is written here on purpose: no wizard
// exists for this key, so precedence never gets past the .env/ambient layer.
func TestTRONAPIKeyReadsEnvSnapshotNotAmbient(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".env"), "TRON_API_KEY=envkey123\n")
	c := mustLoad(t, dir)
	if got := c.TRONAPIKey(); got != "envkey123" {
		t.Errorf("TRONAPIKey() = %q, want the .env value %q", got, "envkey123")
	}
	if _, ok := os.LookupEnv("TRON_API_KEY"); ok {
		t.Fatal("fixture broken: TRON_API_KEY is set in the ambient environment, so this test would pass even if TRONAPIKey() ignored the .env snapshot")
	}
}

// TestTRONAPIKeyEmptyWhenUnset pins the keyless fallback: TronGrid's free
// tier works without a key, so an absent TRON_API_KEY must resolve to ""
// (apiGet then sends no TRON-PRO-API-KEY header) rather than a compiled
// placeholder that would be sent as a bogus credential.
func TestTRONAPIKeyEmptyWhenUnset(t *testing.T) {
	c := mustLoad(t, t.TempDir())
	if got := c.TRONAPIKey(); got != "" {
		t.Errorf("TRONAPIKey() = %q, want empty when unset", got)
	}
}

// TestKiroLoginPythonReadsEnvSnapshotNotAmbient is TRON_API_KEY's twin for
// task #16's other live instance, internal/provision/login.go:79: a
// KIRO_LOGIN_PYTHON set only in dir/.env (never os.Setenv) must reach
// KiroLoginPython() through the .env snapshot, the same way it must reach
// TRONAPIKey() above.
func TestKiroLoginPythonReadsEnvSnapshotNotAmbient(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".env"), "KIRO_LOGIN_PYTHON=/venv/bin/python3\n")
	c := mustLoad(t, dir)
	if got := c.KiroLoginPython(); got != "/venv/bin/python3" {
		t.Errorf("KiroLoginPython() = %q, want the .env value %q", got, "/venv/bin/python3")
	}
	if _, ok := os.LookupEnv("KIRO_LOGIN_PYTHON"); ok {
		t.Fatal("fixture broken: KIRO_LOGIN_PYTHON is set in the ambient environment, so this test would pass even if KiroLoginPython() ignored the .env snapshot")
	}
}

// TestKiroLoginPythonFallsBackToPython3WhenUnset pins the compiled default:
// no KIRO_LOGIN_PYTHON anywhere resolves to "python3" (resolved through
// PATH), matching provision.LoginPythonFromEnv's own fallback.
func TestKiroLoginPythonFallsBackToPython3WhenUnset(t *testing.T) {
	c := mustLoad(t, t.TempDir())
	if got := c.KiroLoginPython(); got != "python3" {
		t.Errorf("KiroLoginPython() = %q, want the python3 fallback", got)
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
	if !c.PayEnabled("bep20") {
		t.Error(`PayEnabled("bep20") = false, want true default`)
	}
	if !c.PayEnabled("tron") {
		t.Error(`PayEnabled("tron") = false, want true default`)
	}
}

// TestPayEnabledOnChainRailsDefaultOn is the regression guard for the
// restart-goes-dark bug: a production settings.json written before the USDT
// migration has NO "pay_bep20"/"pay_tron" keys at all (PayPal/Binance
// carried the shop back then), so PayEnabled must treat a wholly EMPTY
// settings.json exactly like a freshly created one — both rails ON. If
// either rail's default ever flips back to OFF (e.g. a reintroduced
// _PAY_DEFAULT_OFF-style map), this test fails; bep20.Configured()/
// tron.Configured() are the correct place to gate an unconfigured rail out
// of the UI, not this default (settings.go's PayEnabled doc comment).
func TestPayEnabledOnChainRailsDefaultOn(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "settings.json"), `{}`)
	c := mustLoad(t, dir)
	if !c.PayEnabled("bep20") {
		t.Error(`PayEnabled("bep20") = false on an empty settings.json, want true — the shop would go dark on restart`)
	}
	if !c.PayEnabled("tron") {
		t.Error(`PayEnabled("tron") = false on an empty settings.json, want true — the shop would go dark on restart`)
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
