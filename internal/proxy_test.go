package internal

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestNormalizeTargetURLRejectsUnsafeForms(t *testing.T) {
	for _, target := range []string{
		"file://server/path",
		"http://user:password@example.com",
		"https://example.com/path#fragment",
		"http://example.com:70000",
	} {
		if _, err := normalizeTargetURL(target); err == nil {
			t.Errorf("normalizeTargetURL(%q) unexpectedly succeeded", target)
		}
	}

	target, err := normalizeTargetURL("example.com:8096")
	if err != nil {
		t.Fatalf("normalizeTargetURL valid target: %v", err)
	}
	if target.String() != "http://example.com:8096" {
		t.Fatalf("normalized target = %q, want http://example.com:8096", target)
	}
}

func TestNormalizeTargetURLInfersHTTPSForPort443(t *testing.T) {
	for _, input := range []string{"example.com:443", "example.com：443"} {
		target, err := normalizeTargetURL(input)
		if err != nil {
			t.Fatalf("normalizeTargetURL(%q): %v", input, err)
		}
		if target.String() != "https://example.com:443" {
			t.Fatalf("normalizeTargetURL(%q) = %q, want https://example.com:443", input, target)
		}
	}

	explicitHTTP, err := normalizeTargetURL("http://example.com:443")
	if err != nil {
		t.Fatalf("normalize explicit HTTP target: %v", err)
	}
	if explicitHTTP.Scheme != "http" {
		t.Fatalf("explicit HTTP scheme = %q, want http", explicitHTTP.Scheme)
	}
}

