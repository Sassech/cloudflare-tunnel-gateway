package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"

	"cf-watcher/internal/config"
	"cf-watcher/internal/state"
	"cf-watcher/internal/watcher"
)

// TunnelClient is the subset of *cfapi.Client's behavior startupReconcile
// and the sync loop depend on. *cfapi.Client satisfies this directly; tests
// supply a fake to avoid a real Cloudflare API dependency (FR-9).
type TunnelClient interface {
	AssertConfigSrc(ctx context.Context) error
	PutTunnelConfig(ctx context.Context, ingress []map[string]any) error
}

// DNSSyncer is the subset of *dns.Client's behavior startupReconcile and the
// sync loop depend on. *dns.Client satisfies this directly.
type DNSSyncer interface {
	CreateCNAME(ctx context.Context, hostname, tunnelID string) (string, error)
	DeleteCNAME(ctx context.Context, hostname string) error
	TrackedHostnames() map[string]string
}

// errZoneIDRequired is returned when a running container uses cf.dns=true
// but CF_ZONE_ID is not configured — REQ-5.5/SC-08.
var errZoneIDRequired = errors.New("CF_ZONE_ID is required when cf.dns=true is used on any container")

// startupReconcile runs the boot-time reconciliation sequence, once, before
// the live event watcher starts:
//
//  1. Assert the tunnel is in remote-managed mode (REQ-4.1) — aborts on
//     failure (e.g. config_src=local, SC-10).
//  2. List all running containers (REQ-4.2).
//  3. Extract cf.* labels and populate store (REQ-4.3), warning on any
//     labeled container not on dockerNetwork (REQ-8.5).
//  4. If any entry has DNS=true and CF_ZONE_ID is unset, abort BEFORE
//     issuing any PUT (REQ-5.5, SC-08).
//  5. Issue a single reconcile PUT with the full desired ingress (REQ-4.4).
//  6. Create CNAMEs for any DNS=true entries found (REQ-5.1/5.2).
//
// Returns the number of labeled containers found (REQ-4.5), or an error if
// any step fails. The caller (run, then main) is responsible for treating a
// non-nil error as fatal.
func startupReconcile(
	ctx context.Context,
	cfg *config.Config,
	docker watcher.DockerClient,
	cf TunnelClient,
	dnsClient DNSSyncer,
	store *state.Store,
) (int, error) {
	log.Print("INFO: startup_reconcile: asserting tunnel config_src")
	if err := cf.AssertConfigSrc(ctx); err != nil {
		return 0, fmt.Errorf("startup reconcile: %w", err)
	}

	log.Print("INFO: startup_reconcile: scanning running containers")
	containers, err := docker.ContainerList(ctx, containerListOptions())
	if err != nil {
		return 0, fmt.Errorf("startup reconcile: cannot list Docker containers: %w", err)
	}

	found := 0
	for _, c := range containers {
		name := containerDisplayName(c)

		entry, ok := watcher.ExtractDesiredState(c.ID, name, c.Labels)
		if !ok {
			continue
		}

		if c.NetworkSettings != nil {
			watcher.CheckNetworkMembership(name, cfg.DockerNetwork, c.NetworkSettings.Networks)
		}

		store.Add(*entry)
		found++
		log.Printf(
			"INFO: startup_reconcile: labeled container found name=%s hostname=%s port=%d",
			name, entry.Hostname, entry.Port,
		)
	}

	log.Printf("INFO: startup_reconcile: %d labeled container(s) found — issuing reconcile PUT", found)

	// REQ-5.5: if any container uses cf.dns=true, CF_ZONE_ID must be set —
	// this MUST be checked before the first PUT (SC-08).
	dnsEntries := store.DNSEntries()
	if len(dnsEntries) > 0 && cfg.CFZoneID == "" {
		log.Print("ERROR: CF_ZONE_ID is required when cf.dns=true is used on any container")
		return found, errZoneIDRequired
	}

	ingress := store.BuildIngress()
	if err := cf.PutTunnelConfig(ctx, ingress); err != nil {
		return found, fmt.Errorf("startup reconcile: PutTunnelConfig: %w", err)
	}

	// REQ-5.1/5.2: idempotent DNS sync for any startup entries with
	// cf.dns=true. Errors here are logged, not fatal — the tunnel ingress
	// is already correctly applied; a DNS create failure will be retried
	// on the next sync cycle once the container's entry is re-diffed.
	if cfg.CFZoneID != "" {
		for _, entry := range dnsEntries {
			if _, err := dnsClient.CreateCNAME(ctx, entry.Hostname, cfg.CFTunnelID); err != nil {
				log.Printf("ERROR: startup_reconcile: DNS create failed for hostname=%s — %v", entry.Hostname, err)
			}
		}
	}

	ruleCount := len(ingress) - 1 // -1 for the catch-all
	log.Printf("INFO: startup_reconcile: complete — %d ingress rule(s) applied", ruleCount)

	return found, nil
}

// containerDisplayName strips the leading "/" from Docker container names.
func containerDisplayName(c types.Container) string {
	if len(c.Names) == 0 {
		return c.ID
	}
	return strings.TrimPrefix(c.Names[0], "/")
}

// containerListOptions builds the container.ListOptions for
// startupReconcile's scan — only running containers are relevant (REQ-4.2);
// container.ListOptions{} defaults to All=false, which already means
// "running only", so no explicit filter is required beyond making the
// call. Kept as its own function so tests and future filters have a single
// place to change.
func containerListOptions() container.ListOptions {
	return container.ListOptions{
		Filters: filters.NewArgs(),
	}
}
