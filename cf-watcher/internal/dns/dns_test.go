package dns

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"cf-watcher/internal/config"
)

// newTestClient builds a Client pointed at server, bypassing NewClient's
// hardcoded production base URL.
func newTestClient(server *httptest.Server) *Client {
	c := NewClient(&config.Config{
		CFAPIToken: "test-token",
		CFZoneID:   "test-zone",
	})
	c.baseURL = server.URL
	return c
}

// assertCreatePayload decodes a CNAME create POST body and asserts the
// payload fields cf-watcher must send (REQ-4.x): type=CNAME and content
// pointing at {tunnelID}.cfargotunnel.com, proxied.
func assertCreatePayload(t *testing.T, r *http.Request, tunnelID string) {
	t.Helper()
	var body map[string]any
	json.NewDecoder(r.Body).Decode(&body)
	if body["type"] != "CNAME" {
		t.Errorf("POST body type = %v, want CNAME", body["type"])
	}
	wantContent := tunnelID + ".cfargotunnel.com"
	if body["content"] != wantContent {
		t.Errorf("POST body content = %v, want %q", body["content"], wantContent)
	}
	if body["proxied"] != true {
		t.Errorf("POST body proxied = %v, want true", body["proxied"])
	}
}

// TestCreateThenDeleteCNAME_RoundTrip verifies that creating a CNAME tracks
// its record ID in session memory, and deleting it later uses that tracked
// ID and removes the tracking entry.
func TestCreateThenDeleteCNAME_RoundTrip(t *testing.T) {
	const hostname = "warden.vantus.site"
	const tunnelID = "tunnel-abc"
	const wantRecordID = "rec-123"

	var deleteCalls int32

	mux := http.NewServeMux()
	mux.HandleFunc("/zones/test-zone/dns_records", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			// No existing record — safe to create.
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]any{"success": true, "result": []any{}})
		case http.MethodPost:
			assertCreatePayload(t, r, tunnelID)
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"result":  map[string]any{"id": wantRecordID},
			})
		default:
			t.Errorf("unexpected method %s on /dns_records", r.Method)
		}
	})
	mux.HandleFunc("/zones/test-zone/dns_records/"+wantRecordID, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("unexpected method %s on /dns_records/%s", r.Method, wantRecordID)
			return
		}
		atomic.AddInt32(&deleteCalls, 1)
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{"success": true})
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	client := newTestClient(server)
	ctx := context.Background()

	recordID, err := client.CreateCNAME(ctx, hostname, tunnelID)
	if err != nil {
		t.Fatalf("CreateCNAME() error = %v, want nil", err)
	}
	if recordID != wantRecordID {
		t.Fatalf("CreateCNAME() record ID = %q, want %q", recordID, wantRecordID)
	}

	client.mu.Lock()
	tracked, ok := client.tracked[hostname]
	client.mu.Unlock()
	if !ok || tracked != wantRecordID {
		t.Fatalf("after CreateCNAME, tracked[%q] = (%q, %v), want (%q, true)", hostname, tracked, ok, wantRecordID)
	}

	if err := client.DeleteCNAME(ctx, hostname); err != nil {
		t.Fatalf("DeleteCNAME() error = %v, want nil", err)
	}

	if got := atomic.LoadInt32(&deleteCalls); got != 1 {
		t.Errorf("server received %d DELETE call(s), want exactly 1", got)
	}

	client.mu.Lock()
	_, stillTracked := client.tracked[hostname]
	client.mu.Unlock()
	if stillTracked {
		t.Errorf("after DeleteCNAME, hostname %q is still tracked, want untracked", hostname)
	}
}

