package cfapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"cf-watcher/internal/config"
)

// defaultBaseURL is the production Cloudflare API base URL.
const defaultBaseURL = "https://api.cloudflare.com/client/v4"

// Client is a Cloudflare Tunnel Configurations API client.
//
// Zero value is not usable — construct with NewClient.
type Client struct {
	httpClient *http.Client
	baseURL    string
	apiToken   string
	accountID  string

	// tunnelMu guards tunnelID, which is resolved at startup (configured,
	// reused by name, or freshly created) and set via SetTunnelID.
	tunnelMu sync.RWMutex
	tunnelID string

	// putMu serializes PutTunnelConfig calls — no concurrent writes to the
	// tunnel ingress configuration.
	putMu sync.Mutex
}

// errTunnelIDUnresolved is returned by tunnel-scoped calls made before the
// tunnel ID has been resolved (CF_TUNNEL_ID unset and SetTunnelID not yet
// called).
var errTunnelIDUnresolved = errors.New("cfapi: tunnel ID not resolved yet — call EnsureTunnel/SetTunnelID first")

// NewClient builds a Client for the given config, targeting the production
// Cloudflare API base URL. The tunnel ID is seeded from cfg.CFTunnelID,
// which may be empty until provisioning resolves it (see SetTunnelID).
func NewClient(cfg *config.Config) *Client {
	return &Client{
		httpClient: &http.Client{Timeout: 30 * time.Second},
		baseURL:    defaultBaseURL,
		apiToken:   cfg.CFAPIToken,
		accountID:  cfg.CFAccountID,
		tunnelID:   cfg.CFTunnelID,
	}
}

// SetTunnelID sets the tunnel every tunnel-scoped call targets. Safe for
// concurrent use with those calls.
func (c *Client) SetTunnelID(tunnelID string) {
	c.tunnelMu.Lock()
	defer c.tunnelMu.Unlock()
	c.tunnelID = tunnelID
}

// TunnelID returns the currently resolved tunnel ID ("" if unresolved).
func (c *Client) TunnelID() string {
	c.tunnelMu.RLock()
	defer c.tunnelMu.RUnlock()
	return c.tunnelID
}

// resolvedTunnelID returns the tunnel ID, or errTunnelIDUnresolved if it
// has not been resolved yet.
func (c *Client) resolvedTunnelID() (string, error) {
	id := c.TunnelID()
	if id == "" {
		return "", errTunnelIDUnresolved
	}
	return id, nil
}

// tunnelSummary is the subset of a Cloudflare Tunnel object cf-watcher
// reads.
type tunnelSummary struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// EnsureTunnel returns the ID of the non-deleted tunnel named name in the
// account, creating it (remotely managed, config_src=cloudflare) if none
// exists. created reports whether a new tunnel was created.
//
//	GET  /accounts/{account_id}/cfd_tunnel?name={name}&is_deleted=false
//	POST /accounts/{account_id}/cfd_tunnel
//
// More than one live tunnel with the same name is ambiguous and returns an
// error rather than guessing — the operator must delete the duplicates or
// pin one via CF_TUNNEL_ID.
func (c *Client) EnsureTunnel(ctx context.Context, name string) (tunnelID string, created bool, err error) {
	if strings.TrimSpace(name) == "" {
		return "", false, errors.New("cfapi: tunnel name must not be empty")
	}

	query := url.Values{}
	query.Set("name", name)
	query.Set("is_deleted", "false")
	listPath := fmt.Sprintf("/accounts/%s/cfd_tunnel?%s", c.accountID, query.Encode())

	var listResp struct {
		Result []tunnelSummary `json:"result"`
	}
	if err := c.doJSON(ctx, http.MethodGet, listPath, nil, &listResp); err != nil {
		return "", false, err
	}

	// The API's name filter is applied server-side; re-check for an exact
	// match so a looser server-side match can never adopt a foreign tunnel.
	var matches []tunnelSummary
	for _, t := range listResp.Result {
		if t.Name == name {
			matches = append(matches, t)
		}
	}

	switch len(matches) {
	case 1:
		return matches[0].ID, false, nil
	case 0:
		// Fall through to creation below.
	default:
		ids := make([]string, 0, len(matches))
		for _, m := range matches {
			ids = append(ids, m.ID)
		}
		return "", false, fmt.Errorf(
			"cfapi: %d tunnels named %q exist (%s) — delete the duplicates or set CF_TUNNEL_ID to pick one",
			len(matches), name, strings.Join(ids, ", "),
		)
	}

	createPath := fmt.Sprintf("/accounts/%s/cfd_tunnel", c.accountID)
	payload := map[string]any{"name": name, "config_src": "cloudflare"}

	var createResp struct {
		Result tunnelSummary `json:"result"`
	}
	if err := c.doJSON(ctx, http.MethodPost, createPath, payload, &createResp); err != nil {
		return "", false, err
	}
	if createResp.Result.ID == "" {
		return "", false, fmt.Errorf("cfapi: create tunnel %q: response did not include a tunnel ID", name)
	}
	return createResp.Result.ID, true, nil
}

