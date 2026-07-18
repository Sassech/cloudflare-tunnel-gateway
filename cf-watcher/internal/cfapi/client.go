package cfapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
	tunnelID   string

	// putMu serializes PutTunnelConfig calls — no concurrent writes to the
	// tunnel ingress configuration.
	putMu sync.Mutex
}

// NewClient builds a Client for the given config, targeting the production
// Cloudflare API base URL.
func NewClient(cfg *config.Config) *Client {
	return &Client{
		httpClient: &http.Client{Timeout: 30 * time.Second},
		baseURL:    defaultBaseURL,
		apiToken:   cfg.CFAPIToken,
		accountID:  cfg.CFAccountID,
		tunnelID:   cfg.CFTunnelID,
	}
}

// GetTunnelConfig retrieves the current tunnel ingress configuration.
//
// GET /accounts/{account_id}/cfd_tunnel/{tunnel_id}/configurations
//
// On sync, the current tunnel config must be read from the CF API.
func (c *Client) GetTunnelConfig(ctx context.Context) (map[string]any, error) {
	path := fmt.Sprintf("/accounts/%s/cfd_tunnel/%s/configurations", c.accountID, c.tunnelID)

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
	c.putMu.Lock()
	defer c.putMu.Unlock()

	path := fmt.Sprintf("/accounts/%s/cfd_tunnel/%s/configurations", c.accountID, c.tunnelID)
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
	path := fmt.Sprintf("/accounts/%s/cfd_tunnel/%s", c.accountID, c.tunnelID)

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
			c.tunnelID, resp.Result.ConfigSrc,
		)
	}
	return nil
}

// doJSON performs a JSON HTTP request against the Cloudflare API with
// retry/backoff (WithRetry), decoding the response body into out (if
// non-nil).
//
// Error handling:
//   - network error: retryable
//   - 5xx response: retryable
//   - 4xx response: returned immediately, NOT retried
func (c *Client) doJSON(ctx context.Context, method, path string, body, out any) error {
	return WithRetry(ctx, func() error {
		var reqBody io.Reader
		if body != nil {
			b, err := json.Marshal(body)
			if err != nil {
				return fmt.Errorf("cfapi: failed to encode request body for %s %s: %w", method, path, err)
			}
			reqBody = bytes.NewReader(b)
		}

		req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reqBody)
		if err != nil {
			return fmt.Errorf("cfapi: failed to build request for %s %s: %w", method, path, err)
		}
		req.Header.Set("Authorization", "Bearer "+c.apiToken)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			// Network error — retryable.
			return Retryable(fmt.Errorf("cfapi: %s %s: %w", method, path, err))
		}
		defer resp.Body.Close()

		respBody, _ := io.ReadAll(resp.Body)

		if resp.StatusCode >= 500 {
			// 5xx — retryable.
			return Retryable(fmt.Errorf("cfapi: %s %s returned %d: %s", method, path, resp.StatusCode, respBody))
		}
		if resp.StatusCode >= 400 {
			// 4xx — permanent, do NOT retry.
			return fmt.Errorf("cfapi: %s %s returned %d: %s", method, path, resp.StatusCode, respBody)
		}

		if out != nil && len(respBody) > 0 {
			if err := json.Unmarshal(respBody, out); err != nil {
				return fmt.Errorf("cfapi: failed to decode response from %s %s: %w", method, path, err)
			}
		}
		return nil
	})
}
