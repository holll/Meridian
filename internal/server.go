package internal

import (
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

var startTime = time.Now()

// App holds application state shared across all handlers.
type App struct {
	DB              *DB
	PM              *ProxyManager
	SiteLifecycleMu sync.Mutex
	SetupToken      string
	RoutePrefix     string
	RelayToken      string // shared secret for Relay ↔ Master API authentication
	loginLimiter    *loginRateLimiter
	limiterOnce     sync.Once
	TrustedProxies  []*net.IPNet
	GeoLite         *GeoLite           // IP geolocation/ASN lookup; nil when unavailable
	UpdateRequests  UpdateRequestStore // one-shot relay self-update requests
}

const (
	maxLoginFailures       = 5
	maxTrackedLoginClients = 10000
	loginFailureWindow     = 15 * time.Minute
	loginLockoutDuration   = 15 * time.Minute
)

type loginAttempt struct {
	failures     int
	firstFailure time.Time
	blockedUntil time.Time
}

type loginRateLimiter struct {
	mu       sync.Mutex
	attempts map[string]loginAttempt
}

func newLoginRateLimiter() *loginRateLimiter {
	return &loginRateLimiter{attempts: make(map[string]loginAttempt)}
}

func (l *loginRateLimiter) keyFor(client string) string {
	if _, exists := l.attempts[client]; !exists && len(l.attempts) >= maxTrackedLoginClients {
		return "__overflow__"
	}
	return client
}

func (l *loginRateLimiter) allow(client string, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	client = l.keyFor(client)
	attempt, ok := l.attempts[client]
	if !ok {
		return true, 0
	}
	if now.Before(attempt.blockedUntil) {
		return false, attempt.blockedUntil.Sub(now)
	}
	if now.Sub(attempt.firstFailure) >= loginFailureWindow {
		delete(l.attempts, client)
	}
	return true, 0
}

func (l *loginRateLimiter) recordFailure(client string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	client = l.keyFor(client)
	attempt := l.attempts[client]
	if attempt.firstFailure.IsZero() || now.Sub(attempt.firstFailure) >= loginFailureWindow {
		attempt = loginAttempt{firstFailure: now}
	}
	attempt.failures++
	if attempt.failures >= maxLoginFailures {
		attempt.blockedUntil = now.Add(loginLockoutDuration)
	}
	l.attempts[client] = attempt
}

func (l *loginRateLimiter) reset(client string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.attempts, l.keyFor(client))
}

func (a *App) limiter() *loginRateLimiter {
	a.limiterOnce.Do(func() {
		if a.loginLimiter == nil {
			a.loginLimiter = newLoginRateLimiter()
		}
	})
	return a.loginLimiter
}

// ParseTrustedProxyCIDRs parses comma-separated CIDR strings.
func ParseTrustedProxyCIDRs(value string) ([]*net.IPNet, error) {
	var networks []*net.IPNet
	for _, raw := range strings.Split(value, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		_, network, err := net.ParseCIDR(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid TRUSTED_PROXY_CIDRS entry %q: %w", raw, err)
		}
		networks = append(networks, network)
	}
	return networks, nil
}

func remoteAddressIP(remoteAddr string) net.IP {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err == nil {
		return net.ParseIP(host)
	}
	return net.ParseIP(remoteAddr)
}

func isTrustedProxy(ip net.IP, networks []*net.IPNet) bool {
	if ip == nil {
		return false
	}
	for _, network := range networks {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

func requestClientKey(r *http.Request, trustedProxies []*net.IPNet) string {
	peerIP := remoteAddressIP(r.RemoteAddr)
	if isTrustedProxy(peerIP, trustedProxies) {
		if forwarded := net.ParseIP(strings.TrimSpace(r.Header.Get("X-Real-IP"))); forwarded != nil {
			return forwarded.String()
		}
		for _, raw := range strings.Split(r.Header.Get("X-Forwarded-For"), ",") {
			if forwarded := net.ParseIP(strings.TrimSpace(raw)); forwarded != nil {
				return forwarded.String()
			}
		}
	}
	if peerIP != nil {
		return peerIP.String()
	}
	if r.RemoteAddr != "" {
		return r.RemoteAddr
	}
	return "unknown"
}

// StaticHandler serves embedded SPA files with cache-busting headers.
func StaticHandler(staticFS fs.FS) http.Handler {
	fileServer := http.FileServer(http.FS(staticFS))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")
		path := r.URL.Path
		if path == "/" {
			path = "/index.html"
		}
		f, err := staticFS.Open(strings.TrimPrefix(path, "/"))
		if err == nil {
			_ = f.Close()
			fileServer.ServeHTTP(w, r)
			return
		}
		r.URL.Path = "/"
		fileServer.ServeHTTP(w, r)
	})
}

// ValidateRoutePrefix normalizes and validates a global route prefix.
// Returns the normalized prefix (starts with /, no trailing slash) or empty string if disabled.
func ValidateRoutePrefix(prefix string) (string, error) {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return "", nil
	}
	if !strings.HasPrefix(prefix, "/") {
		prefix = "/" + prefix
	}
	prefix = strings.TrimRight(prefix, "/")
	if prefix == "" || prefix == "/" {
		return "", fmt.Errorf("ROUTE_PREFIX must not be /")
	}
	if len(prefix) > 256 {
		return "", fmt.Errorf("ROUTE_PREFIX must be at most 256 characters")
	}
	if strings.Contains(prefix, "//") || strings.Contains(prefix, "..") {
		return "", fmt.Errorf("ROUTE_PREFIX must not contain // or ..")
	}
	for _, r := range prefix {
		if r <= 0x20 || r > 0x7e {
			return "", fmt.Errorf("ROUTE_PREFIX must contain printable non-space ASCII only")
		}
	}
	for _, reserved := range reservedPathPrefixes {
		if prefix == reserved || strings.HasPrefix(prefix+"/", reserved+"/") {
			return "", fmt.Errorf("ROUTE_PREFIX %q conflicts with a reserved panel path", prefix)
		}
	}
	return prefix, nil
}
