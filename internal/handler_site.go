package internal

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// GET /api/sites
func (a *App) listSites(c *gin.Context) {
	sites, err := a.DB.ListSites()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	type SiteWithStatus struct {
		Site
		Running bool `json:"running"`
	}
	result := make([]SiteWithStatus, len(sites))
	for i, s := range sites {
		result[i] = SiteWithStatus{Site: s, Running: a.PM.IsRunning(s.ID)}
	}
	c.JSON(http.StatusOK, result)
}

// POST /api/sites
func (a *App) createSite(c *gin.Context) {
	var req struct {
		Name              string   `json:"name" binding:"required"`
		PathPrefix        string   `json:"path_prefix" binding:"required"`
		TargetURL         string   `json:"target_url" binding:"required"`
		PlaybackTargetURL string   `json:"playback_target_url"`
		PlaybackMode      string   `json:"playback_mode"`
		StreamHosts       []string `json:"stream_hosts"`
		UAMode            string   `json:"ua_mode"`
		CustomUserAgent   string   `json:"custom_user_agent"`
		CustomClient      string   `json:"custom_client"`
		CustomVersion     string   `json:"custom_version"`
		Quota             int64    `json:"traffic_quota"`
		SpeedLimit        int      `json:"speed_limit"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name, path_prefix, and target_url are required"})
		return
	}
	if req.UAMode == "" {
		req.UAMode = "infuse"
	}
	if req.PlaybackMode == "" {
		req.PlaybackMode = "direct"
	}
	req.Name = strings.TrimSpace(req.Name)
	req.PlaybackMode = strings.ToLower(strings.TrimSpace(req.PlaybackMode))
	normalizedMode, customUserAgent, customClient, customVersion, err := normalizeUAConfig(req.UAMode, req.CustomUserAgent, req.CustomClient, req.CustomVersion)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	req.UAMode = normalizedMode
	req.CustomUserAgent = customUserAgent
	req.CustomClient = customClient
	req.CustomVersion = customVersion
	if err := validateSiteSettings(req.Name, req.PathPrefix, req.TargetURL, req.PlaybackTargetURL, req.PlaybackMode, req.StreamHosts, req.UAMode, req.CustomUserAgent, req.CustomClient, req.CustomVersion, req.Quota, req.SpeedLimit); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	streamHostsJSON, _ := json.Marshal(req.StreamHosts)
	if req.StreamHosts == nil {
		streamHostsJSON = []byte("[]")
	}
	a.SiteLifecycleMu.Lock()
	defer a.SiteLifecycleMu.Unlock()
	site, err := a.DB.CreateSiteRecord(Site{
		Name:              req.Name,
		PathPrefix:        req.PathPrefix,
		TargetURL:         req.TargetURL,
		PlaybackTargetURL: req.PlaybackTargetURL,
		PlaybackMode:      req.PlaybackMode,
		StreamHosts:       string(streamHostsJSON),
		UAMode:            req.UAMode,
		CustomUserAgent:   req.CustomUserAgent,
		CustomClient:      req.CustomClient,
		CustomVersion:     req.CustomVersion,
		TrafficQuota:      req.Quota,
		SpeedLimit:        req.SpeedLimit,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if site.Enabled {
		if err := a.PM.StartSite(*site); err != nil {
			if deleteErr := a.DB.DeleteSite(site.ID); deleteErr != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("start site: %v; rollback create: %v", err, deleteErr)})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	}
	c.JSON(http.StatusCreated, site)
}

// PUT /api/sites/:id
func (a *App) updateSite(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid site id"})
		return
	}
	a.SiteLifecycleMu.Lock()
	defer a.SiteLifecycleMu.Unlock()
	oldSite, err := a.DB.GetSite(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "site not found"})
		return
	}
	var req struct {
		Name              string    `json:"name"`
		PathPrefix        string    `json:"path_prefix"`
		TargetURL         string    `json:"target_url"`
		PlaybackTargetURL *string   `json:"playback_target_url"`
		PlaybackMode      *string   `json:"playback_mode"`
		StreamHosts       *[]string `json:"stream_hosts"`
		UAMode            *string   `json:"ua_mode"`
		CustomUserAgent   *string   `json:"custom_user_agent"`
		CustomClient      *string   `json:"custom_client"`
		CustomVersion     *string   `json:"custom_version"`
		Quota             int64     `json:"traffic_quota"`
		SpeedLimit        *int      `json:"speed_limit"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	playbackTargetURL := oldSite.PlaybackTargetURL
	if req.PlaybackTargetURL != nil {
		playbackTargetURL = *req.PlaybackTargetURL
	}
	playbackMode := oldSite.PlaybackMode
	if req.PlaybackMode != nil {
		playbackMode = *req.PlaybackMode
	}
	streamHosts := oldSite.StreamHosts
	if req.StreamHosts != nil {
		sh, _ := json.Marshal(*req.StreamHosts)
		streamHosts = string(sh)
	}
	speedLimit := oldSite.SpeedLimit
	if req.SpeedLimit != nil {
		speedLimit = *req.SpeedLimit
	}
	uaMode, customUserAgent, customClient, customVersion, uaErr := mergeSiteUAConfig(*oldSite, req.UAMode, req.CustomUserAgent, req.CustomClient, req.CustomVersion)
	if uaErr != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": uaErr.Error()})
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	playbackMode = strings.ToLower(strings.TrimSpace(playbackMode))
	var streamHostList []string
	if err := json.Unmarshal([]byte(streamHosts), &streamHostList); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid stream_hosts"})
		return
	}
	candidate := *oldSite
	candidate.Name = req.Name
	candidate.PathPrefix = req.PathPrefix
	candidate.TargetURL = req.TargetURL
	candidate.PlaybackTargetURL = playbackTargetURL
	candidate.PlaybackMode = playbackMode
	candidate.StreamHosts = streamHosts
	candidate.UAMode = uaMode
	candidate.CustomUserAgent = customUserAgent
	candidate.CustomClient = customClient
	candidate.CustomVersion = customVersion
	candidate.TrafficQuota = req.Quota
	candidate.SpeedLimit = speedLimit
	if err := validateSiteSettings(candidate.Name, candidate.PathPrefix, candidate.TargetURL, candidate.PlaybackTargetURL, candidate.PlaybackMode, streamHostList, candidate.UAMode, candidate.CustomUserAgent, candidate.CustomClient, candidate.CustomVersion, candidate.TrafficQuota, candidate.SpeedLimit); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := a.DB.UpdateSiteRecord(candidate); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	site, err := a.DB.GetSite(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if site.Enabled {
		if a.PM.IsRunning(id) {
			a.PM.StopSite(id)
		}
		if err := a.PM.StartSite(*site); err != nil {
			if rollbackErr := a.DB.UpdateSiteRecord(*oldSite); rollbackErr != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("start updated site: %v; rollback update: %v", err, rollbackErr)})
				return
			}
			restoredSite, getErr := a.DB.GetSite(id)
			if getErr != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("start updated site: %v; reload rollback site: %v", err, getErr)})
				return
			}
			if oldSite.Enabled {
				if restartErr := a.PM.StartSite(*restoredSite); restartErr != nil {
					c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("start updated site: %v; restored configuration is enabled but proxy is not running: %v", err, restartErr)})
					return
				}
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	}
	c.JSON(http.StatusOK, site)
}