func TestRedirectModeTreatsExplicit443AsDefaultHTTPSPort(t *testing.T) {
	configured, err := normalizeTargetURL("media.example.com:443")
	if err != nil {
		t.Fatalf("normalize configured playback target: %v", err)
	}
	if got := redirectHostKey(configured); got != "media.example.com" {
		t.Fatalf("redirect host key = %q, want media.example.com", got)
	}

	calls := 0
	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return &http.Response{
				StatusCode: http.StatusFound,
				Header:     http.Header{"Location": []string{"https://media.example.com/Videos/1/stream"}},
				Body:       io.NopCloser(strings.NewReader("")),
				Request:    req,
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("proxied")),
			Request:    req,
		}, nil
	})
	transport := &clientTransport{
		client: &http.Client{Transport: base},
	}
	req := httptest.NewRequest(http.MethodGet, "http://api.example.com/Videos/1/stream", nil)
	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer resp.Body.Close()
	if calls != 2 || resp.StatusCode != http.StatusOK {
		t.Fatalf("redirect follow calls=%d status=%d, want calls=2 status=200", calls, resp.StatusCode)
	}
	if got := resp.Request.URL.String(); got != "https://media.example.com/Videos/1/stream" {
		t.Fatalf("followed URL = %q", got)
	}

	t.Run("follows redirect regardless of scheme", func(t *testing.T) {
		calls := 0
		base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			if calls == 1 {
				return &http.Response{
					StatusCode: http.StatusFound,
					Header:     http.Header{"Location": []string{"http://media.example.com/Videos/1/stream"}},
					Body:       io.NopCloser(strings.NewReader("")),
					Request:    req,
				}, nil
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("proxied")),
				Request:    req,
			}, nil
		})
		transport := &clientTransport{
			client: &http.Client{Transport: base},
		}
		req := httptest.NewRequest(http.MethodGet, "http://api.example.com/Videos/1/stream", nil)
		resp, err := transport.RoundTrip(req)
		if err != nil {
			t.Fatalf("RoundTrip: %v", err)
		}
		defer resp.Body.Close()
		if calls != 2 || resp.StatusCode != http.StatusOK {
			t.Fatalf("redirect calls=%d status=%d, want calls=2 status=200", calls, resp.StatusCode)
		}
	})

	t.Run("follows custom GET redirect path", func(t *testing.T) {
		calls := 0
		base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			if calls == 1 {
				return &http.Response{
					StatusCode: http.StatusFound,
					Header:     http.Header{"Location": []string{"https://media.example.com/custom/play/path"}},
					Body:       io.NopCloser(strings.NewReader("")),
					Request:    req,
				}, nil
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("proxied")),
				Request:    req,
			}, nil
		})
		transport := &clientTransport{
			client: &http.Client{Transport: base},
		}
		req := httptest.NewRequest(http.MethodGet, "http://api.example.com/custom/play/path", nil)
		resp, err := transport.RoundTrip(req)
		if err != nil {
			t.Fatalf("RoundTrip: %v", err)
		}
		defer resp.Body.Close()
		if calls != 2 || resp.StatusCode != http.StatusOK {
			t.Fatalf("custom redirect calls=%d status=%d, want calls=2 status=200", calls, resp.StatusCode)
		}
	})

	t.Run("follows protocol-relative redirect", func(t *testing.T) {
		calls := 0
		base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			if calls == 1 {
				return &http.Response{
					StatusCode: http.StatusFound,
					Header:     http.Header{"Location": []string{"//media.example.com/custom/play/path"}},
					Body:       io.NopCloser(strings.NewReader("")),
					Request:    req,
				}, nil
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("proxied")),
				Request:    req,
			}, nil
		})
		transport := &clientTransport{
			client: &http.Client{Transport: base},
		}
		req := httptest.NewRequest(http.MethodGet, "https://api.example.com/custom/play/path", nil)
		resp, err := transport.RoundTrip(req)
		if err != nil {
			t.Fatalf("RoundTrip: %v", err)
		}
		defer resp.Body.Close()
		if calls != 2 || resp.StatusCode != http.StatusOK {
			t.Fatalf("protocol-relative redirect calls=%d status=%d, want calls=2 status=200", calls, resp.StatusCode)
		}
		if got := resp.Request.URL.String(); got != "https://media.example.com/custom/play/path" {
			t.Fatalf("protocol-relative redirect URL = %q", got)
		}
	})

	t.Run("follows POST redirect with method preserved", func(t *testing.T) {
		calls := 0
		var secondMethod string
		base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			if calls == 1 {
				return &http.Response{
					StatusCode: http.StatusTemporaryRedirect,
					Header:     http.Header{"Location": []string{"https://media.example.com/Users/AuthenticateByName"}},
					Body:       io.NopCloser(strings.NewReader("")),
					Request:    req,
				}, nil
			}
			secondMethod = req.Method
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("ok")),
				Request:    req,
			}, nil
		})
		transport := &clientTransport{
			client: &http.Client{Transport: base},
		}
		req := httptest.NewRequest(http.MethodPost, "http://api.example.com/Users/AuthenticateByName", strings.NewReader(`{"Username":"test"}`))
		// http.Client only replays a 307/308 body when GetBody is set;
		// httptest.NewRequest leaves it nil.
		req.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader(`{"Username":"test"}`)), nil
		}
		resp, err := transport.RoundTrip(req)
		if err != nil {
			t.Fatalf("RoundTrip: %v", err)
		}
		defer resp.Body.Close()
		if calls != 2 || resp.StatusCode != http.StatusOK {
			t.Fatalf("POST redirect calls=%d status=%d, want calls=2 status=200", calls, resp.StatusCode)
		}
		if secondMethod != http.MethodPost {
			t.Fatalf("redirected method = %q, want POST preserved by 307", secondMethod)
		}
	})
}