// GetTunnelToken retrieves the connector token cloudflared needs to run
// the given tunnel.
//
//	GET /accounts/{account_id}/cfd_tunnel/{tunnel_id}/token
//
// The returned token is a secret — callers must never log it.
func (c *Client) GetTunnelToken(ctx context.Context, tunnelID string) (string, error) {
	if tunnelID == "" {
		return "", errTunnelIDUnresolved
	}
	path := fmt.Sprintf("/accounts/%s/cfd_tunnel/%s/token", c.accountID, tunnelID)

	var resp struct {
		Result string `json:"result"`
	}
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return "", err
	}
	if resp.Result == "" {
		return "", fmt.Errorf("cfapi: tunnel %s: token response was empty", tunnelID)
	}
	return resp.Result, nil
}

// GetTunnelConfig retrieves the current tunnel ingress configuration.
//
// GET /accounts/{account_id}/cfd_tunnel/{tunnel_id}/configurations
//
// On sync, the current tunnel config must be read from the CF API.
func (c *Client) GetTunnelConfig(ctx context.Context) (map[string]any, error) {
	tunnelID, err := c.resolvedTunnelID()
	if err != nil {
		return nil, err
	}
	path := fmt.Sprintf("/accounts/%s/cfd_tunnel/%s/configurations", c.accountID, tunnelID)

	var resp struct {
		Result map[string]any `json:"result"`
	}
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Result, nil
}

// PutTunnelConfig replaces the full tunnel ingress configuration.
//
// PUT /accounts/{account_id}/cfd_tunnel/{tunnel_id}/configurations
//
// Calls are serialized via an internal mutex — all CF API PUTs must be
// serialized to prevent concurrent writes. ingress must already include the
// trailing catch-all rule; callers should build it via
// internal/state.Store.BuildIngress.
func (c *Client) PutTunnelConfig(ctx context.Context, ingress []map[string]any) error {
	tunnelID, err := c.resolvedTunnelID()
	if err != nil {
		return err
	}

	c.putMu.Lock()
	defer c.putMu.Unlock()

	path := fmt.Sprintf("/accounts/%s/cfd_tunnel/%s/configurations", c.accountID, tunnelID)
	payload := map[string]any{"config": map[string]any{"ingress": ingress}}

	return c.doJSON(ctx, http.MethodPut, path, payload, nil)
}

// AssertConfigSrc verifies the tunnel is in remote (Cloudflare-managed)
// configuration mode.
//
// GET /accounts/{account_id}/cfd_tunnel/{tunnel_id}
//
// On boot, assert tunnel config_src == "cloudflare"; returns an error if
// it is "local" (or anything else).
func (c *Client) AssertConfigSrc(ctx context.Context) error {
	tunnelID, err := c.resolvedTunnelID()
	if err != nil {
		return err
	}
	path := fmt.Sprintf("/accounts/%s/cfd_tunnel/%s", c.accountID, tunnelID)

	var resp struct {
		Result struct {
			ConfigSrc string `json:"config_src"`
		} `json:"result"`
	}
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return err
	}

	if resp.Result.ConfigSrc != "cloudflare" {
		return fmt.Errorf(
			"cfapi: tunnel %s is in local config mode (config_src=%q); cf-watcher requires remote-managed mode (config_src=\"cloudflare\")",
			tunnelID, resp.Result.ConfigSrc,
		)
	}
	return nil
}

// doJSON performs a JSON HTTP request against the Cloudflare API with
// retry/backoff (DoJSON — shared helper also used by internal/dns), decoding
// the response body into out (if non-nil).
//
// Error handling:
//   - network error: retryable
//   - 5xx response: retryable
//   - 4xx response: returned immediately, NOT retried
func (c *Client) doJSON(ctx context.Context, method, path string, body, out any) error {
	return NewJSONClient(c.httpClient, c.baseURL, c.apiToken, "cfapi").Do(ctx, method, path, body, out)
}
