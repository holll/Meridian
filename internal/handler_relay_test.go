package internal

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandleRelayInstallCmd(t *testing.T) {
	t.Run("requires panel authentication", func(t *testing.T) {
		app := newTestApp(t)
		app.RelayToken = "relay-secret"
		router := setupTestRouter(app)

		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/relay/install-cmd", nil))
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", w.Code)
		}
	})

	t.Run("503 when relay token unset", func(t *testing.T) {
		app := newTestApp(t)
		app.RelayToken = ""
		router := setupTestRouter(app)

		w := httptest.NewRecorder()
		router.ServeHTTP(w, authRequest(
			httptest.NewRequest(http.MethodGet, "/api/relay/install-cmd", nil),
			createTestAdmin(t, app),
		))
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", w.Code)
		}
	})

	t.Run("embeds panel URL, token and placeholder node name", func(t *testing.T) {
		app := newTestApp(t)
		app.RelayToken = "relay-secret"
		router := setupTestRouter(app)

		req := httptest.NewRequest(http.MethodGet, "/api/relay/install-cmd", nil)
		req.Host = "panel.example.com"
		w := httptest.NewRecorder()
		router.ServeHTTP(w, authRequest(req, createTestAdmin(t, app)))

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
		}
		var body map[string]interface{}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		masterURL, _ := body["master_url"].(string)
		if masterURL != "http://panel.example.com" {
			t.Fatalf("master_url = %q, want http://panel.example.com", masterURL)
		}
		command, _ := body["command"].(string)
		for _, want := range []string{
			"curl -L https://raw.githubusercontent.com/holll/Meridian/master/install-relay.sh -o install-relay.sh",
			"MASTER_URL=http://panel.example.com",
			"RELAY_TOKEN=relay-secret",
			"RELAY_NAME=__NODE__",
			"./install-relay.sh install",
		} {
			if !strings.Contains(command, want) {
				t.Fatalf("command missing %q:\n%s", want, command)
			}
		}
	})

	t.Run("honors X-Forwarded-Proto https", func(t *testing.T) {
		app := newTestApp(t)
		app.RelayToken = "relay-secret"
		router := setupTestRouter(app)

		req := httptest.NewRequest(http.MethodGet, "/api/relay/install-cmd", nil)
		req.Host = "vps.example.com"
		req.Header.Set("X-Forwarded-Proto", "https")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, authRequest(req, createTestAdmin(t, app)))

		var body map[string]interface{}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		command, _ := body["command"].(string)
		if !strings.Contains(command, "MASTER_URL=https://vps.example.com") {
			t.Fatalf("command missing https master URL:\n%s", command)
		}
	})
}

func TestHandleRelayNodeUpdateSignalsNextHeartbeat(t *testing.T) {
	app := newTestApp(t)
	app.RelayToken = "relay-secret"
	router := setupTestRouter(app)
	token := createTestAdmin(t, app)

	// Panel endpoint requires JWT.
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/relay/nodes/update",
		strings.NewReader(`{"name":"node1"}`)))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", w.Code)
	}

	// Missing name is rejected.
	w = httptest.NewRecorder()
	router.ServeHTTP(w, authRequest(httptest.NewRequest(http.MethodPost, "/api/relay/nodes/update",
		strings.NewReader(`{}`)), token))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("empty name status = %d, want 400", w.Code)
	}

	// Request an update for node1.
	w = httptest.NewRecorder()
	router.ServeHTTP(w, authRequest(httptest.NewRequest(http.MethodPost, "/api/relay/nodes/update",
		strings.NewReader(`{"name":"node1"}`)), token))
	if w.Code != http.StatusOK {
		t.Fatalf("request update status = %d body=%s", w.Code, w.Body.String())
	}

	heartbeat := func() map[string]interface{} {
		req := httptest.NewRequest(http.MethodPost, "/api/relay/traffic",
			strings.NewReader(`{"relay_name":"node1","timestamp":12345,"sites":[]}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer relay-secret")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("heartbeat status = %d body=%s", w.Code, w.Body.String())
		}
		return decodeBody(t, w)
	}

	// The node's next heartbeat carries the update flag; the signal is
	// consumed, so the following heartbeat does not.
	body := heartbeat()
	if got, ok := body["update"].(bool); !ok || !got {
		t.Fatalf("first heartbeat update = %#v, want true", body["update"])
	}
	body = heartbeat()
	if got, ok := body["update"].(bool); !ok || got {
		t.Fatalf("second heartbeat update = %#v, want false", body["update"])
	}

	// A different node is unaffected.
	req := httptest.NewRequest(http.MethodPost, "/api/relay/traffic",
		strings.NewReader(`{"relay_name":"node2","timestamp":12345,"sites":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer relay-secret")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	body = decodeBody(t, w)
	if got, ok := body["update"].(bool); !ok || got {
		t.Fatalf("other node update = %#v, want false", body["update"])
	}
}

func TestHandleRelayNodesReturnsEmptyList(t *testing.T) {
	app := newTestApp(t)
	router := setupTestRouter(app)

	w := httptest.NewRecorder()
	router.ServeHTTP(w, authRequest(
		httptest.NewRequest(http.MethodGet, "/api/relay/nodes", nil),
		createTestAdmin(t, app),
	))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var body map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	nodes, ok := body["nodes"].([]interface{})
	if !ok {
		t.Fatalf("nodes = %#v, want JSON array", body["nodes"])
	}
	if len(nodes) != 0 {
		t.Fatalf("nodes = %#v, want empty", nodes)
	}
}
