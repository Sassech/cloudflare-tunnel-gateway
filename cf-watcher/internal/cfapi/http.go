package cfapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// JSONClient performs JSON HTTP requests against the Cloudflare API with
// the shared retry/backoff policy. It is shared by internal/cfapi and
// internal/dns so both packages honor identical error-handling semantics.
type JSONClient struct {
	httpClient *http.Client
	baseURL    string
	apiToken   string
	prefix     string
}

// NewJSONClient builds a JSONClient for the Cloudflare API. prefix is
// prepended to error messages (e.g. "cfapi" or "dns") so callers can
// attribute failures to their own package.
func NewJSONClient(httpClient *http.Client, baseURL, apiToken, prefix string) *JSONClient {
	return &JSONClient{
		httpClient: httpClient,
		baseURL:    baseURL,
		apiToken:   apiToken,
		prefix:     prefix,
	}
}

// Do performs a JSON HTTP request with retry/backoff (WithRetry), decoding
// the response body into out (if non-nil).
//
// Error handling:
//   - network error: retryable
//   - 5xx response: retryable
//   - 4xx response: returned immediately, NOT retried
func (c *JSONClient) Do(ctx context.Context, method, path string, body, out any) error {
	return WithRetry(ctx, func() error {
		req, err := c.newRequest(ctx, method, path, body)
		if err != nil {
			return err
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			// Network error — retryable.
			return Retryable(fmt.Errorf("%s: %s %s: %w", c.prefix, method, path, err))
		}
		defer resp.Body.Close()

		return c.processResponse(method, path, resp, out)
	})
}

// newRequest builds an authenticated JSON HTTP request. body, if non-nil,
// is JSON-encoded as the request payload.
func (c *JSONClient) newRequest(ctx context.Context, method, path string, body any) (*http.Request, error) {
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("%s: failed to encode request body for %s %s: %w", c.prefix, method, path, err)
		}
		reqBody = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reqBody)
	if err != nil {
		return nil, fmt.Errorf("%s: failed to build request for %s %s: %w", c.prefix, method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// processResponse classifies the response status and decodes the response
// body into out (if non-nil and non-empty). 5xx responses are retryable;
// 4xx responses are permanent.
func (c *JSONClient) processResponse(method, path string, resp *http.Response, out any) error {
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 500 {
		// 5xx — retryable.
		return Retryable(fmt.Errorf("%s: %s %s returned %d: %s", c.prefix, method, path, resp.StatusCode, respBody))
	}
	if resp.StatusCode >= 400 {
		// 4xx — permanent, do NOT retry.
		return fmt.Errorf("%s: %s %s returned %d: %s", c.prefix, method, path, resp.StatusCode, respBody)
	}

	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("%s: failed to decode response from %s %s: %w", c.prefix, method, path, err)
		}
	}
	return nil
}
