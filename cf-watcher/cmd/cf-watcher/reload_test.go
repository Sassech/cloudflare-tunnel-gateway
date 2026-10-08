package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/docker/docker/api/types"

	"cf-watcher/internal/state"
)

func TestReloadHandler_DisabledWhenTokenUnset(t *testing.T) {
	docker := newFakeDockerClient(nil)
	cf := newFakeTunnelClient()
	dnsClient := newFakeDNSSyncer()
	store := state.NewStore()
	cfg := testConfig() // ReloadToken is "" (unset) by default

	handler := reloadHandler(cfg, docker, cf, dnsClient, store, nil, 0)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/reload", "", nil)
	if err != nil {
		t.Fatalf("POST /reload error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("POST /reload status = %d, want 404 (disabled when RELOAD_TOKEN unset)", resp.StatusCode)
	}
}

func TestReloadHandler_RejectsNonPOST(t *testing.T) {
	docker := newFakeDockerClient(nil)
	cf := newFakeTunnelClient()
	dnsClient := newFakeDNSSyncer()
	store := state.NewStore()
	cfg := testConfig()
	cfg.ReloadToken = "secret"

	handler := reloadHandler(cfg, docker, cf, dnsClient, store, nil, 0)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/reload", nil)
	req.Header.Set("X-Reload-Token", "secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /reload error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /reload status = %d, want 405", resp.StatusCode)
	}
}

func TestReloadHandler_RejectsMissingOrWrongToken(t *testing.T) {
	docker := newFakeDockerClient(nil)
	cf := newFakeTunnelClient()
	dnsClient := newFakeDNSSyncer()
	store := state.NewStore()
	cfg := testConfig()
	cfg.ReloadToken = "secret"

	handler := reloadHandler(cfg, docker, cf, dnsClient, store, nil, 0)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	tests := []struct {
		name  string
		token string
	}{
		{"missing header", ""},
		{"wrong token", "not-the-secret"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPost, srv.URL+"/reload", nil)
			if tt.token != "" {
				req.Header.Set("X-Reload-Token", tt.token)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("POST /reload error = %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("POST /reload status = %d, want 401", resp.StatusCode)
			}
		})
	}
}

// TestReloadHandler_ResyncsRemovesStaleEntries verifies that /reload fully
// resyncs desired state: a stale entry for a container no longer reported
// by ContainerList must be dropped, not merged.
func TestReloadHandler_ResyncsRemovesStaleEntries(t *testing.T) {
	docker := newFakeDockerClient([]types.Container{
		containerFixture("c1", "app1", map[string]string{"cf.hostname": "app1.vantus.site", "cf.port": "8080"}, "cloudflared-network"),
	})
	cf := newFakeTunnelClient()
	dnsClient := newFakeDNSSyncer()
	store := state.NewStore()
	cfg := testConfig()
	cfg.ReloadToken = "secret"

	// Simulate a stale entry left over from a container that has since
	// been removed or relabeled without a stop/start event.
	store.Add(state.Entry{
		ContainerID:   "stale",
		ContainerName: "stale-container",
		Hostname:      "stale.vantus.site",
		Port:          1234,
		Service:       "http",
	})

	handler := reloadHandler(cfg, docker, cf, dnsClient, store, nil, 0)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/reload", nil)
	req.Header.Set("X-Reload-Token", "secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /reload error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("POST /reload status = %d, want 200", resp.StatusCode)
	}

	_, putCalls, lastIngress := cf.snapshot()
	if putCalls != 1 {
		t.Fatalf("PutTunnelConfig called %d time(s), want 1", putCalls)
	}
	if len(lastIngress) != 2 { // app1 entry + catch-all
		t.Fatalf("last ingress = %d rule(s), want 2 (app1.vantus.site + catch-all)", len(lastIngress))
	}
	for _, rule := range lastIngress {
		if rule["hostname"] == "stale.vantus.site" {
			t.Errorf("last ingress still contains stale entry: %+v", rule)
		}
	}
}

func TestReloadHandler_ReturnsErrorOnReconcileFailure(t *testing.T) {
	docker := newFakeDockerClient(nil)
	cf := newFakeTunnelClient()
	cf.configSrc = "local" // forces AssertConfigSrc to fail
	dnsClient := newFakeDNSSyncer()
	store := state.NewStore()
	cfg := testConfig()
	cfg.ReloadToken = "secret"

	handler := reloadHandler(cfg, docker, cf, dnsClient, store, nil, 0)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/reload", nil)
	req.Header.Set("X-Reload-Token", "secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /reload error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("POST /reload status = %d, want 500", resp.StatusCode)
	}
}

// TestReloadHandler_RateLimitsRapidSuccessiveCalls verifies that a second
// reload immediately following a successful one is rejected with 429,
// independent of the shared Cloudflare API retry budget.
func TestReloadHandler_RateLimitsRapidSuccessiveCalls(t *testing.T) {
	docker := newFakeDockerClient(nil)
	cf := newFakeTunnelClient()
	dnsClient := newFakeDNSSyncer()
	store := state.NewStore()
	cfg := testConfig()
	cfg.ReloadToken = "secret"

	handler := reloadHandler(cfg, docker, cf, dnsClient, store, nil, defaultMinReloadInterval)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	doReload := func(t *testing.T) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/reload", nil)
		req.Header.Set("X-Reload-Token", "secret")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST /reload error = %v", err)
		}
		return resp
	}

	first := doReload(t)
	first.Body.Close()
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first POST /reload status = %d, want 200", first.StatusCode)
	}

	second := doReload(t)
	defer second.Body.Close()
	if second.StatusCode != http.StatusTooManyRequests {
		t.Errorf("second POST /reload (immediate) status = %d, want 429", second.StatusCode)
	}
}

// TestReloadHandler_AllowsReloadAfterIntervalElapses verifies that a reload
// is accepted again once minReloadInterval has passed since the last one.
func TestReloadHandler_AllowsReloadAfterIntervalElapses(t *testing.T) {
	docker := newFakeDockerClient(nil)
	cf := newFakeTunnelClient()
	dnsClient := newFakeDNSSyncer()
	store := state.NewStore()
	cfg := testConfig()
	cfg.ReloadToken = "secret"

	const interval = 30 * time.Millisecond
	handler := reloadHandler(cfg, docker, cf, dnsClient, store, nil, interval)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	doReload := func(t *testing.T) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/reload", nil)
		req.Header.Set("X-Reload-Token", "secret")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST /reload error = %v", err)
		}
		return resp
	}

	first := doReload(t)
	first.Body.Close()
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first POST /reload status = %d, want 200", first.StatusCode)
	}

	time.Sleep(interval * 2)

	third := doReload(t)
	defer third.Body.Close()
	if third.StatusCode != http.StatusOK {
		t.Errorf("POST /reload after interval elapsed status = %d, want 200", third.StatusCode)
	}
}
