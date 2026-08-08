package config

import (
	"encoding/json"
	"maps"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// defaultSettings mirrors _SETTINGS_DEFAULT (telegram_bot.py:5097-5103) — the
// baseline every settings.json read merges over. Values use the same Go
// types encoding/json would decode them into (bool, map[string]any,
// float64) so a merged result is uniformly typed whether a value came from
// this default or from the JSON file.
func defaultSettings() map[string]any {
	return map[string]any{
		"sale_broadcast":       true,
		"auto_stock_broadcast": true,
		"admin_sale_notify":    true,
		"admin_deposit_notify": true,
		"pay_balance":          true,
		"private_sale":         false,
		"suspended":            map[string]any{},
		"max_qty":              float64(0),
	}
}

// settingsCache mirrors _settings_cache (telegram_bot.py:5109): re-read
// settings.json only when its mtime changes, so the hottest call site (a
// settings lookup on every message) doesn't open+JSON-parse every time.
type settingsCache struct {
	mu     sync.Mutex
	haveMT bool // mirrors Python's cached mtime being a float vs None
	mtime  time.Time
	data   map[string]any // nil until the first Settings() call, like _settings_cache["data"] starting None
}

// statMTime stands in for Python's `os.path.getmtime` / `OSError -> None`
// pattern (telegram_bot.py:5114-5117): returns the file's mtime and whether
// it exists at all.
func statMTime(path string) (time.Time, bool) {
	fi, err := os.Stat(path)
	if err != nil {
		return time.Time{}, false
	}
	return fi.ModTime(), true
}

// Settings returns the bot settings merged over defaultSettings(), cached
// until settings.json's mtime changes — mirrors load_settings
// (telegram_bot.py:5112-5126). The returned map is a DEEP copy: nested
// values (e.g. "suspended") are cloned too, so a caller may freely mutate
// them (the admin suspend flow does exactly that before SaveSettings)
// without racing the 16 worker goroutines reading the cache on every
// message. Python's `dict(cache)` shallow copy was only safe under the GIL;
// in Go a shared nested map mutated concurrently with reads is a fatal
// runtime error, so the copy must go all the way down.
func (c *Config) Settings() map[string]any {
	c.cache.mu.Lock()
	defer c.cache.mu.Unlock()

	mtime, haveMT := statMTime(c.settingsPath)
	if c.cache.data == nil || c.cache.haveMT != haveMT || !c.cache.mtime.Equal(mtime) {
		s := defaultSettings()
		if raw, err := os.ReadFile(c.settingsPath); err == nil {
			var file map[string]any
			// Mirrors `try: s.update(json.load(...)) except Exception: pass`
			// (telegram_bot.py:5120-5123) — a corrupt/unreadable file just
			// leaves the defaults in place, it never surfaces as an error.
			if json.Unmarshal(raw, &file) == nil {
				maps.Copy(s, file)
			}
		}
		c.cache.data = s
		c.cache.mtime = mtime
		c.cache.haveMT = haveMT
	}

	// Deep-copy on the way out so no nested map/slice is ever shared
	// between the cache and a caller (see doc comment above).
	out := make(map[string]any, len(c.cache.data))
	for k, v := range c.cache.data {
		out[k] = deepCopyValue(v)
	}
	return out
}

// deepCopyValue clones the JSON-shaped value graphs settings hold
// (map[string]any / []any nesting, scalars at the leaves). Scalars are
// immutable in Go, so returning them as-is is a full copy. Used at both
// cache boundaries (Settings out, SaveSettings in) so cached data is never
// reachable — and thus never mutable — from outside the mutex.
func deepCopyValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(t))
		for k, vv := range t {
			m[k] = deepCopyValue(vv)
		}
		return m
	case []any:
		s := make([]any, len(t))
		for i, vv := range t {
			s[i] = deepCopyValue(vv)
		}
		return s
	default:
		return v
	}
}

// SaveSettings persists the full settings dict atomically (write
// settings.json.tmp, then rename over settings.json) and immediately
// refreshes the read cache, so an admin change is visible without waiting on
// filesystem mtime granularity — mirrors save_settings
// (telegram_bot.py:5164-5176). Unknown keys round-trip: the caller is
// expected to pass Settings() plus their edits, never a hand-built subset,
// so anything this package doesn't know about survives untouched.
func (c *Config) SaveSettings(s map[string]any) error {
	raw, err := json.Marshal(s)
	if err != nil {
		return err
	}
	tmp := c.settingsPath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, c.settingsPath); err != nil {
		return err
	}

	c.cache.mu.Lock()
	defer c.cache.mu.Unlock()
	// Deep copy into the cache: the caller keeps ownership of s (and may
	// keep mutating its nested maps afterwards) without ever touching the
	// cached copy that concurrent Settings() readers will clone from.
	cp := make(map[string]any, len(s))
	for k, v := range s {
		cp[k] = deepCopyValue(v)
	}
	c.cache.data = cp
	if mtime, ok := statMTime(c.settingsPath); ok {
		c.cache.mtime = mtime
		c.cache.haveMT = true
	} else {
		// Force a re-read on the next Settings() call, mirroring Python's
		// `_settings_cache["mtime"] = None` fallback on error
		// (telegram_bot.py:5175-5176).
		c.cache.haveMT = false
	}
	return nil
}

