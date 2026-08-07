package internal

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// handleUpdateCheck reports the current panel version and the latest GitHub
// release. GET /api/admin/update/check (panel JWT)
func (a *App) handleUpdateCheck(c *gin.Context) {
	latest := ""
	if a.Updater != nil {
		if v, err := a.Updater.LatestVersion(); err == nil {
			latest = v
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"current":          a.Version,
		"latest":           latest,
		"update_available": latest != "" && latest != a.Version,
	})
}

// handleAdminSettings returns read-only panel information for the settings
// dialog. GET /api/admin/settings (panel JWT)
func (a *App) handleAdminSettings(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"version":           a.Version,
		"panel_url":         requestPanelURL(c),
		"route_prefix":      a.RoutePrefix,
		"relay_api_enabled": a.RelayToken != "",
		"geolite_enabled":   a.GeoLite != nil,
	})
}

// handleRepoInfo returns GitHub repository metadata for the About dialog.
// GET /api/admin/repo-info (panel JWT). Returns 200 with an error field so
// the dialog degrades gracefully when GitHub is unreachable.
func (a *App) handleRepoInfo(c *gin.Context) {
	if a.Updater == nil {
		c.JSON(http.StatusOK, gin.H{"error": "repository info unavailable"})
		return
	}
	info, err := a.Updater.RepoInfo()
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"error": "无法获取仓库信息"})
		return
	}
	c.JSON(http.StatusOK, info)
}

// handleUpdateStart triggers the panel self-update in the background; the
// process execs itself with the new binary on success.
// POST /api/admin/update (panel JWT)
func (a *App) handleUpdateStart(c *gin.Context) {
	if a.Updater == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "self-update not available"})
		return
	}
	a.Updater.UpdateAsync()
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
