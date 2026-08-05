package internal

import (
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
)

// relayAccessLogsRequest is the body of POST /api/relay/access_logs.
type relayAccessLogsRequest struct {
	RelayName string           `json:"relay_name"`
	Logs      []AccessLogEntry `json:"logs"`
}

// handleRelayAccessLogs receives batched per-request access logs from a relay node.
// POST /api/relay/access_logs
func (a *App) handleRelayAccessLogs(c *gin.Context) {
	var req relayAccessLogsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	if req.RelayName == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "relay_name is required"})
		return
	}
	if len(req.Logs) > 2000 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "too many logs in one batch"})
		return
	}
	if err := a.DB.AddAccessLogs(req.RelayName, req.Logs); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to record access logs"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// parseAccessLogFilters reads the shared site_id/relay_name/from/to query
// parameters used by both access log endpoints.
func parseAccessLogFilters(c *gin.Context) (siteID, from, to int64, relayName string) {
	if v := c.Query("site_id"); v != "" {
		siteID, _ = strconv.ParseInt(v, 10, 64)
	}
	relayName = c.Query("relay_name")
	if v := c.Query("from"); v != "" {
		from, _ = strconv.ParseInt(v, 10, 64)
	}
	if v := c.Query("to"); v != "" {
		to, _ = strconv.ParseInt(v, 10, 64)
	}
	return
}

// handleAccessLogs returns a paginated list of access logs for the management panel.
// GET /api/access_logs?site_id=&relay_name=&from=&to=&page=&page_size=
func (a *App) handleAccessLogs(c *gin.Context) {
	siteID, from, to, relayName := parseAccessLogFilters(c)
	page := 1
	if v := c.Query("page"); v != "" {
		if p, err := strconv.Atoi(v); err == nil && p > 0 {
			page = p
		}
	}
	pageSize := 50
	if v := c.Query("page_size"); v != "" {
		if ps, err := strconv.Atoi(v); err == nil && ps > 0 {
			pageSize = ps
		}
	}
	if pageSize > 200 {
		pageSize = 200
	}

	logs, total, err := a.DB.QueryAccessLogs(siteID, relayName, from, to, page, pageSize)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to query access logs"})
		return
	}
	if a.GeoLite != nil {
		for i := range logs {
			logs[i].Geo = a.GeoLite.Lookup(logs[i].ClientIP)
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"total":     total,
		"page":      page,
		"page_size": pageSize,
		"logs":      logs,
	})
}

// handleAccessLogStats returns aggregated access log statistics for the analysis page.
// GET /api/access_logs/stats?site_id=&relay_name=&from=&to=
func (a *App) handleAccessLogStats(c *gin.Context) {
	siteID, from, to, relayName := parseAccessLogFilters(c)
	if from <= 0 {
		from = time.Now().Add(-24 * time.Hour).Unix()
	}
	if to <= 0 {
		to = time.Now().Unix()
	}

	stats, err := a.DB.QueryAccessLogStats(siteID, relayName, from, to)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to query access log stats"})
		return
	}
	stats.Regions = []GeoAgg{}
	stats.Orgs = []GeoAgg{}
	if a.GeoLite != nil {
		for i := range stats.TopIPs {
			stats.TopIPs[i].Geo = a.GeoLite.Lookup(stats.TopIPs[i].IP)
		}
		// Region / ISP rollups over all distinct client IPs in the window.
		ipAggs, aggErr := a.DB.QueryAccessLogIPAggs(siteID, relayName, from, to)
		if aggErr != nil {
			log.Printf("access log geo aggregation failed: %v", aggErr)
		} else {
			stats.Regions, stats.Orgs = AggregateGeo(ipAggs, a.GeoLite)
		}
	}
	c.JSON(http.StatusOK, stats)
}
