package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Config is the immutable-env-snapshot + live-settings view over one bot
// instance's configuration. Build it once via Load() at boot; Env/EnvOr read
// the frozen snapshot, while Settings()/SaveSettings() and the typed getters
// below layer settings.json's live overrides on top, mtime-cached. Safe for
// concurrent use: env is never mutated after Load(), and settingsCache guards
// its own state with a mutex.
type Config struct {
	env          map[string]string
	settingsPath string
	cache        settingsCache
}

// Load builds the immutable env snapshot and wires up the settings.json path
// for the mtime-cached settings reader.
//
// The snapshot merges the ambient process environment with dir/.env, ambient
// always winning — mirroring load_env's "only fills unset env keys" rule
// (telegram_bot.py:6558-6572: `if k and k not in os.environ`). Unlike the
// Python original, which mutates the real process os.environ so every later
// os.environ.get() call sees the merge, Go freezes the merge into this
// struct at Load() time; callers read it back via Env/EnvOr instead of
// consulting os.Getenv.
//
// dir is the directory holding both .env and settings.json — production
// layout keeps them alongside the binary, matching the Python original's
// script directory (telegram_bot.py:5087 SETTINGS_PATH). Pass "" to use the
// current working directory.
func Load(dir string) (*Config, error) {
	if dir == "" {
		dir = "."
	}

	// Seed the snapshot from the ambient process environment first: ambient
	// always wins over .env (telegram_bot.py:6571's `k not in os.environ`
	// check runs against whatever os.environ already holds, which at
	// load_env()'s call site is the ambient/systemd-supplied env).
	snapshot := make(map[string]string)
	for _, kv := range os.Environ() {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		snapshot[k] = v
	}

	f, err := os.Open(filepath.Join(dir, ".env"))
	switch {
	case err == nil:
		defer f.Close()
		pairs, perr := parseEnvLines(f)
		if perr != nil {
			return nil, fmt.Errorf("config: reading .env: %w", perr)
		}
		// Apply in file order so a duplicate key's FIRST .env occurrence
		// wins over later ones too, exactly like Python's progressive
		// os.environ mutation line-by-line (telegram_bot.py:6571-6572) —
		// after the first line sets a key, subsequent lines for the same
		// key see it as already present and skip it.
		for _, p := range pairs {
			if _, exists := snapshot[p.Key]; !exists {
				snapshot[p.Key] = p.Val
			}
		}
	case os.IsNotExist(err):
		// No .env file present — mirrors load_env's
		// `if not os.path.exists(path): return` (telegram_bot.py:6560-6561).
	default:
		return nil, fmt.Errorf("config: opening .env: %w", err)
	}

	return &Config{
		env:          snapshot,
		settingsPath: filepath.Join(dir, "settings.json"),
	}, nil
}

// Env returns the snapshot value for key, or "" if it was never set by the
// ambient environment or .env.
func (c *Config) Env(key string) string {
	return c.env[key]
}

// EnvOr returns the snapshot value for key, or def if it is unset or empty.
func (c *Config) EnvOr(key, def string) string {
	if v, ok := c.env[key]; ok && v != "" {
		return v
	}
	return def
}

// BootGuard enforces the multi-tenant money-safety check from main()
// (telegram_bot.py:18957-18971): refuse to boot when the ambient/.env value
// for envKey disagrees with boundValue, the value this binary/instance was
// wired with at init (e.g. the SePay account number it was built for, or the
// configured ADMIN_ID). A mismatch means this instance started without its
// systemd EnvironmentFile and would otherwise silently act as a DIFFERENT
// tenant — e.g. routing a customer's payment into the wrong bank account.
//
// An unset envKey is NOT a mismatch — Python only compares when the .env
// value is present (`if env_acct and env_acct != sepay.ACCOUNT_NUMBER`,
// telegram_bot.py:18963).
// The returned error names the KEY and shows only masked values. BootGuard is
// generic over envKey, so it is a print-any-env-value primitive whose output
// goes to log.Fatal at both call sites (cmd/bot/main.go). Today it is called
// only with SEPAY_ACCOUNT and ADMIN_ID — public identifiers — but the guard's
// whole purpose is to catch a missing EnvironmentFile, and the next value
// worth guarding that way is a secret (KIRO_GO_ADMIN_KEY, BINANCE_SECRET_KEY),
// which the old formatting would have written to the journal verbatim. The key
// name plus "they differ" is the entire diagnostic an operator needs; the
// masked tails only help confirm WHICH of the two sources won, and an operator
// can read both values themselves.
func (c *Config) BootGuard(envKey, boundValue string) error {
	v := c.Env(envKey)
	if v != "" && v != boundValue {
		return fmt.Errorf("%s mismatch (.env=%s but bound=%s). Env must be set BEFORE import (systemd EnvironmentFile). Refusing to boot.",
			envKey, maskValue(v), maskValue(boundValue))
	}
	return nil
}

// maskValue renders a config value as a length plus its last 4 characters —
// enough to tell two values apart at a glance, not enough to be a credential.
//
// Values of 8 characters or fewer show no characters at all: a short secret
// (a PIN, a short account number) would be substantially recovered by a 4-char
// tail, and the "no partial redaction" rule from provision/parse.go:107-108
// applies here for the same reason.
func maskValue(s string) string {
	if s == "" {
		return "<empty>"
	}
	if len(s) <= 8 {
		return fmt.Sprintf("<%d chars>", len(s))
	}
	return fmt.Sprintf("<%d chars ending %s>", len(s), s[len(s)-4:])
}