// boolValue reads a bool-typed settings value; ok is false if the key is
// absent or holds a non-bool value.
func boolValue(s map[string]any, key string) (v bool, ok bool) {
	raw, present := s[key]
	if !present {
		return false, false
	}
	v, ok = raw.(bool)
	return v, ok
}

// stringValue reads a string-typed settings value, or "" if absent or
// holds a non-string value.
func stringValue(s map[string]any, key string) string {
	raw, ok := s[key]
	if !ok {
		return ""
	}
	str, ok := raw.(string)
	if !ok {
		return ""
	}
	return str
}

// intValue coerces a settings value to int leniently, matching the
// `int(x or 0)` guarded by `except (TypeError, ValueError): 0` idiom used
// throughout telegram_bot.py's settings getters (e.g. max_bulk,
// telegram_bot.py:5156-5161).
func intValue(s map[string]any, key string) int {
	raw, ok := s[key]
	if !ok {
		return 0
	}
	switch t := raw.(type) {
	case float64:
		return int(t)
	case int:
		return t
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(t))
		if err != nil {
			return 0
		}
		return n
	case bool:
		if t {
			return 1
		}
		return 0
	default:
		return 0
	}
}

// truthyFloat coerces a settings value to float64, treating anything
// Python's `if r:` would consider falsy (missing, zero, empty/unparseable
// string, false) as 0 — mirrors apply_saved_rate's `if r:` guard
// (telegram_bot.py:5284-5286).
func truthyFloat(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case int:
		return float64(t)
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		if err != nil {
			return 0
		}
		return f
	case bool:
		if t {
			return 1
		}
		return 0
	default:
		return 0
	}
}

// ReferralEnabled reports whether the customer REFERRAL program is on
// (referral codes accepted + referral info in My Info). Admin PROMO codes
// are always usable regardless of this. Falls back to the legacy combined
// "promo_enabled" key so an instance configured before the split keeps its
// referral state. Mirrors referral_enabled (telegram_bot.py:5129-5135).
func (c *Config) ReferralEnabled() bool {
	s := c.Settings()
	if v, ok := boolValue(s, "referral_enabled"); ok {
		return v
	}
	if v, ok := boolValue(s, "promo_enabled"); ok {
		return v
	}
	return false
}

// PayEnabled reports whether payment method `method` (balance/bep20/tron) is
// admin-enabled. All three default ON.
//
// This used to default on-chain methods (bep20/tron) OFF, mirroring
// _PAY_DEFAULT_OFF (telegram_bot.py:5147) — the idea being an admin who
// hasn't set a receiving address yet shouldn't advertise the rail. That
// rationale is already enforced, independently and more precisely, by
// Bot.bep20Configured()/Bot.tronConfigured() (internal/handlers/dispatch.go;
// thin closures over ValidAddress(this package's BEP20Address()/
// TRONAddress()), not internal/pay/bep20.Configured()/tron.Configured()
// directly — see task #17): every render site (cmdDeposit in deposit.go, the
// admin payments menu, the buy screen) gates on PayEnabled(method) &&
// Bot.<rail>Configured(), so an unconfigured rail can NEVER appear
// regardless of what this function returns. Defaulting the
// setting OFF therefore added nothing but a silent failure mode: PayPal and
// BEP20/TRON were removed from defaultSettings() and pay_bep20/pay_tron were
// never written into an already-running instance's settings.json (PayPal
// and Binance carried the shop before the USDT migration), so on restart
// BOTH surviving rails would read as OFF here, cmdDeposit would offer zero
// funding methods, and the shop goes dark with no error and a green test
// suite. Do NOT "restore" the old default — see
// TestPayEnabledOnChainRailsDefaultOn, which exists specifically to fail if
// this regresses. Mirrors pay_enabled (telegram_bot.py:5150-5153), with the
// on-chain-defaults-off behavior deliberately dropped as described above.
func (c *Config) PayEnabled(method string) bool {
	if v, ok := boolValue(c.Settings(), "pay_"+method); ok {
		return v
	}
	return true
}

// MaxBulk returns the admin-configured max accounts per order; 0 means
// unlimited. Mirrors max_bulk (telegram_bot.py:5156-5161).
func (c *Config) MaxBulk() int {
	v := intValue(c.Settings(), "max_qty")
	if v < 0 {
		return 0
	}
	return v
}

