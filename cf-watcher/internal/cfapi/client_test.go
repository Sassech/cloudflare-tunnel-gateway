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

// tunnelAPIFake is an httptest-backed fake of the Cloudflare Tunnel
// list/create/token endpoints used by EnsureTunnel and GetTunnelToken.
type tunnelAPIFake struct {
	existing []map[string]any // tunnels returned by the list call

	listCalls   int32
	createCalls int32
	gotQuery    string
	gotCreate   map[string]any
}

func (f *tunnelAPIFake) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const base = "/accounts/test-account/cfd_tunnel"
		switch {
		case r.Method == http.MethodGet && r.URL.Path == base:
			atomic.AddInt32(&f.listCalls, 1)
			f.gotQuery = r.URL.RawQuery
			json.NewEncoder(w).Encode(map[string]any{"success": true, "result": f.existing})
		case r.Method == http.MethodPost && r.URL.Path == base:
			atomic.AddInt32(&f.createCalls, 1)
			if err := json.NewDecoder(r.Body).Decode(&f.gotCreate); err != nil {
				t.Errorf("failed to decode create body: %v", err)
			}
			json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"result":  map[string]any{"id": "new-tunnel-id", "name": f.gotCreate["name"]},
			})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func TestEnsureTunnel(t *testing.T) {
	tests := []struct {
		name        string
		existing    []map[string]any
		wantID      string
		wantCreated bool
		wantErr     string // substring; "" means no error
		wantCreates int32
	}{
		{
			name:        "reuses the single existing tunnel with that name",
			existing:    []map[string]any{{"id": "existing-id", "name": "cf-watcher-vps1"}},
			wantID:      "existing-id",
			wantCreated: false,
			wantCreates: 0,
		},
		{
			name:        "creates a remotely managed tunnel when none exists",
			existing:    []map[string]any{},
			wantID:      "new-tunnel-id",
			wantCreated: true,
			wantCreates: 1,
		},
		{
			name:        "ignores non-exact name matches and creates",
			existing:    []map[string]any{{"id": "other-id", "name": "cf-watcher-vps1-old"}},
			wantID:      "new-tunnel-id",
			wantCreated: true,
			wantCreates: 1,
		},
		{
			name: "multiple tunnels with the same name is an error",
			existing: []map[string]any{
				{"id": "dup-1", "name": "cf-watcher-vps1"},
				{"id": "dup-2", "name": "cf-watcher-vps1"},
			},
			wantErr:     "2 tunnels named",
			wantCreates: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &tunnelAPIFake{existing: tt.existing}
			server := httptest.NewServer(fake.handler(t))
			defer server.Close()

			client := newTestClient(server)
			id, created, err := client.EnsureTunnel(context.Background(), "cf-watcher-vps1")

			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("EnsureTunnel() error = %v, want error containing %q", err, tt.wantErr)
				}
			} else if err != nil {
				t.Fatalf("EnsureTunnel() error = %v, want nil", err)
			}
			if id != tt.wantID || created != tt.wantCreated {
				t.Errorf("EnsureTunnel() = (%q, %v), want (%q, %v)", id, created, tt.wantID, tt.wantCreated)
			}
			if got := atomic.LoadInt32(&fake.createCalls); got != tt.wantCreates {
				t.Errorf("create calls = %d, want %d", got, tt.wantCreates)
			}
			if !strings.Contains(fake.gotQuery, "name=cf-watcher-vps1") || !strings.Contains(fake.gotQuery, "is_deleted=false") {
				t.Errorf("list query = %q, want name and is_deleted=false filters", fake.gotQuery)
			}
			if tt.wantCreates > 0 {
				if fake.gotCreate["name"] != "cf-watcher-vps1" || fake.gotCreate["config_src"] != "cloudflare" {
					t.Errorf("create body = %v, want name=cf-watcher-vps1 config_src=cloudflare", fake.gotCreate)
				}
			}
		})
	}
}

func TestEnsureTunnel_EmptyNameRejected(t *testing.T) {
	client := NewClient(&config.Config{CFAccountID: "test-account"})
	if _, _, err := client.EnsureTunnel(context.Background(), "  "); err == nil {
		t.Fatal("EnsureTunnel(\"  \") error = nil, want error")
	}
}

func TestGetTunnelToken(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		body      map[string]any
		wantToken string
		wantErr   bool
	}{
		{
			name:      "returns the token string",
			status:    http.StatusOK,
			body:      map[string]any{"success": true, "result": "eyJhIjoiYiJ9"},
			wantToken: "eyJhIjoiYiJ9",
		},
		{
			name:    "empty token is an error",
			status:  http.StatusOK,
			body:    map[string]any{"success": true, "result": ""},
			wantErr: true,
		},
		{
			name:    "4xx is an error",
			status:  http.StatusForbidden,
			body:    map[string]any{"success": false},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/accounts/test-account/cfd_tunnel/tid-1/token" {
					t.Errorf("request path = %q, want token endpoint for tid-1", r.URL.Path)
				}
				w.WriteHeader(tt.status)
				json.NewEncoder(w).Encode(tt.body)
			}))
			defer server.Close()

			token, err := newTestClient(server).GetTunnelToken(context.Background(), "tid-1")
			if tt.wantErr != (err != nil) {
				t.Fatalf("GetTunnelToken() error = %v, wantErr %v", err, tt.wantErr)
			}
			if token != tt.wantToken {
				t.Errorf("GetTunnelToken() = %q, want %q", token, tt.wantToken)
			}
		})
	}
}

// TestTunnelScopedCalls_RequireResolvedTunnelID verifies no tunnel-scoped
// request is sent before the tunnel ID has been resolved.
func TestTunnelScopedCalls_RequireResolvedTunnelID(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
	}))
	defer server.Close()

	client := newTestClient(server)
	client.SetTunnelID("")

	if err := client.AssertConfigSrc(context.Background()); err == nil {
		t.Error("AssertConfigSrc() error = nil, want unresolved tunnel ID error")
	}
	if err := client.PutTunnelConfig(context.Background(), nil); err == nil {
		t.Error("PutTunnelConfig() error = nil, want unresolved tunnel ID error")
	}
	if _, err := client.GetTunnelConfig(context.Background()); err == nil {
		t.Error("GetTunnelConfig() error = nil, want unresolved tunnel ID error")
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Errorf("server received %d call(s), want 0", got)
	}

	client.SetTunnelID("resolved")
	if got := client.TunnelID(); got != "resolved" {
		t.Errorf("TunnelID() = %q, want %q", got, "resolved")
	}
}
