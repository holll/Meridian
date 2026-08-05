package internal

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type clientTransport struct {
	client *http.Client
}

func (t *clientTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.RequestURI = "" // http.Client.Do rejects non-empty RequestURI
	return t.client.Do(req)
}

type ProxyInstance struct {
	Site             Site
	handler          http.Handler
	startedAt        time.Time
	bytesIn          atomic.Int64
	bytesOut         atomic.Int64
	reqCount         atomic.Int64
	persistedTraffic atomic.Int64
}

type ProxyManager struct {
	mu             sync.RWMutex
	proxies        map[int64]*ProxyInstance
	database       *DB
	accessLogs     *AccessLogBuffer // relay mode only; nil on master
	trustedProxies []*net.IPNet     // CIDRs allowed to supply X-Forwarded-For / X-Real-IP
}

// SetTrustedProxies configures which peer CIDRs may supply client IP headers
// (TRUSTED_PROXY_CIDRS). Empty disables header-based client IP detection.
func (pm *ProxyManager) SetTrustedProxies(networks []*net.IPNet) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.trustedProxies = networks
}

func (pm *ProxyManager) trustedProxyNetworks() []*net.IPNet {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	return pm.trustedProxies
}

func NewProxyManager(db *DB) *ProxyManager {
	var logs *AccessLogBuffer
	if db == nil { // relay mode — collect access logs for reporting to master
		logs = NewAccessLogBuffer(1000)
	}
	return &ProxyManager{
		proxies:    make(map[int64]*ProxyInstance),
		database:   db,
		accessLogs: logs,
	}
}

// metered response writer
type meteredWriter struct {
	http.ResponseWriter
	written *atomic.Int64
}

func (m *meteredWriter) Write(b []byte) (int, error) {
	n, err := m.ResponseWriter.Write(b)
	m.written.Add(int64(n))
	return n, err
}

