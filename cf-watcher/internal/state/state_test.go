package state

import "testing"

func TestStore_BuildIngress_EmptyStore(t *testing.T) {
	// Mirrors the state-side effect of SC-09 (invalid cf.port): when a
	// container is excluded before ever reaching Store.Add, the ingress
	// list contains only the catch-all rule.
	s := NewStore()

	got := s.BuildIngress()

	want := []map[string]any{
		{"service": "http_status:404"},
	}
	assertIngressEqual(t, got, want)
}

func TestStore_AddThenBuildIngress(t *testing.T) {
	// SC-01: happy path — a labeled container's entry appears in the
	// ingress list with the correct service URL, and the catch-all rule is
	// always last.
	s := NewStore()

	s.Add(Entry{
		ContainerID:   "c1",
		ContainerName: "app",
		Hostname:      "app.vantus.site",
		Port:          8080,
		Service:       "http",
		DNS:           false,
	})

	got := s.BuildIngress()

	want := []map[string]any{
		{"hostname": "app.vantus.site", "service": "http://app:8080"},
		{"service": "http_status:404"},
	}
	assertIngressEqual(t, got, want)

	if len(got) == 0 || got[len(got)-1]["service"] != "http_status:404" {
		t.Fatalf("BuildIngress() catch-all rule must be last, got %+v", got)
	}
}

func TestStore_RemoveThenBuildIngress(t *testing.T) {
	// SC-03: container stops — its ingress rule is removed, leaving only
	// the catch-all.
	s := NewStore()
	s.Add(Entry{
		ContainerID:   "c1",
		ContainerName: "app",
		Hostname:      "app.vantus.site",
		Port:          8080,
		Service:       "http",
	})

	s.Remove("c1")

	got := s.BuildIngress()
	want := []map[string]any{
		{"service": "http_status:404"},
	}
	assertIngressEqual(t, got, want)
}

func TestStore_Remove_UntrackedContainerIsNoOp(t *testing.T) {
	s := NewStore()
	s.Add(Entry{ContainerID: "c1", ContainerName: "app", Hostname: "app.vantus.site", Port: 8080, Service: "http"})

	s.Remove("does-not-exist")

	got := s.BuildIngress()
	if len(got) != 2 {
		t.Fatalf("BuildIngress() = %d rules after no-op remove, want 2 (entry + catch-all)", len(got))
	}
}

func TestStore_BuildIngress_CatchAllAlwaysLast_MultipleEntries(t *testing.T) {
	s := NewStore()
	s.Add(Entry{ContainerID: "c1", ContainerName: "app", Hostname: "app.vantus.site", Port: 8080, Service: "http"})
	s.Add(Entry{ContainerID: "c2", ContainerName: "warden", Hostname: "warden.vantus.site", Port: 80, Service: "https", DNS: true})

	got := s.BuildIngress()

	if len(got) != 3 {
		t.Fatalf("BuildIngress() = %d rules, want 3 (2 entries + catch-all)", len(got))
	}
	last := got[len(got)-1]
	if last["service"] != "http_status:404" || len(last) != 1 {
		t.Fatalf("BuildIngress() last rule = %+v, want catch-all only", last)
	}
}

func TestStore_Clear(t *testing.T) {
	s := NewStore()
	s.Add(Entry{ContainerID: "c1", ContainerName: "app", Hostname: "app.vantus.site", Port: 8080, Service: "http"})
	s.Add(Entry{ContainerID: "c2", ContainerName: "warden", Hostname: "warden.vantus.site", Port: 80, Service: "https"})

	s.Clear()

	got := s.BuildIngress()
	want := []map[string]any{
		{"service": "http_status:404"},
	}
	assertIngressEqual(t, got, want)
}

func TestStore_DNSEntries(t *testing.T) {
	// Only entries with DNS=true must be returned, used by the startup
	// CF_ZONE_ID guard and the sync loop's DNS diff.
	s := NewStore()
	s.Add(Entry{ContainerID: "c1", ContainerName: "app", Hostname: "app.vantus.site", Port: 8080, Service: "http", DNS: false})
	s.Add(Entry{ContainerID: "c2", ContainerName: "warden", Hostname: "warden.vantus.site", Port: 80, Service: "https", DNS: true})

	got := s.DNSEntries()

	if len(got) != 1 {
		t.Fatalf("DNSEntries() = %d entries, want 1", len(got))
	}
	if got[0].Hostname != "warden.vantus.site" {
		t.Errorf("DNSEntries()[0].Hostname = %q, want %q", got[0].Hostname, "warden.vantus.site")
	}
}

func TestStore_DNSEntries_Empty(t *testing.T) {
	s := NewStore()
	s.Add(Entry{ContainerID: "c1", ContainerName: "app", Hostname: "app.vantus.site", Port: 8080, Service: "http", DNS: false})

	got := s.DNSEntries()
	if len(got) != 0 {
		t.Fatalf("DNSEntries() = %d entries, want 0", len(got))
	}
}

// assertIngressEqual compares two ingress rule slices for equality,
// ignoring rule order except for the mandatory catch-all-last invariant
// (already asserted separately where relevant).
func assertIngressEqual(t *testing.T, got, want []map[string]any) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("BuildIngress() = %+v, want %+v (length mismatch)", got, want)
	}
	for i := range want {
		if len(got[i]) != len(want[i]) {
			t.Fatalf("BuildIngress()[%d] = %+v, want %+v", i, got[i], want[i])
		}
		for k, v := range want[i] {
			if got[i][k] != v {
				t.Fatalf("BuildIngress()[%d][%q] = %v, want %v", i, k, got[i][k], v)
			}
		}
	}
}
