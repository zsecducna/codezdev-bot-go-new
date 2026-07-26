package config

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestBootGuardNeverPrintsRawValues — BootGuard is generic over the env key and
// its error goes straight to log.Fatal at both call sites, so it is a
// print-any-env-value primitive. Today it is only handed public identifiers,
// but the moment someone guards KIRO_GO_ADMIN_KEY or BINANCE_SECRET_KEY the
// old formatting would have written that secret to the journal. This test
// makes the masking a contract rather than a convention.
func TestBootGuardNeverPrintsRawValues(t *testing.T) {
	const envSecret = "sk-env-side-secret-value"
	const boundSecret = "sk-bound-side-secret-value"

	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".env"), "KIRO_GO_ADMIN_KEY="+envSecret+"\n")
	c := mustLoad(t, dir)

	err := c.BootGuard("KIRO_GO_ADMIN_KEY", boundSecret)
	if err == nil {
		t.Fatal("BootGuard should refuse to boot on a mismatch")
	}
	msg := err.Error()
	if strings.Contains(msg, envSecret) {
		t.Errorf("the .env value appears verbatim in %q", msg)
	}
	if strings.Contains(msg, boundSecret) {
		t.Errorf("the bound value appears verbatim in %q", msg)
	}
	// The key NAME is the diagnostic an operator actually needs and must stay.
	if !strings.Contains(msg, "KIRO_GO_ADMIN_KEY") {
		t.Errorf("the env key name is missing from %q", msg)
	}
}

// TestMaskValue pins the masking rule itself, including the short-value case:
// a 4-character tail of an 8-character secret is most of the secret, so short
// values reveal nothing at all — the same "a partial redaction is no
// redaction" reasoning as provision/parse.go:107-108.
func TestMaskValue(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", "<empty>"},
		{"1234", "<4 chars>"},
		{"12345678", "<8 chars>"},
		{"123456789", "<9 chars ending 6789>"},
		{"sk-a-very-long-admin-key-value", "<30 chars ending alue>"},
	}
	for _, tc := range cases {
		if got := maskValue(tc.in); got != tc.want {
			t.Errorf("maskValue(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
