package entra

import (
	"regexp"
	"strings"
)

// urlUserinfoRE matches a `scheme://user:pass@` (or `scheme://user@`) prefix
// so redactChildOutput can strip embedded credentials it does NOT hold the
// literal value for. FALLBACK ONLY — the primary defense is exact-substring
// replacement of the secret VALUES the caller actually passed to the child,
// which is immune to any character inside an admin-typed secret, unlike a
// character-class regex. Copied verbatim from internal/awsidc/redact.go
// (this package's brief: "copy the approach", not reinvent it) — see that
// file's doc for the full history of why value-based redaction beats a
// fixed key list.
var urlUserinfoRE = regexp.MustCompile(`://[^\s"']*?@`)

// minRedactSecretLen is the shortest secret value substring-redaction will
// act on, mirroring internal/awsidc/redact.go's identical guard: a one- or
// few-character value is far more likely to coincide with ordinary text
// than to carry real credential entropy, so redacting it would do more harm
// (mangling unrelated output) than good.
const minRedactSecretLen = 8

// redactChildOutput makes raw child stdout/stderr SAFE to embed in an
// admin-visible error message. This package's script runners (enterpriseapp.go,
// users.go) call this on every synthesized error before it is ever returned,
// so an admin-visible string is safe by construction regardless of what the
// child process printed — including a crash before its own error handling
// ever installs. This repo has had FOUR review rounds on exactly this class
// of leak (task brief); do not special-case this away.
//
// Two layers, same order as internal/awsidc's fix:
//  1. Exact-substring redaction of every secret VALUE the caller holds (at
//     or above minRedactSecretLen) — immune to any character in the value,
//     and not limited to a fixed set of key names (a secret nobody
//     remembered to list still gets removed, because this takes VALUES, not
//     key names).
//  2. A structural fallback for any OTHER URL-shaped userinfo this call
//     doesn't hold the literal secret for.
func redactChildOutput(s string, secrets ...string) string {
	for _, v := range secrets {
		v = strings.TrimSpace(v)
		if len(v) < minRedactSecretLen {
			continue
		}
		s = strings.ReplaceAll(s, v, "***")
	}
	return urlUserinfoRE.ReplaceAllString(s, "://***@")
}

// tail returns the last n runes of s — the same tail-trimming
// internal/awsidc's error paths apply (activate.go's tail, discover.go /
// provision.go's redact-then-tail order) so a raw traceback embedded in an
// admin-visible error string never runs unbounded.
func tail(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}