// Flush support for streaming
func (m *meteredWriter) Flush() {
	if f, ok := m.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack support for WebSocket upgrade
func (m *meteredWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := m.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, fmt.Errorf("hijack not supported")
}

// metered request body reader
type meteredReader struct {
	io.ReadCloser
	read *atomic.Int64
}

func (m *meteredReader) Read(p []byte) (int, error) {
	n, err := m.ReadCloser.Read(p)
	m.read.Add(int64(n))
	return n, err
}

type rateLimitedWriter struct {
	http.ResponseWriter
	bytesPerSec    int64
	written        *atomic.Int64
	requestWritten int64
	start          time.Time
}

func (w *rateLimitedWriter) Write(b []byte) (int, error) {
	if w.bytesPerSec <= 0 {
		n, err := w.ResponseWriter.Write(b)
		w.written.Add(int64(n))
		return n, err
	}
	totalWritten := 0
	for len(b) > 0 {
		elapsed := time.Since(w.start).Seconds()
		if elapsed < 0.001 {
			elapsed = 0.001
		}
		allowed := int64(elapsed*float64(w.bytesPerSec)) - w.requestWritten
		if allowed <= 0 {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		chunk := b
		if int64(len(chunk)) > allowed {
			chunk = b[:allowed]
		}
		n, err := w.ResponseWriter.Write(chunk)
		w.written.Add(int64(n))
		w.requestWritten += int64(n)
		totalWritten += n
		b = b[n:]
		if err != nil {
			return totalWritten, err
		}
		if n == 0 {
			return totalWritten, io.ErrNoProgress
		}
	}
	return totalWritten, nil
}

func (w *rateLimitedWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *rateLimitedWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := w.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, fmt.Errorf("hijack not supported")
}

// statusWriter records the HTTP status code written to the client. Used to
// capture the response status for access logging.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

func (s *statusWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := s.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, fmt.Errorf("hijack not supported")
}

// skipLogPaths lists path prefixes that should not be access-logged
// (health checks and similar long-lived or noisy endpoints).
var skipLogPaths = []string{"/healthz"}

// logAccess records one access log entry unless the path is on the skip list.
func (pm *ProxyManager) logAccess(e AccessLogEntry) {
	if pm.accessLogs == nil {
		return
	}
	for _, skip := range skipLogPaths {
		if strings.HasPrefix(e.Path, skip) {
			return
		}
	}
	pm.accessLogs.Append(e)
}

// DrainAccessLogs atomically takes all pending access log entries.
func (pm *ProxyManager) DrainAccessLogs() []AccessLogEntry {
	if pm.accessLogs == nil {
		return nil
	}
	return pm.accessLogs.Drain()
}

func isWebSocketUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

func normalizeTargetURL(addr string) (*url.URL, error) {
	addr = strings.TrimSpace(addr)
	addr = strings.ReplaceAll(addr, "：", ":")
	if addr == "" {
		return nil, fmt.Errorf("target URL is required")
	}
	if len(addr) > 2048 {
		return nil, fmt.Errorf("target URL is too long")
	}
	explicitScheme := strings.Contains(addr, "://")
	if !explicitScheme {
		addr = "http://" + addr
	}
	parsed, err := url.Parse(addr)
	if err != nil {
		return nil, err
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	if !explicitScheme && parsed.Port() == "443" {
		parsed.Scheme = "https"
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.Hostname() == "" || parsed.Opaque != "" {
		return nil, fmt.Errorf("invalid target URL")
	}
	if parsed.User != nil {
		return nil, fmt.Errorf("target URL must not contain credentials")
	}
	if parsed.Fragment != "" {
		return nil, fmt.Errorf("target URL must not contain a fragment")
	}
	if port := parsed.Port(); port != "" {
		value, err := strconv.Atoi(port)
		if err != nil || value < 1 || value > 65535 {
			return nil, fmt.Errorf("target URL contains an invalid port")
		}
	}
	return parsed, nil
}

func redirectHostKey(target *url.URL) string {
	host := strings.ToLower(strings.TrimSuffix(target.Hostname(), "."))
	if host == "" {
		return ""
	}
	port := target.Port()
	scheme := strings.ToLower(target.Scheme)
	if scheme != "http" && scheme != "https" {
		return ""
	}
	// Only match by host(:port), ignore scheme so http/https both work.
	if port == "" || (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
		return host
	}
	return net.JoinHostPort(host, port)
}

func singleJoiningSlash(a, b string) string {
	aSlash := strings.HasSuffix(a, "/")
	bSlash := strings.HasPrefix(b, "/")
	switch {
	case aSlash && bSlash:
		return a + b[1:]
	case !aSlash && !bSlash:
		return a + "/" + b
	default:
		return a + b
	}
}

func joinURLPath(base, request *url.URL) (joinedPath, joinedRawPath string) {
	if base.RawPath == "" && request.RawPath == "" {
		return singleJoiningSlash(base.Path, request.Path), ""
	}
	basePath := base.EscapedPath()
	requestPath := request.EscapedPath()
	baseSlash := strings.HasSuffix(basePath, "/")
	requestSlash := strings.HasPrefix(requestPath, "/")
	switch {
	case baseSlash && requestSlash:
		return base.Path + request.Path[1:], basePath + requestPath[1:]
	case !baseSlash && !requestSlash:
		return base.Path + "/" + request.Path, basePath + "/" + requestPath
	default:
		return base.Path + request.Path, basePath + requestPath
	}
}

func applyUpstreamURL(requestURL, upstream *url.URL) {
	requestURL.Scheme = upstream.Scheme
	requestURL.Host = upstream.Host
	requestURL.Path, requestURL.RawPath = joinURLPath(upstream, requestURL)
	switch {
	case upstream.RawQuery == "":
	case requestURL.RawQuery == "":
		requestURL.RawQuery = upstream.RawQuery
	default:
		requestURL.RawQuery = upstream.RawQuery + "&" + requestURL.RawQuery
	}
}

var reservedPathPrefixes = []string{"/api", "/css", "/js", "/favicon"}

func validatePathPrefix(prefix string) error {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return fmt.Errorf("path_prefix is required")
	}
	if len(prefix) > 256 {
		return fmt.Errorf("path_prefix must be at most 256 characters")
	}
	if prefix[0] != '/' {
		return fmt.Errorf("path_prefix must start with /")
	}
	if prefix == "/" {
		return fmt.Errorf("path_prefix must not be /")
	}
	if strings.Contains(prefix, "//") || strings.Contains(prefix, "..") {
		return fmt.Errorf("path_prefix must not contain // or ..")
	}
	for _, r := range prefix {
		if r <= 0x20 || r > 0x7e {
			return fmt.Errorf("path_prefix must contain printable non-space ASCII only")
		}
	}
	clean := strings.TrimRight(prefix, "/")
	for _, reserved := range reservedPathPrefixes {
		if clean == reserved || strings.HasPrefix(clean+"/", reserved+"/") {
			return fmt.Errorf("path_prefix %q conflicts with a reserved panel path", prefix)
		}
	}
	return nil
}

func validateSiteSettings(name string, pathPrefix string, targetURL, playbackTargetURL, playbackMode string, streamHosts []string, uaMode, customUserAgent, customClient, customVersion string, quota int64, speedLimit int) error {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 100 || strings.ContainsAny(name, "\r\n") {
		return fmt.Errorf("name must be 1-100 characters without line breaks")
	}
	if err := validatePathPrefix(pathPrefix); err != nil {
		return err
	}
	if _, err := normalizeTargetURL(targetURL); err != nil {
		return fmt.Errorf("invalid target_url: %w", err)
	}
	if strings.TrimSpace(playbackTargetURL) != "" {
		if _, err := normalizeTargetURL(playbackTargetURL); err != nil {
			return fmt.Errorf("invalid playback_target_url: %w", err)
		}
	}
	if playbackMode != "direct" && playbackMode != "redirect" {
		return fmt.Errorf("playback_mode must be direct or redirect")
	}
	if _, _, _, _, err := normalizeUAConfig(uaMode, customUserAgent, customClient, customVersion); err != nil {
		return err
	}
	if quota < 0 || speedLimit < 0 {
		return fmt.Errorf("traffic_quota and speed_limit must not be negative")
	}
	if len(streamHosts) > 128 {
		return fmt.Errorf("stream_hosts must contain at most 128 entries")
	}
	for _, host := range streamHosts {
		if _, err := normalizeTargetURL(host); err != nil {
			return fmt.Errorf("invalid stream host %q: %w", host, err)
		}
	}
	return nil
}

func isPlaybackRequest(path string) bool {
	path = strings.ToLower(path)
	switch {
	case strings.HasPrefix(path, "/videos/"),
		strings.HasPrefix(path, "/emby/videos/"),
		strings.HasPrefix(path, "/audio/"),
		strings.HasPrefix(path, "/emby/audio/"),
		strings.HasPrefix(path, "/livetv/"),
		strings.HasPrefix(path, "/emby/livetv/"):
		return true
	case strings.HasPrefix(path, "/items/"),
		strings.HasPrefix(path, "/emby/items/"):
		return strings.Contains(path, "/download") || strings.Contains(path, "/file")
	default:
		return false
	}
}

func upstreamTargetForRequest(r *http.Request, apiTarget, playbackTarget *url.URL) *url.URL {
	if playbackTarget != nil && isPlaybackRequest(r.URL.Path) {
		return playbackTarget
	}
	return apiTarget
}

func removeClientForwardingHeaders(header http.Header) {
	for name := range header {
		lowerName := strings.ToLower(name)
		if lowerName == "forwarded" || lowerName == "x-real-ip" || strings.HasPrefix(lowerName, "x-forwarded-") {
			delete(header, name)
		}
	}
}

func setTrustedForwardingHeaders(header http.Header, inbound *http.Request) {
	removeClientForwardingHeaders(header)
	if inbound == nil {
		return
	}
	if peerIP := remoteAddressIP(inbound.RemoteAddr); peerIP != nil {
		header.Set("X-Forwarded-For", peerIP.String())
		header.Set("X-Real-IP", peerIP.String())
	}
	if inbound.Host != "" {
		header.Set("X-Forwarded-Host", inbound.Host)
	}
	forwardedProto := "http"
	if inbound.TLS != nil {
		forwardedProto = "https"
	}
	header.Set("X-Forwarded-Proto", forwardedProto)
}

func prepareUpstreamHeaders(header http.Header, inbound *http.Request, profile UAProfile) {
	setTrustedForwardingHeaders(header, inbound)
	applyUAProfileHeaders(header, profile)
}

func prepareWebSocketUpstreamHeaders(inbound *http.Request, target *url.URL, profile UAProfile) http.Header {
	header := inbound.Header.Clone()
	for _, name := range []string{
		"Keep-Alive",
		"Proxy-Authenticate",
		"Proxy-Authorization",
		"Proxy-Connection",
		"TE",
		"Trailer",
		"Transfer-Encoding",
		"Upgrade",
	} {
		header.Del(name)
	}
	header.Set("Connection", "Upgrade")
	header.Set("Upgrade", "websocket")
	header.Set("Host", target.Host)
	prepareUpstreamHeaders(header, inbound, profile)
	return header
}

func handleWebSocket(w http.ResponseWriter, r *http.Request, target *url.URL, profile UAProfile, inst *ProxyInstance) {
	scheme := "ws"
	if target.Scheme == "https" {
		scheme = "wss"
	}
	// Hijack client connection
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "WebSocket not supported", 500)
		return
	}
	clientConn, clientBuf, err := hj.Hijack()
	if err != nil {
		log.Printf("[WS] hijack error: %v", err)
		return
	}
	defer clientConn.Close()

	// Connect to upstream
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	var upstreamConn net.Conn
	port := target.Port()
	if port == "" {
		if scheme == "wss" {
			port = "443"
		} else {
			port = "80"
		}
	}
	host := net.JoinHostPort(target.Hostname(), port)
	if scheme == "wss" {
		upstreamConn, err = tls.DialWithDialer(dialer, "tcp", host, secureTLSConfig(target.Hostname()))
	} else {
		upstreamConn, err = dialer.Dial("tcp", host)
	}
	if err != nil {
		log.Printf("[WS] upstream dial error: %v", err)
		clientConn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		return
	}
	defer upstreamConn.Close()

	// Send upgrade request to upstream
	if err := upstreamConn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		log.Printf("[WS] set handshake deadline: %v", err)
		return
	}
	upstreamURL := *r.URL
	applyUpstreamURL(&upstreamURL, target)
	reqLine := fmt.Sprintf("%s %s HTTP/1.1\r\n", r.Method, upstreamURL.RequestURI())
	if _, err := io.WriteString(upstreamConn, reqLine); err != nil { // #nosec G705 -- net/http rejects control characters in the parsed method and RequestURI.
		log.Printf("[WS] write request line: %v", err)
		return
	}
	upstreamHeader := prepareWebSocketUpstreamHeaders(r, target, profile)
	if err := upstreamHeader.Write(upstreamConn); err != nil {
		log.Printf("[WS] write request headers: %v", err)
		return
	}
	if _, err := io.WriteString(upstreamConn, "\r\n"); err != nil {
		log.Printf("[WS] finish request headers: %v", err)
		return
	}
	if err := upstreamConn.SetWriteDeadline(time.Time{}); err != nil {
		log.Printf("[WS] clear handshake deadline: %v", err)
		return
	}

	log.Printf("[WS] tunnel established: client <-> %s", target.Host)

	// Bidirectional copy. Wait for both directions to finish so that byte
	// counters are complete when the caller reads them for access logging.
	done := make(chan struct{}, 2)
	go func() {
		n, _ := io.Copy(upstreamConn, clientBuf)
		inst.bytesIn.Add(n)
		done <- struct{}{}
	}()
	go func() {
		n, _ := io.Copy(clientConn, upstreamConn)
		inst.bytesOut.Add(n)
		done <- struct{}{}
	}()
	<-done
	<-done
}

