package internal

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// secureCompare does constant-time string comparison to resist timing attacks.
func secureCompare(a, b string) bool {
	ha := sha256.Sum256([]byte(a))
	hb := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ha[:], hb[:]) == 1
}

// unmarshalJSON is a thin wrapper used to decode JSON stored in string columns.
func unmarshalJSON(s string, v interface{}) error {
	return json.Unmarshal([]byte(s), v)
}

// relayTokenMiddleware validates the shared RELAY_TOKEN from the Authorization header.
func relayTokenMiddleware(token string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if token == "" {
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "relay API not configured"})
			return
		}
		auth := c.GetHeader("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing bearer token"})
			return
		}
		provided := strings.TrimPrefix(auth, "Bearer ")
		if !secureCompare(provided, token) {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid relay token"})
			return
		}
		c.Next()
	}
}

// handleRelayGetSites returns the full enabled site list for relay nodes to proxy.
// GET /api/relay/sites
func (a *App) handleRelayGetSites(c *gin.Context) {
	sites, err := a.DB.ListSites()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list sites"})
		return
	}

	// Build a lightweight payload — omit sensitive internal fields.
	type relaySite struct {
		ID                int64    `json:"id"`
		Name              string   `json:"name"`
		PathPrefix        string   `json:"path_prefix"`
		TargetURL         string   `json:"target_url"`
		PlaybackTargetURL string   `json:"playback_target_url"`
		PlaybackMode      string   `json:"playback_mode"`
		StreamHosts       []string `json:"stream_hosts"`
		UAMode            string   `json:"ua_mode"`
		CustomUserAgent   string   `json:"custom_user_agent,omitempty"`
		CustomClient      string   `json:"custom_client,omitempty"`
		CustomVersion     string   `json:"custom_version,omitempty"`
		SpeedLimit        int      `json:"speed_limit"`
		TrafficQuota      int64    `json:"traffic_quota"`
		Enabled           bool     `json:"enabled"`
	}

	payload := make([]relaySite, 0, len(sites))
	for _, s := range sites {
		var hosts []string
		if s.StreamHosts != "" && s.StreamHosts != "[]" {
			_ = unmarshalJSON(s.StreamHosts, &hosts)
		}
		if hosts == nil {
			hosts = []string{}
		}
		payload = append(payload, relaySite{
			ID:                s.ID,
			Name:              s.Name,
			PathPrefix:        s.PathPrefix,
			TargetURL:         s.TargetURL,
			PlaybackTargetURL: s.PlaybackTargetURL,
			PlaybackMode:      s.PlaybackMode,
			StreamHosts:       hosts,
			UAMode:            s.UAMode,
			CustomUserAgent:   s.CustomUserAgent,
			CustomClient:      s.CustomClient,
			CustomVersion:     s.CustomVersion,
			SpeedLimit:        s.SpeedLimit,
			TrafficQuota:      s.TrafficQuota,
			Enabled:           s.Enabled,
		})
	}
	c.JSON(http.StatusOK, gin.H{
		"route_prefix": a.RoutePrefix,
		"sites":        payload,
	})
}

// relayTrafficRequest is the body of POST /api/relay/traffic.
type relayTrafficRequest struct {
	RelayName string             `json:"relay_name"`
	Timestamp int64              `json:"timestamp"`
	Sites     []RelayTrafficSite `json:"sites"`
}

// handleRelayTraffic receives per-site traffic increments from a relay node.
// POST /api/relay/traffic
func (a *App) handleRelayTraffic(c *gin.Context) {
	var req relayTrafficRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	if req.RelayName == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "relay_name is required"})
		return
	}
	if req.Timestamp <= 0 {
		req.Timestamp = time.Now().Unix()
	}
	if err := a.DB.AddRelayTraffic(req.RelayName, req.Timestamp, req.Sites); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to record traffic"})
		return
	}
	// Refresh the observed public IP (and auto-detected operator) on every
	// heartbeat so they stay current.
	if ip := requestClientKey(c.Request, a.TrustedProxies); ip != "unknown" {
		_ = a.DB.UpdateRelayNodeIP(req.RelayName, ip)
		if a.GeoLite != nil {
			if isp := DetectISP(a.GeoLite.Lookup(ip)); isp != "" {
				_ = a.DB.UpdateRelayNodeISP(req.RelayName, isp)
			}
		}
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// relayRegisterRequest is the body of POST /api/relay/nodes/register.
type relayRegisterRequest struct {
	Name     string `json:"name"`
	ISP      string `json:"isp"`
	PublicIP string `json:"public_ip"`
	Version  string `json:"version"`
}

// handleRelayRegister registers or updates a relay node's metadata.
// POST /api/relay/nodes/register
func (a *App) handleRelayRegister(c *gin.Context) {
	var req relayRegisterRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	if req.Name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}
	// Prefer the address the Master observes for the connection (through
	// trusted proxies when configured); fall back to a self-reported IP.
	publicIP := requestClientKey(c.Request, a.TrustedProxies)
	if publicIP == "unknown" && req.PublicIP != "" {
		publicIP = req.PublicIP
	}
	// Operator is auto-detected from GeoLite when available; the relay-supplied
	// value is only a fallback.
	isp := req.ISP
	if a.GeoLite != nil && publicIP != "unknown" {
		if detected := DetectISP(a.GeoLite.Lookup(publicIP)); detected != "" {
			isp = detected
		}
	}
	now := time.Now().Unix()
	if err := a.DB.RegisterRelayNode(req.Name, isp, publicIP, req.Version, now); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to register node"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// handleRelayNodes lists all relay nodes for the management panel.
// GET /api/relay/nodes
func (a *App) handleRelayNodes(c *gin.Context) {
	nodes, err := a.DB.GetRelayNodes()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list relay nodes"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"nodes": nodes})
}
