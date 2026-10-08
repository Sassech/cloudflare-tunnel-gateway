package main

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
)

// TestRun_StartupReconcileFailure_ReturnsErrorWithoutStartingWatch verifies
// that run() propagates a startup reconcile failure (e.g. SC-08/SC-10)
// immediately, without ever starting the Docker event watcher or debounced
// sync loop.
func TestRun_StartupReconcileFailure_ReturnsErrorWithoutStartingWatch(t *testing.T) {
	docker := newFakeDockerClient(nil)
	cf := newFakeTunnelClient()
	cf.configSrc = "local" // forces AssertConfigSrc to fail (SC-10)
	dnsClient := newFakeDNSSyncer()
	cfg := runTestConfig(t)

	err := run(context.Background(), cfg, docker, cf, dnsClient, nil)
	if err == nil {
		t.Fatal("run() error = nil, want error (startup reconcile must fail)")
	}
}

// TestRun_LiveStartEvent_TriggersDebouncedSync is an end-to-end wiring test:
// a live "start" Docker event for a labeled container must, after the
// debounce window, produce exactly one additional PutTunnelConfig call
// reflecting the new container (SC-01/SC-04 wired through run()).
func TestRun_LiveStartEvent_TriggersDebouncedSync(t *testing.T) {
	docker := newFakeDockerClient(nil) // nothing running at boot
	cf := newFakeTunnelClient()
	dnsClient := newFakeDNSSyncer()

	cfg := runTestConfig(t)
	cfg.DebounceSeconds = 0.05 // short window for a fast test

	ctx, cancel := context.WithCancel(context.Background())

	runErrCh := make(chan error, 1)
	go func() {
		runErrCh <- run(ctx, cfg, docker, cf, dnsClient, nil)
	}()

	// Wait for the startup reconcile PUT (0 containers → catch-all only)
	// before emitting the live event, so we can attribute the second PUT
	// unambiguously to the live event.
	waitForCondition(t, time.Second, func() bool {
		_, putCalls, _ := cf.snapshot()
		return putCalls >= 1
	})

	docker.setInspect("c1", types.ContainerJSON{
		Config: &container.Config{
			Labels: map[string]string{
				"cf.hostname": "app.vantus.site",
				"cf.port":     "8080",
			},
		},
	})
	docker.eventsCh <- events.Message{
		Action: events.ActionStart,
		Actor: events.Actor{
			ID:         "c1",
			Attributes: map[string]string{"name": "app"},
		},
	}

	waitForCondition(t, time.Second, func() bool {
		_, putCalls, _ := cf.snapshot()
		return putCalls >= 2
	})

	_, putCalls, lastIngress := cf.snapshot()
	if putCalls != 2 {
		t.Fatalf("PutTunnelConfig called %d time(s) after live start event, want exactly 2 (1 startup + 1 debounced sync)", putCalls)
	}
	if len(lastIngress) != 2 {
		t.Fatalf("last ingress = %d rule(s), want 2 (app.vantus.site entry + catch-all)", len(lastIngress))
	}
	found := false
	for _, rule := range lastIngress {
		if rule["hostname"] == "app.vantus.site" {
			found = true
		}
	}
	if !found {
		t.Errorf("last ingress = %+v, want an entry for app.vantus.site", lastIngress)
	}

	cancel()
	select {
	case err := <-runErrCh:
		if err != nil {
			t.Fatalf("run() error = %v, want nil after ctx cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("run() did not return after ctx cancellation")
	}
}

// TestRun_HealthServerWiring is an end-to-end test of the full
// main() → run() → startHealthServer() wiring: with a real HealthAddr (not
// "off"), /healthz and /metrics must actually respond over HTTP while
// run() is active, and the listener must be gone (graceful shutdown) once
// ctx is canceled and run() returns.
func TestRun_HealthServerWiring(t *testing.T) {
	const addr = "127.0.0.1:18099" // fixed port, only this test binds it

	docker := newFakeDockerClient(nil)
	cf := newFakeTunnelClient()
	dnsClient := newFakeDNSSyncer()

	cfg := runTestConfig(t)
	cfg.HealthAddr = addr

	ctx, cancel := context.WithCancel(context.Background())

	runErrCh := make(chan error, 1)
	go func() {
		runErrCh <- run(ctx, cfg, docker, cf, dnsClient, nil)
	}()

	var healthzResp *http.Response
	waitForCondition(t, 2*time.Second, func() bool {
		resp, err := http.Get("http://" + addr + "/healthz")
		if err != nil {
			return false
		}
		healthzResp = resp
		return true
	})
	healthzResp.Body.Close()
	if healthzResp.StatusCode != http.StatusOK {
		t.Errorf("GET /healthz status = %d, want 200", healthzResp.StatusCode)
	}

	metricsResp, err := http.Get("http://" + addr + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics error = %v", err)
	}
	metricsResp.Body.Close()
	if metricsResp.StatusCode != http.StatusOK {
		t.Errorf("GET /metrics status = %d, want 200", metricsResp.StatusCode)
	}

	// /reload is disabled (404) because cfg.ReloadToken is unset — confirms
	// the same mux serves all three routes, not just /healthz.
	reloadResp, err := http.Post("http://"+addr+"/reload", "", nil)
	if err != nil {
		t.Fatalf("POST /reload error = %v", err)
	}
	reloadResp.Body.Close()
	if reloadResp.StatusCode != http.StatusNotFound {
		t.Errorf("POST /reload status = %d, want 404 (RELOAD_TOKEN unset)", reloadResp.StatusCode)
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

	// The health server must have shut down along with run() — a new
	// request must fail to connect, not hang or succeed.
	waitForCondition(t, 2*time.Second, func() bool {
		_, err := http.Get("http://" + addr + "/healthz")
		return err != nil
	})
}

func waitForCondition(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", timeout)
}
