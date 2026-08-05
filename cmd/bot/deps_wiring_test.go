package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestMainWiresRefundStoreIntoDeps is a regression guard for the bug where
// handlers.Deps grew a RefundStore field (M2.6) but main()'s
// handlers.New(handlers.Deps{...}) literal was never updated to set it. That
// left RefundStore nil in the real binary while every unit test built its own
// handlers.Deps by hand (and so never caught the omission) — the whole
// customer refund-request flow was silently inert in production, with any
// nil-guard downstream just no-op'ing the request instead of erroring.
//
// main() itself can't be exercised in a unit test (it dials live Telegram and
// polls forever), so this statically inspects main.go's source for the
// handlers.Deps{...} composite literal and fails if the RefundStore key is
// missing, or present but bound to a literal nil — both leave the field
// uninitialized at boot exactly like the original bug did. This will catch a
// future regression the same way it would have caught this one, without
// asserting anything true today for the wrong reason.
func TestMainWiresRefundStoreIntoDeps(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	lit := findHandlersDepsLiteral(f)
	if lit == nil {
		t.Fatal("could not find a handlers.Deps{...} composite literal in main.go")
	}

	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok || key.Name != "RefundStore" {
			continue
		}
		if ident, ok := kv.Value.(*ast.Ident); ok && ident.Name == "nil" {
			t.Fatal("handlers.Deps{RefundStore: nil} in main.go — the refund store is not wired into the running bot")
		}
		return // RefundStore is present and bound to something other than a literal nil.
	}
	t.Fatal("handlers.Deps{...} literal in main.go has no RefundStore field — " +
		"the refund store is never wired into the bot, so the customer refund-request flow is inert in production")
}

// findHandlersDepsLiteral walks the AST for the first handlers.Deps{...}
// composite literal (main.go builds exactly one, passed to handlers.New).
func findHandlersDepsLiteral(f *ast.File) *ast.CompositeLit {
	var found *ast.CompositeLit
	ast.Inspect(f, func(n ast.Node) bool {
		if found != nil {
			return false
		}
		cl, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		sel, ok := cl.Type.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "handlers" && sel.Sel.Name == "Deps" {
			found = cl
			return false
		}
		return true
	})
	return found
}
