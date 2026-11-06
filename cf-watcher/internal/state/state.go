// Package state maintains the in-memory desired-state store for cf-watcher
// and produces the Cloudflare Tunnel ingress list on demand.
package state

import (
	"fmt"
	"sort"
	"sync"

	"cf-watcher/internal/logging"
)

// Entry is a single container's desired Cloudflare Tunnel routing
// configuration.
type Entry struct {
	ContainerID   string
	ContainerName string
	Hostname      string
	Port          int
	Service       string
	// Origin is the cf.origin override for the ingress origin host — the
	// shared Docker network alias used so replicas sharing a hostname route
	// to one origin. Empty means "derive from ContainerName".
	Origin string
	DNS    bool
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

// BuildIngress builds the full Cloudflare Tunnel ingress list from the current
// desired state. Entries that share a hostname are collapsed into a single
// ingress rule: Cloudflare Tunnel ingress matches one rule per hostname (first
// match wins), so emitting duplicate hostname rules would leave all but one
// dead. Replicas that share a hostname and a cf.origin alias therefore resolve
// to one origin, and Docker's embedded DNS round-robins across them.
//
// Output is deterministic — rules are sorted by hostname and the catch-all
// {"service": "http_status:404"} is always last (the Cloudflare API rejects
// the config without it). Service URL format: "{service}://{origin}:{port}",
// where origin is the entry's cf.origin override or its container name.
//
// When entries share a hostname but resolve to different service URLs — a
// misconfiguration, since replicas should share one cf.origin — the
// lexicographically smallest URL is chosen deterministically and a WARNING is
// logged naming the conflict.
func (s *Store) BuildIngress() []map[string]any {
	s.mu.Lock()
	entries := make([]Entry, 0, len(s.entries))
	for _, e := range s.entries {
		entries = append(entries, e)
	}
	s.mu.Unlock()

	byHostname := make(map[string][]string, len(entries))
	for _, e := range entries {
		byHostname[e.Hostname] = append(byHostname[e.Hostname], serviceURL(e))
	}

	hostnames := make([]string, 0, len(byHostname))
	for h := range byHostname {
		hostnames = append(hostnames, h)
	}
	sort.Strings(hostnames)

	rules := make([]map[string]any, 0, len(hostnames)+1)
	for _, hostname := range hostnames {
		urls := byHostname[hostname]
		sort.Strings(urls)
		chosen := urls[0]
		if distinct := dedupeSorted(urls); len(distinct) > 1 {
			logging.Warningf(
				"ingress: hostname %s maps to conflicting origins %v — using %s; configure a shared cf.origin / Docker network alias for replicas",
				hostname, distinct, chosen,
			)
		}
		rules = append(rules, map[string]any{
			"hostname": hostname,
			"service":  chosen,
		})
	}
	rules = append(rules, map[string]any{"service": "http_status:404"})
	return rules
}

// serviceURL builds an entry's Cloudflare Tunnel origin URL
// "{service}://{origin}:{port}". The origin host is the entry's cf.origin
// override when set, otherwise its container name.
func serviceURL(e Entry) string {
	host := e.Origin
	if host == "" {
		host = e.ContainerName
	}
	return fmt.Sprintf("%s://%s:%d", e.Service, host, e.Port)
}

// dedupeSorted returns the distinct values of an already-sorted slice, without
// mutating the input.
func dedupeSorted(sorted []string) []string {
	out := make([]string, 0, len(sorted))
	for i, v := range sorted {
		if i == 0 || v != sorted[i-1] {
			out = append(out, v)
		}
	}
	return out
}
