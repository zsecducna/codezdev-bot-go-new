// Package i18n is the Go port of telegram_bot.py's localization layer: the
// {"vi","en","zh"} string-dict constants (~lines 359-4300), the per-language
// command menus (COMMANDS_BY_LANG @359), the legacy command aliases
// (COMMAND_ALIASES @390), and the boot/runtime white-label rewrite of the
// support @handle (_apply_support_handle @18883-18912).
//
// Design (handlers-go.md §5):
//   - Every user string is a `Str{EN,VI,ZH}`; `Str.T(lang)` mirrors Python's
//     `D.get(lang, D["en"])` (English fallback).
//   - All constants are FIELDS of one `Catalog` value (not scattered package
//     vars) so the white-label rewrite can deep-copy + atomic-swap the whole
//     table, and so the completeness test can reflection-walk it.
//   - `C()` reads the live catalog from an atomic.Pointer, giving every worker
//     goroutine a consistent snapshot (old-or-new, never a torn mix) exactly
//     like Python's GIL-atomic module-global rebind (@18888-18889).
package i18n

import (
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
)

// Lang is a supported language code. The three concrete values mirror Python's
// LANGS tuple (@357). A plain string type keeps map keys / JSON round-trips
// identical to the Python side.
type Lang string

const (
	EN Lang = "en"
	VI Lang = "vi"
	ZH Lang = "zh"
)

// LANGS is the ordered set of supported languages — English first because it is
// the default and leads the picker (telegram_bot.py:357). Order matters: the
// language keyboard and the completeness test both iterate it.
var LANGS = []Lang{EN, VI, ZH}

// Str is one trilingual message constant — the Go shape of a Python
// `{"vi":..,"en":..,"zh":..}` dict literal.
type Str struct {
	EN string
	VI string
	ZH string
}

// T resolves the string for lang, falling back to English for any unknown code
// — the exact semantics of Python's `.get(lang, D["en"])` used at every call
// site (e.g. WELCOME[lang] with lang already normalized to a known code, and
// KB_REFRESH_NOTE.get(lang, KB_REFRESH_NOTE["en"]) @6955 for the defensive
// path).
func (s Str) T(lang Lang) string {
	switch lang {
	case VI:
		return s.VI
	case ZH:
		return s.ZH
	default:
		return s.EN
	}
}

// COMMAND_ALIASES maps legacy Vietnamese slash commands to their current
// English names (telegram_bot.py:390-395). handle_command resolves these on
// entry so an old bookmarked /muahang keeps working after the rename.
var COMMAND_ALIASES = map[string]string{
	"muahang": "buy",
	"naptien": "deposit",
	"lichsu":  "history",
	"sodu":    "info",
}

// ResolveAlias mirrors `COMMAND_ALIASES.get(cmd, cmd)` (telegram_bot.py:1632):
// return the aliased command, or the input unchanged when it is not a legacy
// alias.
func ResolveAlias(cmd string) string {
	if c, ok := COMMAND_ALIASES[cmd]; ok {
		return c
	}
	return cmd
}

// F renders a string's `{token}` placeholders, port of Python's
// `s.format(**kwargs)` call sites (e.g. CODE_PROMPT[lang].format(n=...),
// BALANCE[lang].format(name=..,chat_id=..,balance=..)). Placeholder syntax is
// kept verbatim as `{name}` in the tables so they stay byte-diffable against
// the Python source; F substitutes via a single strings.Replacer pass.
//
// kv is a flat (key, value, key, value, …) list. A trailing odd argument is
// ignored (there is no value to pair it with) rather than panicking, so a
// mis-call degrades to a no-op on that key instead of crashing a worker.
func F(s string, kv ...string) string {
	if len(kv) < 2 {
		return s
	}
	pairs := make([]string, 0, len(kv))
	for i := 0; i+1 < len(kv); i += 2 {
		pairs = append(pairs, "{"+kv[i]+"}", kv[i+1])
	}
	return strings.NewReplacer(pairs...).Replace(s)
}

// ---- live catalog + white-label rewrite -----------------------------------

// current holds the live Catalog. Workers read it via C(); ApplySupportHandle
// swaps in a fully rewritten copy. atomic.Pointer gives the same old-or-new
// (never torn) guarantee Python got for free from the GIL on a dict rebind.
var current atomic.Pointer[Catalog]

