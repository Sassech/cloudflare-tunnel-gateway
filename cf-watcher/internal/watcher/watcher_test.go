package watcher

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/network"

	"cf-watcher/internal/state"
)

// fakeDocker is an in-memory DockerClient for watcher tests: it returns a
// canned ContainerList, streams the exact event/error channels the test
// wires up, and serves ContainerInspect results from an inspect map.
type fakeDocker struct {
	containers []types.Container
	eventsCh   chan events.Message
	errCh      chan error
	inspects   map[string]types.ContainerJSON
}

func newFakeDocker() *fakeDocker {
	return &fakeDocker{
		eventsCh: make(chan events.Message),
		errCh:    make(chan error),
		inspects: make(map[string]types.ContainerJSON),
	}
}

func (f *fakeDocker) ContainerList(_ context.Context, _ container.ListOptions) ([]types.Container, error) {
	return f.containers, nil
}

func (f *fakeDocker) Events(_ context.Context, _ events.ListOptions) (<-chan events.Message, <-chan error) {
	return f.eventsCh, f.errCh
}

func (f *fakeDocker) ContainerInspect(_ context.Context, containerID string) (types.ContainerJSON, error) {
	if inspect, ok := f.inspects[containerID]; ok {
		return inspect, nil
	}
	return types.ContainerJSON{}, errors.New("fake: unknown container " + containerID)
}

// inspectFixture builds a ContainerInspect result carrying the given labels
// and network membership. NetworkSettings can be nil to simulate a container
// whose inspection returned no network settings.
func inspectFixture(labels map[string]string, networks map[string]*network.EndpointSettings) types.ContainerJSON {
	return types.ContainerJSON{
		Config: &container.Config{
			Labels: labels,
		},
		NetworkSettings: &container.NetworkSettings{
			Networks: networks,
		},
	}
}

// labeledContainer builds a fully-labeled inspect fixture (cf.hostname +
// cf.port on the configured network).
func labeledContainer(containerID string, networks map[string]*network.EndpointSettings) types.ContainerJSON {
	return inspectFixture(map[string]string{
		"cf.hostname": "app.example.com",
		"cf.port":     "8080",
	}, networks)
}

func onTestNetwork() map[string]*network.EndpointSettings {
	return map[string]*network.EndpointSettings{
		"cloudflared-network": {},
	}
}

func startMessage(containerID, containerName string) events.Message {
	return events.Message{
		Action: events.ActionStart,
		Actor: events.Actor{
			ID:         containerID,
			Attributes: map[string]string{"name": containerName},
		},
	}
}

// countEntries returns the number of non-catch-all entries in the store by
// counting BuildIngress rules minus the trailing catch-all.
func countEntries(store *state.Store) int {
	return len(store.BuildIngress()) - 1
}

// --- handleEvent ----------------------------------------------------------

// TestHandleEvent_StartAddsEntryAndSignalsDirty verifies the happy path:
// a "start" event for a fully-labeled container on the configured network is
// added to the store, and a dirty signal is emitted.
func TestHandleEvent_StartAddsEntryAndSignalsDirty(t *testing.T) {
	docker := newFakeDocker()
	docker.inspects["c1"] = labeledContainer("c1", onTestNetwork())

	store := state.NewStore()
	dirty := make(chan struct{}, 1)

	handleEvent(context.Background(), docker, store, "cloudflared-network",
		startMessage("c1", "app1"), dirty)

	entries := store.BuildIngress()
	if len(entries) != 2 {
		t.Fatalf("store has %d ingress rule(s), want 2 (entry + catch-all)", len(entries))
	}
	got := entries[0]
	if got["hostname"] != "app.example.com" {
		t.Errorf("store entry hostname = %q, want app.example.com", got["hostname"])
	}
	if got["service"] != "http://app1:8080" {
		t.Errorf("store entry service = %q, want http://app1:8080", got["service"])
	}

	select {
	case <-dirty:
	default:
		t.Error("dirty was not signaled after a start event")
	}
}

