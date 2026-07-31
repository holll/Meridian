// Package relay implements the configuration sync and traffic reporting loop
// for a meridian-relay node. A Syncer periodically polls the Master for the
// current site list, applies any changes to the local ProxyManager, and
// reports accumulated traffic back to the Master.
package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"meridian/internal"
)

const (
	syncInterval    = 30 * time.Second
	trafficInterval = 60 * time.Second
	httpTimeout     = 15 * time.Second
)

// Config holds the static configuration for a Syncer.
type Config struct {
	MasterURL  string // e.g. https://panel.example.com
	RelayToken string // shared secret (Authorization: Bearer <token>)
	RelayName  string // unique node identifier
	ISP        string // e.g. "telecom", "unicom", "mobile"
	Version    string // relay binary version string
}

// Syncer manages config synchronisation and traffic reporting for one relay node.
type Syncer struct {
	cfg         Config
	pm          *internal.ProxyManager
	httpClient  *http.Client
	mu          sync.RWMutex
	routePrefix string // learned from Master on first Sync()
}

// NewSyncer constructs a Syncer backed by the given ProxyManager.
func NewSyncer(cfg Config, pm *internal.ProxyManager) *Syncer {
	return &Syncer{
		cfg: cfg,
		pm:  pm,
		httpClient: &http.Client{
			Timeout: httpTimeout,
		},
	}
}

// RoutePrefix returns the global route prefix received from Master.
// Empty string until the first successful Sync().
func (s *Syncer) RoutePrefix() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.routePrefix
}

// Run starts the sync/traffic-report loops and blocks until ctx is cancelled.
// Call Sync() once before Run() to perform the initial synchronous fetch.
// On exit it performs a final traffic flush.
func (s *Syncer) Run(ctx context.Context) {
	// Register node immediately so it appears in the panel right away.
	s.register()

	syncTicker := time.NewTicker(syncInterval)
	trafficTicker := time.NewTicker(trafficInterval)
	defer syncTicker.Stop()
	defer trafficTicker.Stop()

	for {
		select {
		case <-syncTicker.C:
			s.Sync()
		case <-trafficTicker.C:
			s.reportTraffic()
		case <-ctx.Done():
			s.flushTraffic()
			return
		}
	}
}

// register sends a POST /api/relay/nodes/register so the Master knows this node.
func (s *Syncer) register() {
	body := map[string]string{
		"name":    s.cfg.RelayName,
		"isp":     s.cfg.ISP,
		"version": s.cfg.Version,
	}
	if err := s.post("/api/relay/nodes/register", body); err != nil {
		log.Printf("[relay] register failed: %v", err)
	} else {
		log.Printf("[relay] registered as %q (isp: %s)", s.cfg.RelayName, s.cfg.ISP)
	}
}

// sitesResponse is the JSON structure returned by GET /api/relay/sites.
type sitesResponse struct {
	RoutePrefix string             `json:"route_prefix"`
	Sites       []relaySitePayload `json:"sites"`
}

type relaySitePayload struct {
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

// Sync fetches the current site list from Master and applies it.
// Also updates the locally cached route_prefix.
func (s *Syncer) Sync() {
	resp, err := s.get("/api/relay/sites")
	if err != nil {
		log.Printf("[relay] sync failed: %v", err)
		return
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("[relay] read sites response: %v", err)
		return
	}
	if resp.StatusCode != http.StatusOK {
		log.Printf("[relay] GET /api/relay/sites → %d", resp.StatusCode)
		return
	}

	var payload sitesResponse
	if err := json.Unmarshal(data, &payload); err != nil {
		log.Printf("[relay] parse sites response: %v", err)
		return
	}

	// Cache the global route prefix from Master.
	s.mu.Lock()
	s.routePrefix = payload.RoutePrefix
	s.mu.Unlock()

	sites := make([]internal.Site, 0, len(payload.Sites))
	for _, p := range payload.Sites {
		hosts, _ := json.Marshal(p.StreamHosts)
		if hosts == nil {
			hosts = []byte("[]")
		}
		sites = append(sites, internal.Site{
			ID:                p.ID,
			Name:              p.Name,
			PathPrefix:        p.PathPrefix,
			TargetURL:         p.TargetURL,
			PlaybackTargetURL: p.PlaybackTargetURL,
			PlaybackMode:      p.PlaybackMode,
			StreamHosts:       string(hosts),
			UAMode:            p.UAMode,
			CustomUserAgent:   p.CustomUserAgent,
			CustomClient:      p.CustomClient,
			CustomVersion:     p.CustomVersion,
			SpeedLimit:        p.SpeedLimit,
			TrafficQuota:      p.TrafficQuota,
			Enabled:           p.Enabled,
		})
	}

	s.pm.ApplyConfig(sites)
	log.Printf("[relay] synced %d sites, route_prefix=%q", len(sites), payload.RoutePrefix)
}

// reportTraffic drains counters and POSTs them to Master.
// Always sends a request even when there is no traffic so that Master can
// update last_seen — this doubles as the heartbeat.
func (s *Syncer) reportTraffic() {
	deltas := s.pm.DrainTraffic()
	type siteEntry struct {
		ID       int64 `json:"id"`
		BytesIn  int64 `json:"bytes_in"`
		BytesOut int64 `json:"bytes_out"`
	}
	sites := make([]siteEntry, 0, len(deltas))
	for _, d := range deltas {
		sites = append(sites, siteEntry{ID: d.SiteID, BytesIn: d.BytesIn, BytesOut: d.BytesOut})
	}
	body := map[string]interface{}{
		"relay_name": s.cfg.RelayName,
		"timestamp":  time.Now().Unix(),
		"sites":      sites,
	}
	if err := s.post("/api/relay/traffic", body); err != nil {
		log.Printf("[relay] heartbeat/traffic report failed: %v", err)
	}
}

// flushTraffic is a best-effort final traffic report on graceful shutdown.
func (s *Syncer) flushTraffic() {
	log.Println("[relay] flushing traffic before exit...")
	s.reportTraffic()
}

// --- HTTP helpers ---

func (s *Syncer) url(path string) string {
	base := strings.TrimRight(s.cfg.MasterURL, "/")
	return base + path
}

func (s *Syncer) get(path string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, s.url(path), nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+s.cfg.RelayToken)
	return s.httpClient.Do(req)
}

func (s *Syncer) post(path string, body interface{}) error {
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal body: %w", err)
	}
	req, err := http.NewRequest(http.MethodPost, s.url(path), bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+s.cfg.RelayToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}