// handleMu serializes ApplySupportHandle so two concurrent rewrites (boot env
// apply racing the hourly owner-@username refresher, telegram_bot.py:18935)
// can't interleave their read-modify-swap of appliedHandle. Readers never take
// it — they only Load the atomic pointer.
var handleMu sync.Mutex

// appliedHandle tracks the support @handle currently baked into `current`, so
// the next rewrite replaces the RIGHT token (a delta rewrite, mirroring
// Python's `old = _support_applied` @18895). Starts at the source literal that
// the buildCatalog() strings actually contain.
var appliedHandle = DefaultSupportHandle

func init() {
	c := buildCatalog()
	current.Store(&c)
}

// C returns the live catalog snapshot. Handlers must call C() per use and never
// cache the returned strings across sends (handlers-go.md §5), so a mid-flight
// white-label swap is always picked up on the next render.
func C() *Catalog { return current.Load() }

// AppliedHandle reports the support @handle currently baked into the catalog —
// used by the boot/refresh code to decide whether a re-apply is a no-op.
func AppliedHandle() string {
	handleMu.Lock()
	defer handleMu.Unlock()
	return appliedHandle
}

// ApplySupportHandle rewrites every string in the catalog from the currently
// applied support handle to `handle`, then atomically swaps the new catalog in
// — the port of _apply_support_handle (telegram_bot.py:18883-18912).
//
// Parity notes:
//   - No-op when handle is empty or already applied (@18892), so calling it at
//     boot with the unchanged default literal costs nothing.
//   - The rewrite is a deep reflection walk over the Catalog value graph
//     (struct fields, slices, maps, nested Str), replacing the old handle
//     substring in every string — the Go equivalent of Python's `_wl` recursive
//     replace over the UPPER-CASE module dicts (@18897-18909).
//   - The swap is a single atomic.Store, so a concurrent worker sees the whole
//     old catalog or the whole new one (@18888-18889).
func ApplySupportHandle(handle string) {
	handleMu.Lock()
	defer handleMu.Unlock()
	if handle == "" || handle == appliedHandle {
		return
	}
	old := appliedHandle
	cur := current.Load()
	rewritten := rewriteStrings(reflect.ValueOf(*cur), old, handle).Interface().(Catalog)
	current.Store(&rewritten)
	appliedHandle = handle
}

// rewriteStrings returns a deep copy of v with every string leaf having `old`
// replaced by `new` (strings.ReplaceAll). It recurses through structs, slices,
// arrays, maps and pointers — the value graph a Catalog actually is. Non-string
// scalars are returned unchanged (immutable in Go, so a shallow return is a
// full copy). Port of Python's `_wl` (telegram_bot.py:18897-18904); the Go
// reflection form is verbose but exercises the exact same "walk everything,
// replace in strings only" contract.
func rewriteStrings(v reflect.Value, old, newStr string) reflect.Value {
	switch v.Kind() {
	case reflect.String:
		return reflect.ValueOf(strings.ReplaceAll(v.String(), old, newStr)).Convert(v.Type())
	case reflect.Struct:
		out := reflect.New(v.Type()).Elem()
		for i := 0; i < v.NumField(); i++ {
			// All Catalog / Command / Str fields are exported, so CanSet holds
			// on the fresh addressable copy; skip anything unexported defensively.
			if out.Field(i).CanSet() {
				out.Field(i).Set(rewriteStrings(v.Field(i), old, newStr))
			}
		}
		return out
	case reflect.Slice:
		if v.IsNil() {
			return v
		}
		out := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		for i := 0; i < v.Len(); i++ {
			out.Index(i).Set(rewriteStrings(v.Index(i), old, newStr))
		}
		return out
	case reflect.Array:
		out := reflect.New(v.Type()).Elem()
		for i := 0; i < v.Len(); i++ {
			out.Index(i).Set(rewriteStrings(v.Index(i), old, newStr))
		}
		return out
	case reflect.Map:
		if v.IsNil() {
			return v
		}
		out := reflect.MakeMapWithSize(v.Type(), v.Len())
		for _, k := range v.MapKeys() {
			out.SetMapIndex(k, rewriteStrings(v.MapIndex(k), old, newStr))
		}
		return out
	case reflect.Pointer:
		if v.IsNil() {
			return v
		}
		out := reflect.New(v.Type().Elem())
		out.Elem().Set(rewriteStrings(v.Elem(), old, newStr))
		return out
	default:
		return v
	}
}