// DefaultUSDToVND is the compiled fallback VND-per-USD rate used when
// neither settings.json nor the environment set one — the "25000" literal
// in current_rate_str (telegram_bot.py:5291).
const DefaultUSDToVND = "25000"

// USDToVND returns the effective VND-per-USD exchange rate: settings.json's
// "usd_to_vnd" (int-truncated) overrides the .env/ambient USD_TO_VND, which
// in turn falls back to DefaultUSDToVND. Mirrors apply_saved_rate +
// current_rate_str (telegram_bot.py:5279-5291). Unlike the Python original,
// which lets a non-numeric settings value raise inside apply_saved_rate,
// this falls through to the env/default instead of erroring — a deliberate
// hardening, not a behavior this package needs to reproduce.
//
// TrimSpace on the returned string, structurally, not as a per-caller
// convention: EnvOr/Env read straight from the Load-time snapshot, and that
// snapshot copies the AMBIENT process environment VERBATIM (config.go:42-53
// — only .env-FILE lines go through parseEnvLines's own trim). An
// operator-exported `USD_TO_VND=" 25000 "` therefore used to reach every
// caller of this method with its whitespace intact, and strconv.ParseFloat
// rejects surrounding whitespace outright. Every one of this method's five
// callers across the handlers package parses the result with ParseFloat, so
// a single un-trimmed rate silently broke five independent call sites the
// same way — two were display-only ("— USDT"), but two were FUNCTIONAL
// rejections (a customer's deposit refused outright, an admin's price
// refused outright). Trimming once here, at the one place both paths
// (settings.json's typed float and the raw env string) converge, retires
// the whole class instead of requiring every future ParseFloat(cfg.
// USDToVND()) call site to remember to do it too. (The settings.json branch
// above never needs this: strconv.Itoa on a parsed float64 never produces
// surrounding whitespace.)
func (c *Config) USDToVND() string {
	if v, ok := c.Settings()["usd_to_vnd"]; ok {
		if f := truthyFloat(v); f != 0 {
			return strconv.Itoa(int(f))
		}
	}
	return strings.TrimSpace(c.EnvOr("USD_TO_VND", DefaultUSDToVND))
}

// BEP20Address returns the effective USDT-BEP20 receiving address:
// settings.json's "bep20_address" overrides .env/ambient BEP20_ADDRESS.
// Mirrors apply_saved_bep20 (telegram_bot.py:5294-5306); "" means unset.
func (c *Config) BEP20Address() string {
	if a := strings.TrimSpace(stringValue(c.Settings(), "bep20_address")); a != "" {
		return a
	}
	return c.Env("BEP20_ADDRESS")
}

// TRONAddress returns the effective USDT-TRC20 (TRON) receiving address:
// settings.json's "tron_address" overrides .env/ambient TRON_ADDRESS.
// Mirrors apply_saved_tron (telegram_bot.py:5309-5321); "" means unset.
func (c *Config) TRONAddress() string {
	if a := strings.TrimSpace(stringValue(c.Settings(), "tron_address")); a != "" {
		return a
	}
	return c.Env("TRON_ADDRESS")
}

// KiroGoAdminKey returns the effective Kiro-Go admin key: settings.json's
// "kiro_go_admin_key" overrides .env/ambient KIRO_GO_ADMIN_KEY. This is a
// secret — never log it (repo CLAUDE.md; KIRO_GO_API.md). Mirrors
// apply_saved_kirogo (telegram_bot.py:5326-5341).
func (c *Config) KiroGoAdminKey() string {
	if k := strings.TrimSpace(stringValue(c.Settings(), "kiro_go_admin_key")); k != "" {
		return k
	}
	return c.Env("KIRO_GO_ADMIN_KEY")
}

// KiroGoBaseURL returns the effective Kiro-Go admin base URL:
// settings.json's "kiro_go_base_url" overrides .env/ambient
// KIRO_GO_BASE_URL. Mirrors apply_saved_kirogo (telegram_bot.py:5326-5341).
func (c *Config) KiroGoBaseURL() string {
	if u := strings.TrimSpace(stringValue(c.Settings(), "kiro_go_base_url")); u != "" {
		return u
	}
	return c.Env("KIRO_GO_BASE_URL")
}

// KiroGoAccountID returns the effective Kiro-Go account id: settings.json's
// "kiro_go_account_id" overrides .env/ambient KIRO_GO_ACCOUNT_ID. Mirrors
// apply_saved_kirogo (telegram_bot.py:5326-5341).
func (c *Config) KiroGoAccountID() string {
	if a := strings.TrimSpace(stringValue(c.Settings(), "kiro_go_account_id")); a != "" {
		return a
	}
	return c.Env("KIRO_GO_ACCOUNT_ID")
}
