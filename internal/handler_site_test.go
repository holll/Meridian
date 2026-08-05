package internal

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandleSitesCreateRollsBackOnDuplicatePathPrefix(t *testing.T) {
	app := newTestApp(t)
	token := createTestAdmin(t, app)
	router := setupTestRouter(app)

	// Create the first site successfully.
	req1 := httptest.NewRequest(http.MethodPost, "/api/sites", strings.NewReader(`{"name":"first","path_prefix":"/s/dup","target_url":"http://127.0.0.1:8096","ua_mode":"infuse"}`))
	req1.Header.Set("Content-Type", "application/json")
	rr1 := httptest.NewRecorder()
	router.ServeHTTP(rr1, authRequest(req1, token))
	if rr1.Code != http.StatusCreated {
		t.Fatalf("first create status = %d body=%s", rr1.Code, rr1.Body.String())
	}

	// Attempt to create a second site with the same path_prefix — DB UNIQUE constraint must reject it.
	req2 := httptest.NewRequest(http.MethodPost, "/api/sites", strings.NewReader(`{"name":"conflict","path_prefix":"/s/dup","target_url":"http://127.0.0.1:8097","ua_mode":"infuse"}`))
	req2.Header.Set("Content-Type", "application/json")
	rr2 := httptest.NewRecorder()
	router.ServeHTTP(rr2, authRequest(req2, token))
	if rr2.Code != http.StatusInternalServerError {
		t.Fatalf("duplicate path_prefix status = %d body=%s", rr2.Code, rr2.Body.String())
	}
	if count := lenMust(app.DB.ListSites()); count != 1 {
		t.Fatalf("site count = %d, want 1 after failed duplicate create", count)
	}
}

func TestHandleSiteToggleRevertsWhenStartFails(t *testing.T) {
	// With path-based routing StartSite cannot fail from a port conflict.
	// Verify the toggle path still works end-to-end: enabling a disabled site
	// makes it running, and disabling it stops it.
	app := newTestApp(t)
	token := createTestAdmin(t, app)
	site, err := app.DB.CreateSite("toggle-test", "/s/toggle", "http://127.0.0.1:8096", "", "direct", "[]", "infuse", 0, 0)
	if err != nil {
		t.Fatalf("CreateSite: %v", err)
	}
	if _, err := app.DB.DB.Exec("UPDATE sites SET enabled=0 WHERE id=?", site.ID); err != nil {
		t.Fatalf("disable site: %v", err)
	}

	router := setupTestRouter(app)

	// Toggle enable
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/sites/%d/toggle", site.ID), nil)
	router.ServeHTTP(w, authRequest(req, token))

	if w.Code != http.StatusOK {
		t.Fatalf("toggle enable status = %d body=%s", w.Code, w.Body.String())
	}
	if !app.PM.IsRunning(site.ID) {
		t.Fatal("site should be running after toggle enable")
	}

	// Toggle disable
	w2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/sites/%d/toggle", site.ID), nil)
	router.ServeHTTP(w2, authRequest(req2, token))
	if w2.Code != http.StatusOK {
		t.Fatalf("toggle disable status = %d body=%s", w2.Code, w2.Body.String())
	}
	if app.PM.IsRunning(site.ID) {
		t.Fatal("site should not be running after toggle disable")
	}
}

