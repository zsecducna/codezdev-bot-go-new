package entra

import (
	"strings"
	"testing"
)

// TestRedactChildOutputRemovesPlantedSecret and its siblings mirror
// internal/awsidc/redact_test.go's coverage for the identical logic copied
// into this package (redact.go's doc: "copy the approach").

func TestRedactChildOutputRemovesPlantedSecret(t *testing.T) {
	const secret = "ENTRA-SCIM-TOP-SECRET-TOKEN-VALUE"
	in := "SCIM probe failed: bearer " + secret + " rejected"
	out := redactChildOutput(in, secret)
	if strings.Contains(out, secret) {
		t.Errorf("redactChildOutput(%q) = %q, still contains the secret", in, out)
	}
}

func TestRedactChildOutputEmptyOrWhitespaceSecretIsNoop(t *testing.T) {
	const msg = "an ordinary message with no secrets in it"
	for _, secret := range []string{"", "   "} {
		if out := redactChildOutput(msg, secret); out != msg {
			t.Errorf("redactChildOutput(%q, %q) = %q, want unchanged %q", msg, secret, out, msg)
		}
	}
}

func TestRedactChildOutputShortSecretSkipped(t *testing.T) {
	const msg = "reading /nope/x.json failed"
	if out := redactChildOutput(msg, "x"); out != msg {
		t.Errorf("redactChildOutput(%q, \"x\") = %q, want unchanged %q (secret too short to redact)", msg, out, msg)
	}
}

func TestRedactChildOutputURLUserinfoFallback(t *testing.T) {
	const in = "connect failed: https://user:unknownpass@scim.example.com/Users"
	out := redactChildOutput(in) // no secrets held — only the structural fallback applies
	if strings.Contains(out, "unknownpass") {
		t.Errorf("redactChildOutput(%q) = %q, want userinfo scrubbed by the URL fallback", in, out)
	}
}

func TestTailTrimsToLastNRunes(t *testing.T) {
	const s = "0123456789"
	if got := tail(s, 4); got != "6789" {
		t.Errorf("tail(%q, 4) = %q, want %q", s, got, "6789")
	}
	if got := tail(s, 100); got != s {
		t.Errorf("tail(%q, 100) = %q, want unchanged %q", s, got, s)
	}
}