func TestReverseProxyRebuildsForwardingHeadersAfterHopHeaderRemoval(t *testing.T) {
	target, err := normalizeTargetURL("https://upstream.example.com/emby")
	if err != nil {
		t.Fatalf("normalize target: %v", err)
	}
	profile := getUAProfile("infuse")
	var captured *http.Request
	proxy := &httputil.ReverseProxy{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			captured = req.Clone(req.Context())
			captured.Header = req.Header.Clone()
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("ok")),
				Request:    req,
			}, nil
		}),
		Rewrite: func(proxyReq *httputil.ProxyRequest) {
			applyUpstreamURL(proxyReq.Out.URL, target)
			proxyReq.Out.Host = target.Host
			prepareUpstreamHeaders(proxyReq.Out.Header, proxyReq.In, profile)
		},
	}

	req := httptest.NewRequest(http.MethodGet, "http://meridian.example:50001/Videos/1/stream", nil)
	req.RemoteAddr = "198.51.100.24:43210"
	req.Header.Set("Connection", "User-Agent, X-Forwarded-For")
	req.Header.Set("User-Agent", "attacker-controlled")
	req.Header.Set("Forwarded", "for=203.0.113.8;proto=https")
	req.Header.Set("X-Forwarded-For", "203.0.113.8")
	req.Header.Set("X-Forwarded-Host", "attacker.example")
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Custom", "must-not-pass")
	req.Header.Set("X-Real-IP", "203.0.113.9")
	rr := httptest.NewRecorder()
	proxy.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("proxy status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if captured == nil {
		t.Fatal("transport did not receive an outbound request")
	}
	if captured.URL.String() != "https://upstream.example.com/emby/Videos/1/stream" {
		t.Fatalf("outbound URL = %q", captured.URL.String())
	}
	if captured.Host != target.Host {
		t.Fatalf("outbound Host = %q, want %q", captured.Host, target.Host)
	}
	if got := captured.Header.Get("User-Agent"); got != profile.UserAgent {
		t.Fatalf("outbound User-Agent = %q, want profile value %q", got, profile.UserAgent)
	}
	for name, want := range map[string]string{
		"X-Forwarded-For":   "198.51.100.24",
		"X-Real-IP":         "198.51.100.24",
		"X-Forwarded-Host":  "meridian.example:50001",
		"X-Forwarded-Proto": "http",
	} {
		if got := captured.Header.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	for _, name := range []string{"Forwarded", "X-Forwarded-Custom"} {
		if got := captured.Header.Get(name); got != "" {
			t.Errorf("untrusted %s leaked upstream: %q", name, got)
		}
	}
}

