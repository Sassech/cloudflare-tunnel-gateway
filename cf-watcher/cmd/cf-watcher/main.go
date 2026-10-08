// Command cf-watcher watches Docker container lifecycle events and keeps a
// Cloudflare Tunnel's ingress configuration (and optionally DNS CNAME
// records) in sync with cf.* labeled containers.
//
// Startup sequence:
//  1. Load config from environment (+ optional .env).
//  2. Connect to the Docker daemon — fail fast if the socket is unavailable.
//  3. provisionTunnel: reuse/create this host's tunnel (or use CF_TUNNEL_ID)
//     and write its connector token for cloudflared; only then is /healthz
//     allowed to report healthy.
//  4. startupReconcile: assert tunnel mode, scan running containers, issue
//     one reconcile PUT.
//  5. Run the live event watcher and the debounced sync loop concurrently
//     until SIGINT/SIGTERM (graceful shutdown).
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	dockerclient "github.com/docker/docker/client"

	"cf-watcher/internal/cfapi"
	"cf-watcher/internal/config"
	"cf-watcher/internal/dns"
	"cf-watcher/internal/health"
	"cf-watcher/internal/logging"
	"cf-watcher/internal/state"
	"cf-watcher/internal/watcher"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("CRITICAL: %v", err)
	}
	logging.SetLevel(logging.ParseLevel(cfg.LogLevel))

	logging.Infof(
		"cf-watcher starting account_id=%s tunnel_name=%s tunnel_id=%s zone_id=%s docker_network=%s debounce=%.1fs",
		cfg.CFAccountID, cfg.CFTunnelName, valueOrNotSet(cfg.CFTunnelID), valueOrNotSet(cfg.CFZoneID), cfg.DockerNetwork, cfg.DebounceSeconds,
	)

	status := health.NewStatus()

	// docker.sock unavailable is unrecoverable — fail fast.
	dockerClient, err := dockerclient.NewClientWithOpts(dockerclient.FromEnv, dockerclient.WithAPIVersionNegotiation())
	if err != nil {
		log.Fatalf("CRITICAL: cannot create Docker client — is /var/run/docker.sock mounted? %v", err)
	}
	defer dockerClient.Close()

	if _, err := dockerClient.Ping(context.Background()); err != nil {
		log.Fatalf("CRITICAL: cannot connect to Docker socket — is /var/run/docker.sock mounted? %v", err)
	}

	cfClient := cfapi.NewClient(cfg)
	dnsClient := dns.NewClient(cfg)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, cfg, dockerClient, cfClient, dnsClient, status); err != nil {
		log.Fatalf("CRITICAL: %v", err)
	}
}

func valueOrNotSet(v string) string {
	if v == "" {
		return "(not set)"
	}
	return v
}

// startHealthServer starts the /healthz, /metrics, and /reload HTTP server
// in the background, unless cfg.HealthAddr is "off" (case-insensitive). A
// server failure is logged, never fatal — it must not prevent the core
// sync loop from running. The server is gracefully shut down when ctx is
// canceled, so it never outlives run() (e.g. across successive test runs
// reusing the same address, or leaking a listener past SIGTERM).
func startHealthServer(ctx context.Context, cfg *config.Config, docker watcher.DockerClient, cf TunnelClient, dnsClient DNSSyncer, store *state.Store, status *health.Status) {
	if strings.EqualFold(cfg.HealthAddr, "off") {
		logging.Info("health: server disabled (HEALTH_ADDR=off)")
		return
	}

	healthH := health.Handler(status)
	mux := http.NewServeMux()
	mux.Handle("/healthz", healthH)
	mux.Handle("/metrics", healthH)
	mux.Handle("/reload", reloadHandler(cfg, docker, cf, dnsClient, store, status, defaultMinReloadInterval))

	srv := &http.Server{Addr: cfg.HealthAddr, Handler: mux}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logging.Errorf("health: graceful shutdown failed — %v", err)
		}
	}()

	go func() {
		logging.Infof("health: listening on %s (/healthz, /metrics)", cfg.HealthAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logging.Errorf("health: server stopped — %v", err)
		}
	}()
}

