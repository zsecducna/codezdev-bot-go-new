package handlers

import (
	"context"
	"sync"
)

// workerRegistry tracks the daemon goroutines the handlers spawn (today: the
// per-order expiry timers, order_expiry_worker @8974) so Shutdown can drain
// them on exit. Python leaked its daemon threads at process death; Go joins
// them for a clean shutdown (handlers-go.md §3/§8).
type workerRegistry struct {
	mu      sync.Mutex
	wg      sync.WaitGroup
	stop    chan struct{}
	stopped bool
}

// newWorkerRegistry builds an empty, running registry.
func newWorkerRegistry() *workerRegistry {
	return &workerRegistry{stop: make(chan struct{})}
}

// Go runs fn in a tracked goroutine. fn receives the registry's stop channel so
// a long sleep (the 600s expiry wait) can be cut short on shutdown. A Go call
// after Shutdown is a no-op — the goroutine never starts, so a late spawn can't
// outlive the process.
func (w *workerRegistry) Go(fn func(stop <-chan struct{})) { w.TryGo(fn) }

// TryGo is Go with a reported outcome: false means the registry was already shut
// down and fn was NOT started. Callers that must not silently drop work use this
// to fail loudly instead — the PayPal webhook is the motivating case: acking a
// captured payment 200 and then dropping it would suppress PayPal's retry and
// lose the settlement for good, so it answers 503 when this returns false.
func (w *workerRegistry) TryGo(fn func(stop <-chan struct{})) bool {
	w.mu.Lock()
	if w.stopped {
		w.mu.Unlock()
		return false
	}
	w.wg.Add(1)
	w.mu.Unlock()
	go func() {
		defer w.wg.Done()
		fn(w.stop)
	}()
	return true
}

// Shutdown signals every tracked goroutine to stop and waits for them to drain.
// Idempotent.
func (w *workerRegistry) Shutdown() {
	w.mu.Lock()
	if !w.stopped {
		w.stopped = true
		close(w.stop)
	}
	w.mu.Unlock()
	w.wg.Wait()
}

// runOnStoppableCtx (H3 fix, originally landed for the AWS IDC provisioning
// feature — since removed — now shared by every background admin run that
// spawns via workerRegistry.Go/TryGo) runs fn on a FRESH cancellable context,
// wiring the registry's stop channel into that context's cancel — the same
// shape otp.go establishes for background work that must react to Shutdown()
// rather than block. Running fn on cc.Ctx directly would be wrong: cc.Ctx
// traces back to main.go's rootCtx (context.Background(), passed unchanged to
// every update — cmd/bot/main.go's "bot.Process(rootCtx, u)"), which is NEVER
// cancelled, so Shutdown() would have no way to interrupt an in-flight run and
// would block on it for up to its own worst-case timeout instead of draining.
func runOnStoppableCtx(stop <-chan struct{}, fn func(ctx context.Context)) {
	fctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-stop:
			cancel()
		case <-done:
		}
	}()
	fn(fctx)
}
