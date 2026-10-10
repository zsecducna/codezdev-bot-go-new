package llmgate

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRedactSecrets — both secret shapes this API traffic in (the
// sk-llmgate- key and the login JWT) must collapse to prefix+<redacted>
// while ordinary diagnostic text survives untouched.
func TestRedactSecrets(t *testing.T) {
	const key = "sk-llmgate-Ab3xY9zQlive3cretV4lue000000000000000000"
	const jwt = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIyMiJ9.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c"
	cases := []struct {
		name     string
		in       string
		mustDrop string
		mustKeep string
	}{
		{"upstream key echoed", "invalid api key: " + key, key, "invalid api key"},
		{"login JWT echoed", "token rejected: " + jwt, jwt, "token rejected"},
		{"both at once", key + " " + jwt, key, "<redacted>"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := redactSecrets(tc.in)
			if strings.Contains(got, tc.mustDrop) {
				t.Errorf("secret still present in %q", got)
			}
			if !strings.Contains(got, tc.mustKeep) {
				t.Errorf("diagnostic text %q lost from %q", tc.mustKeep, got)
			}
			if !strings.Contains(got, "<redacted>") {
				t.Errorf("no redaction marker in %q", got)
			}
		})
	}
	if got := redactSecrets("account 42 has insufficient credits"); got != "account 42 has insufficient credits" {
		t.Errorf("ordinary text mangled: %q", got)
	}
}

// TestRedactSecretsIdempotent — the placeholder must not itself match
// either pattern, or repeated passes would keep growing the string.
func TestRedactSecretsIdempotent(t *testing.T) {
	in := "bad key sk-llmgate-AbCdEfGhIjKl0123456789 and jwt eyJabc.def_ghi.jkl_mno"
	once := redactSecrets(in)
	if once == in {
		t.Fatalf("nothing was redacted in %q", once)
	}
	if twice := redactSecrets(once); twice != once {
		t.Errorf("not idempotent:\n once  = %q\n twice = %q", once, twice)
	}
}

// TestParseErrorMessageRedacts — parseErrorMessage is the one place a
// SERVER-controlled string enters APIError.Message, which Error() renders
// into any %v — a 401 body echoing the rejected secret would put it in a
// log line. Whether llmgate ever does that is not knowable from this repo;
// redacting unconditionally makes the answer irrelevant.
func TestParseErrorMessageRedacts(t *testing.T) {
	const key = "sk-llmgate-Zzz999live3cretV4lue000000000000000000"
	got := parseErrorMessage([]byte(`{"code":401,"message":"Authentication failed for `+key+`"}`), 401)
	if strings.Contains(got, key) {
		t.Errorf("key survived into %q", got)
	}
	if !strings.Contains(got, "Authentication failed") {
		t.Errorf("message text lost from %q", got)
	}
	// Non-wrapper bodies fall back to the generic status text, redaction
	// moot but the shape must stay stable.
	if got := parseErrorMessage([]byte("<html>502</html>"), 502); got != "HTTP 502" {
		t.Errorf("fallback = %q, want \"HTTP 502\"", got)
	}
}

// TestAPIErrorFromEchoedKeyIsRedacted — end-to-end: a live 401 from a
// server that echoes the customer key must not produce error text carrying
// it, because that text is what the stats failure path logs.
func TestAPIErrorFromEchoedKeyIsRedacted(t *testing.T) {
	const buyerKey = "sk-llmgate-Buyer5ecretKeyValue0001112223334"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		writeErr(w, http.StatusUnauthorized, "invalid api key: "+buyerKey)
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL}, nil)
	_, err := c.KeyStats(context.Background(), buyerKey)
	if err == nil {
		t.Fatal("expected a 401 error")
	}
	if strings.Contains(err.Error(), buyerKey) {
		t.Errorf("the buyer's cleartext key reached the error text: %q", err.Error())
	}
}
