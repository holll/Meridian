package internal

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
)

// GET /api/dashboard
func (a *App) handleDashboard(c *gin.Context) {
	stats, err := a.DB.DashboardStats()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "dashboard unavailable"})
		return
	}
	stats["running_sites"] = a.PM.GetRunningCount()
	c.JSON(http.StatusOK, stats)
}

// GET /api/traffic/overview
func (a *App) trafficOverview(c *gin.Context) {
	stats, err := a.DB.DashboardStats()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "traffic overview unavailable"})
		return
	}
	c.JSON(http.StatusOK, stats)
}

// GET /api/traffic/:site_id
func (a *App) siteTraffic(c *gin.Context) {
	siteID, err := strconv.ParseInt(c.Param("site_id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid site id"})
		return
	}
	hours := 24
	if h := c.Query("hours"); h != "" {
		if v, err := strconv.Atoi(h); err == nil && v >= 1 && v <= 24*366 {
			hours = v
		} else {
			c.JSON(http.StatusBadRequest, gin.H{"error": "hours must be between 1 and 8784"})
			return
		}
	}
	logs, err := a.DB.GetTrafficLogs(siteID, hours)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, logs)
}

// GET /api/ua-profiles
func (a *App) handleUAProfiles(c *gin.Context) {
	profiles := make([]UAProfile, 0, len(uaProfiles))
	for _, p := range uaProfiles {
		profiles = append(profiles, p)
	}
	c.JSON(http.StatusOK, profiles)
}

// GET /api/events (SSE)
func (a *App) handleSSE(c *gin.Context) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Writer.Flush()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	ctx := c.Request.Context()

	if err := a.sendSSEEvent(c); err != nil {
		log.Printf("send initial SSE event: %v", err)
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := a.sendSSEEvent(c); err != nil {
				log.Printf("send SSE event: %v", err)
				return
			}
		}
	}
}

func (a *App) sendSSEEvent(c *gin.Context) error {
	stats, err := a.DB.DashboardStats()
	if err != nil {
		return err
	}
	stats["running_sites"] = a.PM.GetRunningCount()
	stats["total_requests"] = a.PM.GetTotalRequests()
	stats["uptime_seconds"] = int(time.Since(startTime).Seconds())
	stats["live_sites"] = a.PM.GetLiveSiteStats()

	data, err := json.Marshal(stats)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(c.Writer, "data: %s\n\n", data); err != nil {
		return err
	}
	c.Writer.Flush()
	return nil
}
