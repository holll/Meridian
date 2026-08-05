package internal

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMigrateAddsCustomUAColumnsForLegacyDatabases(t *testing.T) {
	for _, withHourlyIndex := range []bool{false, true} {
		t.Run(fmt.Sprintf("hourly index=%v", withHourlyIndex), func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "legacy.DB")
			createLegacySiteDatabase(t, dbPath, withHourlyIndex)

			db, err := OpenDB(dbPath)
			if err != nil {
				t.Fatalf("migrate legacy database: %v", err)
			}
			defer db.Close()

			for _, column := range []string{"path_prefix", "playback_target_url", "playback_mode", "stream_hosts", "custom_user_agent", "custom_client", "custom_version"} {
				var count int
				if err := db.DB.QueryRow("SELECT COUNT(*) FROM pragma_table_info('sites') WHERE name=?", column).Scan(&count); err != nil {
					t.Fatalf("inspect %s: %v", column, err)
				}
				if count != 1 {
					t.Fatalf("column %s count=%d, want 1", column, count)
				}
			}
			site, err := db.GetSite(1)
			if err != nil {
				t.Fatalf("read migrated site: %v", err)
			}
			if site.PathPrefix != "/19001" {
				t.Fatalf("migrated site path_prefix = %q, want /19001", site.PathPrefix)
			}
			if site.UAMode != "infuse" || site.CustomUserAgent != "" || site.CustomClient != "" || site.CustomVersion != "" {
				t.Fatalf("migrated site UA config = %#v", site)
			}
		})
	}
}

func TestMigrateSerializesConcurrentLegacyDatabaseOpens(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "concurrent-legacy.DB")
	createLegacySiteDatabase(t, dbPath, false)

	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			db, err := OpenDB(dbPath)
			if err == nil {
				db.Close()
			}
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent migration: %v", err)
		}
	}
}

func TestResetAdminPasswordUpdatesOnlyConfiguredAdministrator(t *testing.T) {
	app := newTestApp(t)
	const oldPassword = "correct horse battery staple"
	const newPassword = "new correct horse battery staple"
	if _, err := app.DB.CreateInitialUser("admin", oldPassword); err != nil {
		t.Fatalf("CreateInitialUser: %v", err)
	}
	if err := app.DB.ResetAdminPassword(newPassword); err != nil {
		t.Fatalf("ResetAdminPassword: %v", err)
	}
	if _, err := app.DB.VerifyUser("admin", oldPassword); !errors.Is(err, errInvalidCredentials) {
		t.Fatalf("old password error = %v, want invalid credentials", err)
	}
	if _, err := app.DB.VerifyUser("admin", newPassword); err != nil {
		t.Fatalf("new password rejected: %v", err)
	}
}

func TestResetAdminPasswordRejectsInvalidDatabaseStateAndLength(t *testing.T) {
	app := newTestApp(t)
	if err := app.DB.ResetAdminPassword("long enough password"); !errors.Is(err, errAdminNotConfigured) {
		t.Fatalf("empty database error = %v, want administrator not configured", err)
	}
	if _, err := app.DB.CreateUser("admin-one", "correct horse battery staple"); err != nil {
		t.Fatalf("CreateUser one: %v", err)
	}
	if _, err := app.DB.CreateUser("admin-two", "correct horse battery staple"); err != nil {
		t.Fatalf("CreateUser two: %v", err)
	}
	if err := app.DB.ResetAdminPassword("another valid password"); !errors.Is(err, errMultipleAdmins) {
		t.Fatalf("multiple users error = %v, want multiple administrators", err)
	}
	for _, password := range []string{"7chars!", strings.Repeat("x", 73)} {
		if err := app.DB.ResetAdminPassword(password); !errors.Is(err, errInvalidAdminPassword) {
			t.Fatalf("password length %d error = %v, want invalid password", len(password), err)
		}
	}
}

func TestResetAdminPasswordAcceptsLengthBoundaries(t *testing.T) {
	for _, length := range []int{8, 72} {
		app := newTestApp(t)
		if _, err := app.DB.CreateInitialUser("admin", "correct horse battery staple"); err != nil {
			t.Fatalf("CreateInitialUser: %v", err)
		}
		password := strings.Repeat("x", length)
		if err := app.DB.ResetAdminPassword(password); err != nil {
			t.Fatalf("length %d rejected: %v", length, err)
		}
		if _, err := app.DB.VerifyUser("admin", password); err != nil {
			t.Fatalf("length %d password did not verify: %v", length, err)
		}
	}
}

