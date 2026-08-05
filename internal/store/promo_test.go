package store

// Tests for promo.go's admin promo-code surface (store.py:997-1063) — the
// data layer the 🏷️ PRICING & DISCOUNTS admin category (adminpricing.go)
// drives. TestPromoReserveBounds (store_test.go) already covers the
// sequential reserve/release/deactivate/floor rules; this file adds the
// create/list/delete round trip plus the CONCURRENT oversell guard, which is
// the actual money-safety property PromoReserveUse exists for (the same
// single-conditional-UPDATE pattern as deposit crediting,
// architecture-go.md §5).

import (
	"path/filepath"
	"sync"
	"testing"
)

// TestPromoCreateListDelete proves the basic admin lifecycle: create shows up
// in ListPromos (newest first), GetPromo resolves case-insensitively, and
// DeletePromo removes it for good (store.py:995-1063).
func TestPromoCreateListDelete(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreatePromo("first10", 10, 5, false); err != nil {
		t.Fatalf("CreatePromo first10: %v", err)
	}
	if err := s.CreatePromo("second20", 20, 3, true); err != nil {
		t.Fatalf("CreatePromo second20: %v", err)
	}

	// Case-insensitive lookup; codes are stored upper-case.
	p, err := s.GetPromo("first10")
	if err != nil || p == nil {
		t.Fatalf("GetPromo(first10) = %+v, err=%v", p, err)
	}
	if p.Code != "FIRST10" || p.Percent != 10 || p.MaxUses != 5 || p.Stack || !p.Active {
		t.Errorf("unexpected promo row: %+v", p)
	}

	promos, err := s.ListPromos()
	if err != nil {
		t.Fatalf("ListPromos: %v", err)
	}
	if len(promos) != 2 {
		t.Fatalf("ListPromos len = %d, want 2", len(promos))
	}
	// ORDER BY ts DESC -> the most recently created (second20) comes first.
	if promos[0].Code != "SECOND20" {
		t.Errorf("ListPromos[0] = %q, want SECOND20 (newest first)", promos[0].Code)
	}

	if err := s.DeletePromo("FIRST10"); err != nil {
		t.Fatalf("DeletePromo: %v", err)
	}
	if p, err := s.GetPromo("first10"); err != nil || p != nil {
		t.Fatalf("first10 should be gone after delete: %+v err=%v", p, err)
	}
	promos, err = s.ListPromos()
	if err != nil {
		t.Fatalf("ListPromos after delete: %v", err)
	}
	if len(promos) != 1 || promos[0].Code != "SECOND20" {
		t.Fatalf("ListPromos after delete = %+v, want just SECOND20", promos)
	}
}

// TestPromoReserveUseConcurrent is the money-safety property PromoReserveUse
// exists for: a promo with max_uses=10 hit by 30 concurrent buyers must let
// EXACTLY 10 through, never 11 (an oversold code is a discount the shop
// didn't agree to give). The `used_count < max_uses` guard lives INSIDE the
// UPDATE's WHERE clause (promo.go), so the read-check-write is one atomic
// statement per attempt.
//
// A single *Store's db.SetMaxOpenConns(1) (store.go:79) already serializes
// every call through its own connection pool — hammering ONE Store proves
// only the boundary (won == maxUses exactly), not atomicity: a naive
// SELECT-used_count-then-UPDATE would pass identically under that
// serialization, since nothing ever runs concurrently against it. TWO
// independent *Store handles (two real *sql.DB pools, both WAL +
// busy_timeout=30000) opened on the SAME store.db file give genuine
// cross-connection contention on the same row, which is what the atomic
// UPDATE guard actually has to survive.
func TestPromoReserveUseConcurrent(t *testing.T) {
	const maxUses = 10
	const attempts = 30

	path := filepath.Join(t.TempDir(), "store.db")
	s1, err := Open(path)
	if err != nil {
		t.Fatalf("Open s1: %v", err)
	}
	t.Cleanup(func() { s1.Close() })
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("Open s2: %v", err)
	}
	t.Cleanup(func() { s2.Close() })

	if err := s1.CreatePromo("race10", 15, maxUses, false); err != nil {
		t.Fatalf("CreatePromo: %v", err)
	}

	var wg sync.WaitGroup
	wins := make(chan *Promo, attempts)
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		s := s1
		if i%2 == 1 { // alternate attempts across the two independent pools
			s = s2
		}
		go func(s *Store) {
			defer wg.Done()
			p, err := s.PromoReserveUse("RACE10")
			if err != nil {
				t.Errorf("PromoReserveUse err: %v", err)
				return
			}
			if p != nil {
				wins <- p
			}
		}(s)
	}
	wg.Wait()
	close(wins)

	won := 0
	for range wins {
		won++
	}
	if won != maxUses {
		t.Fatalf("won %d reservations, want exactly %d (oversell guard failed)", won, maxUses)
	}

	final, err := s1.GetPromo("race10")
	if err != nil || final == nil {
		t.Fatalf("GetPromo(race10) after race: %+v err=%v", final, err)
	}
	if final.UsedCount != maxUses {
		t.Errorf("final UsedCount = %d, want %d", final.UsedCount, maxUses)
	}
}