func (pm *ProxyManager) StartSite(site Site) error {
	target, err := normalizeTargetURL(site.TargetURL)
	if err != nil {
		return fmt.Errorf("invalid target URL: %w", err)
	}
	var playbackTarget *url.URL
	if strings.TrimSpace(site.PlaybackTargetURL) != "" {
		playbackTarget, err = normalizeTargetURL(site.PlaybackTargetURL)
		if err != nil {
			return fmt.Errorf("invalid playback target URL: %w", err)
		}
	}

	// Build playback hosts set from target + playback_target_url + stream_hosts.
	// The main target host must be included so that cover-image and other
	// API responses that redirect back to the Emby server are followed
	// through the proxy rather than leaked to the client as bare 302s.
	playbackHostsSet := make(map[string]bool)
	playbackHostsSet[redirectHostKey(target)] = true
	if playbackTarget != nil {
		playbackHostsSet[redirectHostKey(playbackTarget)] = true
	}
	var extraHosts []string
	if strings.TrimSpace(site.StreamHosts) != "" {
		if err := json.Unmarshal([]byte(site.StreamHosts), &extraHosts); err != nil {
			return fmt.Errorf("invalid stream_hosts: %w", err)
		}
	}
	for _, raw := range extraHosts {
		parsed, err := normalizeTargetURL(raw)
		if err != nil {
			return fmt.Errorf("invalid stream host %q: %w", raw, err)
		}
		playbackHostsSet[redirectHostKey(parsed)] = true
		if playbackTarget == nil {
			playbackTarget = parsed
		}
	}
	if len(playbackHostsSet) > 0 {
		keys := make([]string, 0, len(playbackHostsSet))
		for k := range playbackHostsSet {
			keys = append(keys, k)
		}
		log.Printf("[%s] playback hosts registered: %v", site.Name, keys)
	}

	profile, err := resolveSiteUAProfile(site)
	if err != nil {
		return fmt.Errorf("invalid UA profile: %w", err)
	}
	inst := &ProxyInstance{Site: site, startedAt: time.Now()}
	inst.persistedTraffic.Store(site.TrafficUsed)

	isRedirectMode := playbackTarget != nil && site.PlaybackMode == "redirect"
	proxyTransport := http.DefaultTransport.(*http.Transport).Clone()
	proxyTransport.TLSClientConfig = secureTLSConfig("")
	proxyTransport.ResponseHeaderTimeout = 30 * time.Second
	proxyTransport.MaxIdleConnsPerHost = 32

	proxy := &httputil.ReverseProxy{
		Transport: proxyTransport,
		Rewrite: func(proxyReq *httputil.ProxyRequest) {
			// The inbound request carries the raw request-line URI (RequestURI),
			// which is preserved by Clone. Clear it so transports rebuild the
			// request line from the rewritten URL: http.Transport would otherwise
			// send the stale URI, and http.Client.Do rejects non-empty RequestURI.
			proxyReq.Out.RequestURI = ""
			var upstream *url.URL
			if isRedirectMode {
				upstream = target
			} else {
				upstream = upstreamTargetForRequest(proxyReq.In, target, playbackTarget)
			}
			applyUpstreamURL(proxyReq.Out.URL, upstream)
			proxyReq.Out.Host = upstream.Host
			prepareUpstreamHeaders(proxyReq.Out.Header, proxyReq.In, profile)
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("[%s] proxy error: %v", site.Name, err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			w.Write([]byte(`{"error":"upstream unavailable"}`))
		},
	}

	if isRedirectMode {
		proxy.Transport = &clientTransport{
			client: &http.Client{
				Transport: proxyTransport,
				CheckRedirect: func(req *http.Request, via []*http.Request) error {
					applyUAProfileHeaders(req.Header, profile)
					return nil
				},
			},
		}
	}

	// Speed limit in bytes/sec (field is in Mbps, 0 = unlimited)
	speedLimitBytes := int64(site.SpeedLimit) * 125000 // Mbps -> bytes/sec

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inst.reqCount.Add(1)
		start := time.Now()
		bin0, bout0 := inst.bytesIn.Load(), inst.bytesOut.Load()
		sw := &statusWriter{ResponseWriter: w}
		// Prefer real client IP from trusted proxies (X-Real-IP / X-Forwarded-For);
		// falls back to the peer address.
		clientIP := requestClientKey(r, pm.trustedProxyNetworks())
		defer func() {
			status := sw.status
			if status == 0 {
				status = http.StatusOK
			}
			pm.logAccess(AccessLogEntry{
				Timestamp: time.Now().Unix(),
				SiteID:    site.ID,
				ClientIP:  clientIP,
				Method:    r.Method,
				Path:      r.URL.Path,
				Status:    status,
				LatencyMs: time.Since(start).Milliseconds(),
				BytesIn:   inst.bytesIn.Load() - bin0,
				BytesOut:  inst.bytesOut.Load() - bout0,
			})
		}()

		if site.TrafficQuota > 0 {
			currentUsed := inst.persistedTraffic.Load() + inst.bytesIn.Load() + inst.bytesOut.Load()
			if currentUsed >= site.TrafficQuota {
				log.Printf("[%s] quota exceeded (%d/%d bytes), rejecting %s", site.Name, currentUsed, site.TrafficQuota, r.URL.Path)
				sw.Header().Set("Content-Type", "application/json")
				sw.WriteHeader(http.StatusForbidden)
				sw.Write([]byte(`{"error":"traffic quota exceeded"}`))
				return
			}
		}

		if isWebSocketUpgrade(r) {
			wsTarget := upstreamTargetForRequest(r, target, playbackTarget)
			if isRedirectMode {
				wsTarget = target
			}
			log.Printf("[%s] websocket -> %s %s", site.Name, wsTarget.Host, r.URL.Path)
			handleWebSocket(sw, r, wsTarget, profile, inst)
			return
		}

		// Log upstream routing decision
		if playbackTarget != nil {
			chosen := upstreamTargetForRequest(r, target, playbackTarget)
			if isRedirectMode {
				chosen = target
			}
			if chosen == playbackTarget {
				log.Printf("[%s] playback -> %s %s", site.Name, chosen.Host, r.URL.Path)
			}
		}

		if r.Body != nil {
			r.Body = &meteredReader{ReadCloser: r.Body, read: &inst.bytesIn}
		}

		var rw http.ResponseWriter
		if speedLimitBytes > 0 {
			rw = &rateLimitedWriter{
				ResponseWriter: sw,
				bytesPerSec:    speedLimitBytes,
				written:        &inst.bytesOut,
				start:          time.Now(),
			}
		} else {
			rw = &meteredWriter{ResponseWriter: sw, written: &inst.bytesOut}
		}

		// httputil.ReverseProxy panics with "net/http: abort Handler" when the
		// client disconnects mid-stream. This is expected behavior, not a bug.
		defer func() {
			if p := recover(); p != nil {
				if s, ok := p.(string); ok && s == "net/http: abort Handler" {
					log.Printf("[%s] client disconnected: %s", site.Name, r.URL.Path)
					return
				}
				panic(p) // re-panic for unexpected panics
			}
		}()
		proxy.ServeHTTP(rw, r)
	})

	inst.handler = handler

	pm.mu.Lock()
	delete(pm.proxies, site.ID)
	pm.proxies[site.ID] = inst
	pm.mu.Unlock()

	if len(playbackHostsSet) > 0 {
		hosts := make([]string, 0, len(playbackHostsSet))
		for h := range playbackHostsSet {
			hosts = append(hosts, h)
		}
		log.Printf("[%s] proxy %s -> %s (playback hosts: %s, mode: %s, UA: %s)", site.Name, site.PathPrefix, site.TargetURL, strings.Join(hosts, ", "), site.PlaybackMode, site.UAMode)
	} else {
		log.Printf("[%s] proxy %s -> %s (UA: %s)", site.Name, site.PathPrefix, site.TargetURL, site.UAMode)
	}

	return nil
}

