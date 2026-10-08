package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/docker/docker/api/types"

	"cf-watcher/internal/health"
)

func TestProvisionTunnel(t *testing.T) {
	tests := []struct {
		name           string
		configuredID   string
		existing       map[string]string // live tunnels by name
		ensureErr      error
		tokenErr       error
		wantID         string
		wantErr        bool
		wantEnsure     int
		wantCreate     int
		wantTokenWrite bool
	}{
		{
			name:           "configured CF_TUNNEL_ID skips lookup and creation",
			configuredID:   "pinned-id",
			wantID:         "pinned-id",
			wantEnsure:     0,
			wantTokenWrite: true,
		},
		{
			name:           "reuses an existing tunnel by name",
			existing:       map[string]string{"cf-watcher-vps1": "existing-id"},
			wantID:         "existing-id",
			wantEnsure:     1,
			wantCreate:     0,
			wantTokenWrite: true,
		},
		{
			name:           "creates the tunnel when missing",
			wantID:         "created-cf-watcher-vps1",
			wantEnsure:     1,
			wantCreate:     1,
			wantTokenWrite: true,
		},
		{
			name:       "EnsureTunnel failure aborts before any token write",
			ensureErr:  errors.New("multiple tunnels"),
			wantErr:    true,
			wantEnsure: 1,
		},
		{
			name:       "token fetch failure aborts without writing a file",
			tokenErr:   errors.New("403"),
			wantErr:    true,
			wantEnsure: 1,
			wantCreate: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cf := newFakeTunnelClient()
			for k, v := range tt.existing {
				cf.tunnels[k] = v
			}
			cf.ensureErr = tt.ensureErr
			cf.tokenErr = tt.tokenErr

			cfg := runTestConfig(t)
			cfg.CFTunnelID = tt.configuredID
			cfg.CFTunnelName = "cf-watcher-vps1"

			id, err := provisionTunnel(context.Background(), cfg, cf)
			if tt.wantErr != (err != nil) {
				t.Fatalf("provisionTunnel() error = %v, wantErr %v", err, tt.wantErr)
			}
			if id != tt.wantID {
				t.Errorf("provisionTunnel() id = %q, want %q", id, tt.wantID)
			}
			if cf.ensureCalls != tt.wantEnsure || cf.createCalls != tt.wantCreate {
				t.Errorf("ensure/create calls = %d/%d, want %d/%d", cf.ensureCalls, cf.createCalls, tt.wantEnsure, tt.wantCreate)
			}

			got, readErr := os.ReadFile(cfg.TunnelTokenFile)
			if tt.wantTokenWrite {
				if readErr != nil || string(got) != cf.token {
					t.Errorf("token file = %q (err %v), want %q", got, readErr, cf.token)
				}
				if cf.tunnelID != tt.wantID {
					t.Errorf("SetTunnelID = %q, want %q", cf.tunnelID, tt.wantID)
				}
			} else if !os.IsNotExist(readErr) {
				t.Errorf("token file exists (err %v), want none on failure", readErr)
			}
		})
	}
}