// DELETE /api/sites/:id
func (a *App) deleteSite(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid site id"})
		return
	}
	a.SiteLifecycleMu.Lock()
	defer a.SiteLifecycleMu.Unlock()
	a.PM.StopSite(id)
	if err := a.DB.DeleteSite(id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "deleted"})
}

// POST /api/sites/:id/toggle
func (a *App) toggleSite(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid site id"})
		return
	}
	a.SiteLifecycleMu.Lock()
	defer a.SiteLifecycleMu.Unlock()
	newState, err := a.DB.ToggleSite(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if newState {
		site, err := a.DB.GetSite(id)
		if err != nil {
			if _, revertErr := a.DB.ToggleSite(id); revertErr != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("load site: %v; rollback toggle: %v", err, revertErr)})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if err := a.PM.StartSite(*site); err != nil {
			if _, revertErr := a.DB.ToggleSite(id); revertErr != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("start site: %v; rollback toggle: %v", err, revertErr)})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	} else {
		a.PM.StopSite(id)
	}
	c.JSON(http.StatusOK, gin.H{"enabled": newState})
}

// GET /api/sites/:id/diag
func (a *App) diagSite(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid site id"})
		return
	}
	site, err := a.DB.GetSite(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "site not found"})
		return
	}
	result := diagnoseSite(site, a.PM)
	c.JSON(http.StatusOK, result)
}