func (pm *ProxyManager) StopSite(id int64) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if inst, ok := pm.proxies[id]; ok {
		pm.flushProxyTraffic(inst)
		delete(pm.proxies, id)
	}
}

// TryServe routes an incoming request to the matching proxy site by path prefix.
// It strips the site prefix before calling the site handler. Returns false if no
// site prefix matches the request path.
func (pm *ProxyManager) TryServe(w http.ResponseWriter, r *http.Request) bool {
	pm.mu.RLock()
	var best *ProxyInstance
	bestLen := 0
	for _, inst := range pm.proxies {
		prefix := inst.Site.PathPrefix
		if len(prefix) > bestLen && (r.URL.Path == prefix || strings.HasPrefix(r.URL.Path, prefix+"/")) {
			best = inst
			bestLen = len(prefix)
		}
	}
	pm.mu.RUnlock()

	if best == nil {
		return false
	}

	// Strip the site prefix so the upstream sees the bare path.
	r2 := r.Clone(r.Context())
	r2.URL.Path = strings.TrimPrefix(r.URL.Path, best.Site.PathPrefix)
	if r2.URL.Path == "" {
		r2.URL.Path = "/"
	}
	if r.URL.RawPath != "" {
		r2.URL.RawPath = strings.TrimPrefix(r.URL.RawPath, best.Site.PathPrefix)
		if r2.URL.RawPath == "" {
			r2.URL.RawPath = "/"
		}
	}

	best.handler.ServeHTTP(w, r2)
	return true
}

