package bands

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// Provider names a USDT rail. All three share ONE Registry so the collision set
// spans providers (see the package doc's "DELIBERATE DEVIATION" note). Python's
// three dicts: _binance_pending @9072, _bep20_pending @9393, _tron_pending
// @9885.
type Provider string

const (
	ProviderBinance Provider = "binance"
	ProviderBEP20   Provider = "bep20"
	ProviderTron    Provider = "tron"
)

// DefaultEvictAfter is USDT_BAND_EVICT_S (telegram_bot.py:9422-9428): under band
// pressure ONLY — i.e. when the first pass found no free slot — rows dead for at
// least this long stop blocking the picker. Their late payment stays submittable
// against their own order (the row is KEPT, not deleted), so the only thing that
// narrows is the claim-jack guarantee for sessions dead >30min. That is a
// deliberate tradeoff against a price point being unsellable for the full 2h
// pending grace.
const DefaultEvictAfter = 30 * time.Minute

// DefaultPendingGrace is USDT_PENDING_GRACE_S (telegram_bot.py:9542-9548): how
// long a pending row (its unique-amount reservation) is KEPT after its order
// leaves "reserved", so a late-confirming on-chain payment can still be
// submitted and recovered. Only Sweep frees the band, and only past this grace.
const DefaultPendingGrace = 2 * time.Hour

// EvictAfterFromEnv reads USDT_BAND_EVICT_S (seconds, telegram_bot.py:9428).
func EvictAfterFromEnv() time.Duration {
	return time.Duration(envInt("USDT_BAND_EVICT_S", int(DefaultEvictAfter/time.Second))) * time.Second
}

// Deliberately no PendingGraceFromEnv here (task #42 item 6, deleted): it had
// zero production callers. internal/handlers/dispatch.go's b.usdtPendingGrace
// is the one live reader of USDT_PENDING_GRACE_S, resolved through
// b.envInt/config.Config (the .env-visible chain) rather than this package's
// raw env primitives — see the Sweep doc comment below for the real rule.

// Reservation is one live band: the unique amount held for order `Des` on
// `Provider`. Mirrors the value side of Python's pending dicts, minus the
// rail-specific timestamps (t_start / t_click) which stay with the rails
// (telegram_bot.py:9392-9394 documents the Python row shape).
type Reservation struct {
	Provider Provider
	// Des is the order id (Python's dict key).
	Des string
	// ChatID owns the session. Same-chat rules (own-dead-sessions-never-block,
	// sibling redirect) key off this.
	ChatID string
	// Amount is the customer-facing unique amount string, e.g. "6.21".
	Amount string
	// Units is Amount at Decimals precision (621 at 2 decimals).
	Units int64
	// Decimals is the precision Units is expressed in.
	Decimals int
	// CreatedAt is when the band was handed out — Python's pending-row `ts`
	// (telegram_bot.py:9619), the anchor Sweep's grace is measured from.
	CreatedAt time.Time
	// seq is a registry-monotonic stamp assigned by Reserve/Adopt. It is this
	// row's identity across Sweep's unlocked window: a des released and
	// re-reserved in between gets a strictly higher seq, so the stale decision
	// cannot delete the fresh row. A timestamp would not do — two reservations
	// can share a CreatedAt under a coarse or frozen clock.
	seq uint64
	// Deadline is when the order can no longer be paid. Past it the session is
	// "dead": it stops blocking its OWN chat's next allocation immediately, and
	// stops blocking anyone once it is DefaultEvictAfter stale AND the picker is
	// under pressure (telegram_bot.py:9438-9446).
	Deadline time.Time
}

// Config builds a Registry.
type Config struct {
	// Providers must contain an entry for every Provider that will Reserve.
	Providers map[Provider]Params
	// EvictAfter overrides USDT_BAND_EVICT_S; <=0 takes EvictAfterFromEnv().
	EvictAfter time.Duration
	// Now overrides the clock (tests). nil takes time.Now.
	Now func() time.Time
}

// Registry is the thread-safe band allocator. Every method takes the single
// mutex, so allocation is serialised and two concurrent Reserve calls can never
// be handed the same amount. There is no package-level mutable state: everything
// lives on the value New returns.
type Registry struct {
	mu     sync.Mutex
	now    func() time.Time
	evict  time.Duration
	params map[Provider]Params
	// rows is provider -> des -> reservation.
	rows map[Provider]map[string]Reservation
	// nextSeq stamps each stored row with a monotonic id (see Reservation.seq).
	nextSeq uint64
}