// run wires the full cf-watcher runtime: tunnel provisioning, startup
// reconciliation, then the live Docker event watcher and the debounced sync
// loop, running until ctx is canceled.
//
// Factored out of main() (and depending only on the narrow watcher.DockerClient
// / TunnelManager / DNSSyncer interfaces) so it can be exercised by tests
// without a real Docker daemon, a real Cloudflare API, or os.Exit.
func run(ctx context.Context, cfg *config.Config, docker watcher.DockerClient, cf TunnelManager, dnsClient DNSSyncer, status *health.Status) error {
	tunnelID, err := provisionTunnel(ctx, cfg, cf)
	if err != nil {
		return fmt.Errorf("tunnel provisioning failed: %w", err)
	}

	// From here on, every consumer (startup reconcile, sync loop, /reload)
	// sees the resolved tunnel ID via this private copy — the caller's cfg
	// is never mutated, and the copy is complete before any goroutine that
	// reads it is started.
	resolved := *cfg
	resolved.CFTunnelID = tunnelID
	cfg = &resolved

	// The token is on disk: cloudflared may start. Must precede the health
	// server so /healthz can never report healthy without a token.
	status.MarkReady()

	store := state.NewStore()

	startHealthServer(ctx, cfg, docker, cf, dnsClient, store, status)

	if _, err := startupReconcile(ctx, cfg, docker, cf, dnsClient, store, status); err != nil {
		return fmt.Errorf("startup reconcile failed: %w", err)
	}

	// dirty must be buffered (capacity 1) — watcher.signalDirty sends
	// non-blocking and relies on this.
	dirty := make(chan struct{}, 1)
	debounceWindow := time.Duration(cfg.DebounceSeconds * float64(time.Second))

	var wg sync.WaitGroup
	wg.Add(2)

	var watchErr error
	go func() {
		defer wg.Done()
		watchErr = watcher.Watch(ctx, docker, store, cfg.DockerNetwork, dirty)
	}()

	go func() {
		defer wg.Done()
		logging.Infof("sync_loop: started (debounce=%.1fs)", cfg.DebounceSeconds)
		watcher.Debounce(ctx, dirty, debounceWindow, func() {
			syncTunnelAndDNS(ctx, cfg, store, cf, dnsClient, status)
		})
	}()

	wg.Wait()

	if watchErr != nil && !errors.Is(watchErr, context.Canceled) {
		return fmt.Errorf("docker event watcher stopped: %w", watchErr)
	}
	return nil
}

// syncTunnelAndDNS pushes the full current desired state to the Cloudflare
// Tunnel API, then reconciles DNS CNAMEs against the desired cf.dns=true set.
//
// If the tunnel PUT fails after all retries, this logs CRITICAL and returns
// WITHOUT propagating an error — cf-watcher keeps listening to Docker events
// and will retry on the next dirty signal.
func syncTunnelAndDNS(ctx context.Context, cfg *config.Config, store *state.Store, cf TunnelClient, dnsClient DNSSyncer, status *health.Status) {
	ingress := store.BuildIngress()
	ruleCount := len(ingress) - 1 // -1 for the catch-all

	if err := cf.PutTunnelConfig(ctx, ingress); err != nil {
		log.Printf("CRITICAL: sync_loop: failed to update tunnel config after retries — %v", err)
		status.RecordSync(ruleCount, len(store.DNSEntries()), false)
		return
	}
	logging.Infof("sync_loop: tunnel config updated — %d ingress rule(s)", ruleCount)

	if cfg.CFZoneID == "" {
		status.RecordSync(ruleCount, 0, true)
		return
	}

	desired := make(map[string]bool)
	for _, e := range store.DNSEntries() {
		desired[e.Hostname] = true
	}
	managed := dnsClient.TrackedHostnames()

	for hostname := range desired {
		if _, ok := managed[hostname]; ok {
			continue
		}
		if _, err := dnsClient.CreateCNAME(ctx, hostname, cfg.CFTunnelID); err != nil {
			logging.Errorf("sync_loop: DNS create failed for hostname=%s — %v", hostname, err)
		}
	}

	for hostname := range managed {
		if desired[hostname] {
			continue
		}
		if err := dnsClient.DeleteCNAME(ctx, hostname); err != nil {
			logging.Errorf("sync_loop: DNS delete failed for hostname=%s — %v", hostname, err)
		}
	}

	status.RecordSync(ruleCount, len(desired), true)
}
