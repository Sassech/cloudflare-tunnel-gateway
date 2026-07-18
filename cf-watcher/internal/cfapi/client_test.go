package cfapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"cf-watcher/internal/config"
)

// newTestClient builds a Client pointed at server, bypassing NewClient's
// hardcoded production base URL.
func newTestClient(server *httptest.Server) *Client {
	c := NewClient(&config.Config{
		CFAPIToken:  "test-token",
		CFAccountID: "test-account",
		CFTunnelID:  "test-tunnel",
	})
	c.baseURL = server.URL
	return c
}

// TestGetTunnelConfig_RetriesOn503ThenSucceeds covers SC-05: the CF API is
// down (503) on the first attempt, then recovers — the client must retry
// with backoff and return the successful result, without surfacing an
// error to the caller.
func TestGetTunnelConfig_RetriesOn503ThenSucceeds(t *testing.T) {
	var callCount int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&callCount, 1)
		if n == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`{"success":false,"errors":[{"message":"service unavailable"}]}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"result":  map[string]any{"ingress": []any{}},
		})
	}))
	defer server.Close()

	client := newTestClient(server)

	result, err := client.GetTunnelConfig(context.Background())
	if err != nil {
		t.Fatalf("GetTunnelConfig() error = %v, want nil (should succeed after retry)", err)
	}
	if result == nil {
		t.Fatal("GetTunnelConfig() result = nil, want a decoded result map")
	}
	if got := atomic.LoadInt32(&callCount); got != 2 {
		t.Errorf("server received %d call(s), want exactly 2 (1 failure + 1 retry)", got)
	}
}

// TestAssertConfigSrc_LocalMode covers SC-10: a tunnel in local config mode
// must produce an error so startup can abort.
func TestAssertConfigSrc_LocalMode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"result":  map[string]any{"config_src": "local"},
		})
	}))
	defer server.Close()

	client := newTestClient(server)

	err := client.AssertConfigSrc(context.Background())
	if err == nil {
		t.Fatal("AssertConfigSrc() error = nil, want error for config_src=local")
	}
	if !strings.Contains(err.Error(), "local") {
		t.Errorf("AssertConfigSrc() error = %q, want it to mention config_src=local", err.Error())
	}
}

// TestAssertConfigSrc_CloudflareMode is the SC-10 counterpart: a
// remote-managed tunnel must pass without error.
func TestAssertConfigSrc_CloudflareMode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"result":  map[string]any{"config_src": "cloudflare"},
		})
	}))
	defer server.Close()

	client := newTestClient(server)

	if err := client.AssertConfigSrc(context.Background()); err != nil {
		t.Fatalf("AssertConfigSrc() error = %v, want nil for config_src=cloudflare", err)
	}
}

// TestPutTunnelConfig_4xxNoRetry covers REQ-6.2: a 4xx response is a
// configuration error, not transient — it must be returned immediately
// without any retry attempt.
func TestPutTunnelConfig_4xxNoRetry(t *testing.T) {
	var callCount int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&callCount, 1)
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"success":false,"errors":[{"message":"invalid ingress rule"}]}`))
	}))
	defer server.Close()

	client := newTestClient(server)

	ingress := []map[string]any{{"service": "http_status:404"}}
	err := client.PutTunnelConfig(context.Background(), ingress)
	if err == nil {
		t.Fatal("PutTunnelConfig() error = nil, want error for 400 response")
	}
	if got := atomic.LoadInt32(&callCount); got != 1 {
		t.Errorf("server received %d call(s), want exactly 1 (no retry on 4xx)", got)
	}
}

// TestPutTunnelConfig_Success verifies the happy path: a 200 response with
// no error, and that the request body carries the ingress list under
// config.ingress (REQ-3.2).
func TestPutTunnelConfig_Success(t *testing.T) {
	var gotBody map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("request method = %s, want PUT", r.Method)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("failed to decode request body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{"success": true, "result": map[string]any{}})
	}))
	defer server.Close()

	client := newTestClient(server)

	ingress := []map[string]any{
		{"hostname": "app.example.com", "service": "http://app:8080"},
		{"service": "http_status:404"},
	}
	if err := client.PutTunnelConfig(context.Background(), ingress); err != nil {
		t.Fatalf("PutTunnelConfig() error = %v, want nil", err)
	}

	config, ok := gotBody["config"].(map[string]any)
	if !ok {
		t.Fatalf("request body missing config field: %v", gotBody)
	}
	gotIngress, ok := config["ingress"].([]any)
	if !ok || len(gotIngress) != 2 {
		t.Fatalf("request body config.ingress = %v, want 2 entries", config["ingress"])
	}
}