// New validates the per-provider geometry and returns an empty registry.
func New(cfg Config) (*Registry, error) {
	if len(cfg.Providers) == 0 {
		return nil, fmt.Errorf("bands: New needs at least one provider")
	}
	params := make(map[Provider]Params, len(cfg.Providers))
	rows := make(map[Provider]map[string]Reservation, len(cfg.Providers))
	for p, prm := range cfg.Providers {
		if err := prm.Validate(); err != nil {
			return nil, fmt.Errorf("bands: provider %q: %w", p, err)
		}
		params[p] = prm
		rows[p] = make(map[string]Reservation)
	}
	r := &Registry{now: cfg.Now, evict: cfg.EvictAfter, params: params, rows: rows}
	if r.now == nil {
		r.now = time.Now
	}
	if r.evict <= 0 {
		r.evict = EvictAfterFromEnv()
	}
	return r, nil
}

// Params returns the geometry registered for a provider.
func (r *Registry) Params(p Provider) (Params, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	prm, ok := r.params[p]
	return prm, ok
}

// Reserve allocates a unique amount for (base price, provider, chat) and records
// it. Called when a pay/deposit screen is opened.
//
// TWO PASSES, exactly as in Python (_binance_unique_amount @9095,
// _bep20_unique_amount @9397, _tron_unique_amount @9888):
//
//	pass 1  avoid every live row except THIS chat's own dead sessions
//	pass 2  (only if pass 1 found no slot) additionally ignore ANY row dead for
//	        more than EvictAfter — "band exhausted" is the incident case, so we
//	        yield stale claim-jack protection before telling a paying customer
//	        we are busy
//
// Returns ErrBandExhausted if both passes fail; the caller must then refuse the
// sale (show BUSY), never fall back to a colliding amount.
//
// Re-reserving an existing Des overwrites its row and — deliberately — does not
// let the old row block the new pick (a row can never collide with itself).
func (r *Registry) Reserve(p Provider, base, chatID, des string, deadline time.Time) (Reservation, error) {
	if des == "" {
		return Reservation{}, fmt.Errorf("bands: Reserve needs a non-empty des")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	prm, ok := r.params[p]
	if !ok {
		return Reservation{}, fmt.Errorf("bands: unknown provider %q", p)
	}
	baseU, err := AmountUnits(base, prm.Decimals)
	if err != nil {
		return Reservation{}, err
	}
	// Pass 1, then pass 2 with long-dead rows evicted under pressure.
	cu, ok := uniqueAmountUnits(prm, baseU, r.collisionUnits(p, prm, chatID, des, false))
	if !ok {
		cu, ok = uniqueAmountUnits(prm, baseU, r.collisionUnits(p, prm, chatID, des, true))
	}
	if !ok {
		return Reservation{}, ErrBandExhausted
	}
	res := Reservation{
		Provider:  p,
		Des:       des,
		ChatID:    chatID,
		Amount:    FormatUnits(cu, prm.Decimals),
		Units:     cu,
		Decimals:  prm.Decimals,
		CreatedAt: r.now(),
		Deadline:  deadline,
		seq:       r.stampSeq(),
	}
	r.rows[p][des] = res
	return res, nil
}

// stampSeq hands out the next monotonic row id. Caller holds r.mu.
func (r *Registry) stampSeq() uint64 {
	r.nextSeq++
	return r.nextSeq
}

// collisionUnits is _usdt_collision_amounts (telegram_bot.py:9431-9451),
// rescaled into `prm.Decimals` and widened to span EVERY provider.
//
// Excluded from the set:
//   - the requester's own row (a des cannot collide with itself)
//   - rows owned by `chatID` whose deadline has passed — one customer's
//     abandoned sessions must not exhaust the 4-slot band against themselves
//     (the 2026-07-02 incident, telegram_bot.py:9400-9409). Reusing your OWN
//     dead session's amount is claim-jack-safe: the band never belonged to
//     another customer, and verify still re-checks ownership/window/hash-dedupe.
//     An empty chatID means "no chat" (Python's chat_id=None legacy call) and
//     excludes nothing.
//   - when evictStale: ANY row dead for more than r.evict (pressure pass only).
//
// Each surviving row becomes a blocker carrying the gap that keeps ITS band
// disjoint from the candidate: the requester's own MinGap for a same-rail row,
// and max(reqTol, rowTol)*2+1 for a row on another rail, so cross-provider
// disjointness holds from both tolerances (package doc, deviation 1).
//
// Caller holds r.mu.
func (r *Registry) collisionUnits(reqP Provider, prm Params, chatID, des string, evictStale bool) []blocker {
	now := r.now()
	out := make([]blocker, 0, 16)
	for prov, m := range r.rows {
		// The gap this rail's rows impose, expressed in the REQUESTER's units: a
		// foreign row with a wider tolerance (after rescaling its tol into the
		// requester's precision) dictates the gap, otherwise the requester's own
		// MinGap applies.
		rowPrm := r.params[prov]
		tol := prm.Tol
		if prov != reqP {
			if t := rescaleTolCeil(rowPrm.Tol, rowPrm.Decimals, prm.Decimals); t > tol {
				tol = t
			}
		}
		gap := int64(2*tol + 1)
		// COARSE-NEIGHBOUR SLACK (review fix). Receipt matching floors the paid
		// amount to the rail's own precision, so a band on a COARSER rail extends
		// one full coarse unit past its nominal top edge:
		// floor(X*10^b) <= u+tol  <=>  X < (u+tol+1)*10^-b. Against such a row the
		// exact disjointness requirement in the requester's units is
		// tolReq + tolRow' + 10^(a-b), and 2*max(tol)+1 alone falls short by up to
		// 10^(a-b)-1. Concrete ambiguity without this: requester at 3dp/tol 100,
		// foreign 2dp row 5.80/tol 10, candidate 6.005 (apparent distance 205 >=
		// 201): a 5.906 receipt floors to 5906 (inside 6.005's band) AND to 590
		// (inside 5.80's band) — one payment, two orders. Widening by
		// 10^(a-b)-1 restores gap >= tolReq + tolRow' + 10^(a-b) (since
		// 2*max >= tolReq + tolRow'). Same-precision rails (all shipped ones,
		// AMOUNT_DECIMALS=2 everywhere) take slack 0 and keep Python's exact
		// 2*tol+1; a FINER foreign row needs no slack (its floor into requester
		// units only ever overstates its distance on the unsafe side by <1
		// requester unit, and tolReq + tolRow' + 1 <= 2*max+1 already covers it).
		if prov != reqP && rowPrm.Decimals < prm.Decimals {
			gap += pow10Int64(prm.Decimals-rowPrm.Decimals) - 1
		}
		for k, res := range m {
			if prov == reqP && k == des {
				continue // own row never blocks its own (re-)reservation
			}
			dead := !res.Deadline.After(now) // deadline <= now
			if chatID != "" && res.ChatID == chatID && dead {
				continue // own dead session never blocks
			}
			// dead_at + USDT_BAND_EVICT_S <= now
			if evictStale && !res.Deadline.Add(r.evict).After(now) {
				continue // long-dead row yields under pressure
			}
			out = append(out, blocker{
				units:  rescaleUnits(res.Units, res.Decimals, prm.Decimals),
				minGap: gap,
			})
		}
	}
	return out
}

// Adopt re-inserts a reservation without picking a new amount — used to
// rehydrate the registry from persisted pending rows after a restart, so a
// customer can still submit a hash post-reboot and concurrent amounts stay
// non-overlapping (telegram_bot.py:9503 / 9959 rehydrate). Units/Decimals are
// recomputed from Amount at the provider's current precision, so a changed
// AMOUNT_DECIMALS env can never desync stored units from the string. A zero
// CreatedAt (a persisted row with no usable ts) starts the Sweep grace clock
// now — the safe direction, since it can only delay freeing a band, never free
// one early.
func (r *Registry) Adopt(res Reservation) error {
	if res.Des == "" {
		return fmt.Errorf("bands: Adopt needs a non-empty des")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	prm, ok := r.params[res.Provider]
	if !ok {
		return fmt.Errorf("bands: unknown provider %q", res.Provider)
	}
	u, err := AmountUnits(res.Amount, prm.Decimals)
	if err != nil {
		return err
	}
	res.Units, res.Decimals = u, prm.Decimals
	if res.CreatedAt.IsZero() {
		res.CreatedAt = r.now()
	}
	res.seq = r.stampSeq() // callers cannot forge an identity stamp
	r.rows[res.Provider][res.Des] = res
	return nil
}

// Get returns the live reservation for an order, if any. Rails call this on
// txid verify to learn the expected amount (telegram_bot.py:9685-9693: a missing
// row means "session amount gone" -> nothing to verify against).
func (r *Registry) Get(p Provider, des string) (Reservation, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	res, ok := r.rows[p][des]
	return res, ok
}

// Release frees a band explicitly — call it on settle, on refund, and when a
// deposit/pay flow is abandoned by the customer. Mirrors
// _drop_usdt_pending (telegram_bot.py:9808). Reports whether a row was removed.
func (r *Registry) Release(p Provider, des string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.rows[p][des]; !ok {
		return false
	}
	delete(r.rows[p], des)
	return true
}

// Len is the number of live rows for a provider (tests / metrics).
func (r *Registry) Len(p Provider) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.rows[p])
}

