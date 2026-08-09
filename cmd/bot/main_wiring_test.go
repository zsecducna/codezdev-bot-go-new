package main

// main_wiring_test.go — regression guard for task #26's two periodic
// workers, StartStockSweep (folder-stock expiry janitor) and
// StartReconcileStuckOrders (report-only stuck-order alarm).
//
// Both are pure registrations against the tracked worker registry, called
// once from main() and never again. Deleting either call leaves every other
// test in this repo green: there is no runtime observable for it —
// workerRegistry (internal/handlers/workers.go) exposes only
// Go/TryGo/Shutdown, no count — and main() itself cannot run in a unit test
// (it dials live Telegram and polls forever). A silent regression here means
// folder-stock expiry falls back to lazy-only reclaim (the exact state task
// #26 fixed), or the stuck-order alarm never fires at all, with nothing
// failing to say so.
//
// Same technique TestMainWiresRefundStoreIntoDeps (deps_wiring_test.go)
// already uses for this exact class of bug: a static AST parse of main.go,
// asserting the call expressions are present as statements, since there is
// no way to observe the effect at runtime from a unit test.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestMainWiresPeriodicWorkers asserts main.go contains both
// bot.StartStockSweep(...) and bot.StartReconcileStuckOrders(...) as call
// statements. Either missing means the corresponding periodic worker never
// starts in the real binary, even though every unit test in
// internal/handlers builds its own *Bot and calls these methods directly —
// so the unit suite alone never catches the omission.
//
// Falsification: deleting either bot.Start*(ctx) line from main() makes
// this test's `want[...]` entry for that method stay false, and the loop at
// the bottom reports it.
func TestMainWiresPeriodicWorkers(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	want := map[string]bool{
		"StartStockSweep":           false,
		"StartReconcileStuckOrders": false,
	}

	ast.Inspect(f, func(n ast.Node) bool {
		// Only a bare call statement counts (`bot.StartX(ctx)` on its own
		// line) — matching the spec's ask to assert the call expressions are
		// present AS STATEMENTS, not merely referenced (e.g. passed as a
		// func value) somewhere in the file.
		stmt, ok := n.(*ast.ExprStmt)
		if !ok {
			return true
		}
		call, ok := stmt.X.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if _, tracked := want[sel.Sel.Name]; tracked {
			want[sel.Sel.Name] = true
		}
		return true
	})

	for name, found := range want {
		if !found {
			t.Errorf("main.go has no bot.%s(ctx) call statement — the periodic "+
				"worker never starts in the real binary (task #26)", name)
		}
	}
}
