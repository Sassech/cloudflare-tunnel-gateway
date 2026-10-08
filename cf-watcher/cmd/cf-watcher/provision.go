package main

import (
	"context"
	"fmt"
	"regexp"

	"cf-watcher/internal/config"
	"cf-watcher/internal/logging"
	"cf-watcher/internal/tokenfile"
)

// TunnelProvisioner is the subset of *cfapi.Client's behavior used to
// resolve (reuse or create) this host's tunnel and fetch its connector
// token. *cfapi.Client satisfies this directly.
type TunnelProvisioner interface {
	EnsureTunnel(ctx context.Context, name string) (tunnelID string, created bool, err error)
	GetTunnelToken(ctx context.Context, tunnelID string) (string, error)
	SetTunnelID(tunnelID string)
}

// TunnelManager combines provisioning with the ingress operations the
// startup reconcile and sync loop use.
type TunnelManager interface {
	TunnelClient
	TunnelProvisioner
}

// containerIDHostname matches Docker's default container hostname (the
// first 12 hex chars of the container ID).
var containerIDHostname = regexp.MustCompile(`^cf-watcher-[0-9a-f]{12}$`)

// provisionTunnel resolves the tunnel this cf-watcher manages and writes
// its connector token for cloudflared:
//
//  1. If CF_TUNNEL_ID is set, use it as-is (no lookup, no creation).
//     Otherwise reuse the live tunnel named CF_TUNNEL_NAME, or create it
//     (remotely managed) if none exists.
//  2. Point the tunnel client at the resolved ID (SetTunnelID) — every
//     later tunnel-scoped call depends on it.
//  3. Fetch the connector token and write it atomically to
//     TUNNEL_TOKEN_FILE (no-op if unchanged).
//
// Returns the resolved tunnel ID. The caller must not report healthy until
// this returns nil — cloudflared waits on that to start.
func provisionTunnel(ctx context.Context, cfg *config.Config, prov TunnelProvisioner) (string, error) {
	tunnelID := cfg.CFTunnelID
	if tunnelID != "" {
		logging.Infof("provision: using configured tunnel CF_TUNNEL_ID=%s (CF_TUNNEL_NAME ignored)", tunnelID)
	} else {
		if containerIDHostname.MatchString(cfg.CFTunnelName) {
			logging.Warningf(
				"provision: tunnel name %q looks derived from a container ID — it will change when the container is recreated, creating a new tunnel each time. Use `uts: host` (see docker-compose.yaml) or set CF_TUNNEL_NAME",
				cfg.CFTunnelName,
			)
		}
		id, created, err := prov.EnsureTunnel(ctx, cfg.CFTunnelName)
		if err != nil {
			return "", fmt.Errorf("provision: resolve tunnel %q: %w", cfg.CFTunnelName, err)
		}
		tunnelID = id
		if created {
			logging.Infof("provision: created tunnel name=%s tunnel_id=%s", cfg.CFTunnelName, tunnelID)
		} else {
			logging.Infof("provision: reusing existing tunnel name=%s tunnel_id=%s", cfg.CFTunnelName, tunnelID)
		}
	}

	prov.SetTunnelID(tunnelID)

	token, err := prov.GetTunnelToken(ctx, tunnelID)
	if err != nil {
		return "", fmt.Errorf("provision: fetch connector token for tunnel %s: %w", tunnelID, err)
	}

	changed, err := tokenfile.Write(cfg.TunnelTokenFile, token)
	if err != nil {
		return "", fmt.Errorf("provision: write connector token: %w", err)
	}
	if changed {
		logging.Infof("provision: connector token written to %s", cfg.TunnelTokenFile)
	} else {
		logging.Infof("provision: connector token at %s already up to date", cfg.TunnelTokenFile)
	}

	return tunnelID, nil
}