// OtherAmounts lists the unique amounts of every OTHER live session on the same
// provider — the `other_amounts` snapshot the verify path takes at
// telegram_bot.py:9689 and then feeds to RecvMatchesAnyBand as the claim-jack
// gate. Same-provider only (that is what Python compares), and sorted so the
// caller's audit lines are stable.
func (r *Registry) OtherAmounts(p Provider, des string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.rows[p]))
	for k, res := range r.rows[p] {
		if k == des {
			continue
		}
		out = append(out, res.Amount)
	}
	sort.Strings(out)
	return out
}

// Snapshot returns the order's own reservation AND the other live amounts on
// that rail in ONE locked read — the atomic snapshot Python takes before the
// (seconds-long) chain lookup (telegram_bot.py:9685-9689: `expected` and
// `other_amounts` come out of the same `with lock` block). Rails should prefer
// this over Get + OtherAmounts so the expected amount and the claim-jack gate
// cannot disagree about the registry's state. ok is false when the session's
// amount is gone (Python's ORDER_GONE branch at :9690-9693).
func (r *Registry) Snapshot(p Provider, des string) (res Reservation, otherAmounts []string, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	res, ok = r.rows[p][des]
	if !ok {
		return Reservation{}, nil, false
	}
	otherAmounts = make([]string, 0, len(r.rows[p]))
	for k, q := range r.rows[p] {
		if k == des {
			continue
		}
		otherAmounts = append(otherAmounts, q.Amount)
	}
	sort.Strings(otherAmounts)
	return res, otherAmounts, true
}

