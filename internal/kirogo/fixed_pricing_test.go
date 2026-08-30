package kirogo

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func fixedTestClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	return New(Config{BaseURL: srv.URL, AdminKey: "admin-secret"}, nil)
}

// A fixed-class mint must not carry an accountId: the capacity is a pooled
// account rotating its own vendor keys, so there is no per-customer upstream
// key to bind. The field must be structurally absent, not merely empty.
func TestProvisionCustomerFixedOmitsAccountIDAndSkipsUpstream(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.Write([]byte(`{"success":true,"id":"k1","key":"sk-cust","saleClass":"fixed"}`))
	}))
	defer srv.Close()

	_, err := fixedTestClient(t, srv).ProvisionCustomer(context.Background(), ProvisionCustomerRequest{
		Name: "buyer", Credits: 100, SaleClass: SaleClassFixed,
	})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if _, present := body["accountId"]; present {
		t.Fatalf("accountId must be structurally absent for fixed: %+v", body)
	}
	if body["saleClass"] != "fixed" {
		t.Fatalf("saleClass = %v", body["saleClass"])
	}
	if body["skipUpstream"] != true {
		t.Fatalf("skipUpstream = %v, want true", body["skipUpstream"])
	}
}

// Passing an accountId for a fixed mint is a misunderstanding of which class is
// being sold, and is rejected before the round trip.
func TestProvisionCustomerFixedRejectsAccountID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("the client should not have sent a request")
	}))
	defer srv.Close()

	_, err := fixedTestClient(t, srv).ProvisionCustomer(context.Background(), ProvisionCustomerRequest{
		Name: "buyer", Credits: 100, SaleClass: SaleClassFixed, AccountID: "acct-1",
	})
	if err != ErrAccountIDForbiddenForFixed {
		t.Fatalf("err = %v, want ErrAccountIDForbiddenForFixed", err)
	}
}

// An omitted price is rejected client-side: sending nothing would price the
// model free on every subsequent request instead of failing loudly once.
func TestSetModelPriceRequiresAnExplicitPrice(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("the client should not have sent a request")
	}))
	defer srv.Close()
	c := fixedTestClient(t, srv)

	if _, err := c.SetModelPrice(context.Background(), SetModelPriceRequest{Model: "glm-5.3-flash"}); err != ErrPerRequestRequired {
		t.Fatalf("err = %v, want ErrPerRequestRequired", err)
	}
	neg := -1.0
	if _, err := c.SetModelPrice(context.Background(), SetModelPriceRequest{Model: "m", PerRequest: &neg}); err != ErrPerRequestNegative {
		t.Fatalf("err = %v, want ErrPerRequestNegative", err)
	}
	zero := 0.0
	if _, err := c.SetModelPrice(context.Background(), SetModelPriceRequest{PerRequest: &zero}); err != ErrModelRequired {
		t.Fatalf("err = %v, want ErrModelRequired", err)
	}
}

// A deliberate zero IS allowed through — free is a legitimate price, it just has
// to be stated.
func TestSetModelPriceSendsExplicitZero(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.Write([]byte(`{"success":true,"model":"m","perRequest":0}`))
	}))
	defer srv.Close()

	zero := 0.0
	out, err := fixedTestClient(t, srv).SetModelPrice(context.Background(), SetModelPriceRequest{Model: " m ", PerRequest: &zero})
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if body["model"] != "m" {
		t.Fatalf("model not trimmed on the wire: %+v", body)
	}
	if !out.Success {
		t.Fatal("response not decoded")
	}
}
