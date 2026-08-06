package internal

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHandleRelayAccessLogsRequiresRelayToken(t *testing.T) {
	app := newTestApp(t) // RelayToken empty
	router := setupTestRouter(app)

	req := httptest.NewRequest(http.MethodPost, "/api/relay/access_logs", strings.NewReader(`{"relay_name":"node1","logs":[]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("no relay token status = %d, want 503", w.Code)
	}

	app.RelayToken = "testsecret"
	router = setupTestRouter(app) // rebuild: middleware captures the token at setup time
	req = httptest.NewRequest(http.MethodPost, "/api/relay/access_logs", strings.NewReader(`{"relay_name":"node1","logs":[]}`))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token status = %d, want 401", w.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/relay/access_logs", strings.NewReader(`{"relay_name":"node1","logs":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer testsecret")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("valid token status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
}

func TestHandleRelayAccessLogsValidatesBatch(t *testing.T) {
	app := newTestApp(t)
	app.RelayToken = "testsecret"
	router := setupTestRouter(app)
	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/relay/access_logs", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer testsecret")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}

	if w := post(`{"logs":[{}]}`); w.Code != http.StatusBadRequest {
		t.Fatalf("missing relay_name status = %d, want 400", w.Code)
	}

	oversized := `{"relay_name":"node1","logs":[` + strings.Repeat(`{},`, 2001) + `{}]}`
	if w := post(oversized); w.Code != http.StatusBadRequest {
		t.Fatalf("oversized batch status = %d, want 400", w.Code)
	}

	w := post(fmt.Sprintf(`{"relay_name":"node1","logs":[{"timestamp":%d,"site_id":1,"client_ip":"1.2.3.4","method":"GET","path":"/emby","status":200}]}`, time.Now().Unix()))
	if w.Code != http.StatusOK {
		t.Fatalf("valid batch status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	var total int
	if err := app.DB.DB.QueryRow("SELECT COUNT(*) FROM access_logs").Scan(&total); err != nil {
		t.Fatalf("count: %v", err)
	}
	if total != 1 {
		t.Fatalf("stored rows = %d, want 1", total)
	}
}

func TestHandleAccessLogsEndpointRequiresJWT(t *testing.T) {
	app := newTestApp(t)
	router := setupTestRouter(app)

	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/access_logs", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", w.Code)
	}
}

func TestHandleAccessLogsEndpointPaginates(t *testing.T) {
	app := newTestApp(t)
	router := setupTestRouter(app)
	token := createTestAdmin(t, app)

	base := time.Now().Unix() - 3600
	entries := make([]AccessLogEntry, 0, 3)
	for i := 0; i < 3; i++ {
		entries = append(entries, AccessLogEntry{
			Timestamp: base + int64(i), SiteID: 1, ClientIP: "1.2.3.4",
			Method: "GET", Path: fmt.Sprintf("/p%d", i), Status: 200, BytesOut: 10,
		})
	}
	if err := app.DB.AddAccessLogs("node1", entries); err != nil {
		t.Fatalf("AddAccessLogs: %v", err)
	}

	w := httptest.NewRecorder()
	router.ServeHTTP(w, authRequest(httptest.NewRequest(http.MethodGet, "/api/access_logs?page=1&page_size=2&relay_name=node1", nil), token))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	body := decodeBody(t, w)
	if got := mustNumberValue(t, body, "total"); got != 3 {
		t.Fatalf("total = %d, want 3", got)
	}
	if got := mustNumberValue(t, body, "page_size"); got != 2 {
		t.Fatalf("page_size = %d, want 2", got)
	}
}

func TestHandleAccessLogsSearchParams(t *testing.T) {
	app := newTestApp(t)
	router := setupTestRouter(app)
	token := createTestAdmin(t, app)

	base := time.Now().Unix()
	if err := app.DB.AddAccessLogs("node1", []AccessLogEntry{
		{Timestamp: base, SiteID: 1, ClientIP: "1.2.3.4", Method: "GET", Path: "/emby/Users/1", Status: 200},
		{Timestamp: base - 1, SiteID: 1, ClientIP: "9.9.9.9", Method: "GET", Path: "/other", Status: 404},
	}); err != nil {
		t.Fatalf("AddAccessLogs: %v", err)
	}

	query := func(qs string) map[string]interface{} {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, authRequest(httptest.NewRequest(http.MethodGet, "/api/access_logs?"+qs, nil), token))
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
		}
		return decodeBody(t, w)
	}

	// Path prefix filter.
	body := query("path=/emby/Users")
	if got := mustNumberValue(t, body, "total"); got != 1 {
		t.Fatalf("path prefix total = %d, want 1", got)
	}

	// IP prefix filter.
	body = query("ip=1.2.3")
	if got := mustNumberValue(t, body, "total"); got != 1 {
		t.Fatalf("ip prefix total = %d, want 1", got)
	}

	// ISP filter with no GeoLite configured: 200 with an empty result set.
	body = query("isp=telecom")
	if got := mustNumberValue(t, body, "total"); got != 0 {
		t.Fatalf("isp total = %d, want 0 (no GeoLite)", got)
	}
	logs, ok := body["logs"].([]interface{})
	if !ok || len(logs) != 0 {
		t.Fatalf("isp logs = %#v, want empty array", body["logs"])
	}
}

func TestHandleAccessLogStatsEndpointDefaultsToLast24h(t *testing.T) {
	app := newTestApp(t)
	router := setupTestRouter(app)
	token := createTestAdmin(t, app)

	// A row older than 24h and one inside the default window.
	stale := time.Now().Add(-48 * time.Hour).Unix()
	fresh := time.Now().Unix()
	if err := app.DB.AddAccessLogs("node1", []AccessLogEntry{
		{Timestamp: stale, SiteID: 1, ClientIP: "1.1.1.1", Method: "GET", Path: "/stale", Status: 200, BytesOut: 1},
		{Timestamp: fresh, SiteID: 1, ClientIP: "2.2.2.2", Method: "GET", Path: "/fresh", Status: 200, BytesOut: 2},
	}); err != nil {
		t.Fatalf("AddAccessLogs: %v", err)
	}

	w := httptest.NewRecorder()
	router.ServeHTTP(w, authRequest(httptest.NewRequest(http.MethodGet, "/api/access_logs/stats", nil), token))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	body := decodeBody(t, w)
	trend, ok := body["trend"].([]interface{})
	if !ok {
		t.Fatalf("trend = %T, want []interface{}", body["trend"])
	}
	total := 0
	for _, point := range trend {
		reqs, ok := point.(map[string]interface{})["requests"].(float64)
		if !ok {
			t.Fatalf("trend point requests = %T, want float64", point)
		}
		total += int(reqs)
	}
	if total != 1 {
		t.Fatalf("default window trend requests = %d, want 1 (stale row excluded)", total)
	}
}
