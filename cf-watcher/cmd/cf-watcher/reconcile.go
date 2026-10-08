package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"

	"cf-watcher/internal/config"
	"cf-watcher/internal/health"
	"cf-watcher/internal/logging"
	"cf-watcher/internal/state"
	"cf-watcher/internal/watcher"
)

// TunnelClient is the subset of *cfapi.Client's behavior startupReconcile
// and the sync loop depend on. *cfapi.Client satisfies this directly; tests
// supply a fake to avoid a real Cloudflare API dependency.
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
	ListManagedRecords(ctx context.Context, tunnelID string) (map[string]string, error)
	AdoptRecord(hostname, recordID string)
}

// errZoneIDRequired is returned when a running container uses cf.dns=true
// but CF_ZONE_ID is not configured.
var errZoneIDRequired = errors.New("CF_ZONE_ID is required when cf.dns=true is used on any container")

// startupReconcile runs the boot-time reconciliation sequence, once, before
// the live event watcher starts:
//
//  1. Assert the tunnel is in remote-managed mode — aborts on failure (e.g.
//     config_src=local).
//  2. List all running containers.
//  3. Extract cf.* labels and populate store, warning on any labeled
//     container not on dockerNetwork.
//  4. If any entry has DNS=true and CF_ZONE_ID is unset, abort BEFORE
//     issuing any PUT.
//  5. Issue a single reconcile PUT with the full desired ingress.
//  6. Adopt and reconcile any DNS records left over from a previous process
//     session, then create CNAMEs for any DNS=true entries found.
//
// Returns the number of labeled containers found, or an error if any step
// fails. The caller (run, then main) is responsible for treating a non-nil
// error as fatal.
func startupReconcile(
	ctx context.Context,
	cfg *config.Config,
	docker watcher.DockerClient,
	cf TunnelClient,
	dnsClient DNSSyncer,
	store *state.Store,
	status *health.Status,
) (int, error) {
	logging.Info("startup_reconcile: asserting tunnel config_src")
	if err := cf.AssertConfigSrc(ctx); err != nil {
		status.RecordSync(0, 0, false)
		return 0, fmt.Errorf("startup reconcile: %w", err)
	}

	logging.Info("startup_reconcile: scanning running containers")
	found, err := scanLabeledContainers(ctx, docker, cfg, store)
	if err != nil {
		status.RecordSync(0, 0, false)
		return 0, err
	}

	logging.Infof("startup_reconcile: %d labeled container(s) found — issuing reconcile PUT", found)

	// If any container uses cf.dns=true, CF_ZONE_ID must be set — this MUST
	// be checked before the first PUT.
	dnsEntries := store.DNSEntries()
	if len(dnsEntries) > 0 && cfg.CFZoneID == "" {
		logging.Error("CF_ZONE_ID is required when cf.dns=true is used on any container")
		status.RecordSync(found, len(dnsEntries), false)
		return found, errZoneIDRequired
	}

	ingress := store.BuildIngress()
	if err := cf.PutTunnelConfig(ctx, ingress); err != nil {
		status.RecordSync(found, len(dnsEntries), false)
		return found, fmt.Errorf("startup reconcile: PutTunnelConfig: %w", err)
	}

	// Idempotent DNS sync for any startup entries with cf.dns=true. Errors
	// here are logged, not fatal — the tunnel ingress is already correctly
	// applied; a DNS create failure will be retried on the next sync cycle
	// once the container's entry is re-diffed.
	if cfg.CFZoneID != "" {
		reconcileOrphanedDNS(ctx, dnsClient, cfg.CFTunnelID, dnsEntries)

		for _, entry := range dnsEntries {
			if _, err := dnsClient.CreateCNAME(ctx, entry.Hostname, cfg.CFTunnelID); err != nil {
				logging.Errorf("startup_reconcile: DNS create failed for hostname=%s — %v", entry.Hostname, err)
			}
		}
	}

	ruleCount := len(ingress) - 1 // -1 for the catch-all
	logging.Infof("startup_reconcile: complete — %d ingress rule(s) applied", ruleCount)

	status.RecordSync(found, len(dnsEntries), true)
	return found, nil
}

// scanLabeledContainers lists all running containers, extracts cf.* labels
// into the store (warning on any labeled container not on dockerNetwork),
// and returns the number of labeled containers found.
func scanLabeledContainers(ctx context.Context, docker watcher.DockerClient, cfg *config.Config, store *state.Store) (int, error) {
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
		logging.Infof(
			"startup_reconcile: labeled container found name=%s hostname=%s port=%d",
			name, entry.Hostname, entry.Port,
		)
	}
	return found, nil
}

// reconcileOrphanedDNS adopts every CNAME currently routed through this
// tunnel into the session's tracked-records map, then deletes any that no
// longer correspond to a running cf.dns=true container.
//
// This recovers from a process restart: Client.tracked always starts empty,
// so without this step a CNAME left over from a container removed while
// cf-watcher was down would never be cleaned up.
func reconcileOrphanedDNS(ctx context.Context, dnsClient DNSSyncer, tunnelID string, desiredEntries []state.Entry) {
	managed, err := dnsClient.ListManagedRecords(ctx, tunnelID)
	if err != nil {
		logging.Warningf("startup_reconcile: failed to list existing tunnel-managed DNS records — %v", err)
		return
	}

	desired := make(map[string]bool, len(desiredEntries))
	for _, e := range desiredEntries {
		desired[e.Hostname] = true
	}

	for hostname, recordID := range managed {
		dnsClient.AdoptRecord(hostname, recordID)
		if desired[hostname] {
			continue
		}
		if err := dnsClient.DeleteCNAME(ctx, hostname); err != nil {
			logging.Errorf("startup_reconcile: failed to delete orphaned CNAME hostname=%s — %v", hostname, err)
			continue
		}
		logging.Infof("startup_reconcile: deleted orphaned CNAME hostname=%s record_id=%s (no active container)", hostname, recordID)
	}
}

// containerDisplayName strips the leading "/" from Docker container names.
func containerDisplayName(c types.Container) string {
	if len(c.Names) == 0 {
		return c.ID
	}
	return strings.TrimPrefix(c.Names[0], "/")
}

// containerListOptions builds the container.ListOptions for
// startupReconcile's scan — only running containers are relevant;
// container.ListOptions{} defaults to All=false, which already means
// "running only", so no explicit filter is required beyond making the
// call. Kept as its own function so tests and future filters have a single
// place to change.
func containerListOptions() container.ListOptions {
	return container.ListOptions{
		Filters: filters.NewArgs(),
	}
}
