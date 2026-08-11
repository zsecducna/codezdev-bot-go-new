package handlers

// apikey_refund_test.go — owner-alert regression coverage for the #39 site
// in apikey_refund.go: doRefundApikeyGroup's CreditBalance failure. The
// group claims already committed (every refunded order in the group IS
// 'refunded'), so this is the group-refund twin of adminorders.go's
// TestDoRefundCreditFailedNotifiesOwner.

import (
	"context"
	"strings"
	"testing"
)

// TestDoRefundApikeyGroupCreditFailedNotifiesOwner covers
// apikey_refund.go:353 (#39 site 5): CreditBalance errors AFTER the group
// claim already committed — every refunded order stays 'refunded' with
// nothing credited, and no automatic path will ever retry a 'refunded'
// order. Before #39 the CRITICAL log line was the only trace. Deleting the
// notifyOwners call in doRefundApikeyGroup must turn this test red.
func TestDoRefundApikeyGroupCreditFailedNotifiesOwner(t *testing.T) {
	ctx := context.Background()
	b, f, st, fk, _ := newAPIKeyBot(t)
	const chat = 100
	const keyDes = "1111aaaa1111aaaa1111aaaa1111aaaa"
	seedKey(t, st, fk, keyDes, chat, "poolkey-credfail", "sk-test-credfail-0123456789a", "bedrock", 1000, 1000, 200)

	if _, err := st.DB().Exec("DROP TABLE balances"); err != nil {
		t.Fatal(err)
	}

	o, _ := st.GetOrder(keyDes)
	res := b.doRefundApikeyGroup(ctx, o, "balance", false)
	if res == nil || !res.CreditFailed {
		t.Fatalf("res = %+v, want a claimed group refund with CreditFailed=true", res)
	}
	got, _ := st.GetOrder(keyDes)
	if got.Status != "refunded" {
		t.Errorf("order status = %q, want refunded (the claim committed even though the credit failed)", got.Status)
	}
	alert := lastSendMsgTo(f, 999)
	if alert == nil || !strings.Contains(alert.Text, "REFUND CREDIT FAILED") {
		t.Fatalf("owner alert = %v, want a REFUND CREDIT FAILED message", alert)
	}
	if !strings.Contains(alert.Text, keyDes) {
		t.Errorf("owner alert missing the order des: %q", alert.Text)
	}
}
