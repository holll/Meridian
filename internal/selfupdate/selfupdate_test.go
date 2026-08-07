//go:build unix

package selfupdate

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"
)

// newTestUpdater wires an Updater to a mock GitHub server serving the release
// API, the binary asset and SHA256SUMS.
func newTestUpdater(t *testing.T, tag string, payload []byte) (*Updater, *httptest.Server) {
	t.Helper()
	sums := fmt.Sprintf("%x  meridian-relay-%s-%s\n", sha256.Sum256(payload), runtime.GOOS, runtime.GOARCH)
	asset := "meridian-relay-" + runtime.GOOS + "-" + runtime.GOARCH
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/repos/holll/Meridian"):
			w.Write([]byte(`{"html_url":"https://github.com/holll/Meridian","description":"test repo","stargazers_count":42,"forks_count":7,"open_issues_count":3,"license":{"spdx_id":"MIT"}}`))
		case strings.HasSuffix(r.URL.Path, "/releases/latest"):
			fmt.Fprintf(w, `{"tag_name":%q}`, tag)
		case strings.HasSuffix(r.URL.Path, "/"+asset):
			w.Write(payload)
		case strings.HasSuffix(r.URL.Path, "/SHA256SUMS"):
			fmt.Fprint(w, sums)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	u := New("meridian-relay-")
	u.HTTPClient = &http.Client{Transport: http.DefaultTransport}
	u.GitHubAPI = srv.URL
	u.DownloadURL = srv.URL
	return u, srv
}

func TestUpdaterLatestVersion(t *testing.T) {
	u, _ := newTestUpdater(t, "v9.9.9", []byte("payload"))
	ver, err := u.LatestVersion()
	if err != nil {
		t.Fatalf("latestVersion: %v", err)
	}
	if ver != "v9.9.9" {
		t.Fatalf("version = %q, want v9.9.9", ver)
	}
}

func TestUpdaterRepoInfo(t *testing.T) {
	u, _ := newTestUpdater(t, "v9.9.9", []byte("payload"))
	info, err := u.RepoInfo()
	if err != nil {
		t.Fatalf("RepoInfo: %v", err)
	}
	if info.HTMLURL != "https://github.com/holll/Meridian" {
		t.Fatalf("html_url = %q", info.HTMLURL)
	}
	if info.Stars != 42 || info.Forks != 7 || info.OpenIssues != 3 {
		t.Fatalf("stats = %+v, want 42/7/3", info)
	}
	if info.License != "MIT" {
		t.Fatalf("license = %q, want MIT", info.License)
	}
}

func TestUpdaterDownloadAndVerify(t *testing.T) {
	payload := []byte("fake relay binary")
	u, _ := newTestUpdater(t, "v9.9.9", payload)

	tmp, err := os.CreateTemp(t.TempDir(), "relay-bin-*")
	if err != nil {
		t.Fatalf("create temp: %v", err)
	}
	defer tmp.Close()
	asset := "meridian-relay-" + runtime.GOOS + "-" + runtime.GOARCH
	base := u.downloadURL() + "/holll/Meridian/releases/download/v9.9.9"
	if err := u.downloadAndVerify(base+"/"+asset, base+"/SHA256SUMS", asset, tmp); err != nil {
		t.Fatalf("downloadAndVerify: %v", err)
	}
	got, err := os.ReadFile(tmp.Name())
	if err != nil {
		t.Fatalf("read downloaded: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("downloaded = %q, want %q", got, payload)
	}
}

func TestUpdaterDownloadRejectsChecksumMismatch(t *testing.T) {
	u, srv := newTestUpdater(t, "v9.9.9", []byte("payload"))

	// Serve the same binary asset but a wrong SHA256SUMS so the check fails.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/SHA256SUMS") {
			fmt.Fprintf(w, "%s  meridian-relay-%s-%s\n", strings.Repeat("0", 64), runtime.GOOS, runtime.GOARCH)
			return
		}
		w.Write([]byte("payload"))
	}))
	defer srv2.Close()
	u.DownloadURL = srv2.URL

	tmp, err := os.CreateTemp(t.TempDir(), "relay-bin-*")
	if err != nil {
		t.Fatalf("create temp: %v", err)
	}
	defer tmp.Close()
	asset := "meridian-relay-" + runtime.GOOS + "-" + runtime.GOARCH
	base := u.downloadURL() + "/holll/Meridian/releases/download/v9.9.9"
	if err := u.downloadAndVerify(base+"/"+asset, base+"/SHA256SUMS", asset, tmp); err == nil {
		t.Fatal("downloadAndVerify must fail on checksum mismatch")
	}
	_ = srv
}

func TestUpdaterDownloadRejectsMissingAsset(t *testing.T) {
	u, srv := newTestUpdater(t, "v9.9.9", []byte("payload"))

	// A server without the binary asset.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/SHA256SUMS") {
			fmt.Fprintf(w, "%s  meridian-relay-%s-%s\n", strings.Repeat("0", 64), runtime.GOOS, runtime.GOARCH)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv2.Close()
	u.DownloadURL = srv2.URL

	tmp, err := os.CreateTemp(t.TempDir(), "relay-bin-*")
	if err != nil {
		t.Fatalf("create temp: %v", err)
	}
	defer tmp.Close()
	asset := "meridian-relay-" + runtime.GOOS + "-" + runtime.GOARCH
	base := u.downloadURL() + "/holll/Meridian/releases/download/v9.9.9"
	if err := u.downloadAndVerify(base+"/"+asset, base+"/SHA256SUMS", asset, tmp); err == nil {
		t.Fatal("downloadAndVerify must fail when the binary asset is missing")
	}
	_ = srv
}
