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

// TestCreateThenDeleteCNAME_RoundTrip covers SC-07: creating a CNAME tracks
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

// TestCreateCNAME_SkipsExistingUntrackedRecord covers REQ-5.3/REQ-5.4: a
// record that exists but was not created by cf-watcher in this session must
// be skipped (not adopted into tracking, and never deleted later).
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

	// REQ-5.4: since the record was never adopted into session tracking,
	// DeleteCNAME must be a no-op — cf-watcher must never delete a record
	// it did not create.
	if err := client.DeleteCNAME(ctx, hostname); err != nil {
		t.Fatalf("DeleteCNAME() error = %v, want nil", err)
	}
	if got := atomic.LoadInt32(&deleteCalls); got != 0 {
		t.Errorf("server received %d DELETE call(s), want 0 (must not delete untracked record)", got)
	}
}
