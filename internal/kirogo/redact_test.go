package kirogo

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestParseErrorMessageRedactsKeyShapes — parseErrorMessage is the one place a
// SERVER-controlled string enters APIError.Message, and Error() renders that
// into any %v. CustomerMe authenticates with the buyer's cleartext key, so a
// 401 body that echoes the rejected key would put it in a log line. Whether
// Kiro-Go actually does that is not knowable from this repo; redacting
// unconditionally makes the answer irrelevant.
func TestParseErrorMessageRedactsKeyShapes(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		mustDrop string
		mustKeep string
	}{
		{
			name:     "customer sk- key echoed in a 401",
			body:     `{"error":"invalid api key: sk-Ab3xY9zQlive3cretV4lue"}`,
			mustDrop: "sk-Ab3xY9zQlive3cretV4lue",
			mustKeep: "invalid api key",
		},
		{
			name:     "upstream ksk_ key echoed",
			body:     `{"error":"pool key ksk_9f8e7d6c5b4a3210 is exhausted"}`,
			mustDrop: "ksk_9f8e7d6c5b4a3210",
			mustKeep: "is exhausted",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseErrorMessage([]byte(tc.body), http.StatusUnauthorized)
			if strings.Contains(got, tc.mustDrop) {
				t.Errorf("key still present in %q", got)
			}
			if !strings.Contains(got, "<redacted>") {
				t.Errorf("no redaction marker in %q", got)
			}
			if !strings.Contains(got, tc.mustKeep) {
				t.Errorf("redaction destroyed the diagnostic text %q in %q", tc.mustKeep, got)
			}
		})
	}
}

// TestParseErrorMessagePreservesOrdinaryText — over-eager redaction would make
// every server error unreadable, which is its own operational failure.
func TestParseErrorMessagePreservesOrdinaryText(t *testing.T) {
	const body = `{"error":"account 42 has insufficient credits"}`
	got := parseErrorMessage([]byte(body), http.StatusBadRequest)
	if got != "account 42 has insufficient credits" {
		t.Errorf("parseErrorMessage = %q, want the message unchanged", got)
	}
	// A non-JSON body still falls back to the generic status text.
	if got := parseErrorMessage([]byte("<html>502</html>"), http.StatusBadGateway); got != "HTTP 502" {
		t.Errorf("fallback = %q, want \"HTTP 502\"", got)
	}
}

// TestRedactKeyShapesIdempotent — the placeholder must not itself match the
// pattern, or repeated passes would grow the string.
func TestRedactKeyShapesIdempotent(t *testing.T) {
	in := "bad key sk-AbCdEfGhIjKl and pool ksk_0123456789ab"
	once := redactKeyShapes(in)
	if once == in {
		t.Fatalf("nothing was redacted in %q", once)
	}
	if twice := redactKeyShapes(once); twice != once {
		t.Errorf("not idempotent:\n once  = %q\n twice = %q", once, twice)
	}
}

// TestAPIErrorFromEchoedKeyIsRedacted is the end-to-end version: a live 401
// from a server that echoes the key must not produce an APIError whose
// Error() text contains it, because that text is what the api-key stats path
// logs.
func TestAPIErrorFromEchoedKeyIsRedacted(t *testing.T) {
	const buyerKey = "sk-Buyer5ecretKeyValue0001"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]any{
			"error": "invalid api key: " + r.Header.Get("X-Api-Key"),
		})
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL}, nil)
	_, err := c.CustomerMe(context.Background(), buyerKey)
	if err == nil {
		t.Fatal("expected a 401 error")
	}
	if strings.Contains(err.Error(), buyerKey) {
		t.Errorf("the buyer's cleartext key reached the error text: %q", err.Error())
	}
}