// RecvMatchesAnyOtherBand is OtherAmounts + RecvMatchesAnyBand in one locked
// step, so the gate cannot race a concurrent Reserve between the two calls.
// Same-provider only, matching what Python compares (telegram_bot.py:9689).
//
// An unknown provider is an error, not a false: this gate's false is the ACCEPT
// side of a money refusal, and a miswired provider constant must never be able
// to turn a claim-jack refusal into a credit.
func (r *Registry) RecvMatchesAnyOtherBand(p Provider, des string, recvUnits int64) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	prm, ok := r.params[p]
	if !ok {
		return false, fmt.Errorf("bands: unknown provider %q", p)
	}
	tol := int64(prm.Tol)
	for k, res := range r.rows[p] {
		if k == des {
			continue
		}
		if abs64(recvUnits-rescaleUnits(res.Units, res.Decimals, prm.Decimals)) <= tol {
			return true, nil
		}
	}
	return false, nil
}

// ResolveRedirect implements the SAME-CHAT SIBLING REDIRECT with its DEPTH-1
// guard (_usdt_own_sibling @9620, call sites @9648 and @9824-9832).
//
// The case it fixes (prod audit 2026-07-02): the customer's first order expired
// but its row keeps the band for the 2h grace; they re-order and get a NEW
// offset amount (6.21) — yet they had already sent the OLD amount (6.00). When
// they paste the hash on the newest screen it mismatches, and without this
// redirect they dead-end on "amount conflict, contact support". Rerouting the
// verify to their OWN sibling order is safe: ownership is identical, the
// hash-dedupe ledger still applies, and settling an expired sibling refunds to
// their wallet.
//
// `alreadyRedirected` is Python's `redirected` flag: a redirected verify NEVER
// redirects again. That depth-1 cap is load-bearing — two sessions whose bands
// each match the other's receipt would otherwise ping-pong forever.
//
// Cross-chat band matches are NOT siblings: they stay claim-jack rejects
// (telegram_bot.py:9624-9626).
//
// Sibling rows are matched regardless of their deadline — an EXPIRED sibling is
// the whole point of the feature. Where Python relies on dict insertion order,
// this picks the freshest deadline (des string as tie-break) so the choice is
// deterministic under Go's randomised map iteration.
func (r *Registry) ResolveRedirect(p Provider, chatID, des string, recvUnits int64, alreadyRedirected bool) (string, bool) {
	if alreadyRedirected { // depth-1 guard: never recurse deeper
		return "", false
	}
	if chatID == "" {
		return "", false // same-chat only; an unknown owner is never a sibling
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	prm, ok := r.params[p]
	if !ok {
		return "", false
	}
	tol := int64(prm.Tol)
	best, found := Reservation{}, false
	for k, res := range r.rows[p] {
		if k == des || res.ChatID != chatID {
			continue
		}
		if abs64(recvUnits-rescaleUnits(res.Units, res.Decimals, prm.Decimals)) > tol {
			continue
		}
		if !found || res.Deadline.After(best.Deadline) ||
			(res.Deadline.Equal(best.Deadline) && res.Des < best.Des) {
			best, found = res, true
		}
	}
	if !found {
		return "", false
	}
	return best.Des, true
}

// Sweep is the hard, band-freeing pass — _prune_usdt_pending
// (telegram_bot.py:9600-9623), run from the periodic expiry sweeper. It asks
// `keep` about every live row and DELETES the ones it does not vouch for,
// returning them so the caller can delete the matching persisted rows.
//
// `keep` is the whole rule, exactly as Python writes it inline, so the order
// status and the grace window stay in the caller's hands:
//
//	keep := func(res bands.Reservation) bool {
//	    o := store.GetOrder(res.Des)
//	    if o == nil { return false }                    // order VANISHED -> free the band now
//	    if o.Status == "reserved" { return true }       // still live -> keep
//	    return now.Sub(res.CreatedAt) < b.usdtPendingGrace  // finished but within grace
//	}
//
// b.usdtPendingGrace (not a bands.PendingGraceFromEnv-style package read) is
// USDT_PENDING_GRACE_S resolved through b.envInt/config.Config, the chain
// that actually sees a .env-only value in production (config.Load's doc
// comment) — internal/handlers/usdt.go's sweepUSDTBands is the real `keep`
// this package doc is illustrating.
//
// Note the grace is measured from res.CreatedAt (Python's row `ts`), NOT from
// the deadline — a row's band is held for USDT_PENDING_GRACE_S from when the
// session started, which is the window the Python contract specifies.
//
// `keep` runs WITHOUT r.mu held: it does a DB read per row
// (telegram_bot.py:9616 does the same outside the lock, taking the lock only for
// the pop at :9621), so the money-path allocation mutex is never held across
// I/O, and a `keep` that calls back into the Registry cannot deadlock. Rows are
// re-checked after re-locking, so a des released and re-reserved during the
// unlocked window keeps its NEW band. Idempotent.
//
// Note the division of labour: Sweep DELETES rows, while EvictAfter only makes a
// dead row stop blocking the picker under pressure without deleting it.
func (r *Registry) Sweep(keep func(res Reservation) bool) ([]Reservation, error) {
	if keep == nil {
		return nil, fmt.Errorf("bands: Sweep needs a keep func (it decides which bands survive)")
	}
	// Phase 1: snapshot every row under the lock.
	r.mu.Lock()
	candidates := make([]Reservation, 0, 16)
	for _, m := range r.rows {
		for _, res := range m {
			candidates = append(candidates, res)
		}
	}
	r.mu.Unlock()

	// Phase 2: ask the caller, unlocked (this is where the DB reads happen).
	condemned := make([]Reservation, 0, len(candidates))
	for _, res := range candidates {
		if !keep(res) {
			condemned = append(condemned, res)
		}
	}

	// Phase 3: delete under the lock, but only rows that are still the SAME row.
	// seq is the identity token: a des released and re-reserved while we were
	// unlocked carries a strictly higher seq and must survive the stale verdict.
	r.mu.Lock()
	dropped := make([]Reservation, 0, len(condemned))
	for _, res := range condemned {
		cur, ok := r.rows[res.Provider][res.Des]
		if !ok || cur.seq != res.seq {
			continue
		}
		delete(r.rows[res.Provider], res.Des)
		dropped = append(dropped, cur)
	}
	r.mu.Unlock()

	sort.Slice(dropped, func(i, j int) bool {
		if dropped[i].Provider != dropped[j].Provider {
			return dropped[i].Provider < dropped[j].Provider
		}
		return dropped[i].Des < dropped[j].Des
	})
	return dropped, nil
}
