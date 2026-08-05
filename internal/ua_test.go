package internal

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNormalizeCustomUAConfig(t *testing.T) {
	mode, userAgent, client, version, err := normalizeUAConfig(" CUSTOM ", "  Meridian/$1  ", "  Custom Client  ", " 1.2.3 ")
	if err != nil {
		t.Fatalf("normalize custom config: %v", err)
	}
	if mode != customUAMode || userAgent != "Meridian/$1" || client != "Custom Client" || version != "1.2.3" {
		t.Fatalf("normalized custom config = %#v %#v %#v %#v", mode, userAgent, client, version)
	}

	for _, tc := range []struct {
		name      string
		userAgent string
		client    string
		version   string
	}{
		{"missing user agent", "", "Client", "1.0"},
		{"missing client", "UA", "", "1.0"},
		{"missing version", "UA", "Client", ""},
		{"whitespace only", " ", "Client", "1.0"},
		{"too long user agent", strings.Repeat("a", maxCustomUserAgentLen+1), "Client", "1.0"},
		{"too long client", "UA", strings.Repeat("a", maxCustomClientLen+1), "1.0"},
		{"too long version", "UA", "Client", strings.Repeat("a", maxCustomVersionLen+1)},
		{"new line", "UA\nnext", "Client", "1.0"},
		{"non ascii", "UA", "Clïent", "1.0"},
		{"client quote", "UA", "Client\"", "1.0"},
		{"version backslash", "UA", "Client", "1\\0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, _, _, err := normalizeUAConfig("custom", tc.userAgent, tc.client, tc.version); err == nil {
				t.Fatal("invalid custom UA configuration unexpectedly accepted")
			}
		})
	}

	mode, userAgent, client, version, err = normalizeUAConfig("web", "stale", "stale", "stale")
	if err != nil {
		t.Fatalf("normalize preset config: %v", err)
	}
	if mode != "web" || userAgent != "" || client != "" || version != "" {
		t.Fatalf("preset did not clear custom fields: %#v %#v %#v %#v", mode, userAgent, client, version)
	}
}

func TestMergeSiteUAConfigUsesCompleteSnapshots(t *testing.T) {
	old := Site{
		UAMode:          customUAMode,
		CustomUserAgent: "Old UA",
		CustomClient:    "Old Client",
		CustomVersion:   "1.0",
	}

	mode, userAgent, client, version, err := mergeSiteUAConfig(old, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("preserve existing custom config: %v", err)
	}
	if mode != customUAMode || userAgent != "Old UA" || client != "Old Client" || version != "1.0" {
		t.Fatalf("preserved config = %#v %#v %#v %#v", mode, userAgent, client, version)
	}

	if _, _, _, _, err := mergeSiteUAConfig(old, stringPointer(customUAMode), nil, nil, nil); err == nil {
		t.Fatal("custom mode without its full triplet unexpectedly accepted")
	}
	if _, _, _, _, err := mergeSiteUAConfig(old, nil, stringPointer("New UA"), nil, stringPointer("2.0")); err == nil {
		t.Fatal("partial custom triplet unexpectedly accepted")
	}

	mode, userAgent, client, version, err = mergeSiteUAConfig(old, stringPointer("web"), stringPointer(""), stringPointer(""), stringPointer(""))
	if err != nil {
		t.Fatalf("switch to preset: %v", err)
	}
	if mode != "web" || userAgent != "" || client != "" || version != "" {
		t.Fatalf("preset switch did not clear custom values: %#v %#v %#v %#v", mode, userAgent, client, version)
	}
}

func TestApplyUAProfileHeadersRewritesClientAndVersionIdentity(t *testing.T) {
	header := http.Header{}
	header.Set("User-Agent", "OldUA/1.0")
	header.Set("X-Emby-Authorization", `MediaBrowser Client="Old Client", Device="TV", Version="9.9.9"`)
	header.Set("Authorization", `MediaBrowser Client="Old Client", Device="TV", Version="9.9.9"`)

	applyUAProfileHeaders(header, uaProfiles["client"])

	if got := header.Get("User-Agent"); got != uaProfiles["client"].UserAgent {
		t.Fatalf("User-Agent = %q, want %q", got, uaProfiles["client"].UserAgent)
	}
	if got := header.Get("X-Emby-Authorization"); !strings.Contains(got, `Client="Emby Theater"`) {
		t.Fatalf("X-Emby-Authorization = %q", got)
	}
	if got := header.Get("X-Emby-Authorization"); !strings.Contains(got, `Version="4.7.0"`) {
		t.Fatalf("X-Emby-Authorization version = %q", got)
	}
	if got := header.Get("Authorization"); !strings.Contains(got, `Client="Emby Theater"`) {
		t.Fatalf("Authorization = %q", got)
	}
	if got := header.Get("Authorization"); !strings.Contains(got, `Version="4.7.0"`) {
		t.Fatalf("Authorization version = %q", got)
	}
}