func TestPrepareWebSocketUpstreamHeadersRebuildsForwardingHeaders(t *testing.T) {
	target, err := normalizeTargetURL("https://upstream.example.com/emby")
	if err != nil {
		t.Fatalf("normalize target: %v", err)
	}
	profile := getUAProfile("infuse")
	req := httptest.NewRequest(http.MethodGet, "http://meridian.example:50001/socket", nil)
	req.RemoteAddr = "198.51.100.25:54321"
	req.Header.Set("Connection", "Upgrade, User-Agent")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("User-Agent", "attacker-controlled")
	req.Header.Set("Forwarded", "for=203.0.113.10")
	req.Header.Set("X-Forwarded-For", "203.0.113.10")
	req.Header.Set("X-Forwarded-Custom", "must-not-pass")
	req.Header.Set("X-Real-IP", "203.0.113.11")
	req.Header.Set("Proxy-Connection", "keep-alive")

	header := prepareWebSocketUpstreamHeaders(req, target, profile)
	if got := req.Header.Get("Forwarded"); got == "" {
		t.Fatal("preparing WebSocket headers mutated the inbound request")
	}
	for name, want := range map[string]string{
		"Connection":        "Upgrade",
		"Upgrade":           "websocket",
		"Host":              target.Host,
		"User-Agent":        profile.UserAgent,
		"X-Forwarded-For":   "198.51.100.25",
		"X-Real-IP":         "198.51.100.25",
		"X-Forwarded-Host":  "meridian.example:50001",
		"X-Forwarded-Proto": "http",
	} {
		if got := header.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	for _, name := range []string{"Forwarded", "X-Forwarded-Custom", "Proxy-Connection"} {
		if got := header.Get(name); got != "" {
			t.Errorf("untrusted WebSocket header %s leaked upstream: %q", name, got)
		}
	}
}

func TestRateLimitedWriterUsesPerRequestProgress(t *testing.T) {
	var siteTraffic atomic.Int64
	var perRequest atomic.Int64
	siteTraffic.Store(10 << 20)
	recorder := httptest.NewRecorder()
	writer := &rateLimitedWriter{
		ResponseWriter: recorder,
		bytesPerSec:    1024,
		written:        &siteTraffic,
		local:          &perRequest,
		start:          time.Now().Add(-time.Second),
	}
	payload := bytes.Repeat([]byte("x"), 512)
	n, err := writer.Write(payload)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != len(payload) || recorder.Body.Len() != len(payload) {
		t.Fatalf("wrote=%d body=%d, want %d", n, recorder.Body.Len(), len(payload))
	}
	if writer.requestWritten != int64(len(payload)) {
		t.Fatalf("requestWritten = %d, want %d", writer.requestWritten, len(payload))
	}
	if got := perRequest.Load(); got != int64(len(payload)) {
		t.Fatalf("per-request traffic = %d, want %d", got, len(payload))
	}
	if got := siteTraffic.Load(); got != (10<<20)+int64(len(payload)) {
		t.Fatalf("site traffic = %d, want %d", got, (10<<20)+len(payload))
	}
}

func TestRequestClientKeyUsesOnlyConfiguredTrustedProxy(t *testing.T) {
	trusted, err := ParseTrustedProxyCIDRs("172.17.0.0/16")
	if err != nil {
		t.Fatalf("parse trusted proxies: %v", err)
	}

	trustedRequest := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
	trustedRequest.RemoteAddr = "172.17.0.1:45678"
	trustedRequest.Header.Set("X-Real-IP", "203.0.113.25")
	if got := requestClientKey(trustedRequest, trusted); got != "203.0.113.25" {
		t.Fatalf("trusted proxy client key = %q", got)
	}

	untrustedRequest := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
	untrustedRequest.RemoteAddr = "198.51.100.7:45678"
	untrustedRequest.Header.Set("X-Real-IP", "203.0.113.25")
	if got := requestClientKey(untrustedRequest, trusted); got != "198.51.100.7" {
		t.Fatalf("untrusted proxy client key = %q", got)
	}

	if _, err := ParseTrustedProxyCIDRs("not-a-network"); err == nil {
		t.Fatal("invalid trusted proxy CIDR unexpectedly accepted")
	}
}

func TestFlushTrafficUpdatesBaselineAndStopPersistsPendingUsage(t *testing.T) {
	app := newTestApp(t)
	site, err := app.DB.CreateSite("traffic", "/s/traffic", "http://127.0.0.1:8096", "", "direct", "[]", "infuse", 1024, 0)
	if err != nil {
		t.Fatalf("CreateSite: %v", err)
	}

	inst := &ProxyInstance{Site: *site}
	inst.bytesIn.Store(120)
	inst.bytesOut.Store(80)
	app.PM.proxies[site.ID] = inst

	app.PM.FlushTraffic()

	if got := inst.persistedTraffic.Load(); got != 200 {
		t.Fatalf("persistedTraffic after flush = %d, want 200", got)
	}
	inst.bytesIn.Store(10)
	inst.bytesOut.Store(5)
	app.PM.StopSite(site.ID)

	reloaded, err := app.DB.GetSite(site.ID)
	if err != nil {
		t.Fatalf("GetSite: %v", err)
	}
	if reloaded.TrafficUsed != 215 {
		t.Fatalf("traffic_used = %d, want 215", reloaded.TrafficUsed)
	}
}

func TestStartSiteRejectsCorruptStreamHosts(t *testing.T) {
	app := newTestApp(t)
	base := Site{
		ID:           999,
		Name:         "corrupt-stream-hosts",
		PathPrefix:   "/s/corrupt",
		TargetURL:    "http://127.0.0.1:8096",
		PlaybackMode: "direct",
		UAMode:       "infuse",
		Enabled:      true,
	}

	invalidJSON := base
	invalidJSON.StreamHosts = "{"
	if err := app.PM.StartSite(invalidJSON); err == nil || !strings.Contains(err.Error(), "invalid stream_hosts") {
		t.Fatalf("invalid JSON error = %v", err)
	}

	invalidURL := base
	invalidURL.StreamHosts = `["file://media.example.com/path"]`
	if err := app.PM.StartSite(invalidURL); err == nil || !strings.Contains(err.Error(), "invalid stream host") {
		t.Fatalf("invalid stream host error = %v", err)
	}
	invalidUA := base
	invalidUA.UAMode = customUAMode
	invalidUA.CustomUserAgent = "Custom UA"
	invalidUA.CustomClient = ""
	invalidUA.CustomVersion = "1.0"
	if err := app.PM.StartSite(invalidUA); err == nil || !strings.Contains(err.Error(), "invalid UA profile") {
		t.Fatalf("invalid custom UA profile error = %v", err)
	}
	if app.PM.IsRunning(base.ID) {
		t.Fatal("corrupt site unexpectedly started")
	}
}

func TestProxyRoutesPlaybackRequestsToPlaybackTarget(t *testing.T) {
	app := newTestApp(t)

	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")
		w.Header().Set("Content-Security-Policy", "default-src 'none'")
		w.Write([]byte("api:" + r.URL.Path))
	}))
	defer apiServer.Close()

	playbackServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("playback:" + r.URL.Path))
	}))
	defer playbackServer.Close()

	site, err := app.DB.CreateSite("split", "/s/split", apiServer.URL, playbackServer.URL, "direct", "[]", "infuse", 0, 0)
	if err != nil {
		t.Fatalf("CreateSite: %v", err)
	}
	if err := app.PM.StartSite(*site); err != nil {
		t.Fatalf("StartSite: %v", err)
	}
	t.Cleanup(func() { app.PM.StopSite(site.ID) })

	ts := newTestHTTPServer(t, app)

	mainResp, err := http.Get(ts.URL + site.PathPrefix + "/System/Info")
	if err != nil {
		t.Fatalf("GET main route: %v", err)
	}
	defer mainResp.Body.Close()
	if got := mainResp.Header.Get("X-Frame-Options"); got != "SAMEORIGIN" {
		t.Fatalf("upstream X-Frame-Options = %q, want SAMEORIGIN", got)
	}
	if got := mainResp.Header.Get("Content-Security-Policy"); got != "default-src 'none'" {
		t.Fatalf("upstream Content-Security-Policy = %q, want preserved value", got)
	}

	playbackResp, err := http.Get(ts.URL + site.PathPrefix + "/emby/Videos/123/stream")
	if err != nil {
		t.Fatalf("GET playback route: %v", err)
	}
	defer playbackResp.Body.Close()

	if body := mustReadBody(t, mainResp); !strings.Contains(body, "api:/System/Info") {
		t.Fatalf("main route body = %q", body)
	}
	if body := mustReadBody(t, playbackResp); !strings.Contains(body, "playback:/emby/Videos/123/stream") {
		t.Fatalf("playback route body = %q", body)
	}
}