func TestHandleSiteUpdateRollsBackOnDuplicatePathPrefix(t *testing.T) {
	app := newTestApp(t)
	token := createTestAdmin(t, app)
	site1, err := app.DB.CreateSite("stable", "/s/stable", "http://127.0.0.1:8096", "", "direct", "[]", "infuse", 0, 0)
	if err != nil {
		t.Fatalf("CreateSite stable: %v", err)
	}
	site2, err := app.DB.CreateSite("other", "/s/other", "http://127.0.0.1:8096", "", "direct", "[]", "infuse", 0, 0)
	if err != nil {
		t.Fatalf("CreateSite other: %v", err)
	}
	if err := app.PM.StartSite(*site1); err != nil {
		t.Fatalf("StartSite: %v", err)
	}
	t.Cleanup(func() { app.PM.StopSite(site1.ID) })

	router := setupTestRouter(app)

	// Try to update site2's path_prefix to conflict with site1's.
	req := httptest.NewRequest(http.MethodPut, fmt.Sprintf("/api/sites/%d", site2.ID), strings.NewReader(`{"name":"other","path_prefix":"/s/stable","target_url":"http://127.0.0.1:8096","ua_mode":"infuse"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, authRequest(req, token))

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	reloaded, err := app.DB.GetSite(site2.ID)
	if err != nil {
		t.Fatalf("GetSite: %v", err)
	}
	if reloaded.PathPrefix != "/s/other" {
		t.Fatalf("path_prefix = %q, want /s/other", reloaded.PathPrefix)
	}
	if !app.PM.IsRunning(site1.ID) {
		t.Fatalf("expected site1 to keep running")
	}
}

func TestHandleSiteUpdateFailureRestoresCustomUAFields(t *testing.T) {
	app := newTestApp(t)
	token := createTestAdmin(t, app)
	// Create a blocking site whose path_prefix will be used for the conflict.
	_, err := app.DB.CreateSite("blocker", "/s/blocked", "http://127.0.0.1:8096", "", "direct", "[]", "infuse", 0, 0)
	if err != nil {
		t.Fatalf("CreateSite blocker: %v", err)
	}
	site, err := app.DB.CreateSiteWithCustomUA("custom-stable", "/s/custom", "http://127.0.0.1:8096", "", "direct", "[]", customUAMode, "Old UA", "Old Client", "1.0", 0, 0)
	if err != nil {
		t.Fatalf("CreateSiteWithCustomUA: %v", err)
	}
	if err := app.PM.StartSite(*site); err != nil {
		t.Fatalf("StartSite: %v", err)
	}
	t.Cleanup(func() { app.PM.StopSite(site.ID) })

	payload, err := json.Marshal(map[string]interface{}{
		"name":              "custom-stable",
		"path_prefix":       "/s/blocked",
		"target_url":        "http://127.0.0.1:8096",
		"ua_mode":           "custom",
		"custom_user_agent": "New UA",
		"custom_client":     "New Client",
		"custom_version":    "2.0",
	})
	if err != nil {
		t.Fatalf("marshal update: %v", err)
	}

	router := setupTestRouter(app)
	req := httptest.NewRequest(http.MethodPut, fmt.Sprintf("/api/sites/%d", site.ID), bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, authRequest(req, token))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}

	restored, err := app.DB.GetSite(site.ID)
	if err != nil {
		t.Fatalf("GetSite: %v", err)
	}
	if restored.PathPrefix != "/s/custom" || restored.UAMode != customUAMode || restored.CustomUserAgent != "Old UA" || restored.CustomClient != "Old Client" || restored.CustomVersion != "1.0" {
		t.Fatalf("rollback did not restore full custom snapshot: %#v", restored)
	}
	if !app.PM.IsRunning(site.ID) {
		t.Fatal("original custom proxy should remain running")
	}
}

func TestHandleSiteUpdatePreservesOmittedSpeedLimit(t *testing.T) {
	app := newTestApp(t)
	token := createTestAdmin(t, app)
	site, err := app.DB.CreateSite("limited", "/s/limited", "http://127.0.0.1:8096", "", "direct", "[]", "infuse", 0, 25)
	if err != nil {
		t.Fatalf("CreateSite: %v", err)
	}
	if enabled, err := app.DB.ToggleSite(site.ID); err != nil || enabled {
		t.Fatalf("disable site: enabled=%v err=%v", enabled, err)
	}

	router := setupTestRouter(app)
	req := httptest.NewRequest(http.MethodPut, fmt.Sprintf("/api/sites/%d", site.ID), strings.NewReader(`{"name":"limited","path_prefix":"/s/limited","target_url":"http://127.0.0.1:8096","ua_mode":"infuse"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, authRequest(req, token))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}

	reloaded, err := app.DB.GetSite(site.ID)
	if err != nil {
		t.Fatalf("GetSite: %v", err)
	}
	if reloaded.SpeedLimit != 25 {
		t.Fatalf("speed_limit = %d, want preserved value 25", reloaded.SpeedLimit)
	}
}

func TestHandleSitesCreatePersistsPlaybackTargetURL(t *testing.T) {
	app := newTestApp(t)
	token := createTestAdmin(t, app)
	router := setupTestRouter(app)

	req := httptest.NewRequest(http.MethodPost, "/api/sites", strings.NewReader(`{"name":"split","path_prefix":"/s/split","target_url":"http://127.0.0.1:8096","playback_target_url":"https://media.example.com","ua_mode":"infuse"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, authRequest(req, token))

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if got := w.Result().Header.Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}

	var site Site
	if err := json.Unmarshal(w.Body.Bytes(), &site); err != nil {
		t.Fatalf("decode site: %v body=%s", err, w.Body.String())
	}
	if site.PlaybackTargetURL != "https://media.example.com" {
		t.Fatalf("playback_target_url = %q, want %q", site.PlaybackTargetURL, "https://media.example.com")
	}

	reloaded, err := app.DB.GetSite(site.ID)
	if err != nil {
		t.Fatalf("GetSite: %v", err)
	}
	if reloaded.PlaybackTargetURL != "https://media.example.com" {
		t.Fatalf("persisted playback_target_url = %q, want %q", reloaded.PlaybackTargetURL, "https://media.example.com")
	}
}

func TestHandleSitesCreatesCustomUAAndPresetUpdateClearsIt(t *testing.T) {
	app := newTestApp(t)
	token := createTestAdmin(t, app)
	router := setupTestRouter(app)

	createPayload, err := json.Marshal(map[string]interface{}{
		"name":              "custom-identity",
		"path_prefix":       "/s/custom-identity",
		"target_url":        "http://127.0.0.1:8096",
		"ua_mode":           "custom",
		"custom_user_agent": "Meridian Custom/1.0",
		"custom_client":     "Meridian Custom",
		"custom_version":    "1.0.0",
	})
	if err != nil {
		t.Fatalf("marshal create payload: %v", err)
	}
	createReq := httptest.NewRequest(http.MethodPost, "/api/sites", bytes.NewReader(createPayload))
	createReq.Header.Set("Content-Type", "application/json")
	createRecorder := httptest.NewRecorder()
	router.ServeHTTP(createRecorder, authRequest(createReq, token))
	if createRecorder.Code != http.StatusCreated {
		t.Fatalf("create status = %d body=%s", createRecorder.Code, createRecorder.Body.String())
	}
	var created Site
	if err := json.Unmarshal(createRecorder.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created site: %v", err)
	}
	t.Cleanup(func() { app.PM.StopSite(created.ID) })
	if created.UAMode != customUAMode || created.CustomUserAgent != "Meridian Custom/1.0" || created.CustomClient != "Meridian Custom" || created.CustomVersion != "1.0.0" {
		t.Fatalf("created custom site = %#v", created)
	}

	updatePayload, err := json.Marshal(map[string]interface{}{
		"name":        created.Name,
		"path_prefix": created.PathPrefix,
		"target_url":  created.TargetURL,
		"ua_mode":     "web",
	})
	if err != nil {
		t.Fatalf("marshal update payload: %v", err)
	}
	updateReq := httptest.NewRequest(http.MethodPut, fmt.Sprintf("/api/sites/%d", created.ID), bytes.NewReader(updatePayload))
	updateReq.Header.Set("Content-Type", "application/json")
	updateRecorder := httptest.NewRecorder()
	router.ServeHTTP(updateRecorder, authRequest(updateReq, token))
	if updateRecorder.Code != http.StatusOK {
		t.Fatalf("update status = %d body=%s", updateRecorder.Code, updateRecorder.Body.String())
	}
	reloaded, err := app.DB.GetSite(created.ID)
	if err != nil {
		t.Fatalf("load updated site: %v", err)
	}
	if reloaded.UAMode != "web" || reloaded.CustomUserAgent != "" || reloaded.CustomClient != "" || reloaded.CustomVersion != "" {
		t.Fatalf("preset update did not clear custom fields: %#v", reloaded)
	}
}

func TestHandleSitesRejectsInvalidCustomUA(t *testing.T) {
	for _, values := range []map[string]string{
		{"custom_user_agent": "", "custom_client": "Client", "custom_version": "1.0"},
		{"custom_user_agent": "UA", "custom_client": "Bad\"", "custom_version": "1.0"},
		{"custom_user_agent": "UA\nnext", "custom_client": "Client", "custom_version": "1.0"},
	} {
		t.Run(values["custom_client"]+values["custom_user_agent"], func(t *testing.T) {
			app := newTestApp(t)
			token := createTestAdmin(t, app)
			router := setupTestRouter(app)

			payload := map[string]interface{}{
				"name":        "invalid-custom",
				"path_prefix": "/s/invalid-custom",
				"target_url":  "http://127.0.0.1:8096",
				"ua_mode":     "custom",
			}
			for key, value := range values {
				payload[key] = value
			}
			body, err := json.Marshal(payload)
			if err != nil {
				t.Fatalf("marshal payload: %v", err)
			}
			req := httptest.NewRequest(http.MethodPost, "/api/sites", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			router.ServeHTTP(w, authRequest(req, token))
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
			}
			if sites, err := app.DB.ListSites(); err != nil || len(sites) != 0 {
				t.Fatalf("invalid custom site was persisted: sites=%#v err=%v", sites, err)
			}
		})
	}
}
