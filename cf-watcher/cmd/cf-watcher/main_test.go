package main

import (
	"context"
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
	cfg := testConfig()

	err := run(context.Background(), cfg, docker, cf, dnsClient)
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

	cfg := testConfig()
	cfg.DebounceSeconds = 0.05 // short window for a fast test

	ctx, cancel := context.WithCancel(context.Background())

	runErrCh := make(chan error, 1)
	go func() {
		runErrCh <- run(ctx, cfg, docker, cf, dnsClient)
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
