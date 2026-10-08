package main

import (
	"context"
	"errors"
	"sync"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/network"
)

// fakeDockerClient is a minimal in-memory implementation of
// watcher.DockerClient for tests — no real Docker daemon connection.
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

// fakeTunnelClient is a minimal in-memory implementation of TunnelManager
// (TunnelClient + TunnelProvisioner).
type fakeTunnelClient struct {
	mu sync.Mutex

	configSrc string // "cloudflare" (default) or "local" to simulate remote config assertion failure

	assertCalls int
	putCalls    int
	lastIngress []map[string]any

	// Provisioning.
	tunnels     map[string]string // name -> ID of live tunnels in the "account"
	ensureErr   error
	ensureCalls int
	createCalls int
	tunnelID    string // set via SetTunnelID
	token       string // returned by GetTunnelToken
	tokenErr    error
	// tokenGate, if non-nil, blocks GetTunnelToken until it is closed —
	// lets tests observe the system mid-provisioning.
	tokenGate chan struct{}
	// calls records the order of tunnel API calls, for ordering asserts.
	calls []string
}

func newFakeTunnelClient() *fakeTunnelClient {
	return &fakeTunnelClient{
		configSrc: "cloudflare",
		tunnels:   make(map[string]string),
		token:     "fake-connector-token",
	}
}

func (f *fakeTunnelClient) EnsureTunnel(_ context.Context, name string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureCalls++
	f.calls = append(f.calls, "EnsureTunnel")
	if f.ensureErr != nil {
		return "", false, f.ensureErr
	}
	if id, ok := f.tunnels[name]; ok {
		return id, false, nil
	}
	f.createCalls++
	id := "created-" + name
	f.tunnels[name] = id
	return id, true, nil
}

func (f *fakeTunnelClient) GetTunnelToken(ctx context.Context, tunnelID string) (string, error) {
	f.mu.Lock()
	gate := f.tokenGate
	f.calls = append(f.calls, "GetTunnelToken")
	f.mu.Unlock()

	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tokenErr != nil {
		return "", f.tokenErr
	}
	if tunnelID == "" {
		return "", errors.New("fake: empty tunnel ID")
	}
	return f.token, nil
}

func (f *fakeTunnelClient) SetTunnelID(tunnelID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "SetTunnelID")
	f.tunnelID = tunnelID
}

func (f *fakeTunnelClient) callLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeTunnelClient) AssertConfigSrc(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.assertCalls++
	f.calls = append(f.calls, "AssertConfigSrc")
	if f.configSrc != "cloudflare" {
		return errFakeLocalConfigSrc
	}
	return nil
}

func (f *fakeTunnelClient) PutTunnelConfig(_ context.Context, ingress []map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.putCalls++
	f.calls = append(f.calls, "PutTunnelConfig")
	f.lastIngress = ingress
	return nil
}

func (f *fakeTunnelClient) snapshot() (assertCalls, putCalls int, lastIngress []map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.assertCalls, f.putCalls, f.lastIngress
}

// fakeDNSSyncer is a minimal in-memory implementation of DNSSyncer. tracked
// doubles as the simulated Cloudflare backend's full set of CNAME records
// for the tunnel — tests can pre-populate it directly to simulate records
// left over from a previous process session (orphans).
type fakeDNSSyncer struct {
	mu      sync.Mutex
	tracked map[string]string

	createCalls int
	deleteCalls int

	// tunnelIDs records every tunnel ID passed to CreateCNAME and
	// ListManagedRecords, to assert the resolved ID is used.
	tunnelIDs []string
}

func newFakeDNSSyncer() *fakeDNSSyncer {
	return &fakeDNSSyncer{tracked: make(map[string]string)}
}

func (f *fakeDNSSyncer) CreateCNAME(_ context.Context, hostname, tunnelID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tunnelIDs = append(f.tunnelIDs, tunnelID)
	if existing, ok := f.tracked[hostname]; ok {
		// Mirrors dns.Client.CreateCNAME: already tracked (created this
		// session or adopted) — idempotent no-op, no new API call.
		return existing, nil
	}
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

func (f *fakeDNSSyncer) ListManagedRecords(_ context.Context, tunnelID string) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tunnelIDs = append(f.tunnelIDs, tunnelID)
	snapshot := make(map[string]string, len(f.tracked))
	for k, v := range f.tracked {
		snapshot[k] = v
	}
	return snapshot, nil
}

func (f *fakeDNSSyncer) seenTunnelIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.tunnelIDs...)
}

func (f *fakeDNSSyncer) AdoptRecord(hostname, recordID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tracked[hostname] = recordID
}

// errFakeLocalConfigSrc simulates the tunnel being in local config mode.
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
