package internal

import (
	"sync"
)

// accessLogFilteredPaths are high-frequency request paths excluded from access
// log storage and analysis (e.g. Jellyfin playback progress polling).
var accessLogFilteredPaths = []string{"/emby/Sessions/Playing/Progress"}

// accessLogPathFiltered reports whether a request path is excluded from logs.
func accessLogPathFiltered(path string) bool {
	for _, p := range accessLogFilteredPaths {
		if path == p {
			return true
		}
	}
	return false
}

// AccessLogEntry is a single request access log record reported by a relay node.
type AccessLogEntry struct {
	Timestamp int64  `json:"timestamp"` // unix seconds
	SiteID    int64  `json:"site_id"`
	ClientIP  string `json:"client_ip"`
	Method    string `json:"method"`
	Path      string `json:"path"`
	Status    int    `json:"status"`
	LatencyMs int64  `json:"latency_ms"`
	BytesIn   int64  `json:"bytes_in"`
	BytesOut  int64  `json:"bytes_out"`
}

// AccessLogBuffer is a bounded, concurrency-safe buffer of pending access log
// entries. When full, the oldest entries are dropped to keep memory bounded.
type AccessLogBuffer struct {
	mu     sync.Mutex
	logs   []AccessLogEntry
	maxLen int
}

// NewAccessLogBuffer creates a buffer that keeps at most maxLen entries.
func NewAccessLogBuffer(maxLen int) *AccessLogBuffer {
	return &AccessLogBuffer{
		logs:   make([]AccessLogEntry, 0, 64),
		maxLen: maxLen,
	}
}

// Append adds an entry, dropping the oldest one when the buffer is full.
func (b *AccessLogBuffer) Append(e AccessLogEntry) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.maxLen > 0 && len(b.logs) >= b.maxLen {
		// Shift in place (amortized O(1)); the backing array is reused.
		b.logs = append(b.logs[1:], e)
		return
	}
	b.logs = append(b.logs, e)
}

// Drain removes and returns all pending entries.
func (b *AccessLogBuffer) Drain() []AccessLogEntry {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := b.logs
	b.logs = make([]AccessLogEntry, 0, 64)
	return out
}
