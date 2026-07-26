package i18n

import (
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// tokenRe matches `{placeholder}` tokens, the format() placeholders kept
// verbatim in the tables (handlers-go.md §5).
var tokenRe = regexp.MustCompile(`\{[a-zA-Z0-9_]+\}`)

func tokens(s string) []string {
	m := tokenRe.FindAllString(s, -1)
	sort.Strings(m)
	return m
}

// checkStr asserts a single Str is complete: all three languages non-empty and
// carrying an identical `{token}` set (handlers-go.md §5 completeness contract).
func checkStr(t *testing.T, path string, s Str) {
	t.Helper()
	if strings.TrimSpace(s.EN) == "" {
		t.Errorf("%s: empty EN", path)
	}
	if strings.TrimSpace(s.VI) == "" {
		t.Errorf("%s: empty VI", path)
	}
	if strings.TrimSpace(s.ZH) == "" {
		t.Errorf("%s: empty ZH", path)
	}
	en, vi, zh := tokens(s.EN), tokens(s.VI), tokens(s.ZH)
	if !reflect.DeepEqual(en, vi) {
		t.Errorf("%s: token mismatch EN%v vs VI%v", path, en, vi)
	}
	if !reflect.DeepEqual(en, zh) {
		t.Errorf("%s: token mismatch EN%v vs ZH%v", path, en, zh)
	}
}

// walk recursively finds every Str leaf in an arbitrary value graph and checks
// it — so any Str a later slice adds to Catalog is automatically covered.
func walk(t *testing.T, path string, v reflect.Value) {
	t.Helper()
	strType := reflect.TypeFor[Str]()
	if v.Type() == strType {
		checkStr(t, path, v.Interface().(Str))
		return
	}
	switch v.Kind() {
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			walk(t, path+"."+v.Type().Field(i).Name, v.Field(i))
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			walk(t, path, v.Index(i))
		}
	case reflect.Map:
		for _, k := range v.MapKeys() {
			walk(t, path+"["+k.String()+"]", v.MapIndex(k))
		}
	case reflect.Pointer:
		if !v.IsNil() {
			walk(t, path, v.Elem())
		}
	}
}

// TestStringsComplete — port of test_strings_complete (handlers-go.md §5, test
// contract #5): every Str in the live catalog has all three languages non-empty
// and an identical placeholder set across them.
func TestStringsComplete(t *testing.T) {
	walk(t, "Catalog", reflect.ValueOf(*C()))
}

// TestCatalogAllExported guards the ApplySupportHandle rewrite: every field
// reachable from Catalog must be exported, so rewriteStrings' CanSet skip path
// (which would ZERO an unexported field rather than preserve it) stays provably
// dead. A future unexported field on Catalog/Command/Str fails this.
func TestCatalogAllExported(t *testing.T) {
	strType := reflect.TypeFor[Str]()
	var check func(path string, ty reflect.Type)
	seen := map[reflect.Type]bool{}
	check = func(path string, ty reflect.Type) {
		if ty.Kind() == reflect.Pointer || ty.Kind() == reflect.Slice || ty.Kind() == reflect.Array {
			check(path, ty.Elem())
			return
		}
		if ty.Kind() != reflect.Struct || seen[ty] {
			return
		}
		seen[ty] = true
		for i := 0; i < ty.NumField(); i++ {
			f := ty.Field(i)
			if !f.IsExported() {
				t.Errorf("%s.%s is unexported — ApplySupportHandle would zero it", path, f.Name)
			}
			if f.Type != strType {
				check(path+"."+f.Name, f.Type)
			}
		}
	}
	check("Catalog", reflect.TypeFor[Catalog]())
}

// TestStrFallback pins T()'s English fallback for unknown/blank languages,
// mirroring `.get(lang, D["en"])`.
func TestStrFallback(t *testing.T) {
	s := Str{EN: "e", VI: "v", ZH: "z"}
	cases := map[Lang]string{EN: "e", VI: "v", ZH: "z", Lang("fr"): "e", Lang(""): "e"}
	for lang, want := range cases {
		if got := s.T(lang); got != want {
			t.Errorf("T(%q) = %q, want %q", lang, got, want)
		}
	}
}

// TestF pins the placeholder renderer, including the ignore-trailing-odd-arg
// behavior.
func TestF(t *testing.T) {
	if got := F("hi {name}, bal {b}", "name", "Ann", "b", "10"); got != "hi Ann, bal 10" {
		t.Errorf("F = %q", got)
	}
	if got := F("no tokens"); got != "no tokens" {
		t.Errorf("F(no kv) = %q", got)
	}
	if got := F("keep {x}", "x"); got != "keep {x}" { // odd arg ignored
		t.Errorf("F(odd) = %q", got)
	}
}

// TestResolveAlias pins the legacy-command alias map.
func TestResolveAlias(t *testing.T) {
	for in, want := range map[string]string{
		"muahang": "buy", "naptien": "deposit", "lichsu": "history",
		"sodu": "info", "buy": "buy", "unknown": "unknown",
	} {
		if got := ResolveAlias(in); got != want {
			t.Errorf("ResolveAlias(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestApplySupportHandle verifies the white-label rewrite: the support handle
// changes everywhere it appears, non-handle strings are untouched, a no-op call
// is a no-op, and AppliedHandle tracks the delta. Restores the default at the
// end so package global state stays clean for other tests.
func TestApplySupportHandle(t *testing.T) {
	if AppliedHandle() != DefaultSupportHandle {
		t.Fatalf("precondition: AppliedHandle=%q, want default", AppliedHandle())
	}
	welcomeBefore := C().Welcome.EN

	ApplySupportHandle("@newshop")
	if got := C().Support.EN; !strings.Contains(got, "@newshop") || strings.Contains(got, DefaultSupportHandle) {
		t.Errorf("Support.EN after rewrite = %q", got)
	}
	if got := C().Support.ZH; !strings.Contains(got, "@newshop") {
		t.Errorf("Support.ZH not rewritten = %q", got)
	}
	if got := C().Welcome.EN; got != welcomeBefore {
		t.Errorf("Welcome changed by handle rewrite: %q", got)
	}
	if AppliedHandle() != "@newshop" {
		t.Errorf("AppliedHandle = %q, want @newshop", AppliedHandle())
	}

	// No-op: same handle again must not alter the pointer's content.
	ptrBefore := C()
	ApplySupportHandle("@newshop")
	if C() != ptrBefore {
		t.Errorf("re-applying same handle swapped the catalog")
	}
	// Empty handle is also a no-op.
	ApplySupportHandle("")
	if C() != ptrBefore {
		t.Errorf("empty handle swapped the catalog")
	}

	// Restore.
	ApplySupportHandle(DefaultSupportHandle)
	if AppliedHandle() != DefaultSupportHandle {
		t.Fatalf("failed to restore default handle")
	}
	if got := C().Support.EN; !strings.Contains(got, DefaultSupportHandle) {
		t.Errorf("restore failed: Support.EN = %q", got)
	}
}
