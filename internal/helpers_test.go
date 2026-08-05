package internal

import (
	"database/sql"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/gin-gonic/gin"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func newTestApp(t *testing.T) *App {
	t.Helper()

	db, err := OpenDB(filepath.Join(t.TempDir(), "test.DB"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	return &App{
		DB: db,
		PM: NewProxyManager(db),
	}
}

func freePort(t *testing.T) int {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port listen: %v", err)
	}
	defer ln.Close()

	return ln.Addr().(*net.TCPAddr).Port
}

func init() {
	gin.SetMode(gin.TestMode)
}

// setupTestRouter creates a gin.Engine for testing (no static files).
func setupTestRouter(app *App) *gin.Engine {
	return SetupRouter(app, app.PM, nil, nil)
}

// createTestAdmin creates an admin user and returns a valid JWT token.
func createTestAdmin(t *testing.T, app *App) string {
	t.Helper()
	_, err := app.DB.CreateInitialUser("admin", "correct horse battery staple")
	if err != nil {
		t.Fatalf("create test admin: %v", err)
	}
	token, err := GenerateToken(1, "admin")
	if err != nil {
		t.Fatalf("generate test token: %v", err)
	}
	return token
}

// authRequest adds the Authorization header.
func authRequest(req *http.Request, token string) *http.Request {
	req.Header.Set("Authorization", "Bearer "+token)
	return req
}

func decodeBody(t *testing.T, rr *httptest.ResponseRecorder) map[string]interface{} {
	t.Helper()

	var body map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v body=%s", err, rr.Body.String())
	}
	return body
}

func mustUserCount(t *testing.T, db *DB) int {
	t.Helper()
	count, err := db.UserCount()
	if err != nil {
		t.Fatalf("UserCount: %v", err)
	}
	return count
}

func stringPointer(value string) *string {
	return &value
}

func createLegacySiteDatabase(t *testing.T, dbPath string, withHourlyIndex bool) {
	t.Helper()
	legacy, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open legacy database: %v", err)
	}
	t.Cleanup(func() { legacy.Close() })

	if _, err := legacy.Exec("CREATE TABLE sites (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL, listen_port INTEGER NOT NULL UNIQUE, target_url TEXT NOT NULL, ua_mode TEXT DEFAULT 'infuse', enabled INTEGER DEFAULT 1, traffic_quota BIGINT DEFAULT 0, traffic_used BIGINT DEFAULT 0, speed_limit INTEGER DEFAULT 0, created_at DATETIME DEFAULT CURRENT_TIMESTAMP, updated_at DATETIME DEFAULT CURRENT_TIMESTAMP)"); err != nil {
		t.Fatalf("create legacy sites: %v", err)
	}
	if _, err := legacy.Exec("CREATE TABLE traffic_logs (id INTEGER PRIMARY KEY AUTOINCREMENT, site_id INTEGER NOT NULL, bytes_in BIGINT DEFAULT 0, bytes_out BIGINT DEFAULT 0, recorded_at DATETIME NOT NULL)"); err != nil {
		t.Fatalf("create legacy traffic logs: %v", err)
	}
	if _, err := legacy.Exec("INSERT INTO sites (name, listen_port, target_url, ua_mode, enabled, traffic_quota, traffic_used, speed_limit) VALUES ('legacy', 19001, 'http://127.0.0.1:8096', 'infuse', 1, 0, 0, 0)"); err != nil {
		t.Fatalf("insert legacy site: %v", err)
	}
	if withHourlyIndex {
		if _, err := legacy.Exec("CREATE UNIQUE INDEX idx_traffic_site_hour ON traffic_logs(site_id, recorded_at)"); err != nil {
			t.Fatalf("create legacy hourly index: %v", err)
		}
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy database: %v", err)
	}
}

func newTestHTTPServer(t *testing.T, app *App) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if app.PM.TryServe(w, r) {
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(ts.Close)
	return ts
}

func lenMust(sites []Site, err error) int {
	if err != nil {
		panic(err)
	}
	return len(sites)
}

func jsonNumber(v int) string {
	return strconv.Itoa(v)
}

func jsonNumber64(v int64) string {
	return strconv.FormatInt(v, 10)
}

func mustReadBody(t *testing.T, resp *http.Response) string {
	t.Helper()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(body)
}

func mustMapValue(t *testing.T, body map[string]interface{}, key string) map[string]interface{} {
	t.Helper()

	value, ok := body[key]
	if !ok {
		t.Fatalf("missing key %q in %#v", key, body)
	}
	result, ok := value.(map[string]interface{})
	if !ok {
		t.Fatalf("key %q = %#v, want object", key, value)
	}
	return result
}

func mustStringValue(t *testing.T, body map[string]interface{}, key string) string {
	t.Helper()

	value, ok := body[key]
	if !ok {
		t.Fatalf("missing key %q in %#v", key, body)
	}
	result, ok := value.(string)
	if !ok {
		t.Fatalf("key %q = %#v, want string", key, value)
	}
	return result
}

func mustBoolValue(t *testing.T, body map[string]interface{}, key string) bool {
	t.Helper()

	value, ok := body[key]
	if !ok {
		t.Fatalf("missing key %q in %#v", key, body)
	}
	result, ok := value.(bool)
	if !ok {
		t.Fatalf("key %q = %#v, want bool", key, value)
	}
	return result
}

func mustNumberValue(t *testing.T, body map[string]interface{}, key string) int {
	t.Helper()

	value, ok := body[key]
	if !ok {
		t.Fatalf("missing key %q in %#v", key, body)
	}
	result, ok := value.(float64)
	if !ok {
		t.Fatalf("key %q = %#v, want number", key, value)
	}
	return int(result)
}
