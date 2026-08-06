package handlers

// refund_kind_completeness_test.go — spec T3b (stats-fixes-spec.md §2.3 /
// §7 "TestEveryRefundedWriteSetsAKind"): a STRUCTURAL guard that no call site
// anywhere in this module can reach status='refunded' through the plain
// ClaimOrder wrapper, which writes refund_kind=NULL. That NULL is exactly
// what silently grows the "unclassified" bucket — the fix for A2 recreating
// A2's own bug.
//
// This taxonomy was wrong THREE TIMES while being specced/built (missed
// purchase.go, then both apikey_refund.go sites, then deposit.go used one
// kind for two events a review round caught) and the reviewer's own
// verification method is a grep:
// `grep -rn 'ClaimOrder(' internal/handlers/ | grep '"refunded")'` must
// return nothing. This test goes further than that grep — it parses the Go
// AST rather than matching source text, so it also catches:
//   - a call hand-split across multiple lines (gofmt does not rejoin long
//     argument lists, so a grep anchored on one line misses this)
//   - a toStatus argument passed as a same-file string constant instead of a
//     literal (`const s = "refunded"; ClaimOrder(des, from, s)`)
//   - a toStatus (or kind) argument passed as a backtick raw string literal
//     instead of a double-quoted one (`` ClaimOrder(des, from, `refunded`) ``)
//   - the REALISTIC eighth-site mistake: copying the ClaimOrderKind pattern
//     but leaving the kind blank — `ClaimOrderKind(des, from, "refunded", "")`
//     passes the store's own guard (empty kind is what the plain ClaimOrder
//     wrapper legitimately sends for every OTHER status) and writes exactly
//     the NULL this whole guard exists to prevent.
// and it scans every package in the module, not just internal/handlers, so a
// refund path added anywhere else is still caught.
//
// Falsification: this test fails against the pre-step-3 code (verified by
// mutation, not by deleting a symbol) — reverting any ONE of the seven sites
// below to `b.store.ClaimOrder(des, from, "refunded")` reproduces a violation
// this test catches:
//   deposit.go, apikey_deliver.go (x2), adminorders.go, apikey_refund.go (x2),
//   purchase.go.
// Also verified against every shape a line-based grep or the first version of
// this test missed: a hand-split multi-line call, a toStatus passed via a
// local `const`, a backtick-raw-string toStatus, and
// `ClaimOrderKind(des, from, "refunded", "")`.
//
// NOT chased, deliberately: a kind or toStatus passed through a package-level
// var reassigned at runtime, or a func-typed indirection — those need real
// type-checking (go/types), not an AST walk, to resolve soundly, and no real
// call site in this codebase does either.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// moduleRoot walks up from the current working directory (the package dir,
// per `go test` convention) to the nearest go.mod, so this test scans the
// WHOLE module rather than only its own package.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("moduleRoot: no go.mod found walking up from the package dir")
		}
		dir = parent
	}
}

// fileStringConsts collects `const name = "literal"` declarations at file
// scope, so a toStatus argument passed as a same-file constant (rather than
// an inline literal) still resolves. Deliberately single-file: resolving a
// cross-file or cross-package constant would need full type information,
// which is more machinery than this guard needs — every real call site in
// this codebase passes an inline literal, and the const case only needs to
// be caught, not perfectly generally resolved.
func fileStringConsts(f *ast.File) map[string]string {
	out := map[string]string{}
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range vs.Names {
				if i >= len(vs.Values) {
					continue
				}
				if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
					// strconv.Unquote (not a hand-rolled quote-strip): go/ast
					// string literal VALUES are the raw SOURCE TEXT, which can be
					// a double-quoted OR a backtick-raw-string literal
					// (`refunded`) — a strip-the-first/last-char shortcut only
					// handles the double-quoted form and silently passes a
					// backtick-quoted evasion straight through as unresolved
					// (review finding on this slice).
					if v, err := strconv.Unquote(lit.Value); err == nil {
						out[name.Name] = v
					}
				}
			}
		}
	}
	return out
}

// resolveString returns (value, ok) for an expression that is either a
// string literal (double-quoted or backtick-raw) or a reference to a
// same-file string constant.
func resolveString(e ast.Expr, consts map[string]string) (string, bool) {
	switch v := e.(type) {
	case *ast.BasicLit:
		if v.Kind == token.STRING {
			s, err := strconv.Unquote(v.Value)
			return s, err == nil
		}
	case *ast.Ident:
		s, ok := consts[v.Name]
		return s, ok
	}
	return "", false
}

// TestNoBareClaimOrderToRefunded is spec T3b. It parses every non-test .go
// file in the module and flags two shapes that each write status='refunded'
// with refund_kind left NULL — indistinguishable from a genuine pre-migration
// row and unrecoverable afterward (stats-fixes-spec.md §1.2 — there is no
// backfill):
//
//  1. a call whose selector is EXACTLY "ClaimOrder" (never "ClaimOrderKind" —
//     an AST selector match cannot mistake one for the other regardless of
//     formatting) whose toStatus argument (the 3rd parameter) resolves to
//     "refunded".
//  2. a call whose selector is "ClaimOrderKind" whose toStatus (3rd
//     parameter) resolves to "refunded" AND whose kind (4th parameter)
//     resolves to the empty string "" — the realistic way a future eighth
//     site slips past shape 1 entirely: it copies the ClaimOrderKind
//     pattern correctly but leaves the kind blank, which the store accepts
//     (empty is what ClaimOrder's own delegation legitimately sends for
//     every non-'refunded' toStatus) and writes NULL anyway.
func TestNoBareClaimOrderToRefunded(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()

	var scanned int
	var violations []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == ".omc" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			// Test files seed the store directly via the plain wrapper on
			// purpose (fixtures for an already-refunded row); not a
			// production refund path and out of scope for this guard.
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", path, perr)
		}
		scanned++
		consts := fileStringConsts(f)
		rel, _ := filepath.Rel(root, path)

		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch sel.Sel.Name {
			case "ClaimOrder":
				if len(call.Args) < 3 {
					return true // not the (des, from, to) shape — e.g. the FuncDecl itself
				}
				toStatus, ok := resolveString(call.Args[2], consts)
				if !ok || toStatus != "refunded" {
					return true
				}
				pos := fset.Position(call.Pos())
				violations = append(violations, rel+":"+itoa(pos.Line)+
					" (bare ClaimOrder wrapper to \"refunded\")")
			case "ClaimOrderKind":
				if len(call.Args) < 4 {
					return true // not the (des, from, to, kind) shape — e.g. the FuncDecl itself
				}
				toStatus, ok := resolveString(call.Args[2], consts)
				if !ok || toStatus != "refunded" {
					return true
				}
				kind, ok := resolveString(call.Args[3], consts)
				if !ok || kind != "" {
					return true // a non-empty (or unresolvable) kind is not this shape
				}
				pos := fset.Position(call.Pos())
				violations = append(violations, rel+":"+itoa(pos.Line)+
					` (ClaimOrderKind called with kind="")`)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if scanned == 0 {
		t.Fatal("scanned zero files — test is not scanning anything, would pass vacuously")
	}

	if len(violations) > 0 {
		t.Errorf("%d call site(s) reach status='refunded' via the plain ClaimOrder "+
			"wrapper (writes refund_kind=NULL, unrecoverable — spec §1.2). Every "+
			"forward-to-refunded site must call ClaimOrderKind with an explicit "+
			"kind instead:\n%s", len(violations), strings.Join(violations, "\n"))
	}
}