func TestProxyPreservesConfiguredUpstreamBasePath(t *testing.T) {
	app := newTestApp(t)
	received := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- r.URL.RequestURI()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	site, err := app.DB.CreateSite("base-path", "/s/base-path", upstream.URL+"/emby?from=base", "", "direct", "[]", "infuse", 0, 0)
	if err != nil {
		t.Fatalf("CreateSite: %v", err)
	}
	if err := app.PM.StartSite(*site); err != nil {
		t.Fatalf("StartSite: %v", err)
	}
	t.Cleanup(func() { app.PM.StopSite(site.ID) })

	ts := newTestHTTPServer(t, app)

	resp, err := http.Get(ts.URL + site.PathPrefix + "/System/Info/Public?client=1")
	if err != nil {
		t.Fatalf("GET through proxy: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}

	select {
	case got := <-received:
		if got != "/emby/System/Info/Public?from=base&client=1" {
			t.Fatalf("upstream request URI = %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("upstream did not receive request")
	}
}

func TestProxyPlaybackRequestsFallBackToMainTarget(t *testing.T) {
	app := newTestApp(t)

	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("api:" + r.URL.Path))
	}))
	defer apiServer.Close()

	site, err := app.DB.CreateSite("single", "/s/single", apiServer.URL, "", "direct", "[]", "infuse", 0, 0)
	if err != nil {
		t.Fatalf("CreateSite: %v", err)
	}
	if err := app.PM.StartSite(*site); err != nil {
		t.Fatalf("StartSite: %v", err)
	}
	t.Cleanup(func() { app.PM.StopSite(site.ID) })

	ts := newTestHTTPServer(t, app)

	resp, err := http.Get(ts.URL + site.PathPrefix + "/Videos/42/stream")
	if err != nil {
		t.Fatalf("GET fallback playback route: %v", err)
	}
	defer resp.Body.Close()

	if body := mustReadBody(t, resp); !strings.Contains(body, "api:/Videos/42/stream") {
		t.Fatalf("fallback playback body = %q", body)
	}
}

