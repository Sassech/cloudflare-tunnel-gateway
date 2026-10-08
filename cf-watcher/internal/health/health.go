// Package health exposes cf-watcher's live operational status over HTTP: a
// liveness endpoint (/healthz) and a small set of Prometheus text-format
// metrics (/metrics). It has no dependency on any other internal package so
// it can be wired into main without import cycles.
package health

import (
	"fmt"
	"net/http"
	"sync"
	"time"
)

// Status is a thread-safe snapshot of cf-watcher's most recent sync
// attempt, updated after every startup reconcile and every debounced sync
// loop iteration. The zero value is ready to use (and not yet ready — see
// MarkReady).
type Status struct {
	mu sync.Mutex

	// ready is false until MarkReady is called, i.e. until the tunnel has
	// been resolved and its connector token written for cloudflared.
	ready bool

	containersTracked int
	dnsEntriesTracked int
	syncTotal         int64
	syncErrorsTotal   int64
	lastSyncSuccess   bool
	lastSyncAt        time.Time
}

// NewStatus returns a ready-to-use Status with no recorded syncs yet.
func NewStatus() *Status {
	return &Status{}
}

// MarkReady flags cf-watcher as ready: the tunnel is resolved and the
// connector token file is written. /healthz reports 503 until then, so
// Compose's `depends_on: condition: service_healthy` holds cloudflared back
// until it has a token to read. Safe to call on a nil *Status (no-op).
func (s *Status) MarkReady() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ready = true
}

// Ready reports whether MarkReady has been called. A nil *Status is always
// ready (health tracking disabled).
func (s *Status) Ready() bool {
	if s == nil {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ready
}

// RecordSync records the outcome of one sync attempt (a startup reconcile
// or a debounced sync loop iteration).
//
// Safe to call on a nil *Status, which is treated as a no-op — callers that
// don't need health tracking (e.g. tests) can pass nil instead of
// constructing a real Status.
func (s *Status) RecordSync(containersTracked, dnsEntriesTracked int, success bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	s.containersTracked = containersTracked
	s.dnsEntriesTracked = dnsEntriesTracked
	s.syncTotal++
	if !success {
		s.syncErrorsTotal++
	}
	s.lastSyncSuccess = success
	s.lastSyncAt = time.Now()
}

// snapshotData is an immutable copy of Status taken under lock, safe to read
// without further synchronization.
type snapshotData struct {
	ready             bool
	containersTracked int
	dnsEntriesTracked int
	syncTotal         int64
	syncErrorsTotal   int64
	lastSyncSuccess   bool
	lastSyncAt        time.Time
}

func (s *Status) snapshot() snapshotData {
	if s == nil {
		return snapshotData{ready: true}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return snapshotData{
		ready:             s.ready,
		containersTracked: s.containersTracked,
		dnsEntriesTracked: s.dnsEntriesTracked,
		syncTotal:         s.syncTotal,
		syncErrorsTotal:   s.syncErrorsTotal,
		lastSyncSuccess:   s.lastSyncSuccess,
		lastSyncAt:        s.lastSyncAt,
	}
}

// Handler returns an http.Handler serving:
//
//	GET /healthz — 503 until MarkReady (tunnel token not yet written);
//	               then 200 if the most recent sync attempt succeeded (or
//	               none has run yet), 503 if it failed.
//	GET /metrics — Prometheus text-format counters/gauges.
func Handler(status *Status) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		snap := status.snapshot()
		if !snap.ready {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintln(w, "starting: tunnel token not written yet")
			return
		}
		if snap.syncTotal > 0 && !snap.lastSyncSuccess {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintln(w, "unhealthy: last sync failed")
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})

	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		snap := status.snapshot()
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")

		lastSuccess := 0
		if snap.lastSyncSuccess {
			lastSuccess = 1
		}
		ready := 0
		if snap.ready {
			ready = 1
		}
		fmt.Fprintf(w, "cf_watcher_ready %d\n", ready)
		fmt.Fprintf(w, "cf_watcher_containers_tracked %d\n", snap.containersTracked)
		fmt.Fprintf(w, "cf_watcher_dns_entries_tracked %d\n", snap.dnsEntriesTracked)
		fmt.Fprintf(w, "cf_watcher_sync_total %d\n", snap.syncTotal)
		fmt.Fprintf(w, "cf_watcher_sync_errors_total %d\n", snap.syncErrorsTotal)
		fmt.Fprintf(w, "cf_watcher_last_sync_success %d\n", lastSuccess)
		if !snap.lastSyncAt.IsZero() {
			fmt.Fprintf(w, "cf_watcher_last_sync_timestamp_seconds %d\n", snap.lastSyncAt.Unix())
		}
	})

	return mux
}
