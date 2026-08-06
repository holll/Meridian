//go:build unix

package relay

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

// Updater downloads and installs the latest relay release from GitHub, then
// restarts the process in place via syscall.Exec.
type Updater struct {
	Repo        string // owner/repo of the release source
	HTTPClient  *http.Client
	GitHubAPI   string // override in tests; default https://api.github.com
	DownloadURL string // override in tests; default https://github.com
}

func NewUpdater() *Updater {
	return &Updater{
		Repo:       "holll/Meridian",
		HTTPClient: &http.Client{Timeout: 10 * time.Minute},
	}
}

// UpdateAsync runs Update in the background; failures are logged, not fatal.
func (u *Updater) UpdateAsync() {
	go func() {
		if err := u.Update(); err != nil {
			log.Printf("[relay] self-update failed: %v", err)
		}
	}()
}

// Update downloads the latest release binary matching this platform, verifies
// it against SHA256SUMS, atomically replaces the running binary and execs it.
func (u *Updater) Update() error {
	version, err := u.latestVersion()
	if err != nil {
		return err
	}
	asset := "meridian-relay-" + runtime.GOOS + "-" + runtime.GOARCH
	base := u.downloadURL() + "/" + u.Repo + "/releases/download/" + version
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate executable: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(exe), ".meridian-relay-update-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := u.downloadAndVerify(base+"/"+asset, base+"/SHA256SUMS", asset, tmp); err != nil {
		return err
	}
	if err := tmp.Chmod(0755); err != nil {
		return err
	}

	// Swap with the previous binary kept aside for manual rollback.
	prev := exe + ".previous"
	if err := os.Rename(exe, prev); err != nil {
		return fmt.Errorf("backup current binary: %w", err)
	}
	if err := os.Rename(tmp.Name(), exe); err != nil {
		_ = os.Rename(prev, exe) // roll back
		return fmt.Errorf("install new binary: %w", err)
	}
	log.Printf("[relay] self-updated to %s, restarting in place", version)

	return syscall.Exec(exe, os.Args, os.Environ())
}

// latestVersion returns the tag_name of the newest GitHub release.
func (u *Updater) latestVersion() (string, error) {
	req, err := http.NewRequest(http.MethodGet, u.apiURL()+"/repos/"+u.Repo+"/releases/latest", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "meridian-relay-selfupdate")
	resp, err := u.HTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("latest release -> %d", resp.StatusCode)
	}
	var payload struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", err
	}
	if !strings.HasPrefix(payload.TagName, "v") {
		return "", fmt.Errorf("unexpected tag name %q", payload.TagName)
	}
	return payload.TagName, nil
}

// downloadAndVerify streams binURL into w and checks its SHA-256 against the
// checksum file hosted next to it.
func (u *Updater) downloadAndVerify(binURL, sumsURL, asset string, w *os.File) error {
	resp, err := u.HTTPClient.Get(binURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s -> %d", asset, resp.StatusCode)
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(w, h), resp.Body); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	actual := hex.EncodeToString(h.Sum(nil))

	sumsResp, err := u.HTTPClient.Get(sumsURL)
	if err != nil {
		return err
	}
	defer sumsResp.Body.Close()
	if sumsResp.StatusCode != http.StatusOK {
		return fmt.Errorf("checksum download -> %d", sumsResp.StatusCode)
	}
	data, err := io.ReadAll(sumsResp.Body)
	if err != nil {
		return err
	}
	var expected string
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && (fields[1] == asset || fields[1] == "*"+asset) {
			expected = strings.ToLower(fields[0])
			break
		}
	}
	if len(expected) != 64 {
		return fmt.Errorf("SHA256SUMS missing %s", asset)
	}
	if actual != expected {
		return fmt.Errorf("sha256 mismatch for %s", asset)
	}
	return nil
}

func (u *Updater) apiURL() string {
	if u.GitHubAPI != "" {
		return strings.TrimRight(u.GitHubAPI, "/")
	}
	return "https://api.github.com"
}

func (u *Updater) downloadURL() string {
	if u.DownloadURL != "" {
		return strings.TrimRight(u.DownloadURL, "/")
	}
	return "https://github.com"
}