func TestAdminResetPasswordCommandReadsPasswordOnlyFromStdin(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "command.DB")
	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	if _, err := db.CreateInitialUser("admin", "correct horse battery staple"); err != nil {
		db.Close()
		t.Fatalf("CreateInitialUser: %v", err)
	}
	db.Close()

	const newPassword = "stdin-only replacement password"
	var output bytes.Buffer
	handled, err := RunCommandLine(
		[]string{"admin", "reset-password", "--db", dbPath, "--password-stdin"},
		strings.NewReader(newPassword+"\n"),
		&output, "v1.5.1",
	)
	if err != nil {
		t.Fatalf("runCommandLine: %v", err)
	}
	if !handled {
		t.Fatal("admin command was not handled")
	}
	if strings.Contains(output.String(), newPassword) {
		t.Fatal("command output exposed the password")
	}

	verifyDB, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("reopen database: %v", err)
	}
	defer verifyDB.Close()
	if _, err := verifyDB.VerifyUser("admin", newPassword); err != nil {
		t.Fatalf("new password rejected: %v", err)
	}
}

func TestAdminResetPasswordCommandRejectsUnsafeInputShapes(t *testing.T) {
	const misplacedPassword = "must-not-appear-in-errors"
	for _, tc := range []struct {
		name  string
		args  []string
		input string
	}{
		{name: "missing stdin flag", args: []string{"admin", "reset-password", "--db", "test.DB"}, input: "valid replacement password\n"},
		{name: "password argument", args: []string{"admin", "reset-password", "--db", "test.DB", "--password", misplacedPassword}},
		{name: "multiple lines", args: []string{"admin", "reset-password", "--db", "test.DB", "--password-stdin"}, input: "valid replacement password\nsecond line\n"},
		{name: "too long", args: []string{"admin", "reset-password", "--db", "test.DB", "--password-stdin"}, input: strings.Repeat("x", 73) + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handled, err := RunCommandLine(tc.args, strings.NewReader(tc.input), io.Discard, "v1.5.1")
			if !handled || err == nil {
				t.Fatalf("handled=%v err=%v, want handled error", handled, err)
			}
			if strings.Contains(err.Error(), misplacedPassword) {
				t.Fatal("command error exposed a password-shaped argument")
			}
		})
	}
}

func TestDatabaseReadFailuresAreReported(t *testing.T) {
	app := newTestApp(t)
	app.DB.Close()
	if _, err := app.DB.UserCount(); err == nil {
		t.Fatal("UserCount unexpectedly ignored a closed database")
	}
	if _, err := app.DB.DashboardStats(); err == nil {
		t.Fatal("DashboardStats unexpectedly ignored a closed database")
	}
	if _, err := app.PM.StartAllEnabled(); err == nil {
		t.Fatal("StartAllEnabled unexpectedly ignored a closed database")
	}

	router := setupTestRouter(app)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/api/auth/check", nil))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("auth check status = %d, want 500; body=%s", w.Code, w.Body.String())
	}
}