func (pm *ProxyManager) IsRunning(id int64) bool {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	_, ok := pm.proxies[id]
	return ok
}

func (pm *ProxyManager) StartAllEnabled() (int, error) {
	sites, err := pm.database.ListSites()
	if err != nil {
		return 0, err
	}
	for _, s := range sites {
		if s.Enabled {
			if err := pm.StartSite(s); err != nil {
				log.Printf("[%s] failed to start: %v", s.Name, err)
			}
		}
	}
	return len(sites), nil
}

// Flush traffic counters to DB periodically
func (pm *ProxyManager) FlushTraffic() {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	for _, inst := range pm.proxies {
		pm.flushProxyTraffic(inst)
	}
}

func (pm *ProxyManager) flushProxyTraffic(inst *ProxyInstance) {
	in := inst.bytesIn.Swap(0)
	out := inst.bytesOut.Swap(0)
	if in == 0 && out == 0 {
		return
	}
	log.Printf("[%s] flush traffic: in=%d out=%d", inst.Site.Name, in, out)
	if pm.database == nil {
		// relay mode — restore counters; caller drains via DrainTraffic
		inst.bytesIn.Add(in)
		inst.bytesOut.Add(out)
		return
	}
	if err := pm.database.addTraffic(inst.Site.ID, in, out); err != nil {
		inst.bytesIn.Add(in)
		inst.bytesOut.Add(out)
		log.Printf("[%s] failed to flush traffic: %v", inst.Site.Name, err)
		return
	}
	delta := in + out
	inst.persistedTraffic.Add(delta)
	inst.Site.TrafficUsed += delta
}

