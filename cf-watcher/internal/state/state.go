// Package state maintains the in-memory desired-state store for cf-watcher
// and produces the Cloudflare Tunnel ingress list on demand.
package state

import (
	"fmt"
	"sync"
)

// Entry is a single container's desired Cloudflare Tunnel routing
// configuration.
type Entry struct {
	ContainerID   string
	ContainerName string
	Hostname      string
	Port          int
	Service       string
	DNS           bool
}

// Store is a thread-safe in-memory map of container ID to desired Entry.
// The zero value is not usable — construct with NewStore.
type Store struct {
	mu      sync.Mutex
	entries map[string]Entry
}

// NewStore returns an empty, ready-to-use Store.
func NewStore() *Store {
	return &Store{entries: make(map[string]Entry)}
}

// Add adds or replaces the entry for e.ContainerID.
func (s *Store) Add(e Entry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[e.ContainerID] = e
}

// Remove removes the entry for containerID. It is a no-op if containerID is
// not currently tracked.
func (s *Store) Remove(containerID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, containerID)
}

// Clear removes all tracked entries, resetting the store to empty. Used by
// POST /reload to fully resync state from the live Docker daemon,
// discarding any entry that no longer corresponds to a currently running
// container (e.g. one that lost its cf.* labels without a stop/start
// event).
func (s *Store) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = make(map[string]Entry)
}

// DNSEntries returns a snapshot of all currently tracked entries with
// DNS=true (cf.dns=true label) — the set of hostnames that require a
// managed Cloudflare CNAME record. Order is unspecified.
func (s *Store) DNSEntries() []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []Entry
	for _, e := range s.entries {
		if e.DNS {
			out = append(out, e)
		}
	}
	return out
}

// BuildIngress builds the full Cloudflare Tunnel ingress list from the
// current desired state. The catch-all {"service": "http_status:404"} is
// always the last element — the Cloudflare API rejects the config without it.
// Service URL format: "{service}://{container_name}:{port}".
func (s *Store) BuildIngress() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()

	rules := make([]map[string]any, 0, len(s.entries)+1)
	for _, e := range s.entries {
		rules = append(rules, map[string]any{
			"hostname": e.Hostname,
			"service":  fmt.Sprintf("%s://%s:%d", e.Service, e.ContainerName, e.Port),
		})
	}
	rules = append(rules, map[string]any{"service": "http_status:404"})
	return rules
}