func TestRequestClientKeyForwardingHeaderVariants(t *testing.T) {
	trusted, err := ParseTrustedProxyCIDRs("172.17.0.0/16")
	if err != nil {
		t.Fatalf("parse trusted proxies: %v", err)
	}

	// X-Forwarded-For: first valid IP wins when the peer is trusted.
	xffReq := httptest.NewRequest(http.MethodGet, "/", nil)
	xffReq.RemoteAddr = "172.17.0.1:45678"
	xffReq.Header.Set("X-Forwarded-For", "198.51.100.9, 203.0.113.99")
	if got := requestClientKey(xffReq, trusted); got != "198.51.100.9" {
		t.Fatalf("X-Forwarded-For client key = %q, want 198.51.100.9", got)
	}

	// X-Real-IP takes precedence over X-Forwarded-For.
	bothReq := httptest.NewRequest(http.MethodGet, "/", nil)
	bothReq.RemoteAddr = "172.17.0.1:45678"
	bothReq.Header.Set("X-Real-IP", "203.0.113.25")
	bothReq.Header.Set("X-Forwarded-For", "198.51.100.9")
	if got := requestClientKey(bothReq, trusted); got != "203.0.113.25" {
		t.Fatalf("precedence client key = %q, want X-Real-IP 203.0.113.25", got)
	}

	// Trusted peer without forwarding headers falls back to the peer IP.
	plainReq := httptest.NewRequest(http.MethodGet, "/", nil)
	plainReq.RemoteAddr = "172.17.0.1:45678"
	if got := requestClientKey(plainReq, trusted); got != "172.17.0.1" {
		t.Fatalf("fallback client key = %q, want 172.17.0.1", got)
	}

	// No peer at all maps to "unknown".
	noPeerReq := httptest.NewRequest(http.MethodGet, "/", nil)
	noPeerReq.RemoteAddr = ""
	if got := requestClientKey(noPeerReq, trusted); got != "unknown" {
		t.Fatalf("no-peer client key = %q, want unknown", got)
	}
}

func TestAccessLogBufferDropsOldestWhenFull(t *testing.T) {
	buffer := NewAccessLogBuffer(3)
	for i := int64(1); i <= 5; i++ {
		buffer.Append(AccessLogEntry{Timestamp: i, Path: fmt.Sprintf("/p%d", i)})
	}
	logs := buffer.Drain()
	if len(logs) != 3 {
		t.Fatalf("drained %d logs, want 3", len(logs))
	}
	if logs[0].Timestamp != 3 || logs[2].Timestamp != 5 {
		t.Fatalf("kept timestamps = %d..%d, want 3..5", logs[0].Timestamp, logs[2].Timestamp)
	}
	if got := buffer.Drain(); len(got) != 0 {
		t.Fatalf("second drain = %d entries, want 0", len(got))
	}
}

func TestAccessLogBufferUnboundedWhenMaxLenZero(t *testing.T) {
	buffer := NewAccessLogBuffer(0)
	for i := 0; i < 100; i++ {
		buffer.Append(AccessLogEntry{Timestamp: int64(i)})
	}
	if got := buffer.Drain(); len(got) != 100 {
		t.Fatalf("drained %d entries, want 100 (unbounded)", len(got))
	}
}

func TestStatusWriterRecordsFirstStatusCode(t *testing.T) {
	rec := httptest.NewRecorder()
	writer := &statusWriter{ResponseWriter: rec}
	writer.WriteHeader(http.StatusNotFound)
	if writer.status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", writer.status)
	}
	writer.WriteHeader(http.StatusOK) // later writes must not override
	if writer.status != http.StatusNotFound {
		t.Fatalf("status after second WriteHeader = %d, want 404", writer.status)
	}

	implicit := &statusWriter{ResponseWriter: httptest.NewRecorder()}
	implicit.Write([]byte("ok"))
	if implicit.status != http.StatusOK {
		t.Fatalf("implicit status = %d, want 200", implicit.status)
	}
}
