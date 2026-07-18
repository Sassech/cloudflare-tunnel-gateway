// Command cf-watcher watches Docker container lifecycle events and keeps a
// Cloudflare Tunnel's ingress configuration (and optionally DNS CNAME
// records) in sync with cf.* labeled containers.
//
// Startup sequence:
//  1. Load config from environment (+ optional .env).
//  2. Connect to the Docker daemon — fail fast if the socket is unavailable.
//  3. startupReconcile: assert tunnel mode, scan running containers, issue
//     one reconcile PUT.
//  4. Run the live event watcher and the debounced sync loop concurrently
//     until SIGINT/SIGTERM (graceful shutdown).
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	dockerclient "github.com/docker/docker/client"

	"cf-watcher/internal/cfapi"
	"cf-watcher/internal/config"
	"cf-watcher/internal/dns"
	"cf-watcher/internal/state"
	"cf-watcher/internal/watcher"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("CRITICAL: %v", err)
	}

	log.Printf(
		"INFO: cf-watcher starting account_id=%s tunnel_id=%s zone_id=%s docker_network=%s debounce=%.1fs",
		cfg.CFAccountID, cfg.CFTunnelID, zoneIDOrNotSet(cfg.CFZoneID), cfg.DockerNetwork, cfg.DebounceSeconds,
	)

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

	if err := run(ctx, cfg, dockerClient, cfClient, dnsClient); err != nil {
		log.Fatalf("CRITICAL: %v", err)
	}
}

func zoneIDOrNotSet(zoneID string) string {
	if zoneID == "" {
		return "(not set)"
	}
	return zoneID
}

// run wires the full cf-watcher runtime: startup reconciliation, then the
// live Docker event watcher and the debounced sync loop, running until ctx
// is canceled.
//
// Factored out of main() (and depending only on the narrow watcher.DockerClient
// / TunnelClient / DNSSyncer interfaces) so it can be exercised by tests
// without a real Docker daemon, a real Cloudflare API, or os.Exit.
func run(ctx context.Context, cfg *config.Config, docker watcher.DockerClient, cf TunnelClient, dnsClient DNSSyncer) error {
	store := state.NewStore()

	if _, err := startupReconcile(ctx, cfg, docker, cf, dnsClient, store); err != nil {
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
		log.Printf("INFO: sync_loop: started (debounce=%.1fs)", cfg.DebounceSeconds)
		watcher.Debounce(ctx, dirty, debounceWindow, func() {
			syncTunnelAndDNS(ctx, cfg, store, cf, dnsClient)
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
func syncTunnelAndDNS(ctx context.Context, cfg *config.Config, store *state.Store, cf TunnelClient, dnsClient DNSSyncer) {
	ingress := store.BuildIngress()

	if err := cf.PutTunnelConfig(ctx, ingress); err != nil {
		log.Printf("CRITICAL: sync_loop: failed to update tunnel config after retries — %v", err)
		return
	}
	log.Printf("INFO: sync_loop: tunnel config updated — %d ingress rule(s)", len(ingress)-1)

	if cfg.CFZoneID == "" {
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
			log.Printf("ERROR: sync_loop: DNS create failed for hostname=%s — %v", hostname, err)
		}
	}

	for hostname := range managed {
		if desired[hostname] {
			continue
		}
		if err := dnsClient.DeleteCNAME(ctx, hostname); err != nil {
			log.Printf("ERROR: sync_loop: DNS delete failed for hostname=%s — %v", hostname, err)
		}
	}
}
