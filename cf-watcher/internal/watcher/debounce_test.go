package watcher

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// TestDebounce_CoalescesBurstIntoSingleSignal covers SC-04: a burst of
// dirty signals arriving within the debounce window must collapse into
// exactly one downstream call, fired only after the window elapses with no
// further signals.
//
// Uses a short debounce window (well below the real 2s default) so the test
// runs fast; this must remain race-free under `go test -race` since the
// underlying timer is only ever touched by the Debounce goroutine.
func TestDebounce_CoalescesBurstIntoSingleSignal(t *testing.T) {
	const debounceWindow = 50 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dirty := make(chan struct{})
	var calls int32

	done := make(chan struct{})
	go func() {
		Debounce(ctx, dirty, debounceWindow, func() {
			atomic.AddInt32(&calls, 1)
		})
		close(done)
	}()

	// Burst of 3 dirty signals, each well within the debounce window of the
	// previous one — must coalesce into a single call.
	for i := 0; i < 3; i++ {
		dirty <- struct{}{}
		time.Sleep(debounceWindow / 5)
	}

	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("fn called %d time(s) before the debounce window elapsed, want 0", got)
	}

	// Wait for the window to elapse (plus margin) with no further signals.
	time.Sleep(debounceWindow * 3)

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("fn called %d time(s) after burst settled, want exactly 1", got)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Debounce did not return after ctx cancellation")
	}
}

// TestDebounce_SeparateBurstsProduceSeparateCalls verifies that once a call
// fires, a NEW burst after the window starts a fresh coalescing cycle
// (i.e. Debounce doesn't get stuck after firing once).
func TestDebounce_SeparateBurstsProduceSeparateCalls(t *testing.T) {
	const debounceWindow = 30 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dirty := make(chan struct{})
	var calls int32

	done := make(chan struct{})
	go func() {
		Debounce(ctx, dirty, debounceWindow, func() {
			atomic.AddInt32(&calls, 1)
		})
		close(done)
	}()

	dirty <- struct{}{}
	time.Sleep(debounceWindow * 3)
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("after first burst: fn called %d time(s), want 1", got)
	}

	dirty <- struct{}{}
	time.Sleep(debounceWindow * 3)
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("after second burst: fn called %d time(s), want 2", got)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Debounce did not return after ctx cancellation")
	}
}

// TestDebounce_CtxCancelDuringWindowSuppressesCall verifies that canceling
// ctx before the debounce window elapses stops Debounce without firing fn.
func TestDebounce_CtxCancelDuringWindowSuppressesCall(t *testing.T) {
	const debounceWindow = 100 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())

	dirty := make(chan struct{})
	var calls int32

	done := make(chan struct{})
	go func() {
		Debounce(ctx, dirty, debounceWindow, func() {
			atomic.AddInt32(&calls, 1)
		})
		close(done)
	}()

	dirty <- struct{}{}
	cancel() // cancel well before debounceWindow elapses

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Debounce did not return promptly after ctx cancellation")
	}

	// Give the (already-stopped) timer no chance to fire something we'd miss.
	time.Sleep(debounceWindow + 20*time.Millisecond)

	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("fn called %d time(s) after ctx cancellation mid-window, want 0", got)
	}
}
