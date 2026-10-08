// Package watcher listens for Docker container lifecycle events, extracts
// cf.* label-derived desired state, and drives the debounced sync loop that
// pushes changes to the Cloudflare Tunnel Configurations API.
//
// Label contract:
//
//	cf.hostname  — required — non-empty string, e.g. "app.example.com"
//	cf.port      — required — integer string, 1-65535
//	cf.service   — optional — "http" (default) or "https"
//	cf.dns       — optional — "true" / "false" (default "false")
package watcher

import (
	"context"
	"strconv"
	"strings"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/network"

	"cf-watcher/internal/logging"
	"cf-watcher/internal/state"
)

// DockerClient is the narrow subset of the Docker Engine API client that
// internal/watcher depends on. It exists so tests can supply a fake
// implementation instead of a real Docker daemon connection.
//
// *client.Client from github.com/docker/docker/client satisfies this
// interface directly — types.Container and types.ContainerJSON are
// deprecated aliases for container.Summary and container.InspectResponse
// respectively, so no adapter is required.
type DockerClient interface {
	ContainerList(ctx context.Context, options container.ListOptions) ([]types.Container, error)
	Events(ctx context.Context, options events.ListOptions) (<-chan events.Message, <-chan error)
	ContainerInspect(ctx context.Context, containerID string) (types.ContainerJSON, error)
}

// Label keys read from container labels (FR-1).
const (
	labelHostname = "cf.hostname"
	labelPort     = "cf.port"
	labelService  = "cf.service"
	labelDNS      = "cf.dns"

	defaultService = "http"

	portMin = 1
	portMax = 65535
)

// ExtractDesiredState parses and validates a container's cf.* labels.
//
// It returns (nil, false) if the container should be excluded from the
// desired state:
//   - cf.hostname or cf.port is missing — silently ignored, no WARNING
//     (this is the common case: most containers have no cf.* labels at all).
//   - cf.port is present but not a valid integer 1-65535 — excluded with a
//     WARNING log.
//
// On success it returns a *state.Entry ready for state.Store.Add, with
// cf.service defaulting to "http" and cf.dns defaulting to "false".
func ExtractDesiredState(containerID, containerName string, labels map[string]string) (*state.Entry, bool) {
	hostname := strings.TrimSpace(labels[labelHostname])
	portRaw := strings.TrimSpace(labels[labelPort])

	// Both required; missing → silent skip, no WARNING.
	if hostname == "" || portRaw == "" {
		return nil, false
	}

	// cf.port must be a valid integer 1-65535.
	port, err := strconv.Atoi(portRaw)
	if err != nil || port < portMin || port > portMax {
		logging.Warningf("container %s: cf.port=%q is not a valid port (1-65535) — skipping", containerName, portRaw)
		return nil, false
	}

	// cf.service defaults to "http"; only "http"/"https" accepted.
	service := strings.ToLower(strings.TrimSpace(labels[labelService]))
	switch service {
	case "":
		service = defaultService
	case "http", "https":
		// valid as-is
	default:
		logging.Warningf("container %s: cf.service=%q is not a valid value (expected http/https) — falling back to %q", containerName, service, defaultService)
		service = defaultService
	}

	// cf.dns defaults to "false".
	dns := strings.ToLower(strings.TrimSpace(labels[labelDNS])) == "true"

	return &state.Entry{
		ContainerID:   containerID,
		ContainerName: containerName,
		Hostname:      hostname,
		Port:          port,
		Service:       service,
		DNS:           dns,
	}, true
}

// CheckNetworkMembership logs a WARNING if the labeled container is not on
// dockerNetwork — the Cloudflare Tunnel likely cannot route to it otherwise.
//
// An empty dockerNetwork disables the check (nothing to compare against).
func CheckNetworkMembership(containerName, dockerNetwork string, networks map[string]*network.EndpointSettings) {
	if dockerNetwork == "" {
		return
	}
	if _, ok := networks[dockerNetwork]; !ok {
		logging.Warningf("container %s is not on network %s — tunnel may not reach it", containerName, dockerNetwork)
	}
}
