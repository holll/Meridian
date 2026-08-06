//go:build windows

package relay

import "errors"

// Updater stub: Windows cannot exec-replace the running process, so
// self-update is unsupported there (install via install-relay.sh instead).
type Updater struct {
	Repo string
}

func NewUpdater() *Updater { return &Updater{Repo: "holll/Meridian"} }

func (u *Updater) UpdateAsync() {}

func (u *Updater) Update() error {
	return errors.New("self-update is not supported on Windows")
}
