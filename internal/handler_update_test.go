package internal

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"meridian/internal/selfupdate"
)

func TestHandleUpdateCheckWithoutUpdater(t *testing.T) {
	app := newTestApp(t)
	app.Version = "v2.6.0" // Updater stays nil
	router := setupTestRouter(app)

	w := httptest.NewRecorder()
	router.ServeHTTP(w, authRequest(httptest.NewRequest(http.MethodGet, "/api/admin/update/check", nil), createTestAdmin(t, app)))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	body := decodeBody(t, w)
	if got := mustStringValue(t, body, "current"); got != "v2.6.0" {
		t.Fatalf("current = %q, want v2.6.0", got)
	}
	if got := mustStringValue(t, body, "latest"); got != "" {
		t.Fatalf("latest = %q, want empty without updater", got)
	}
	if got := mustBoolValue(t, body, "update_available"); got {
		t.Fatal("update_available must be false without updater")
	}
}

func TestHandleUpdateStart(t *testing.T) {
	app := newTestApp(t)
	router := setupTestRouter(app)
	token := createTestAdmin(t, app)

	// Without an updater the endpoint is unavailable.
	w := httptest.NewRecorder()
	router.ServeHTTP(w, authRequest(httptest.NewRequest(http.MethodPost, "/api/admin/update", nil), token))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("no updater status = %d, want 503", w.Code)
	}

	// With an updater the update is accepted. The background goroutine must
	// never touch the network in tests: a successful download would swap the
	// test binary for the real meridian binary and exec it (this actually
	// happened in CI). Point its HTTP client at a transport that always fails.
	app.Updater = selfupdate.New("meridian-")
	app.Updater.HTTPClient = &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return nil, errors.New("network disabled in tests")
		}),
	}
	router = setupTestRouter(app) // rebuild: handlers capture app fields
	w = httptest.NewRecorder()
	router.ServeHTTP(w, authRequest(httptest.NewRequest(http.MethodPost, "/api/admin/update", nil), token))
	if w.Code != http.StatusOK {
		t.Fatalf("with updater status = %d body=%s", w.Code, w.Body.String())
	}

	// Unauthenticated requests are rejected.
	w = httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/admin/update/check", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", w.Code)
	}
}

func TestHandleAdminSettings(t *testing.T) {
	app := newTestApp(t)
	app.Version = "v2.6.0"
	app.RelayToken = "relay-secret"
	app.RoutePrefix = "/s"
	router := setupTestRouter(app)
	token := createTestAdmin(t, app)

	// Unauthenticated.
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/admin/settings", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", w.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/admin/settings", nil)
	req.Host = "panel.example.com"
	w = httptest.NewRecorder()
	router.ServeHTTP(w, authRequest(req, token))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	body := decodeBody(t, w)
	if got := mustStringValue(t, body, "version"); got != "v2.6.0" {
		t.Fatalf("version = %q, want v2.6.0", got)
	}
	if got := mustStringValue(t, body, "panel_url"); got != "http://panel.example.com" {
		t.Fatalf("panel_url = %q, want http://panel.example.com", got)
	}
	if got := mustStringValue(t, body, "route_prefix"); got != "/s" {
		t.Fatalf("route_prefix = %q, want /s", got)
	}
	if got := mustBoolValue(t, body, "relay_api_enabled"); !got {
		t.Fatal("relay_api_enabled = false, want true")
	}
	if got := mustBoolValue(t, body, "geolite_enabled"); got {
		t.Fatal("geolite_enabled = true, want false (nil GeoLite)")
	}
}