func TestAddTrafficAggregatesSameHour(t *testing.T) {
	app := newTestApp(t)
	site, err := app.DB.CreateSite("aggregate", "/s/aggregate", "http://127.0.0.1:8096", "", "direct", "[]", "infuse", 0, 0)
	if err != nil {
		t.Fatalf("CreateSite: %v", err)
	}

	app.DB.AddTraffic(site.ID, 10, 20)
	app.DB.AddTraffic(site.ID, 5, 7)

	logs, err := app.DB.GetTrafficLogs(site.ID, 1)
	if err != nil {
		t.Fatalf("GetTrafficLogs: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("len(logs) = %d, want 1", len(logs))
	}
	if logs[0].BytesIn != 15 || logs[0].BytesOut != 27 {
		t.Fatalf("aggregated log = in:%d out:%d", logs[0].BytesIn, logs[0].BytesOut)
	}
}

func TestAddAccessLogsInsertsAndPrunesRetention(t *testing.T) {
	app := newTestApp(t)
	now := time.Now().Unix()
	stale := now - 8*24*3600 // 8 days old, outside the 7-day window

	seed := func(ts int64, path string) {
		_, err := app.DB.DB.Exec(`
			INSERT INTO access_logs (relay_name, site_id, client_ip, method, path, status, latency_ms, bytes_in, bytes_out, ts)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			"node1", 1, "1.2.3.4", "GET", path, 200, 5, 10, 20, ts)
		if err != nil {
			t.Fatalf("seed access log: %v", err)
		}
	}
	seed(stale, "/stale")

	if err := app.DB.AddAccessLogs("node1", []AccessLogEntry{
		{Timestamp: now, SiteID: 1, ClientIP: "1.2.3.4", Method: "GET", Path: "/fresh", Status: 200, LatencyMs: 5, BytesIn: 10, BytesOut: 20},
	}); err != nil {
		t.Fatalf("AddAccessLogs: %v", err)
	}

	var total int
	if err := app.DB.DB.QueryRow("SELECT COUNT(*) FROM access_logs").Scan(&total); err != nil {
		t.Fatalf("count: %v", err)
	}
	if total != 1 {
		t.Fatalf("access_logs rows = %d, want 1 (stale pruned)", total)
	}
	var path string
	if err := app.DB.DB.QueryRow("SELECT path FROM access_logs").Scan(&path); err != nil {
		t.Fatalf("query fresh row: %v", err)
	}
	if path != "/fresh" {
		t.Fatalf("surviving row path = %q, want /fresh", path)
	}

	if err := app.DB.AddAccessLogs("node1", nil); err != nil {
		t.Fatalf("AddAccessLogs(nil) must be a no-op: %v", err)
	}
}

func TestQueryAccessLogsFiltersAndPagination(t *testing.T) {
	app := newTestApp(t)
	site, err := app.DB.CreateSite("emby", "/s/emby", "http://127.0.0.1:8096", "", "direct", "[]", "infuse", 0, 0)
	if err != nil {
		t.Fatalf("CreateSite: %v", err)
	}
	base := time.Now().Unix()
	entries := []AccessLogEntry{
		{Timestamp: base, SiteID: site.ID, ClientIP: "1.1.1.1", Method: "GET", Path: "/a", Status: 200, LatencyMs: 10, BytesOut: 100},
		{Timestamp: base - 100, SiteID: site.ID, ClientIP: "2.2.2.2", Method: "GET", Path: "/b", Status: 404, LatencyMs: 20, BytesOut: 200},
		{Timestamp: base - 200, SiteID: 999, ClientIP: "3.3.3.3", Method: "POST", Path: "/c", Status: 500, LatencyMs: 30, BytesOut: 300},
		{Timestamp: base - 300, SiteID: site.ID, ClientIP: "4.4.4.4", Method: "GET", Path: "/d", Status: 200, LatencyMs: 40, BytesOut: 400},
	}
	if err := app.DB.AddAccessLogs("node1", entries); err != nil {
		t.Fatalf("AddAccessLogs: %v", err)
	}

	// Filter by site: 3 rows for site.ID, all joined with the site name.
	logs, total, err := app.DB.QueryAccessLogs(site.ID, "", 0, 0, 1, 50)
	if err != nil {
		t.Fatalf("QueryAccessLogs: %v", err)
	}
	if total != 3 || len(logs) != 3 {
		t.Fatalf("site filter total=%d len=%d, want 3/3", total, len(logs))
	}
	for _, l := range logs {
		if l.SiteName != "emby" {
			t.Fatalf("site name = %q, want emby", l.SiteName)
		}
	}

	// Filter by relay name.
	_, total, err = app.DB.QueryAccessLogs(0, "node1", 0, 0, 1, 50)
	if err != nil {
		t.Fatalf("relay filter: %v", err)
	}
	if total != 4 {
		t.Fatalf("relay filter total = %d, want 4", total)
	}
	_, total, err = app.DB.QueryAccessLogs(0, "missing", 0, 0, 1, 50)
	if err != nil {
		t.Fatalf("relay miss: %v", err)
	}
	if total != 0 {
		t.Fatalf("relay miss total = %d, want 0", total)
	}

	// Time window: [base-150, base-50] covers the two newest rows.
	_, total, err = app.DB.QueryAccessLogs(0, "", base-150, base+50, 1, 50)
	if err != nil {
		t.Fatalf("time filter: %v", err)
	}
	if total != 2 {
		t.Fatalf("time window total = %d, want 2", total)
	}

	// Pagination: newest first, page 2 of page size 2.
	pageLogs, total, err := app.DB.QueryAccessLogs(0, "", 0, 0, 2, 2)
	if err != nil {
		t.Fatalf("paged query: %v", err)
	}
	if total != 4 || len(pageLogs) != 2 {
		t.Fatalf("page 2 total=%d len=%d, want 4/2", total, len(pageLogs))
	}
	if pageLogs[0].Path != "/b" {
		t.Fatalf("page 2 first path = %q, want /b (id desc)", pageLogs[0].Path)
	}
}

func TestQueryAccessLogStatsAggregations(t *testing.T) {
	app := newTestApp(t)
	site, err := app.DB.CreateSite("emby", "/s/emby", "http://127.0.0.1:8096", "", "direct", "[]", "infuse", 0, 0)
	if err != nil {
		t.Fatalf("CreateSite: %v", err)
	}
	hour := time.Now().Unix() / 3600 * 3600
	entries := []AccessLogEntry{
		{Timestamp: hour, SiteID: site.ID, ClientIP: "1.1.1.1", Method: "GET", Path: "/hot", Status: 200, LatencyMs: 10, BytesOut: 100},
		{Timestamp: hour + 60, SiteID: site.ID, ClientIP: "1.1.1.1", Method: "GET", Path: "/hot", Status: 200, LatencyMs: 30, BytesOut: 200},
		{Timestamp: hour - 3600, SiteID: site.ID, ClientIP: "2.2.2.2", Method: "GET", Path: "/cold", Status: 404, LatencyMs: 50, BytesOut: 400},
	}
	if err := app.DB.AddAccessLogs("node1", entries); err != nil {
		t.Fatalf("AddAccessLogs: %v", err)
	}

	stats, err := app.DB.QueryAccessLogStats(site.ID, "", hour-7200, hour+7200)
	if err != nil {
		t.Fatalf("QueryAccessLogStats: %v", err)
	}

	// Trend: two hourly buckets.
	if len(stats.Trend) != 2 {
		t.Fatalf("trend buckets = %d, want 2", len(stats.Trend))
	}
	var hotBucket *TrendPoint
	for i := range stats.Trend {
		if stats.Trend[i].Bucket == hour {
			hotBucket = &stats.Trend[i]
		}
	}
	if hotBucket == nil {
		t.Fatal("missing current-hour trend bucket")
	}
	if hotBucket.Requests != 2 || hotBucket.BytesOut != 300 {
		t.Fatalf("hot bucket requests=%d bytes_out=%d, want 2/300", hotBucket.Requests, hotBucket.BytesOut)
	}

	// Weighted average latency: (10*1 + 30*1 + 50*1)/3 = 30.
	if stats.AvgLatencyMs != 30 {
		t.Fatalf("avg latency = %d, want 30", stats.AvgLatencyMs)
	}
	if stats.MaxLatencyMs != 50 {
		t.Fatalf("max latency = %d, want 50", stats.MaxLatencyMs)
	}

	// Status distribution: 200 twice, 404 once.
	if len(stats.Status) != 2 {
		t.Fatalf("status buckets = %d, want 2", len(stats.Status))
	}
	byStatus := map[int]int64{}
	for _, s := range stats.Status {
		byStatus[s.Status] = s.Count
	}
	if byStatus[200] != 2 || byStatus[404] != 1 {
		t.Fatalf("status counts = %v, want 200:2 404:1", byStatus)
	}

	// Top paths: /hot first with 300 bytes out.
	if len(stats.TopPaths) != 2 || stats.TopPaths[0].Path != "/hot" || stats.TopPaths[0].Count != 2 {
		t.Fatalf("top paths = %+v, want /hot first with count 2", stats.TopPaths)
	}

	// Top IPs: 1.1.1.1 twice with average latency 20.
	if len(stats.TopIPs) != 2 || stats.TopIPs[0].IP != "1.1.1.1" || stats.TopIPs[0].Count != 2 {
		t.Fatalf("top IPs = %+v, want 1.1.1.1 first with count 2", stats.TopIPs)
	}
	if stats.TopIPs[0].AvgLatencyMs != 20 {
		t.Fatalf("top IP avg latency = %d, want 20", stats.TopIPs[0].AvgLatencyMs)
	}
}

func TestQueryAccessLogStatsRollsUpTopPathsBeyondLimit(t *testing.T) {
	app := newTestApp(t)
	base := time.Now().Unix()
	entries := make([]AccessLogEntry, 0, 12)
	for i := 0; i < 12; i++ {
		entries = append(entries, AccessLogEntry{
			Timestamp: base, SiteID: 1, ClientIP: "1.1.1.1", Method: "GET",
			Path: fmt.Sprintf("/p%d", i), Status: 200, LatencyMs: 1, BytesOut: 10,
		})
	}
	if err := app.DB.AddAccessLogs("node1", entries); err != nil {
		t.Fatalf("AddAccessLogs: %v", err)
	}
	stats, err := app.DB.QueryAccessLogStats(0, "", 0, 0)
	if err != nil {
		t.Fatalf("QueryAccessLogStats: %v", err)
	}
	if len(stats.TopPaths) != topPathCount+1 {
		t.Fatalf("top paths = %d, want %d+1 with rollup", len(stats.TopPaths), topPathCount)
	}
	last := stats.TopPaths[len(stats.TopPaths)-1]
	if !last.IsOther || last.Count != 2 {
		t.Fatalf("rollup bucket = %+v, want other with count 2", last)
	}
}