// TestRun_TokenWrittenBeforeHealthy verifies the startup ordering
// cloudflared relies on: /healthz never reports 200 before the connector
// token file exists, and the tunnel is resolved and its token written
// before any tunnel-scoped API call (AssertConfigSrc, PutTunnelConfig).
func TestRun_TokenWrittenBeforeHealthy(t *testing.T) {
	const addr = "127.0.0.1:18098" // fixed port, only this test binds it

	docker := newFakeDockerClient(nil)
	cf := newFakeTunnelClient()
	cf.tokenGate = make(chan struct{})
	dnsClient := newFakeDNSSyncer()
	status := health.NewStatus()

	cfg := runTestConfig(t)
	cfg.CFTunnelID = ""
	cfg.CFTunnelName = "cf-watcher-vps1"
	cfg.HealthAddr = addr

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runErrCh := make(chan error, 1)
	go func() {
		runErrCh <- run(ctx, cfg, docker, cf, dnsClient, status)
	}()

	// Provisioning is blocked on the token fetch: not ready, no file, and
	// /healthz must not answer 200.
	waitForCondition(t, time.Second, func() bool {
		log := cf.callLog()
		return len(log) > 0 && log[len(log)-1] == "GetTunnelToken"
	})
	for i := 0; i < 5; i++ {
		if resp, err := http.Get("http://" + addr + "/healthz"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				t.Fatal("GET /healthz = 200 before the token file was written")
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if status.Ready() {
		t.Fatal("status Ready() = true before the token file was written")
	}
	if _, err := os.Stat(cfg.TunnelTokenFile); !os.IsNotExist(err) {
		t.Fatalf("token file exists before the token was fetched (err %v)", err)
	}

	close(cf.tokenGate)

	waitForCondition(t, 2*time.Second, func() bool {
		resp, err := http.Get("http://" + addr + "/healthz")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})
	if got, err := os.ReadFile(cfg.TunnelTokenFile); err != nil || string(got) != cf.token {
		t.Fatalf("token file = %q (err %v), want %q once healthy", got, err, cf.token)
	}

	waitForCondition(t, time.Second, func() bool {
		_, putCalls, _ := cf.snapshot()
		return putCalls >= 1
	})
	want := []string{"EnsureTunnel", "SetTunnelID", "GetTunnelToken", "AssertConfigSrc", "PutTunnelConfig"}
	log := cf.callLog()
	for i, w := range want {
		if i >= len(log) || log[i] != w {
			t.Fatalf("tunnel call order = %v, want prefix %v", log, want)
		}
	}

	cancel()
	select {
	case err := <-runErrCh:
		if err != nil {
			t.Fatalf("run() error = %v, want nil after ctx cancellation", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("run() did not return after ctx cancellation")
	}
}

// TestRun_ProvisioningFailure_NeverReady verifies run() aborts on a
// provisioning failure without becoming ready or touching the ingress.
func TestRun_ProvisioningFailure_NeverReady(t *testing.T) {
	cf := newFakeTunnelClient()
	cf.ensureErr = errors.New("2 tunnels named cf-watcher-vps1")
	status := health.NewStatus()

	cfg := runTestConfig(t)
	cfg.CFTunnelID = ""
	cfg.CFTunnelName = "cf-watcher-vps1"

	err := run(context.Background(), cfg, newFakeDockerClient(nil), cf, newFakeDNSSyncer(), status)
	if err == nil {
		t.Fatal("run() error = nil, want provisioning error")
	}
	if status.Ready() {
		t.Error("status Ready() = true after provisioning failure, want false")
	}
	if assertCalls, putCalls, _ := cf.snapshot(); assertCalls != 0 || putCalls != 0 {
		t.Errorf("assert/put calls = %d/%d, want 0/0 after provisioning failure", assertCalls, putCalls)
	}
}

// TestRun_ResolvedTunnelIDUsedForDNS verifies that a tunnel provisioned by
// name (CF_TUNNEL_ID unset) is the one used as the CNAME target and for
// orphan cleanup.
func TestRun_ResolvedTunnelIDUsedForDNS(t *testing.T) {
	docker := newFakeDockerClient([]types.Container{
		containerFixture("c1", "app", map[string]string{
			"cf.hostname": "app.vantus.site",
			"cf.port":     "8080",
			"cf.dns":      "true",
		}, "cloudflared-network"),
	})
	cf := newFakeTunnelClient()
	dnsClient := newFakeDNSSyncer()

	cfg := runTestConfig(t)
	cfg.CFTunnelID = ""
	cfg.CFTunnelName = "cf-watcher-vps1"
	cfg.CFZoneID = "zone"

	ctx, cancel := context.WithCancel(context.Background())
	runErrCh := make(chan error, 1)
	go func() {
		runErrCh <- run(ctx, cfg, docker, cf, dnsClient, nil)
	}()

	waitForCondition(t, time.Second, func() bool {
		return len(dnsClient.seenTunnelIDs()) >= 2 // ListManagedRecords + CreateCNAME
	})
	cancel()
	<-runErrCh

	for _, id := range dnsClient.seenTunnelIDs() {
		if id != "created-cf-watcher-vps1" {
			t.Errorf("DNS call used tunnel ID %q, want the resolved %q", id, "created-cf-watcher-vps1")
		}
	}
	if cfg.CFTunnelID != "" {
		t.Errorf("caller's cfg.CFTunnelID mutated to %q, want it left untouched", cfg.CFTunnelID)
	}
}