func TestApplyCustomUAProfileHeadersSafelyRewritesOnlyValidEmbyValues(t *testing.T) {
	profile := UAProfile{
		Name:      "Custom",
		UserAgent: "Meridian/" + "$" + "1/" + "$" + "{1}$",
		Client:    "Client/" + "$" + "1/" + "$" + "{1}$",
		Version:   "1." + "$" + "0$",
	}
	valid := "MediaBrowser Device=\"TV\", DeviceId=\"device-1\", Version=\"old\", Client=\"old\""
	missing := "Emby Device=\"Tablet\", DeviceId=\"device-2\""
	duplicate := "MediaBrowser Client=\"one\", Client=\"two\", Version=\"old\""
	escaped := "MediaBrowser Client=\"bad\\\\\\\"value\", Device=\"TV\""
	header := http.Header{
		"X-Emby-Authorization": []string{valid, missing, duplicate, escaped},
		"Authorization":        []string{"Bearer opaque-token"},
	}

	applyUAProfileHeaders(header, profile)

	if got := header.Get("User-Agent"); got != profile.UserAgent {
		t.Fatalf("User-Agent = %q, want %q", got, profile.UserAgent)
	}
	values := header.Values("X-Emby-Authorization")
	if len(values) != 4 {
		t.Fatalf("authorization values=%d, want 4", len(values))
	}
	wantValid := "MediaBrowser Device=\"TV\", DeviceId=\"device-1\", Version=\"" + profile.Version + "\", Client=\"" + profile.Client + "\""
	if values[0] != wantValid {
		t.Fatalf("rewritten header = %q, want %q", values[0], wantValid)
	}
	if !strings.Contains(values[1], "Device=\"Tablet\"") || !strings.Contains(values[1], "DeviceId=\"device-2\"") || !strings.Contains(values[1], "Client=\""+profile.Client+"\"") || !strings.Contains(values[1], "Version=\""+profile.Version+"\"") {
		t.Fatalf("missing-field header = %q", values[1])
	}
	if values[2] != duplicate {
		t.Fatalf("duplicate Client header was modified: %q", values[2])
	}
	if values[3] != escaped {
		t.Fatalf("escaped header was modified: %q", values[3])
	}
	if got := header.Get("Authorization"); got != "Bearer opaque-token" {
		t.Fatalf("unsupported Authorization scheme was modified: %q", got)
	}
}

func TestCustomUAProfileIsConsistentAcrossHTTPWebSocketAndRedirects(t *testing.T) {
	profile := UAProfile{
		Name:      "Custom",
		UserAgent: "Meridian Test/1.0",
		Client:    "Meridian Test",
		Version:   "1.0.0",
	}
	authorization := "MediaBrowser Device=\"TV\", DeviceId=\"device-1\", Client=\"old\", Version=\"old\""
	assertIdentity := func(t *testing.T, header http.Header) {
		t.Helper()
		if got := header.Get("User-Agent"); got != profile.UserAgent {
			t.Fatalf("User-Agent = %q, want %q", got, profile.UserAgent)
		}
		got := header.Get("X-Emby-Authorization")
		for _, want := range []string{"Device=\"TV\"", "DeviceId=\"device-1\"", "Client=\"" + profile.Client + "\"", "Version=\"" + profile.Version + "\""} {
			if !strings.Contains(got, want) {
				t.Fatalf("authorization = %q, missing %q", got, want)
			}
		}
	}

	t.Run("http", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodGet, "http://meridian.example/System/Info", nil)
		request.Header.Set("X-Emby-Authorization", authorization)
		header := request.Header.Clone()
		prepareUpstreamHeaders(header, request, profile)
		assertIdentity(t, header)
	})

	t.Run("websocket", func(t *testing.T) {
		target, err := normalizeTargetURL("https://upstream.example.com")
		if err != nil {
			t.Fatalf("normalize target: %v", err)
		}
		request := httptest.NewRequest(http.MethodGet, "http://meridian.example/socket", nil)
		request.Header.Set("Connection", "Upgrade")
		request.Header.Set("Upgrade", "websocket")
		request.Header.Set("X-Emby-Authorization", authorization)
		header := prepareWebSocketUpstreamHeaders(request, target, profile)
		assertIdentity(t, header)
	})

	t.Run("redirect", func(t *testing.T) {
		calls := 0
		var followedHeaders http.Header
		transport := &clientTransport{
			client: &http.Client{
				Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
					calls++
					if calls == 1 {
						return &http.Response{
							StatusCode: http.StatusFound,
							Header:     http.Header{"Location": []string{"https://media.example.com/Videos/1/stream"}},
							Body:       io.NopCloser(strings.NewReader("")),
							Request:    request,
						}, nil
					}
					followedHeaders = request.Header.Clone()
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     make(http.Header),
						Body:       io.NopCloser(strings.NewReader("ok")),
						Request:    request,
					}, nil
				}),
				CheckRedirect: func(req *http.Request, via []*http.Request) error {
					applyUAProfileHeaders(req.Header, profile)
					return nil
				},
			},
		}
		request := httptest.NewRequest(http.MethodGet, "https://api.example.com/Videos/1/stream", nil)
		request.Header.Set("X-Emby-Authorization", authorization)
		applyUAProfileHeaders(request.Header, profile)
		response, err := transport.RoundTrip(request)
		if err != nil {
			t.Fatalf("follow redirect: %v", err)
		}
		response.Body.Close()
		if calls != 2 {
			t.Fatalf("calls = %d, want 2", calls)
		}
		assertIdentity(t, followedHeaders)
	})
}
