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

// TestAPIKeyExpiryCatalogEntries pins the 2026-10-10 expiry-rule strings in
// EVERY language: RCExpired must tell the customer an expired key cannot be
// recharged (and to buy a new one), Status must render the "expired"
// verdict, and DeliveredLlmgate must carry EXACTLY the {name} {credits}
// {key} {apibase} {pubbase} placeholders — {base} is gone from that
// template (the legacy-endpoint line was removed), and a language missing a
// placeholder would leak a raw "{token}" into a customer message.
func TestAPIKeyExpiryCatalogEntries(t *testing.T) {
	c := C().APIKey
	for _, lang := range []Lang{EN, VI, ZH} {
		rc := c.RCExpired.T(lang)
		if rc == "" || !strings.Contains(strings.ToLower(rc), "expired") && !strings.Contains(rc, "hết hạn") && !strings.Contains(rc, "过期") {
			t.Errorf("RCExpired[%v] = %q, must say the key expired", lang, rc)
		}
		if s, ok := c.Status["expired"]; !ok || s.T(lang) == "" {
			t.Errorf("Status[\"expired\"][%v] missing/empty — My-Keys would render a bare status word", lang)
		}
		// tokens() keeps duplicates ({pubbase} appears 3x: dashboard, docs,
		// models) — compare the DEDUPED set so the assertion is about which
		// placeholders exist, not how often each is used.
		got := tokens(c.DeliveredLlmgate.T(lang))
		seen := map[string]bool{}
		var uniq []string
		for _, tok := range got {
			if !seen[tok] {
				seen[tok] = true
				uniq = append(uniq, tok)
			}
		}
		want := []string{"{apibase}", "{credits}", "{key}", "{name}", "{pubbase}"}
		if !reflect.DeepEqual(uniq, want) {
			t.Errorf("DeliveredLlmgate[%v] placeholders = %v, want exactly %v (no {base} — legacy line removed)", lang, uniq, want)
		}
		if strings.Contains(c.DeliveredLlmgate.T(lang), "{base}") {
			t.Errorf("DeliveredLlmgate[%v] still carries {base} — the legacy-endpoint line must be gone", lang)
		}
	}
}