// TestHandleEvent_StartMissingLabelsSkips verifies that a container without
// cf.* labels is silently skipped — no store mutation, no dirty signal.
func TestHandleEvent_StartMissingLabelsSkips(t *testing.T) {
	docker := newFakeDocker()
	docker.inspects["c1"] = inspectFixture(map[string]string{"foo": "bar"}, onTestNetwork())

	store := state.NewStore()
	dirty := make(chan struct{}, 1)

	handleEvent(context.Background(), docker, store, "cloudflared-network",
		startMessage("c1", "plain"), dirty)

	if got := countEntries(store); got != 0 {
		t.Errorf("store has %d entr(ies), want 0 (unlabeled container skipped)", got)
	}
	select {
	case <-dirty:
		t.Error("dirty signaled for an unlabeled container, want none")
	default:
	}
}

// TestHandleEvent_StartInspectFailureSkips verifies that a container whose
// inspection fails (e.g. it died between the event and the inspect) is
// skipped without touching the store.
func TestHandleEvent_StartInspectFailureSkips(t *testing.T) {
	docker := newFakeDocker() // no inspect registered for c1 → inspect errors

	store := state.NewStore()
	dirty := make(chan struct{}, 1)

	handleEvent(context.Background(), docker, store, "cloudflared-network",
		startMessage("c1", "app1"), dirty)

	if got := countEntries(store); got != 0 {
		t.Errorf("store has %d entr(ies), want 0 (inspect failure skips)", got)
	}
	select {
	case <-dirty:
		t.Error("dirty signaled after inspect failure, want none")
	default:
	}
}

// TestHandleEvent_StartNetworkMembershipWarning verifies that a labeled
// container NOT on the configured network is still added (the warning is
// non-fatal) — CheckNetworkMembership only logs.
func TestHandleEvent_StartNetworkMembershipWarning(t *testing.T) {
	docker := newFakeDocker()
	docker.inspects["c1"] = labeledContainer("c1", map[string]*network.EndpointSettings{
		"other-network": {},
	})

	store := state.NewStore()
	dirty := make(chan struct{}, 1)

	handleEvent(context.Background(), docker, store, "cloudflared-network",
		startMessage("c1", "app1"), dirty)

	if got := countEntries(store); got != 1 {
		t.Errorf("store has %d entr(ies), want 1 (off-network container still added)", got)
	}
}

// TestHandleEvent_StopRemovesEntryAndSignals verifies that stop/die/kill
// events remove the container from the store and signal dirty.
func TestHandleEvent_StopRemovesEntryAndSignals(t *testing.T) {
	store := state.NewStore()
	store.Add(state.Entry{
		ContainerID: "c1", ContainerName: "app1",
		Hostname: "app.example.com", Port: 8080,
	})
	dirty := make(chan struct{}, 1)

	for _, action := range []events.Action{events.ActionStop, events.ActionDie, events.ActionKill} {
		store.Add(state.Entry{
			ContainerID: "c2-" + string(action), ContainerName: "app2",
			Hostname: "other.example.com", Port: 9090,
		})

		handleEvent(context.Background(), newFakeDocker(), store, "cloudflared-network",
			events.Message{Action: action, Actor: events.Actor{ID: "c2-" + string(action)}}, dirty)

		if got := countEntries(store); got != 1 {
			t.Errorf("after %s: store has %d entr(ies), want 1 (removed)", action, got)
		}
	}

	select {
	case <-dirty:
	default:
		t.Error("dirty was not signaled after stop/die/kill")
	}
}

// TestHandleEvent_UnwatchedActionIgnored verifies that events outside the
// watched set (e.g. exec_create) leave the store untouched and never signal.
func TestHandleEvent_UnwatchedActionIgnored(t *testing.T) {
	store := state.NewStore()
	store.Add(state.Entry{
		ContainerID: "c1", ContainerName: "app1",
		Hostname: "app.example.com", Port: 8080,
	})
	dirty := make(chan struct{}, 1)

	handleEvent(context.Background(), newFakeDocker(), store, "cloudflared-network",
		events.Message{Action: events.ActionExecCreate, Actor: events.Actor{ID: "c1"}}, dirty)

	if got := countEntries(store); got != 1 {
		t.Errorf("store has %d entr(ies), want 1 (unwatched action must not mutate)", got)
	}
	select {
	case <-dirty:
		t.Error("dirty signaled for an unwatched action, want none")
	default:
	}
}

