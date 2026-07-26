package binance

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
)

// TestRedactSignatureStripsHMAC — the exact shape the leak takes: net/http
// returns a *url.Error carrying the full signed request URL, and the caller
// logs it once per poller cycle.
func TestRedactSignatureStripsHMAC(t *testing.T) {
	const hmacHex = "b2f1c0a9e8d7c6b5a4938271605f4e3d2c1b0a998877665544332211ff00eedd"
	raw := &url.Error{
		Op:  "Get",
		URL: Base + PayTxnPath + "?timestamp=1700000000000&recvWindow=60000&limit=100&signature=" + hmacHex,
		Err: errors.New("dial tcp: i/o timeout"),
	}
	got := redactSignature(raw).Error()
	if strings.Contains(got, hmacHex) {
		t.Fatalf("signature still present in %q", got)
	}
	if !strings.Contains(got, "signature=<redacted>") {
		t.Errorf("missing placeholder in %q", got)
	}
	// The non-secret context must survive, or the log line stops being useful.
	for _, want := range []string{"i/o timeout", PayTxnPath, "limit=100"} {
		if !strings.Contains(got, want) {
			t.Errorf("redaction removed useful context %q from %q", want, got)
		}
	}
}

// TestRedactSignatureIdempotent — a redacted string must not accumulate
// placeholders if it passes through the redactor twice.
func TestRedactSignatureIdempotent(t *testing.T) {
	once := redactSignature(errors.New("Get \"https://x/y?signature=deadbeef\": timeout"))
	twice := redactSignature(once)
	if once.Error() != twice.Error() {
		t.Errorf("not idempotent:\n once  = %q\n twice = %q", once.Error(), twice.Error())
	}
}

// TestRedactSignatureUnwrapDoesNotLeak — the redactor deliberately discards
// the original error's type. If it wrapped instead, errors.As would hand back
// the real *url.Error and anything formatting THAT would print the signature
// again, defeating the whole fix.
func TestRedactSignatureUnwrapDoesNotLeak(t *testing.T) {
	const hmacHex = "aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"
	raw := &url.Error{Op: "Get", URL: "https://x/y?signature=" + hmacHex, Err: errors.New("boom")}
	wrapped := fmt.Errorf("binance: pay/transactions request failed: %w", redactSignature(raw))

	if strings.Contains(wrapped.Error(), hmacHex) {
		t.Fatalf("signature leaked through the wrapper: %q", wrapped.Error())
	}
	var ue *url.Error
	if errors.As(wrapped, &ue) {
		t.Errorf("original *url.Error is still reachable via errors.As; it carries %q", ue.URL)
	}
}

// TestRedactSignatureNil — the helper sits on an error path that may be called
// with nil; it must not panic or invent an error.
func TestRedactSignatureNil(t *testing.T) {
	if got := redactSignature(nil); got != nil {
		t.Errorf("redactSignature(nil) = %v, want nil", got)
	}
}

// TestSignedQueryStillCarriesSignature guards the other direction: the
// redaction must not have been "fixed" by removing the signature from the
// request itself, which would silently break every signed call.
func TestSignedQueryStillCarriesSignature(t *testing.T) {
	q := signedQuery("secret", url.Values{"timestamp": []string{"1"}})
	if !strings.Contains(q, "&signature=") {
		t.Fatalf("signedQuery no longer signs the request: %q", q)
	}
	if signatureRe.FindString(q) == "" {
		t.Errorf("signatureRe does not match a real signed query %q — the redactor would be a no-op", q)
	}
}