// SiteTrafficDelta holds an atomic traffic snapshot for one site.
type SiteTrafficDelta struct {
	SiteID   int64
	BytesIn  int64
	BytesOut int64
}

// DrainTraffic atomically swaps all traffic counters to zero and returns the
// deltas. Used by relay nodes to collect traffic before reporting to Master.
func (pm *ProxyManager) DrainTraffic() []SiteTrafficDelta {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	var deltas []SiteTrafficDelta
	for _, inst := range pm.proxies {
		in := inst.bytesIn.Swap(0)
		out := inst.bytesOut.Swap(0)
		if in == 0 && out == 0 {
			continue
		}
		deltas = append(deltas, SiteTrafficDelta{
			SiteID:   inst.Site.ID,
			BytesIn:  in,
			BytesOut: out,
		})
	}
	return deltas
}

// ApplyConfig reconciles the proxy manager's running set against a new site
// list. Sites that are enabled but not running are started; running sites that
// are missing from the new list (or disabled) are stopped.
func (pm *ProxyManager) ApplyConfig(sites []Site) {
	desired := make(map[int64]Site, len(sites))
	for _, s := range sites {
		if s.Enabled {
			desired[s.ID] = s
		}
	}

	// Stop proxies that are no longer desired
	pm.mu.Lock()
	var toStop []int64
	for id := range pm.proxies {
		if _, ok := desired[id]; !ok {
			toStop = append(toStop, id)
		}
	}
	pm.mu.Unlock()
	for _, id := range toStop {
		pm.StopSite(id)
	}

	// Start proxies that are desired but not running
	for id, s := range desired {
		if !pm.IsRunning(id) {
			if err := pm.StartSite(s); err != nil {
				log.Printf("[relay] failed to start site %s: %v", s.Name, err)
			}
		}
	}
}