// TestHandleEvent_NameFallbackToID verifies that a start event with no
// "name" attribute falls back to the container ID for display purposes.
func TestHandleEvent_NameFallbackToID(t *testing.T) {
	docker := newFakeDocker()
	docker.inspects["c1"] = labeledContainer("c1", onTestNetwork())

	store := state.NewStore()
	dirty := make(chan struct{}, 1)

	msg := events.Message{
		Action: events.ActionStart,
		Actor:  events.Actor{ID: "c1", Attributes: map[string]string{}}, // no name
	}
	handleEvent(context.Background(), docker, store, "cloudflared-network", msg, dirty)

	entries := store.BuildIngress()
	if len(entries) != 2 {
		t.Fatalf("store has %d ingress rule(s), want 2 (entry + catch-all)", len(entries))
	}
	// The service URL embeds ContainerName — the ID fallback proves the
	// name attribute was absent.
	if got := entries[0]["service"]; got != "http://c1:8080" {
		t.Errorf("service = %q, want fallback to container ID in http://c1:8080", got)
	}
}

// --- Watch ------------------------------------------------------------------

// TestWatch_ProcessesEventAndReturnsOnCancel verifies the full event loop:
// a message pushed through the channel is handled (store mutated), and
// canceling ctx makes Watch return nil.
func TestWatch_ProcessesEventAndReturnsOnCancel(t *testing.T) {
	docker := newFakeDocker()
	docker.inspects["c1"] = labeledContainer("c1", onTestNetwork())

	store := state.NewStore()
	dirty := make(chan struct{}, 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- Watch(ctx, docker, store, "cloudflared-network", dirty)
	}()

	// Let Watch subscribe, then push a start event.
	time.Sleep(20 * time.Millisecond)
	docker.eventsCh <- startMessage("c1", "app1")

	// The event must be processed before we cancel.
	deadline := time.Now().Add(time.Second)
	for countEntries(store) != 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := countEntries(store); got != 1 {
		t.Fatalf("store has %d entr(ies) after start event, want 1", got)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Watch() error = %v, want nil on ctx cancel", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Watch did not return after ctx cancellation")
	}
}

// TestWatch_ReturnsOnEventChannelClose verifies that Watch returns nil when
// the underlying event stream closes cleanly.
func TestWatch_ReturnsOnEventChannelClose(t *testing.T) {
	docker := newFakeDocker()
	close(docker.eventsCh)

	done := make(chan error, 1)
	go func() {
		done <- Watch(context.Background(), docker, state.NewStore(), "cloudflared-network", make(chan struct{}, 1))
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Watch() error = %v, want nil on clean channel close", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Watch did not return after event channel closed")
	}
}

// TestWatch_ReturnsStreamError verifies that an error on the error channel
// is propagated to the caller — main treats a broken docker.sock as fatal.
func TestWatch_ReturnsStreamError(t *testing.T) {
	docker := newFakeDocker()
	streamErr := errors.New("fake: docker daemon disconnected")

	go func() { docker.errCh <- streamErr }()

	err := Watch(context.Background(), docker, state.NewStore(), "cloudflared-network", make(chan struct{}, 1))
	if !errors.Is(err, streamErr) {
		t.Errorf("Watch() error = %v, want stream error %v", err, streamErr)
	}
}

// TestWatch_CtxCanceledBeforeSubscribe verifies Watch handles a ctx already
// canceled at entry, returning nil without blocking.
func TestWatch_CtxCanceledBeforeSubscribe(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan error, 1)
	go func() {
		done <- Watch(ctx, newFakeDocker(), state.NewStore(), "cloudflared-network", make(chan struct{}, 1))
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Watch() error = %v, want nil for canceled ctx", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Watch did not return for already-canceled ctx")
	}
}
