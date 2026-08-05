package relay

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"meridian/internal"
)

// TestReportAccessLogsBatchesOverTheLimit drives real proxy requests through a
// relay-mode ProxyManager so the access log buffer fills, then verifies the
// report is split into batches of at most maxAccessLogsPerBatch.
func TestReportAccessLogsBatchesOverTheLimit(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	pm := internal.NewProxyManager(nil) // nil DB: relay mode, buffer active
	site := internal.Site{
		ID:           1,
		Name:         "test",
		PathPrefix:   "/s/test",
		TargetURL:    upstream.URL,
		PlaybackMode: "direct",
		StreamHosts:  "[]",
		UAMode:       "infuse",
		Enabled:      true,
	}
	if err := pm.StartSite(site); err != nil {
		t.Fatalf("StartSite: %v", err)
	}

	const requests = 700 // > 500 batch limit, < buffer capacity 1000
	for i := 0; i < requests; i++ {
		r := httptest.NewRequest(http.MethodGet, "http://meridian.test/s/test/Items/123", nil)
		w := httptest.NewRecorder()
		if !pm.TryServe(w, r) {
			t.Fatalf("TryServe request %d not served", i)
		}
	}
	var batches, reported int64
	var relayName, auth string
	master := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/relay/access_logs" {
			http.NotFound(w, r)
			return
		}
		auth = r.Header.Get("Authorization")
		var body struct {
			RelayName string                    `json:"relay_name"`
			Logs      []internal.AccessLogEntry `json:"logs"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode batch: %v", err)
		}
		relayName = body.RelayName
		atomic.AddInt64(&batches, 1)
		atomic.AddInt64(&reported, int64(len(body.Logs)))
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer master.Close()

	s := NewSyncer(Config{
		MasterURL:        master.URL,
		RelayToken:       "secret",
		RelayName:        "node1",
		AccessLogEnabled: true,
	}, pm)
	s.reportAccessLogs()

	if batches != 2 {
		t.Fatalf("batches = %d, want 2 (500 + 200)", batches)
	}
	if reported != requests {
		t.Fatalf("reported logs = %d, want %d", reported, requests)
	}
	if relayName != "node1" {
		t.Fatalf("relay_name = %q, want node1", relayName)
	}
	if auth != "Bearer secret" {
		t.Fatalf("authorization = %q, want Bearer secret", auth)
	}
}

func TestReportAccessLogsSkipsWhenDisabledOrEmpty(t *testing.T) {
	pm := internal.NewProxyManager(nil)
	hits := 0
	master := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	defer master.Close()

	disabled := NewSyncer(Config{MasterURL: master.URL, AccessLogEnabled: false}, pm)
	disabled.reportAccessLogs()
	if hits != 0 {
		t.Fatalf("disabled report hits = %d, want 0", hits)
	}

	enabled := NewSyncer(Config{MasterURL: master.URL, AccessLogEnabled: true}, pm)
	enabled.reportAccessLogs() // empty buffer
	if hits != 0 {
		t.Fatalf("empty report hits = %d, want 0", hits)
	}
}
