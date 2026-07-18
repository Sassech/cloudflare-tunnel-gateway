package main

import (
	"context"
	"errors"
	"testing"

	"github.com/docker/docker/api/types"

	"cf-watcher/internal/config"
	"cf-watcher/internal/state"
)

func testConfig() *config.Config {
	return &config.Config{
		CFAPIToken:      "token",
		CFAccountID:     "account",
		CFTunnelID:      "tunnel-abc",
		DockerNetwork:   "cloudflared-network",
		DebounceSeconds: 2,
	}
}

// TestStartupReconcile_ThreeRunningContainers covers SC-06: 3 pre-running
// labeled containers at startup produce exactly one PutTunnelConfig call
// with 4 ingress rules (3 entries + catch-all).
func TestStartupReconcile_ThreeRunningContainers(t *testing.T) {
	docker := newFakeDockerClient([]types.Container{
		containerFixture("c1", "app1", map[string]string{"cf.hostname": "app1.vantus.site", "cf.port": "8080"}, "cloudflared-network"),
		containerFixture("c2", "app2", map[string]string{"cf.hostname": "app2.vantus.site", "cf.port": "8081"}, "cloudflared-network"),
		containerFixture("c3", "app3", map[string]string{"cf.hostname": "app3.vantus.site", "cf.port": "8082"}, "cloudflared-network"),
	})

	cf := newFakeTunnelClient()
	dnsClient := newFakeDNSSyncer()
	store := state.NewStore()
	cfg := testConfig()

	found, err := startupReconcile(context.Background(), cfg, docker, cf, dnsClient, store)
	if err != nil {
		t.Fatalf("startupReconcile() error = %v, want nil", err)
	}
	if found != 3 {
		t.Errorf("startupReconcile() found = %d, want 3", found)
	}

	assertCalls, putCalls, lastIngress := cf.snapshot()
	if assertCalls != 1 {
		t.Errorf("AssertConfigSrc called %d time(s), want 1", assertCalls)
	}
	if putCalls != 1 {
		t.Fatalf("PutTunnelConfig called %d time(s), want exactly 1 (SC-06)", putCalls)
	}
	if len(lastIngress) != 4 {
		t.Fatalf("PutTunnelConfig ingress = %d rule(s), want 4 (3 entries + catch-all)", len(lastIngress))
	}
	if lastIngress[len(lastIngress)-1]["service"] != "http_status:404" {
		t.Errorf("last ingress rule = %+v, want catch-all", lastIngress[len(lastIngress)-1])
	}
}

// TestStartupReconcile_DNSTrueButZoneIDUnset_AbortsBeforePut covers SC-08:
// a running container with cf.dns=true but CF_ZONE_ID unset must abort
// startup BEFORE any PutTunnelConfig call.
func TestStartupReconcile_DNSTrueButZoneIDUnset_AbortsBeforePut(t *testing.T) {
	docker := newFakeDockerClient([]types.Container{
		containerFixture("c1", "warden", map[string]string{
			"cf.hostname": "warden.vantus.site",
			"cf.port":     "80",
			"cf.dns":      "true",
		}, "cloudflared-network"),
	})

	cf := newFakeTunnelClient()
	dnsClient := newFakeDNSSyncer()
	store := state.NewStore()
	cfg := testConfig()
	cfg.CFZoneID = "" // explicitly unset — the condition under test

	_, err := startupReconcile(context.Background(), cfg, docker, cf, dnsClient, store)
	if err == nil {
		t.Fatal("startupReconcile() error = nil, want error (CF_ZONE_ID required)")
	}
	if !errors.Is(err, errZoneIDRequired) {
		t.Errorf("startupReconcile() error = %v, want errZoneIDRequired", err)
	}

	_, putCalls, _ := cf.snapshot()
	if putCalls != 0 {
		t.Fatalf("PutTunnelConfig called %d time(s), want 0 (must abort before any PUT — SC-08)", putCalls)
	}
	if dnsClient.createCalls != 0 {
		t.Errorf("DNS CreateCNAME called %d time(s), want 0", dnsClient.createCalls)
	}
}

// TestStartupReconcile_ConfigSrcLocal_Aborts covers SC-10 at the
// reconcile-function level: config_src=local must abort before listing
// containers or issuing any PUT.
func TestStartupReconcile_ConfigSrcLocal_Aborts(t *testing.T) {
	docker := newFakeDockerClient(nil)
	cf := newFakeTunnelClient()
	cf.configSrc = "local"
	dnsClient := newFakeDNSSyncer()
	store := state.NewStore()
	cfg := testConfig()

	_, err := startupReconcile(context.Background(), cfg, docker, cf, dnsClient, store)
	if err == nil {
		t.Fatal("startupReconcile() error = nil, want error for config_src=local")
	}

	_, putCalls, _ := cf.snapshot()
	if putCalls != 0 {
		t.Errorf("PutTunnelConfig called %d time(s), want 0", putCalls)
	}
}

// TestStartupReconcile_DNSTrueWithZoneIDSet_CreatesCNAME verifies the
// happy-path DNS create at startup (REQ-5.1/5.2) once CF_ZONE_ID is set.
func TestStartupReconcile_DNSTrueWithZoneIDSet_CreatesCNAME(t *testing.T) {
	docker := newFakeDockerClient([]types.Container{
		containerFixture("c1", "warden", map[string]string{
			"cf.hostname": "warden.vantus.site",
			"cf.port":     "80",
			"cf.dns":      "true",
		}, "cloudflared-network"),
	})

	cf := newFakeTunnelClient()
	dnsClient := newFakeDNSSyncer()
	store := state.NewStore()
	cfg := testConfig()
	cfg.CFZoneID = "zone-123"

	found, err := startupReconcile(context.Background(), cfg, docker, cf, dnsClient, store)
	if err != nil {
		t.Fatalf("startupReconcile() error = %v, want nil", err)
	}
	if found != 1 {
		t.Errorf("startupReconcile() found = %d, want 1", found)
	}

	_, putCalls, _ := cf.snapshot()
	if putCalls != 1 {
		t.Fatalf("PutTunnelConfig called %d time(s), want 1", putCalls)
	}
	if dnsClient.createCalls != 1 {
		t.Errorf("DNS CreateCNAME called %d time(s), want 1", dnsClient.createCalls)
	}
}

// TestStartupReconcile_NoLabeledContainers_StillIssuesCatchAllPut verifies
// that even with zero labeled containers, a reconcile PUT with only the
// catch-all rule is issued (matches Python's unconditional PUT after scan).
func TestStartupReconcile_NoLabeledContainers_StillIssuesCatchAllPut(t *testing.T) {
	docker := newFakeDockerClient([]types.Container{
		containerFixture("c1", "celery-worker", map[string]string{}, "cloudflared-network"),
	})

	cf := newFakeTunnelClient()
	dnsClient := newFakeDNSSyncer()
	store := state.NewStore()
	cfg := testConfig()

	found, err := startupReconcile(context.Background(), cfg, docker, cf, dnsClient, store)
	if err != nil {
		t.Fatalf("startupReconcile() error = %v, want nil", err)
	}
	if found != 0 {
		t.Errorf("startupReconcile() found = %d, want 0", found)
	}

	_, putCalls, lastIngress := cf.snapshot()
	if putCalls != 1 {
		t.Fatalf("PutTunnelConfig called %d time(s), want 1", putCalls)
	}
	if len(lastIngress) != 1 {
		t.Fatalf("PutTunnelConfig ingress = %d rule(s), want 1 (catch-all only)", len(lastIngress))
	}
}
