package internal

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"meridian/web"
)

func TestMobileModalKeepsBodyScrollableAndActionsVisible(t *testing.T) {
	css, err := web.StaticFiles.ReadFile("static/css/style.css")
	if err != nil {
		t.Fatalf("read embedded CSS: %v", err)
	}
	for _, rule := range []string{
		"max-height: calc(100dvh - 48px)",
		"overflow-y: auto",
		"-webkit-overflow-scrolling: touch",
		".btn-modal { flex: 1; min-height: 44px",
	} {
		if !strings.Contains(string(css), rule) {
			t.Errorf("mobile modal CSS missing %q", rule)
		}
	}

	appJS, err := web.StaticFiles.ReadFile("static/js/app.js")
	if err != nil {
		t.Fatalf("read embedded app JavaScript: %v", err)
	}
	if !strings.Contains(string(appJS), "document.getElementById('modal-body').scrollTop = 0") {
		t.Error("opening a modal must reset the form scroll position")
	}

	sitesJS, err := web.StaticFiles.ReadFile("static/js/pages/sites.js")
	if err != nil {
		t.Fatalf("read embedded sites JavaScript: %v", err)
	}
	if !strings.Contains(string(sitesJS), "openModal({ closeOnBackdrop: false })") {
		t.Error("site add/edit form must not close when its backdrop is clicked")
	}
	for _, snippet := range []string{`id="m-speed"`, "speed_limit: parseInt(document.getElementById('m-speed').value || 0)"} {
		if !strings.Contains(string(sitesJS), snippet) {
			t.Errorf("site form must expose and submit speed limit; missing %q", snippet)
		}
	}

	indexHTML, err := web.StaticFiles.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("read embedded index HTML: %v", err)
	}
	for _, asset := range []string{"/css/style.css", "/js/pages/sites.js", "/js/app.js"} {
		if !strings.Contains(string(indexHTML), asset+"?v=") {
			t.Errorf("index must cache-bust updated asset %q", asset)
		}
	}
	if strings.Contains(string(indexHTML), "fonts.googleapis.com") || strings.Contains(string(indexHTML), "fonts.gstatic.com") {
		t.Error("index must not request fonts blocked by the Content-Security-Policy")
	}
}

func TestStaticHandlerDisablesCaching(t *testing.T) {
	staticFS, err := fs.Sub(web.StaticFiles, "static")
	if err != nil {
		t.Fatalf("static fs: %v", err)
	}
	rr := httptest.NewRecorder()
	StaticHandler(staticFS).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/js/pages/sites.js", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Cache-Control"); got != "no-store, no-cache, must-revalidate" {
		t.Fatalf("Cache-Control = %q", got)
	}
	if got := rr.Header().Get("Pragma"); got != "no-cache" {
		t.Fatalf("Pragma = %q, want no-cache", got)
	}
	if got := rr.Header().Get("Expires"); got != "0" {
		t.Fatalf("Expires = %q, want 0", got)
	}
}

func TestAPIClientClearsRejectedStoredToken(t *testing.T) {
	apiJS, err := web.StaticFiles.ReadFile("static/js/api.js")
	if err != nil {
		t.Fatalf("read embedded API JavaScript: %v", err)
	}
	source := string(apiJS)
	for _, expected := range []string{"res.status === 401", "this.logout()", "window.location.reload()"} {
		if !strings.Contains(source, expected) {
			t.Errorf("API client missing %q", expected)
		}
	}
}

func TestPanelListenAddressSeparatesPanelFromSiteListeners(t *testing.T) {
	for _, tc := range []struct {
		name string
		bind string
		port int
		want string
	}{
		{name: "default", port: 9090, want: "0.0.0.0:9090"},
		{name: "loopback", bind: "127.0.0.1", port: 9090, want: "127.0.0.1:9090"},
		{name: "ipv6", bind: "::1", port: 9090, want: "[::1]:9090"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := PanelListenAddress(tc.bind, tc.port)
			if err != nil || got != tc.want {
				t.Fatalf("PanelListenAddress() = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
	for _, tc := range []struct {
		bind string
		port int
	}{
		{bind: "panel.example.com", port: 9090},
		{bind: "127.0.0.1", port: 0},
		{bind: "127.0.0.1", port: 65536},
	} {
		if _, err := PanelListenAddress(tc.bind, tc.port); err == nil {
			t.Fatalf("PanelListenAddress(%q, %d) unexpectedly succeeded", tc.bind, tc.port)
		}
	}
}
