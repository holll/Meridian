//go:build windows

package selfupdate

import (
	"errors"
	"net/http"
)

// Updater stub: Windows cannot exec-replace the running process, so
// self-update is unsupported there (install via install.sh instead).
type Updater struct {
	Repo        string
	AssetPrefix string
	HTTPClient  *http.Client // unused; kept for API parity with the unix build
}

func New(assetPrefix string) *Updater {
	return &Updater{Repo: "holll/Meridian", AssetPrefix: assetPrefix}
}

func (u *Updater) UpdateAsync() {}

func (u *Updater) Update() error {
	return errors.New("self-update is not supported on Windows")
}

// LatestVersion reports the newest GitHub release; unsupported on Windows.
func (u *Updater) LatestVersion() (string, error) {
	return "", errors.New("self-update is not supported on Windows")
}

// RepoInfo reports GitHub repository metadata; unsupported on Windows.
func (u *Updater) RepoInfo() (*RepoInfo, error) {
	return nil, errors.New("self-update is not supported on Windows")
}
