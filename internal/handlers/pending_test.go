package handlers

import (
	"reflect"
	"sort"
	"testing"
	"time"
)

// customerClearFields / adminClearFields are the EXACT field sets
// ClearCustomerPending / ClearAdminPending must touch (handlers-go.md §3: 8
// customer slots, 46 admin slots). TestClearListsExact asserts the code clears
// precisely these — a new slot added to one clear method but not the expected
// list (or vice versa) fails the test, mirroring the Python "sibling captures
// never collide" invariant.
var customerClearFields = []string{
	"Purchase", "Deposit", "APIKey", "APIKeyRC", "OrderCode",
	"Code", "CustReset", "CustDelete",
}

var adminClearFields = []string{
	"Admin", "CustLookup", "AddAdmin", "AddReseller", "AddWL", "RmAdmin",
	"Reset", "GetOrd", "Refund", "Discount", "Promo", "ResellerDiscount",
	"Upstock", "UpstockConfirm", "UpstockAcc", "UpstockPreviewMsg", "UpstockStoreSubs",
	"Price", "Tier", "Hold", "NewProd", "RenProd", "SetNotice", "SetDelivery",
	"EditFile", "RmProd", "Entra", "SetBep20", "SetTron",
	"SetApiPrice", "SetApiMin", "SetApiMax", "SetKiroPrice", "SetKiroMin",
	"SetKiroMax", "SetApiCap", "SetKiroKey", "SetKiroURL", "SetKiroAcct",
	"ProvPool", "SetMaxQty", "RefundReject",
	"EntraDomains", "EntraVerifyCSV", "RefundDelete",
}

// setAllNonZero fills every exported field of *st with a non-zero value via
// reflection, so a subsequent clear is observable field-by-field. The private
// mutex is skipped (unexported, not settable).
func setAllNonZero(st *ChatState) {
	timeType := reflect.TypeFor[time.Time]()
	v := reflect.ValueOf(st).Elem()
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		if !f.CanSet() { // unexported (mu) — skip
			continue
		}
		if f.Type() == timeType {
			f.Set(reflect.ValueOf(time.Unix(1, 0)))
			continue
		}
		switch f.Kind() {
		case reflect.Bool:
			f.SetBool(true)
		case reflect.String:
			f.SetString("x")
		case reflect.Int, reflect.Int64:
			f.SetInt(1)
		case reflect.Pointer:
			f.Set(reflect.New(f.Type().Elem()))
		case reflect.Slice:
			f.Set(reflect.MakeSlice(f.Type(), 1, 1))
		}
	}
}

// zeroedFields returns the names of exported fields that are now zero-valued.
func zeroedFields(st *ChatState) []string {
	var out []string
	v := reflect.ValueOf(st).Elem()
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		if !f.CanSet() {
			continue
		}
		if f.IsZero() {
			out = append(out, v.Type().Field(i).Name)
		}
	}
	sort.Strings(out)
	return out
}

func TestClearListsExact(t *testing.T) {
	t.Run("customer", func(t *testing.T) {
		st := &ChatState{}
		setAllNonZero(st)
		st.ClearCustomerPending()
		got := zeroedFields(st)
		want := append([]string(nil), customerClearFields...)
		sort.Strings(want)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("ClearCustomerPending cleared %v, want %v", got, want)
		}
	})
	t.Run("admin", func(t *testing.T) {
		st := &ChatState{}
		setAllNonZero(st)
		st.ClearAdminPending()
		got := zeroedFields(st)
		want := append([]string(nil), adminClearFields...)
		sort.Strings(want)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("ClearAdminPending cleared %v, want %v", got, want)
		}
	})
	// Guard the documented counts so a future edit can't silently drift them.
	if len(customerClearFields) != 8 {
		t.Errorf("expected 8 customer clear fields, have %d", len(customerClearFields))
	}
	if len(adminClearFields) != 45 {
		t.Errorf("expected 45 admin clear fields, have %d", len(adminClearFields))
	}
}
