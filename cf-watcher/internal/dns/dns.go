// Package dns is a client for the Cloudflare DNS Records API, managing
// CNAME records for containers labeled cf.dns=true. It only ever deletes
// records it created itself in the current process session.
package dns

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sync"
	"time"

	"cf-watcher/internal/cfapi"
	"cf-watcher/internal/config"
)

// defaultBaseURL is the production Cloudflare API base URL.
const defaultBaseURL = "https://api.cloudflare.com/client/v4"

// Client is a Cloudflare DNS Records API client.
//
// Zero value is not usable — construct with NewClient.
type Client struct {
	httpClient *http.Client
	baseURL    string
	apiToken   string
	zoneID     string

	// mu guards tracked.
	mu sync.Mutex
	// tracked maps hostname -> record_id for CNAME records created by
	// cf-watcher in this process session. Only records present here may be
// deleted — this is what makes create/delete idempotent and prevents
// cf-watcher from touching DNS records it did not create.
	tracked map[string]string
}

// NewClient builds a Client for the given config, targeting the production
// Cloudflare API base URL.
func NewClient(cfg *config.Config) *Client {
	return &Client{
		httpClient: &http.Client{Timeout: 30 * time.Second},
		baseURL:    defaultBaseURL,
		apiToken:   cfg.CFAPIToken,
		zoneID:     cfg.CFZoneID,
		tracked:    make(map[string]string),
	}
}

// GetDNSRecord checks whether a CNAME record exists for hostname.
//
// GET /zones/{zone_id}/dns_records?type=CNAME&name={hostname}
func (c *Client) GetDNSRecord(ctx context.Context, hostname string) (recordID string, found bool, err error) {
	path := fmt.Sprintf("/zones/%s/dns_records?type=CNAME&name=%s", c.zoneID, url.QueryEscape(hostname))

	var resp struct {
		Result []struct {
			ID string `json:"id"`
		} `json:"result"`
	}
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return "", false, err
	}
	if len(resp.Result) == 0 {
		return "", false, nil
	}
	return resp.Result[0].ID, true, nil
}

// CreateCNAME creates a proxied CNAME record for hostname pointing at
// {tunnelID}.cfargotunnel.com.
//
// It is idempotent:
//   - if hostname is already tracked from this session, it returns the
//     tracked record ID without making an API call;
//   - if a record exists but was NOT created by cf-watcher this session, it
//     logs a WARNING and returns ("", nil) — skip, do not error,
//     do not adopt/track someone else's record;
//   - otherwise it creates the record and tracks it.
func (c *Client) CreateCNAME(ctx context.Context, hostname, tunnelID string) (recordID string, err error) {
	c.mu.Lock()
	if existing, ok := c.tracked[hostname]; ok {
		c.mu.Unlock()
		return existing, nil
	}
	c.mu.Unlock()

	existingID, found, err := c.GetDNSRecord(ctx, hostname)
	if err != nil {
		return "", err
	}
	if found {
		// Record exists but cf-watcher did not create it this session —
		// skip creation, do not adopt it into session tracking.
		log.Printf("WARNING: DNS record for %s already exists (record_id=%s) and was not created by cf-watcher — skipping", hostname, existingID)
		return "", nil
	}

	path := fmt.Sprintf("/zones/%s/dns_records", c.zoneID)
	payload := map[string]any{
		"type":    "CNAME",
		"name":    hostname,
		"content": fmt.Sprintf("%s.cfargotunnel.com", tunnelID),
		"proxied": true,
		"ttl":     1,
	}

	var resp struct {
		Result struct {
			ID string `json:"id"`
		} `json:"result"`
	}
	if err := c.doJSON(ctx, http.MethodPost, path, payload, &resp); err != nil {
		return "", err
	}

	c.mu.Lock()
	c.tracked[hostname] = resp.Result.ID
	c.mu.Unlock()

	log.Printf("INFO: CNAME created hostname=%s record_id=%s target=%s.cfargotunnel.com", hostname, resp.Result.ID, tunnelID)
	return resp.Result.ID, nil
}

// TrackedHostnames returns a snapshot of hostnames cf-watcher created this
// session, mapped to their record IDs. Callers diff this against the desired
// cf.dns=true set to decide which CNAMEs to create or delete.
func (c *Client) TrackedHostnames() map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()

	snapshot := make(map[string]string, len(c.tracked))
	for hostname, recordID := range c.tracked {
		snapshot[hostname] = recordID
	}
	return snapshot
}

// DeleteCNAME deletes the CNAME record for hostname, but ONLY if it is
// currently tracked in this session's managed-records map — cf-watcher
// must never delete a DNS record it did not create. If hostname
// is not tracked, this is a no-op.
func (c *Client) DeleteCNAME(ctx context.Context, hostname string) error {
	c.mu.Lock()
	recordID, ok := c.tracked[hostname]
	c.mu.Unlock()
	if !ok {
		log.Printf("DEBUG: %s not managed by cf-watcher, skipping DNS delete", hostname)
		return nil
	}

	path := fmt.Sprintf("/zones/%s/dns_records/%s", c.zoneID, recordID)
	if err := c.doJSON(ctx, http.MethodDelete, path, nil, nil); err != nil {
		// Leave the record tracked on failure so a future attempt can retry
		// the delete instead of silently orphaning it.
		return err
	}

	c.mu.Lock()
	delete(c.tracked, hostname)
	c.mu.Unlock()

	log.Printf("INFO: CNAME deleted hostname=%s record_id=%s", hostname, recordID)
	return nil
}

// doJSON performs a JSON HTTP request against the Cloudflare API with
// retry/backoff (cfapi.WithRetry — shared policy with internal/cfapi),
// decoding the response body into out (if non-nil).
//
// Error handling mirrors internal/cfapi: network errors and 5xx
// responses are retried; 4xx responses are returned immediately.
func (c *Client) doJSON(ctx context.Context, method, path string, body, out any) error {
	return cfapi.WithRetry(ctx, func() error {
		var reqBody io.Reader
		if body != nil {
			b, err := json.Marshal(body)
			if err != nil {
				return fmt.Errorf("dns: failed to encode request body for %s %s: %w", method, path, err)
			}
			reqBody = bytes.NewReader(b)
		}

		req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reqBody)
		if err != nil {
			return fmt.Errorf("dns: failed to build request for %s %s: %w", method, path, err)
		}
		req.Header.Set("Authorization", "Bearer "+c.apiToken)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return cfapi.Retryable(fmt.Errorf("dns: %s %s: %w", method, path, err))
		}
		defer resp.Body.Close()

		respBody, _ := io.ReadAll(resp.Body)

		if resp.StatusCode >= 500 {
			return cfapi.Retryable(fmt.Errorf("dns: %s %s returned %d: %s", method, path, resp.StatusCode, respBody))
		}
		if resp.StatusCode >= 400 {
			return fmt.Errorf("dns: %s %s returned %d: %s", method, path, resp.StatusCode, respBody)
		}

		if out != nil && len(respBody) > 0 {
			if err := json.Unmarshal(respBody, out); err != nil {
				return fmt.Errorf("dns: failed to decode response from %s %s: %w", method, path, err)
			}
		}
		return nil
	})
}
