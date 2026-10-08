package health

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandler_HealthzUnavailableUntilReady(t *testing.T) {
	status := NewStatus()
	srv := httptest.NewServer(Handler(status))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz error = %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("GET /healthz status = %d, want 503 before MarkReady", resp.StatusCode)
	}

	status.MarkReady()

	resp2, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz error = %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("GET /healthz status = %d, want 200 after MarkReady", resp2.StatusCode)
	}
}

func TestHandler_HealthzOKBeforeAnySync(t *testing.T) {
	status := NewStatus()
	status.MarkReady()
	srv := httptest.NewServer(Handler(status))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz error = %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /healthz status = %d, want 200 before any sync has run", resp.StatusCode)
	}
}

func TestHandler_HealthzReflectsLastSyncOutcome(t *testing.T) {
	status := NewStatus()
	status.MarkReady()
	status.RecordSync(2, 1, true)

	srv := httptest.NewServer(Handler(status))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz error = %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /healthz status = %d, want 200 after successful sync", resp.StatusCode)
	}

	status.RecordSync(2, 1, false)

	resp2, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz error = %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("GET /healthz status = %d, want 503 after failed sync", resp2.StatusCode)
	}
}

func TestHandler_MetricsReportsCounters(t *testing.T) {
	status := NewStatus()
	status.MarkReady()
	status.RecordSync(3, 2, true)
	status.RecordSync(3, 2, false)

	srv := httptest.NewServer(Handler(status))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics error = %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading /metrics body: %v", err)
	}
	text := string(body)

	for _, want := range []string{
		"cf_watcher_ready 1",
		"cf_watcher_containers_tracked 3",
		"cf_watcher_dns_entries_tracked 2",
		"cf_watcher_sync_total 2",
		"cf_watcher_sync_errors_total 1",
		"cf_watcher_last_sync_success 0",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics output missing %q, got:\n%s", want, text)
		}
	}
}

func TestRecordSync_NilStatusIsNoOp(t *testing.T) {
	var status *Status
	status.RecordSync(1, 1, true) // must not panic
	status.MarkReady()            // must not panic
	if !status.Ready() {
		t.Error("nil Status Ready() = false, want true (tracking disabled)")
	}
}

func TestHandler_NilStatusServesDefaults(t *testing.T) {
	srv := httptest.NewServer(Handler(nil))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /healthz status = %d, want 200 for a nil Status", resp.StatusCode)
	}
}
