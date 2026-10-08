package main

import (
	"crypto/subtle"
	"fmt"
	"net/http"
	"sync"
	"time"

	"cf-watcher/internal/config"
	"cf-watcher/internal/health"
	"cf-watcher/internal/logging"
	"cf-watcher/internal/state"
	"cf-watcher/internal/watcher"
)

// defaultMinReloadInterval is the minimum time between two accepted
// /reload calls in production. An authorized caller hammering the endpoint
// can still exhaust cfapi.MaxAttempts' retry/backoff budget shared with the
// normal sync loop, so successful reloads are throttled independently of
// token validation.
const defaultMinReloadInterval = 10 * time.Second

// reloadHandler returns an http.Handler serving POST /reload: it fully
// resyncs desired state from the live Docker daemon and issues a fresh
// tunnel + DNS reconcile, without restarting the process.
//
// This exists because the live event watcher only reacts to container
// lifecycle transitions (start/stop/die/kill) — a label changed on an
// already-running container (e.g. via `docker update` or an in-place
// Compose label edit) produces no event, so cf-watcher would otherwise
// never notice until that container restarts. POST /reload is the manual
// escape hatch for that case.
//
// Secure by default: if cfg.ReloadToken is empty, the endpoint is disabled
// entirely (404) rather than left open — RELOAD_TOKEN must be explicitly
// set to opt in, since /reload triggers real Cloudflare API calls and
// shares the sync loop's rate-limit budget. When enabled, the caller must
// send a matching X-Reload-Token header.
//
// Successful reloads are additionally throttled to at most one per
// minReloadInterval — an authorized-but-noisy caller must not be able to
// exhaust the shared Cloudflare API retry budget by hammering this
// endpoint. minReloadInterval <= 0 disables throttling (used by tests).
func reloadHandler(
	cfg *config.Config,
	docker watcher.DockerClient,
	cf TunnelClient,
	dnsClient DNSSyncer,
	store *state.Store,
	status *health.Status,
	minReloadInterval time.Duration,
) http.Handler {
	var mu sync.Mutex
	var lastReload time.Time

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if cfg.ReloadToken == "" {
			http.NotFound(w, r)
			return
		}

		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			w.WriteHeader(http.StatusMethodNotAllowed)
			fmt.Fprintln(w, "only POST is supported")
			return
		}

		if !validReloadToken(cfg.ReloadToken, r.Header.Get("X-Reload-Token")) {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprintln(w, "missing or invalid X-Reload-Token header")
			return
		}

		mu.Lock()
		if wait := minReloadInterval - time.Since(lastReload); !lastReload.IsZero() && wait > 0 {
			mu.Unlock()
			w.Header().Set("Retry-After", fmt.Sprintf("%.0f", wait.Seconds()))
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprintf(w, "reload rate-limited — try again in %.0fs\n", wait.Seconds())
			return
		}
		lastReload = time.Now()
		mu.Unlock()

		logging.Info("reload: triggered via POST /reload")

		// Clear first so containers that lost their cf.* labels (or were
		// removed) while still running are dropped — this is a full
		// resync, not a merge.
		store.Clear()

		found, err := startupReconcile(r.Context(), cfg, docker, cf, dnsClient, store, status)
		if err != nil {
			logging.Errorf("reload: failed — %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprintf(w, "reload failed: %v\n", err)
			return
		}

		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "reload complete — %d labeled container(s) found\n", found)
	})
}

// validReloadToken compares got against want using a constant-time
// comparison, so response timing can't be used to brute-force the token. An
// empty got is always rejected.
func validReloadToken(want, got string) bool {
	if got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(want), []byte(got)) == 1
}
