package watcher

import (
	"context"
	"log"
	"time"

	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"

	"cf-watcher/internal/state"
)

// watchedActions are the Docker container lifecycle events cf-watcher reacts
// to.
var watchedActions = map[events.Action]bool{
	events.ActionStart: true,
	events.ActionStop:  true,
	events.ActionDie:   true,
	events.ActionKill:  true,
}

// Watch subscribes to the Docker daemon's container event stream and
// mutates store accordingly:
//   - on "start": inspect the container, extract cf.* labels, add to store,
//     check network membership
//   - on "stop"/"die"/"kill": remove from store
//
// Every mutation signals dirty (non-blocking send — dirty must be buffered
// with capacity >= 1) so a separate debounce loop (see Debounce) can
// coalesce bursts into a single downstream sync.
//
// Watch blocks until ctx is canceled (returns nil) or the underlying event
// stream reports an error (returned to the caller so main can decide how to
// react — a broken docker.sock is unrecoverable).
func Watch(ctx context.Context, docker DockerClient, store *state.Store, dockerNetwork string, dirty chan<- struct{}) error {
	eventsCh, errCh := docker.Events(ctx, events.ListOptions{
		Filters: filters.NewArgs(filters.Arg("type", string(events.ContainerEventType))),
	})

	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-errCh:
			return err
		case msg, ok := <-eventsCh:
			if !ok {
				return nil
			}
			handleEvent(ctx, docker, store, dockerNetwork, msg, dirty)
		}
	}
}

// handleEvent processes a single Docker event message.
func handleEvent(ctx context.Context, docker DockerClient, store *state.Store, dockerNetwork string, msg events.Message, dirty chan<- struct{}) {
	if !watchedActions[msg.Action] {
		return
	}

	containerID := msg.Actor.ID
	containerName := msg.Actor.Attributes["name"]
	if containerName == "" {
		containerName = containerID
	}

	switch msg.Action {
	case events.ActionStart:
		handleStart(ctx, docker, store, dockerNetwork, containerID, containerName, dirty)
	case events.ActionStop, events.ActionDie, events.ActionKill:
		// Remove from state, mark dirty.
		store.Remove(containerID)
		log.Printf("INFO: container %s (%s) — removed from desired state", containerName, msg.Action)
		signalDirty(dirty)
	}
}

// handleStart inspects a newly-started container, extracts its cf.* labels,
// and — on success — adds it to the desired state and marks dirty.
// A container missing required labels or with an invalid cf.port is excluded
// per ExtractDesiredState's own rules.
func handleStart(ctx context.Context, docker DockerClient, store *state.Store, dockerNetwork string, containerID, containerName string, dirty chan<- struct{}) {
	inspect, err := docker.ContainerInspect(ctx, containerID)
	if err != nil {
		log.Printf("WARNING: container %s: inspect failed on start event — skipping: %v", containerName, err)
		return
	}

	var labels map[string]string
	if inspect.Config != nil {
		labels = inspect.Config.Labels
	}

	entry, ok := ExtractDesiredState(containerID, containerName, labels)
	if !ok {
		return
	}

	// Warn if the labeled container is not on the configured Docker
	// network — the tunnel likely cannot route to it.
	if inspect.NetworkSettings != nil {
		CheckNetworkMembership(containerName, dockerNetwork, inspect.NetworkSettings.Networks)
	}

	store.Add(*entry)
	log.Printf(
		"INFO: container %s started — added ingress rule hostname=%s port=%d service=%s dns=%t",
		containerName, entry.Hostname, entry.Port, entry.Service, entry.DNS,
	)
	signalDirty(dirty)
}

// signalDirty sends a non-blocking signal on dirty. dirty must be a
// buffered channel with capacity >= 1: if a signal is already pending, this
// is a no-op — the debounce loop only cares that at least one dirty
// mutation occurred since its last sync, not how many.
func signalDirty(dirty chan<- struct{}) {
	select {
	case dirty <- struct{}{}:
	default:
	}
}

// Debounce reads dirty signals and calls fn once after debounceWindow of
// inactivity, coalescing bursts into a single call. It runs until ctx is
// canceled.
func Debounce(ctx context.Context, dirty <-chan struct{}, debounceWindow time.Duration, fn func()) {
	timer := time.NewTimer(debounceWindow)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()

	var timerC <-chan time.Time // nil until armed by a dirty signal

	for {
		select {
		case <-ctx.Done():
			return

		case <-dirty:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(debounceWindow)
			timerC = timer.C

		case <-timerC:
			timerC = nil
			fn()
		}
	}
}
