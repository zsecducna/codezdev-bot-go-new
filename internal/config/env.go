// Package config ports telegram_bot.py's config handling: .env parsing,
// settings.json (defaults merge + mtime-cached read + atomic save), the
// settings.json > ambient env > .env > compiled-defaults precedence chain,
// and the multi-tenant boot-safety guard. See
// .omc/autopilot/architecture-go.md §7 for the precedence contract this
// package must satisfy, and /Users/z/Desktop/Kiro/telegram_bot.py for the
// exact behavior being reproduced.
package config

import (
	"bufio"
	"io"
	"strings"
)

// envPair is one KEY=VALUE line surviving parseEnvLines, kept in file order
// so Load() can replicate Python's progressive os.environ mutation (first
// occurrence of a duplicate key wins, ambient always wins over any of them).
type envPair struct {
	Key string
	Val string
}

// parseEnvLines parses .env-format text into ordered KEY=VALUE pairs,
// mirroring load_env (telegram_bot.py:6558-6572) line-by-line exactly:
//   - blank lines, "#"-comments, and lines without "=" are skipped
//   - a leading "export " prefix is tolerated and stripped
//   - key/value are split on the FIRST "=" only (Python's str.partition)
//   - value has surrounding whitespace trimmed, then up to a run of double
//     quotes stripped from both ends, then up to a run of single quotes
//     stripped from both ends. This is Python's
//     `v.strip('"').strip("'")` — two independent strips, NOT quote-pair
//     matching, so `"'hello'"` becomes `hello` (strip '"' first -> "'hello'",
//     then strip "'" -> "hello").
//
// Returns pairs in file order; callers decide precedence (Load() merges
// against the ambient environment, only filling still-unset keys).
func parseEnvLines(r io.Reader) ([]envPair, error) {
	var pairs []envPair
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		// strings.Cut on the FIRST "=" matches Python's str.partition("=").
		k, v, _ := strings.Cut(line, "=")
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		v = strings.Trim(v, `"`)
		v = strings.Trim(v, `'`)
		if k == "" {
			continue
		}
		pairs = append(pairs, envPair{Key: k, Val: v})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return pairs, nil
}