// TestCreateCNAME_SkipsExistingUntrackedRecord verifies that a record which
// exists but was not created by cf-watcher in this session is skipped (not
// adopted into tracking, and never deleted later).
func TestCreateCNAME_SkipsExistingUntrackedRecord(t *testing.T) {
	const hostname = "preexisting.vantus.site"
	const tunnelID = "tunnel-abc"

	var postCalls, deleteCalls int32

	mux := http.NewServeMux()
	mux.HandleFunc("/zones/test-zone/dns_records", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"result":  []any{map[string]any{"id": "existing-rec-id"}},
			})
		case http.MethodPost:
			atomic.AddInt32(&postCalls, 1)
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]any{"success": true, "result": map[string]any{"id": "should-not-be-created"}})
		}
	})
	mux.HandleFunc("/zones/test-zone/dns_records/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			atomic.AddInt32(&deleteCalls, 1)
		}
		w.WriteHeader(http.StatusOK)
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	client := newTestClient(server)
	ctx := context.Background()

	recordID, err := client.CreateCNAME(ctx, hostname, tunnelID)
	if err != nil {
		t.Fatalf("CreateCNAME() error = %v, want nil (skip is not an error)", err)
	}
	if recordID != "" {
		t.Errorf("CreateCNAME() record ID = %q, want empty (skipped, not adopted)", recordID)
	}
	if got := atomic.LoadInt32(&postCalls); got != 0 {
		t.Errorf("server received %d POST call(s), want 0 (must not create when record exists untracked)", got)
	}

	client.mu.Lock()
	_, tracked := client.tracked[hostname]
	client.mu.Unlock()
	if tracked {
		t.Errorf("hostname %q is tracked after skip, want untracked", hostname)
	}

	// Since the record was never adopted into session tracking, DeleteCNAME
	// must be a no-op — cf-watcher must never delete a record it did not
	// create.
	if err := client.DeleteCNAME(ctx, hostname); err != nil {
		t.Fatalf("DeleteCNAME() error = %v, want nil", err)
	}
	if got := atomic.LoadInt32(&deleteCalls); got != 0 {
		t.Errorf("server received %d DELETE call(s), want 0 (must not delete untracked record)", got)
	}
}

// TestListManagedRecords_ReturnsHostnameToRecordIDMap verifies that
// ListManagedRecords queries by content=<tunnelID>.cfargotunnel.com and
// maps each result's name to its record ID.
func TestListManagedRecords_ReturnsHostnameToRecordIDMap(t *testing.T) {
	const tunnelID = "tunnel-abc"

	mux := http.NewServeMux()
	mux.HandleFunc("/zones/test-zone/dns_records", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("unexpected method %s", r.Method)
			return
		}
		wantContent := tunnelID + ".cfargotunnel.com"
		if got := r.URL.Query().Get("content"); got != wantContent {
			t.Errorf("query content = %q, want %q", got, wantContent)
		}
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"result": []any{
				map[string]any{"id": "rec-1", "name": "a.vantus.site"},
				map[string]any{"id": "rec-2", "name": "b.vantus.site"},
			},
		})
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	client := newTestClient(server)
	records, err := client.ListManagedRecords(context.Background(), tunnelID)
	if err != nil {
		t.Fatalf("ListManagedRecords() error = %v, want nil", err)
	}

	want := map[string]string{"a.vantus.site": "rec-1", "b.vantus.site": "rec-2"}
	if len(records) != len(want) {
		t.Fatalf("ListManagedRecords() = %v, want %v", records, want)
	}
	for hostname, id := range want {
		if records[hostname] != id {
			t.Errorf("ListManagedRecords()[%q] = %q, want %q", hostname, records[hostname], id)
		}
	}
}

// TestAdoptRecord_TracksWithoutAPICall verifies that AdoptRecord registers a
// hostname/record ID pair into session tracking without any HTTP call, and
// that a subsequent CreateCNAME for the same hostname is then a no-op.
func TestAdoptRecord_TracksWithoutAPICall(t *testing.T) {
	var postCalls int32

	mux := http.NewServeMux()
	mux.HandleFunc("/zones/test-zone/dns_records", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			atomic.AddInt32(&postCalls, 1)
		}
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{"success": true, "result": map[string]any{"id": "should-not-be-created"}})
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	client := newTestClient(server)
	client.AdoptRecord("adopted.vantus.site", "rec-adopted")

	recordID, err := client.CreateCNAME(context.Background(), "adopted.vantus.site", "tunnel-abc")
	if err != nil {
		t.Fatalf("CreateCNAME() error = %v, want nil", err)
	}
	if recordID != "rec-adopted" {
		t.Errorf("CreateCNAME() record ID = %q, want %q (adopted, not recreated)", recordID, "rec-adopted")
	}
	if got := atomic.LoadInt32(&postCalls); got != 0 {
		t.Errorf("server received %d POST call(s), want 0 (adopted record must not be recreated)", got)
	}
}
