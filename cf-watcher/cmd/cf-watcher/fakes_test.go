package main

import (
	"context"
	"sync"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/network"
)

// fakeDockerClient is a minimal in-memory implementation of
// watcher.DockerClient for tests — no real Docker daemon connection (FR-9).
type fakeDockerClient struct {
	containers []types.Container

	eventsCh chan events.Message
	errCh    chan error

	mu       sync.Mutex
	inspects map[string]types.ContainerJSON // containerID -> inspect result
}

func newFakeDockerClient(containers []types.Container) *fakeDockerClient {
	return &fakeDockerClient{
		containers: containers,
		eventsCh:   make(chan events.Message),
		errCh:      make(chan error, 1),
		inspects:   make(map[string]types.ContainerJSON),
	}
}

func (f *fakeDockerClient) ContainerList(_ context.Context, _ container.ListOptions) ([]types.Container, error) {
	return f.containers, nil
}

func (f *fakeDockerClient) Events(_ context.Context, _ events.ListOptions) (<-chan events.Message, <-chan error) {
	return f.eventsCh, f.errCh
}

// setInspect registers the ContainerInspect result to return for
// containerID — lets tests simulate a live "start" event for a container
// carrying specific cf.* labels.
func (f *fakeDockerClient) setInspect(containerID string, result types.ContainerJSON) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inspects[containerID] = result
}

func (f *fakeDockerClient) ContainerInspect(_ context.Context, containerID string) (types.ContainerJSON, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if result, ok := f.inspects[containerID]; ok {
		return result, nil
	}
	return types.ContainerJSON{}, nil
}

// fakeTunnelClient is a minimal in-memory implementation of TunnelClient.
type fakeTunnelClient struct {
	mu sync.Mutex

	configSrc string // "cloudflare" (default) or "local" to simulate SC-10

	assertCalls int
	putCalls    int
	lastIngress []map[string]any
}

func newFakeTunnelClient() *fakeTunnelClient {
	return &fakeTunnelClient{configSrc: "cloudflare"}
}

func (f *fakeTunnelClient) AssertConfigSrc(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.assertCalls++
	if f.configSrc != "cloudflare" {
		return errFakeLocalConfigSrc
	}
	return nil
}

func (f *fakeTunnelClient) PutTunnelConfig(_ context.Context, ingress []map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.putCalls++
	f.lastIngress = ingress
	return nil
}

func (f *fakeTunnelClient) snapshot() (assertCalls, putCalls int, lastIngress []map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.assertCalls, f.putCalls, f.lastIngress
}

// fakeDNSSyncer is a minimal in-memory implementation of DNSSyncer.
type fakeDNSSyncer struct {
	mu      sync.Mutex
	tracked map[string]string

	createCalls int
	deleteCalls int
}

func newFakeDNSSyncer() *fakeDNSSyncer {
	return &fakeDNSSyncer{tracked: make(map[string]string)}
}

func (f *fakeDNSSyncer) CreateCNAME(_ context.Context, hostname, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createCalls++
	id := "rec-" + hostname
	f.tracked[hostname] = id
	return id, nil
}

func (f *fakeDNSSyncer) DeleteCNAME(_ context.Context, hostname string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteCalls++
	delete(f.tracked, hostname)
	return nil
}

func (f *fakeDNSSyncer) TrackedHostnames() map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	snapshot := make(map[string]string, len(f.tracked))
	for k, v := range f.tracked {
		snapshot[k] = v
	}
	return snapshot
}

// errFakeLocalConfigSrc simulates SC-10 (tunnel in local config mode).
type fakeLocalConfigSrcError struct{}

func (fakeLocalConfigSrcError) Error() string {
	return "fake: tunnel is in local config mode"
}

var errFakeLocalConfigSrc error = fakeLocalConfigSrcError{}

// containerFixture builds a types.Container (container.Summary) with the
// given labels and network membership, for use in ContainerList fixtures.
func containerFixture(id, name string, labels map[string]string, networks ...string) types.Container {
	netSettings := &container.NetworkSettingsSummary{}
	if len(networks) > 0 {
		netSettings.Networks = make(map[string]*network.EndpointSettings)
		for _, n := range networks {
			netSettings.Networks[n] = &network.EndpointSettings{}
		}
	}
	return types.Container{
		ID:              id,
		Names:           []string{"/" + name},
		Labels:          labels,
		NetworkSettings: netSettings,
	}
}