func (pm *ProxyManager) GetRunningCount() int {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	return len(pm.proxies)
}

func (pm *ProxyManager) GetSiteRuntime(id int64) (requests int64, startedAt time.Time, running bool) {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	inst, ok := pm.proxies[id]
	if !ok {
		return 0, time.Time{}, false
	}
	return inst.reqCount.Load(), inst.startedAt, true
}

// GracefulShutdown flushes all in-flight traffic counters and removes all proxy registrations.
func (pm *ProxyManager) GracefulShutdown(ctx context.Context) {
	pm.FlushTraffic()
	pm.mu.Lock()
	defer pm.mu.Unlock()
	for _, inst := range pm.proxies {
		log.Printf("[%s] shutting down...", inst.Site.Name)
	}
	pm.proxies = make(map[int64]*ProxyInstance)
}

// GetTotalRequests returns total request count across all proxies
func (pm *ProxyManager) GetTotalRequests() int64 {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	var total int64
	for _, inst := range pm.proxies {
		total += inst.reqCount.Load()
	}
	return total
}

// LiveSiteStat holds real-time counters for a running proxy instance.
type LiveSiteStat struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	BytesIn  int64  `json:"bytes_in"`
	BytesOut int64  `json:"bytes_out"`
	Requests int64  `json:"requests"`
	Running  bool   `json:"running"`
}

// GetLiveSiteStats returns a snapshot of counters for all running proxy instances.
func (pm *ProxyManager) GetLiveSiteStats() []LiveSiteStat {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	stats := make([]LiveSiteStat, 0, len(pm.proxies))
	for _, inst := range pm.proxies {
		stats = append(stats, LiveSiteStat{
			ID:       inst.Site.ID,
			Name:     inst.Site.Name,
			BytesIn:  inst.bytesIn.Load(),
			BytesOut: inst.bytesOut.Load(),
			Requests: inst.reqCount.Load(),
			Running:  true,
		})
	}
	return stats
}
