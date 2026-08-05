package internal

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/go-crypt/x/bcrypt"
	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

type DB struct {
	DB *sql.DB
}

func OpenDB(path string) (*DB, error) {
	SetSecureFileCreationMask()
	sqlDB, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxOpenConns(1)
	d := &DB{DB: sqlDB}
	if err := d.migrate(); err != nil {
		sqlDB.Close()
		return nil, err
	}
	if err := hardenDatabaseFilePermissions(path); err != nil {
		sqlDB.Close()
		return nil, err
	}
	return d, nil
}

func hardenDatabaseFilePermissions(path string) error {
	if path == ":memory:" || strings.HasPrefix(path, "file:") {
		return nil
	}
	for _, candidate := range []string{path, path + "-wal", path + "-shm"} {
		// #nosec G703 -- the database path is operator-controlled and never derived from a request.
		if err := os.Chmod(candidate, 0600); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("secure database file %s: %w", candidate, err)
		}
	}
	return nil
}

func (d *DB) Close() { d.DB.Close() }

const (
	migrationRetryDelay    = 25 * time.Millisecond
	migrationRetryDeadline = 5 * time.Second
)

func (d *DB) migrate() error {
	deadline := time.Now().Add(migrationRetryDeadline)
	for {
		err := d.migrateOnce()
		if err == nil || !isSQLiteBusyError(err) || !time.Now().Before(deadline) {
			return err
		}
		time.Sleep(migrationRetryDelay)
	}
}

func isSQLiteBusyError(err error) bool {
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}
	// SQLite encodes the primary result code in the low byte of extended errors.
	switch sqliteErr.Code() & 0xff {
	case sqlite3.SQLITE_BUSY, sqlite3.SQLITE_LOCKED:
		return true
	default:
		return false
	}
}

func (d *DB) migrateOnce() error {
	ctx := context.Background()
	conn, err := d.DB.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(ctx, "ROLLBACK")
		}
	}()

	if _, err := conn.ExecContext(ctx, `
	CREATE TABLE IF NOT EXISTS users (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		username TEXT UNIQUE NOT NULL,
		password_hash TEXT NOT NULL,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	CREATE TABLE IF NOT EXISTS sites (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL,
		path_prefix TEXT NOT NULL DEFAULT '' UNIQUE,
		target_url TEXT NOT NULL,
		playback_target_url TEXT NOT NULL DEFAULT '',
		playback_mode TEXT NOT NULL DEFAULT 'direct',
		stream_hosts TEXT NOT NULL DEFAULT '[]',
		ua_mode TEXT DEFAULT 'infuse',
		custom_user_agent TEXT NOT NULL DEFAULT '',
		custom_client TEXT NOT NULL DEFAULT '',
		custom_version TEXT NOT NULL DEFAULT '',
		enabled INTEGER DEFAULT 1,
		traffic_quota BIGINT DEFAULT 0,
		traffic_used BIGINT DEFAULT 0,
		speed_limit INTEGER DEFAULT 0,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	CREATE TABLE IF NOT EXISTS traffic_logs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		site_id INTEGER NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
		bytes_in BIGINT DEFAULT 0,
		bytes_out BIGINT DEFAULT 0,
		recorded_at DATETIME NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_traffic_site_time ON traffic_logs(site_id, recorded_at);
	`); err != nil {
		return err
	}

	for _, migration := range []struct {
		column string
		sql    string
	}{
		{"playback_target_url", "ALTER TABLE sites ADD COLUMN playback_target_url TEXT NOT NULL DEFAULT ''"},
		{"playback_mode", "ALTER TABLE sites ADD COLUMN playback_mode TEXT NOT NULL DEFAULT 'direct'"},
		{"stream_hosts", "ALTER TABLE sites ADD COLUMN stream_hosts TEXT NOT NULL DEFAULT '[]'"},
		{"custom_user_agent", "ALTER TABLE sites ADD COLUMN custom_user_agent TEXT NOT NULL DEFAULT ''"},
		{"custom_client", "ALTER TABLE sites ADD COLUMN custom_client TEXT NOT NULL DEFAULT ''"},
		{"custom_version", "ALTER TABLE sites ADD COLUMN custom_version TEXT NOT NULL DEFAULT ''"},
	} {
		exists, err := sqliteColumnExists(ctx, conn, migration.column)
		if err != nil {
			return err
		}
		if !exists {
			if _, err := conn.ExecContext(ctx, migration.sql); err != nil {
				return err
			}
		}
	}

	// Migration: listen_port → path_prefix.
	// Old databases used a per-site TCP port; new databases use a URL path prefix
	// served by the panel server. Derive the path_prefix from the listen_port so
	// existing site configurations remain usable after the upgrade.
	hasPathPrefix, err := sqliteColumnExists(ctx, conn, "path_prefix")
	if err != nil {
		return err
	}
	if !hasPathPrefix {
		hasListenPort, err := sqliteColumnExists(ctx, conn, "listen_port")
		if err != nil {
			return err
		}
		if hasListenPort {
			// Recreate sites table with path_prefix, converting listen_port values.
			for _, stmt := range []string{
				`ALTER TABLE sites RENAME TO _sites_migrate_v2`,
				`CREATE TABLE sites (
					id INTEGER PRIMARY KEY AUTOINCREMENT,
					name TEXT NOT NULL,
					path_prefix TEXT NOT NULL DEFAULT '' UNIQUE,
					target_url TEXT NOT NULL,
					playback_target_url TEXT NOT NULL DEFAULT '',
					playback_mode TEXT NOT NULL DEFAULT 'direct',
					stream_hosts TEXT NOT NULL DEFAULT '[]',
					ua_mode TEXT DEFAULT 'infuse',
					custom_user_agent TEXT NOT NULL DEFAULT '',
					custom_client TEXT NOT NULL DEFAULT '',
					custom_version TEXT NOT NULL DEFAULT '',
					enabled INTEGER DEFAULT 1,
					traffic_quota BIGINT DEFAULT 0,
					traffic_used BIGINT DEFAULT 0,
					speed_limit INTEGER DEFAULT 0,
					created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
					updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
				)`,
				`INSERT INTO sites (id, name, path_prefix, target_url, playback_target_url, playback_mode, stream_hosts, ua_mode, custom_user_agent, custom_client, custom_version, enabled, traffic_quota, traffic_used, speed_limit, created_at, updated_at)
				 SELECT id, name, '/s/' || CAST(listen_port AS TEXT), target_url,
				        COALESCE(playback_target_url, ''), COALESCE(playback_mode, 'direct'), COALESCE(stream_hosts, '[]'),
				        COALESCE(ua_mode, 'infuse'), COALESCE(custom_user_agent, ''), COALESCE(custom_client, ''), COALESCE(custom_version, ''),
				        enabled, traffic_quota, traffic_used, speed_limit, created_at, updated_at
				 FROM _sites_migrate_v2`,
				`DROP TABLE _sites_migrate_v2`,
			} {
				if _, err := conn.ExecContext(ctx, stmt); err != nil {
					return fmt.Errorf("migrate listen_port to path_prefix: %w", err)
				}
			}
		}
	}

	var hasHourlyIndex int
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_traffic_site_hour'").Scan(&hasHourlyIndex); err != nil {
		return err
	}
	if hasHourlyIndex == 0 {
		if _, err := conn.ExecContext(ctx, `
			CREATE TABLE traffic_logs_dedup (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				site_id INTEGER NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
				bytes_in BIGINT DEFAULT 0,
				bytes_out BIGINT DEFAULT 0,
				recorded_at DATETIME NOT NULL
			);
			INSERT INTO traffic_logs_dedup (site_id, bytes_in, bytes_out, recorded_at)
			SELECT site_id, SUM(bytes_in), SUM(bytes_out), recorded_at
			FROM traffic_logs
			GROUP BY site_id, recorded_at;
			DELETE FROM traffic_logs;
			INSERT INTO traffic_logs (site_id, bytes_in, bytes_out, recorded_at)
			SELECT site_id, bytes_in, bytes_out, recorded_at
			FROM traffic_logs_dedup;
			DROP TABLE traffic_logs_dedup;
			CREATE UNIQUE INDEX idx_traffic_site_hour ON traffic_logs(site_id, recorded_at);
		`); err != nil {
			return err
		}
	}

	// Migration: strip /s prefix from path_prefix values (route prefix is now global).
	if _, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT)`); err != nil {
		return err
	}
	var migrated int
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM settings WHERE key='route_prefix_migrated'").Scan(&migrated); err != nil {
		return err
	}
	if migrated == 0 {
		if _, err := conn.ExecContext(ctx, `UPDATE sites SET path_prefix = SUBSTR(path_prefix, 3) WHERE path_prefix LIKE '/s/%' AND LENGTH(path_prefix) > 3`); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES ('route_prefix_migrated', '1')`); err != nil {
			return err
		}
	}

	// Migration: add relay_nodes table
	if _, err := conn.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS relay_nodes (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			name        TEXT NOT NULL UNIQUE,
			isp         TEXT NOT NULL DEFAULT '',
			public_ip   TEXT NOT NULL DEFAULT '',
			version     TEXT NOT NULL DEFAULT '',
			last_seen   INTEGER NOT NULL DEFAULT 0,
			traffic_in  INTEGER NOT NULL DEFAULT 0,
			traffic_out INTEGER NOT NULL DEFAULT 0
		)
	`); err != nil {
		return err
	}

	// Migration: add access_logs table (per-request logs reported by relay nodes).
	// site_id intentionally has no foreign key so audit logs survive site deletion.
	if _, err := conn.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS access_logs (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			relay_name TEXT NOT NULL DEFAULT '',
			site_id    INTEGER NOT NULL DEFAULT 0,
			client_ip  TEXT NOT NULL DEFAULT '',
			method     TEXT NOT NULL DEFAULT '',
			path       TEXT NOT NULL DEFAULT '',
			status     INTEGER NOT NULL DEFAULT 0,
			latency_ms INTEGER NOT NULL DEFAULT 0,
			bytes_in   INTEGER NOT NULL DEFAULT 0,
			bytes_out  INTEGER NOT NULL DEFAULT 0,
			ts         INTEGER NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_access_logs_ts      ON access_logs(ts);
		CREATE INDEX IF NOT EXISTS idx_access_logs_site_ts ON access_logs(site_id, ts);
		CREATE INDEX IF NOT EXISTS idx_access_logs_relay    ON access_logs(relay_name, ts);
	`); err != nil {
		return err
	}

	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	committed = true
	return nil
}

func sqliteColumnExists(ctx context.Context, conn *sql.Conn, column string) (bool, error) {
	var count int
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM pragma_table_info('sites') WHERE name=?", column).Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}

type Site struct {
	ID                int64  `json:"id"`
	Name              string `json:"name"`
	PathPrefix        string `json:"path_prefix"`
	TargetURL         string `json:"target_url"`
	PlaybackTargetURL string `json:"playback_target_url"`
	PlaybackMode      string `json:"playback_mode"`
	StreamHosts       string `json:"stream_hosts"`
	UAMode            string `json:"ua_mode"`
	CustomUserAgent   string `json:"custom_user_agent"`
	CustomClient      string `json:"custom_client"`
	CustomVersion     string `json:"custom_version"`
	Enabled           bool   `json:"enabled"`
	TrafficQuota      int64  `json:"traffic_quota"`
	TrafficUsed       int64  `json:"traffic_used"`
	SpeedLimit        int    `json:"speed_limit"`
	CreatedAt         string `json:"created_at"`
	UpdatedAt         string `json:"updated_at"`
}

type TrafficLog struct {
	ID         int64  `json:"id"`
	SiteID     int64  `json:"site_id"`
	BytesIn    int64  `json:"bytes_in"`
	BytesOut   int64  `json:"bytes_out"`
	RecordedAt string `json:"recorded_at"`
}

func (d *DB) UserCount() (int, error) {
	var n int
	if err := d.DB.QueryRow("SELECT COUNT(*) FROM users").Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

func (d *DB) CreateUser(username, password string) (int64, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return 0, err
	}
	res, err := d.DB.Exec("INSERT INTO users (username, password_hash) VALUES (?, ?)", username, string(hash))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

var errAdminAlreadyExists = errors.New("admin user already exists")
var errInvalidCredentials = errors.New("invalid username or password")
var errAdminNotConfigured = errors.New("administrator is not configured")
var errMultipleAdmins = errors.New("multiple administrator accounts found")
var errInvalidAdminPassword = errors.New("password must be 8-72 bytes")

func ValidateAdminPassword(password string) error {
	if len(password) < 8 || len(password) > 72 {
		return errInvalidAdminPassword
	}
	return nil
}

func (d *DB) CreateInitialUser(username, password string) (int64, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return 0, err
	}
	res, err := d.DB.Exec(`
		INSERT INTO users (username, password_hash)
		SELECT ?, ?
		WHERE NOT EXISTS (SELECT 1 FROM users)
	`, username, string(hash))
	if err != nil {
		return 0, err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if rows != 1 {
		return 0, errAdminAlreadyExists
	}
	return res.LastInsertId()
}

var invalidUserPasswordHash = func() []byte {
	hash, err := bcrypt.GenerateFromPassword([]byte("meridian-invalid-user"), bcrypt.DefaultCost)
	if err != nil {
		panic(err)
	}
	return hash
}()

func (d *DB) VerifyUser(username, password string) (int64, error) {
	var id int64
	var hash string
	err := d.DB.QueryRow("SELECT id, password_hash FROM users WHERE username=?", username).Scan(&id, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		_ = bcrypt.CompareHashAndPassword(invalidUserPasswordHash, []byte(password))
		return 0, errInvalidCredentials
	}
	if err != nil {
		return 0, err
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)); err != nil {
		return 0, errInvalidCredentials
	}
	return id, nil
}

func (d *DB) ResetAdminPassword(password string) error {
	if err := ValidateAdminPassword(password); err != nil {
		return err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}

	tx, err := d.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var count int
	if err := tx.QueryRow("SELECT COUNT(*) FROM users").Scan(&count); err != nil {
		return err
	}
	switch {
	case count == 0:
		return errAdminNotConfigured
	case count != 1:
		return errMultipleAdmins
	}

	result, err := tx.Exec("UPDATE users SET password_hash=?", string(hash))
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return fmt.Errorf("updated %d administrator rows, want 1", rows)
	}
	return tx.Commit()
}

func (d *DB) ListSites() ([]Site, error) {
	rows, err := d.DB.Query("SELECT id, name, path_prefix, target_url, playback_target_url, playback_mode, stream_hosts, ua_mode, custom_user_agent, custom_client, custom_version, enabled, traffic_quota, traffic_used, speed_limit, created_at, updated_at FROM sites ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sites []Site
	for rows.Next() {
		var s Site
		var enabled int
		if err := rows.Scan(&s.ID, &s.Name, &s.PathPrefix, &s.TargetURL, &s.PlaybackTargetURL, &s.PlaybackMode, &s.StreamHosts, &s.UAMode, &s.CustomUserAgent, &s.CustomClient, &s.CustomVersion, &enabled, &s.TrafficQuota, &s.TrafficUsed, &s.SpeedLimit, &s.CreatedAt, &s.UpdatedAt); err != nil {
			return nil, err
		}
		s.Enabled = enabled == 1
		sites = append(sites, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if sites == nil {
		sites = []Site{}
	}
	return sites, nil
}

func (d *DB) GetSite(id int64) (*Site, error) {
	var s Site
	var enabled int
	err := d.DB.QueryRow("SELECT id, name, path_prefix, target_url, playback_target_url, playback_mode, stream_hosts, ua_mode, custom_user_agent, custom_client, custom_version, enabled, traffic_quota, traffic_used, speed_limit, created_at, updated_at FROM sites WHERE id=?", id).
		Scan(&s.ID, &s.Name, &s.PathPrefix, &s.TargetURL, &s.PlaybackTargetURL, &s.PlaybackMode, &s.StreamHosts, &s.UAMode, &s.CustomUserAgent, &s.CustomClient, &s.CustomVersion, &enabled, &s.TrafficQuota, &s.TrafficUsed, &s.SpeedLimit, &s.CreatedAt, &s.UpdatedAt)
	if err != nil {
		return nil, err
	}
	s.Enabled = enabled == 1
	return &s, nil
}

func (d *DB) CreateSite(name string, pathPrefix string, targetURL, playbackTargetURL, playbackMode, streamHosts, uaMode string, quota int64, speedLimit int) (*Site, error) {
	return d.CreateSiteWithCustomUA(name, pathPrefix, targetURL, playbackTargetURL, playbackMode, streamHosts, uaMode, "", "", "", quota, speedLimit)
}

func (d *DB) CreateSiteWithCustomUA(name string, pathPrefix string, targetURL, playbackTargetURL, playbackMode, streamHosts, uaMode, customUserAgent, customClient, customVersion string, quota int64, speedLimit int) (*Site, error) {
	return d.CreateSiteRecord(Site{
		Name:              name,
		PathPrefix:        pathPrefix,
		TargetURL:         targetURL,
		PlaybackTargetURL: playbackTargetURL,
		PlaybackMode:      playbackMode,
		StreamHosts:       streamHosts,
		UAMode:            uaMode,
		CustomUserAgent:   customUserAgent,
		CustomClient:      customClient,
		CustomVersion:     customVersion,
		TrafficQuota:      quota,
		SpeedLimit:        speedLimit,
	})
}

func (d *DB) CreateSiteRecord(site Site) (*Site, error) {
	if site.StreamHosts == "" {
		site.StreamHosts = "[]"
	}
	res, err := d.DB.Exec(
		"INSERT INTO sites (name, path_prefix, target_url, playback_target_url, playback_mode, stream_hosts, ua_mode, custom_user_agent, custom_client, custom_version, traffic_quota, speed_limit) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)",
		site.Name, site.PathPrefix, site.TargetURL, site.PlaybackTargetURL, site.PlaybackMode, site.StreamHosts, site.UAMode, site.CustomUserAgent, site.CustomClient, site.CustomVersion, site.TrafficQuota, site.SpeedLimit,
	)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return d.GetSite(id)
}

func (d *DB) UpdateSite(id int64, name string, pathPrefix string, targetURL, playbackTargetURL, playbackMode, streamHosts, uaMode string, quota int64, speedLimit int) error {
	return d.UpdateSiteWithCustomUA(id, name, pathPrefix, targetURL, playbackTargetURL, playbackMode, streamHosts, uaMode, "", "", "", quota, speedLimit)
}

func (d *DB) UpdateSiteWithCustomUA(id int64, name string, pathPrefix string, targetURL, playbackTargetURL, playbackMode, streamHosts, uaMode, customUserAgent, customClient, customVersion string, quota int64, speedLimit int) error {
	return d.UpdateSiteRecord(Site{
		ID:                id,
		Name:              name,
		PathPrefix:        pathPrefix,
		TargetURL:         targetURL,
		PlaybackTargetURL: playbackTargetURL,
		PlaybackMode:      playbackMode,
		StreamHosts:       streamHosts,
		UAMode:            uaMode,
		CustomUserAgent:   customUserAgent,
		CustomClient:      customClient,
		CustomVersion:     customVersion,
		TrafficQuota:      quota,
		SpeedLimit:        speedLimit,
	})
}

func (d *DB) UpdateSiteRecord(site Site) error {
	if site.StreamHosts == "" {
		site.StreamHosts = "[]"
	}
	_, err := d.DB.Exec(
		"UPDATE sites SET name=?, path_prefix=?, target_url=?, playback_target_url=?, playback_mode=?, stream_hosts=?, ua_mode=?, custom_user_agent=?, custom_client=?, custom_version=?, traffic_quota=?, speed_limit=?, updated_at=CURRENT_TIMESTAMP WHERE id=?",
		site.Name, site.PathPrefix, site.TargetURL, site.PlaybackTargetURL, site.PlaybackMode, site.StreamHosts, site.UAMode, site.CustomUserAgent, site.CustomClient, site.CustomVersion, site.TrafficQuota, site.SpeedLimit, site.ID,
	)
	return err
}

func (d *DB) DeleteSite(id int64) error {
	tx, err := d.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("DELETE FROM traffic_logs WHERE site_id=?", id); err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM sites WHERE id=?", id); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DB) ToggleSite(id int64) (bool, error) {
	var enabled int
	if err := d.DB.QueryRow("SELECT enabled FROM sites WHERE id=?", id).Scan(&enabled); err != nil {
		return false, err
	}
	newVal := 1 - enabled
	_, err := d.DB.Exec("UPDATE sites SET enabled=?, updated_at=CURRENT_TIMESTAMP WHERE id=?", newVal, id)
	return newVal == 1, err
}

func (d *DB) AddTraffic(siteID, bytesIn, bytesOut int64) {
	if err := d.addTraffic(siteID, bytesIn, bytesOut); err != nil {
		log.Printf("[traffic] failed to persist usage for site %d: %v", siteID, err)
	}
}

func (d *DB) addTraffic(siteID, bytesIn, bytesOut int64) error {
	hour := time.Now().Truncate(time.Hour).Format("2006-01-02 15:04:05")
	tx, err := d.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(
		`INSERT INTO traffic_logs (site_id, bytes_in, bytes_out, recorded_at)
		 VALUES (?,?,?,?)
		 ON CONFLICT(site_id, recorded_at) DO UPDATE SET
		 	bytes_in = traffic_logs.bytes_in + excluded.bytes_in,
		 	bytes_out = traffic_logs.bytes_out + excluded.bytes_out`,
		siteID, bytesIn, bytesOut, hour,
	); err != nil {
		return err
	}

	if _, err := tx.Exec(
		"UPDATE sites SET traffic_used=traffic_used+?+?, updated_at=CURRENT_TIMESTAMP WHERE id=?",
		bytesIn, bytesOut, siteID,
	); err != nil {
		return err
	}

	return tx.Commit()
}

func (d *DB) GetTrafficLogs(siteID int64, hours int) ([]TrafficLog, error) {
	since := time.Now().Add(-time.Duration(hours) * time.Hour).Format("2006-01-02 15:04:05")
	rows, err := d.DB.Query(
		"SELECT id, site_id, bytes_in, bytes_out, recorded_at FROM traffic_logs WHERE site_id=? AND recorded_at>=? ORDER BY recorded_at",
		siteID, since,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var logs []TrafficLog
	for rows.Next() {
		var l TrafficLog
		if err := rows.Scan(&l.ID, &l.SiteID, &l.BytesIn, &l.BytesOut, &l.RecordedAt); err != nil {
			return nil, err
		}
		logs = append(logs, l)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if logs == nil {
		logs = []TrafficLog{}
	}
	return logs, nil
}

// DailyTrafficLog holds aggregated traffic for a single calendar day.
type DailyTrafficLog struct {
	Date     string `json:"date"`
	BytesIn  int64  `json:"bytes_in"`
	BytesOut int64  `json:"bytes_out"`
}

func (d *DB) GetDailyTrafficLogs(siteID int64, days int) ([]DailyTrafficLog, error) {
	since := time.Now().AddDate(0, 0, -days).Format("2006-01-02")
	rows, err := d.DB.Query(
		`SELECT DATE(recorded_at) AS day, SUM(bytes_in), SUM(bytes_out)
		 FROM traffic_logs WHERE site_id=? AND DATE(recorded_at)>=?
		 GROUP BY day ORDER BY day`,
		siteID, since,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var logs []DailyTrafficLog
	for rows.Next() {
		var l DailyTrafficLog
		if err := rows.Scan(&l.Date, &l.BytesIn, &l.BytesOut); err != nil {
			return nil, err
		}
		logs = append(logs, l)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if logs == nil {
		logs = []DailyTrafficLog{}
	}
	return logs, nil
}

// RelayNode represents a registered relay node.
type RelayNode struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	ISP        string `json:"isp"`
	PublicIP   string `json:"public_ip"`
	Version    string `json:"version"`
	LastSeen   int64  `json:"last_seen"`
	TrafficIn  int64  `json:"traffic_in"`
	TrafficOut int64  `json:"traffic_out"`
}

// RegisterRelayNode upserts a relay node record (insert or update ip/version/last_seen).
func (d *DB) RegisterRelayNode(name, isp, publicIP, version string, now int64) error {
	_, err := d.DB.Exec(`
		INSERT INTO relay_nodes (name, isp, public_ip, version, last_seen)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			isp       = excluded.isp,
			public_ip = excluded.public_ip,
			version   = excluded.version,
			last_seen = excluded.last_seen
	`, name, isp, publicIP, version, now)
	return err
}

// TouchRelayNode updates last_seen for an existing relay node.
func (d *DB) TouchRelayNode(name string, now int64) {
	_, _ = d.DB.Exec("UPDATE relay_nodes SET last_seen=? WHERE name=?", now, name)
}

// UpdateRelayNodeIP refreshes the public IP the Master observes for a relay node.
func (d *DB) UpdateRelayNodeIP(name, ip string) error {
	_, err := d.DB.Exec("UPDATE relay_nodes SET public_ip=? WHERE name=?", ip, name)
	return err
}

// UpdateRelayNodeISP refreshes the auto-detected operator label for a relay node.
func (d *DB) UpdateRelayNodeISP(name, isp string) error {
	_, err := d.DB.Exec("UPDATE relay_nodes SET isp=? WHERE name=?", isp, name)
	return err
}

// GetRelayNodes returns all registered relay nodes ordered by name.
func (d *DB) GetRelayNodes() ([]RelayNode, error) {
	rows, err := d.DB.Query("SELECT id, name, isp, public_ip, version, last_seen, traffic_in, traffic_out FROM relay_nodes ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var nodes []RelayNode
	for rows.Next() {
		var n RelayNode
		if err := rows.Scan(&n.ID, &n.Name, &n.ISP, &n.PublicIP, &n.Version, &n.LastSeen, &n.TrafficIn, &n.TrafficOut); err != nil {
			return nil, err
		}
		nodes = append(nodes, n)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if nodes == nil {
		nodes = []RelayNode{}
	}
	return nodes, nil
}

// AddRelayTraffic accumulates traffic from a relay node into the relay_nodes table
// and records per-site increments in traffic_logs.
func (d *DB) AddRelayTraffic(relayName string, now int64, sites []RelayTrafficSite) error {
	tx, err := d.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var totalIn, totalOut int64
	hour := time.Unix(now, 0).UTC().Truncate(time.Hour).Format("2006-01-02 15:04:05")
	for _, s := range sites {
		totalIn += s.BytesIn
		totalOut += s.BytesOut
		if _, err := tx.Exec(`
			INSERT INTO traffic_logs (site_id, bytes_in, bytes_out, recorded_at)
			VALUES (?, ?, ?, ?)
			ON CONFLICT(site_id, recorded_at) DO UPDATE SET
				bytes_in  = traffic_logs.bytes_in  + excluded.bytes_in,
				bytes_out = traffic_logs.bytes_out + excluded.bytes_out`,
			s.SiteID, s.BytesIn, s.BytesOut, hour,
		); err != nil {
			return err
		}
		if _, err := tx.Exec(
			"UPDATE sites SET traffic_used=traffic_used+?+?, updated_at=CURRENT_TIMESTAMP WHERE id=?",
			s.BytesIn, s.BytesOut, s.SiteID,
		); err != nil {
			return err
		}
	}

	if totalIn > 0 || totalOut > 0 {
		if _, err := tx.Exec(`
			UPDATE relay_nodes SET
				traffic_in  = traffic_in  + ?,
				traffic_out = traffic_out + ?,
				last_seen   = ?
			WHERE name = ?`, totalIn, totalOut, now, relayName,
		); err != nil {
			return err
		}
	} else {
		if _, err := tx.Exec("UPDATE relay_nodes SET last_seen=? WHERE name=?", now, relayName); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// RelayTrafficSite carries per-site traffic increment from a relay report.
type RelayTrafficSite struct {
	SiteID   int64 `json:"id"`
	BytesIn  int64 `json:"bytes_in"`
	BytesOut int64 `json:"bytes_out"`
}

// accessLogRetention is how long per-request access logs are kept on the Master.
const accessLogRetention = 7 * 24 * time.Hour

// AddAccessLogs bulk-inserts access log entries reported by a relay node and
// prunes entries older than the retention window (same transaction).
func (d *DB) AddAccessLogs(relayName string, logs []AccessLogEntry) error {
	if len(logs) == 0 {
		return nil
	}
	tx, err := d.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`
		INSERT INTO access_logs (relay_name, site_id, client_ip, method, path, status, latency_ms, bytes_in, bytes_out, ts)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, l := range logs {
		if _, err := stmt.Exec(relayName, l.SiteID, l.ClientIP, l.Method, l.Path, l.Status, l.LatencyMs, l.BytesIn, l.BytesOut, l.Timestamp); err != nil {
			return err
		}
	}

	cutoff := time.Now().Add(-accessLogRetention).Unix()
	if _, err := tx.Exec("DELETE FROM access_logs WHERE ts < ?", cutoff); err != nil {
		return err
	}

	return tx.Commit()
}

// AccessLogRow is one access log record joined with its site name.
type AccessLogRow struct {
	ID        int64    `json:"id"`
	Timestamp int64    `json:"timestamp"`
	RelayName string   `json:"relay_name"`
	SiteID    int64    `json:"site_id"`
	SiteName  string   `json:"site_name"`
	ClientIP  string   `json:"client_ip"`
	Method    string   `json:"method"`
	Path      string   `json:"path"`
	Status    int      `json:"status"`
	LatencyMs int64    `json:"latency_ms"`
	BytesIn   int64    `json:"bytes_in"`
	BytesOut  int64    `json:"bytes_out"`
	Geo       *GeoInfo `json:"geo,omitempty"` // filled by handler, not from SQL
}

// accessLogWhere builds the shared WHERE clause for access_logs queries.
// Columns are qualified with the table alias "l".
func accessLogWhere(siteID int64, relayName string, from, to int64) (string, []interface{}) {
	where := "1=1"
	var args []interface{}
	if siteID > 0 {
		where += " AND l.site_id = ?"
		args = append(args, siteID)
	}
	if relayName != "" {
		where += " AND l.relay_name = ?"
		args = append(args, relayName)
	}
	if from > 0 {
		where += " AND l.ts >= ?"
		args = append(args, from)
	}
	if to > 0 {
		where += " AND l.ts <= ?"
		args = append(args, to)
	}
	return where, args
}

// QueryAccessLogs returns a page of access logs matching the given filters
// (siteID/relayName may be 0/"" for "any"), ordered newest first, plus the
// total count for pagination.
func (d *DB) QueryAccessLogs(siteID int64, relayName string, from, to int64, page, pageSize int) ([]AccessLogRow, int64, error) {
	where, args := accessLogWhere(siteID, relayName, from, to)

	var total int64
	if err := d.DB.QueryRow("SELECT COUNT(*) FROM access_logs l WHERE "+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	offset := (page - 1) * pageSize
	if offset < 0 {
		offset = 0
	}
	// #nosec G202 -- accessLogWhere emits only fixed SQL fragments; values are bound via args
	query := `
		SELECT l.id, l.ts, l.relay_name, l.site_id, COALESCE(s.name, ''), l.client_ip,
		       l.method, l.path, l.status, l.latency_ms, l.bytes_in, l.bytes_out
		FROM access_logs l
		LEFT JOIN sites s ON s.id = l.site_id
		WHERE ` + where + `
		ORDER BY l.id DESC
		LIMIT ? OFFSET ?`
	args = append(args, pageSize, offset)
	rows, err := d.DB.Query(query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var logs []AccessLogRow
	for rows.Next() {
		var l AccessLogRow
		if err := rows.Scan(&l.ID, &l.Timestamp, &l.RelayName, &l.SiteID, &l.SiteName, &l.ClientIP,
			&l.Method, &l.Path, &l.Status, &l.LatencyMs, &l.BytesIn, &l.BytesOut); err != nil {
			return nil, 0, err
		}
		logs = append(logs, l)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	if logs == nil {
		logs = []AccessLogRow{}
	}
	return logs, total, nil
}

// TrendPoint is one hourly bucket of access log aggregation.
type TrendPoint struct {
	Bucket       int64 `json:"bucket"`
	Requests     int64 `json:"requests"`
	BytesIn      int64 `json:"bytes_in"`
	BytesOut     int64 `json:"bytes_out"`
	AvgLatencyMs int64 `json:"avg_latency_ms"`
}

// StatusCount is the request count for one HTTP status code.
type StatusCount struct {
	Status int   `json:"status"`
	Count  int64 `json:"count"`
}

// TopPath is one top requested path aggregation.
type TopPath struct {
	Path    string `json:"path"`
	Count   int64  `json:"count"`
	Bytes   int64  `json:"bytes"`
	IsOther bool   `json:"is_other,omitempty"` // rollup of all remaining paths
}

// topPathCount is how many individual paths are shown before the "other" rollup.
const topPathCount = 10

// TopIP is one top client IP aggregation.
type TopIP struct {
	IP           string   `json:"ip"`
	Count        int64    `json:"count"`
	Bytes        int64    `json:"bytes"`
	AvgLatencyMs int64    `json:"avg_latency_ms"`
	Geo          *GeoInfo `json:"geo,omitempty"` // filled by handler
}

// AccessLogStats holds all aggregations for the access log analysis page.
type AccessLogStats struct {
	Trend        []TrendPoint  `json:"trend"`
	Status       []StatusCount `json:"status"`
	TopPaths     []TopPath     `json:"top_paths"`
	TopIPs       []TopIP       `json:"top_ips"`
	Regions      []GeoAgg      `json:"regions"` // filled by handler from GeoLite
	Orgs         []GeoAgg      `json:"orgs"`    // filled by handler from GeoLite
	AvgLatencyMs int64         `json:"avg_latency_ms"`
	MaxLatencyMs int64         `json:"max_latency_ms"`
}

// AccessLogIPAgg is the per-client-IP aggregation feeding geo dimension rollups.
type AccessLogIPAgg struct {
	IP       string
	Count    int64
	BytesOut int64
}

// QueryAccessLogIPAggs aggregates access logs by client IP over the filters.
func (d *DB) QueryAccessLogIPAggs(siteID int64, relayName string, from, to int64) ([]AccessLogIPAgg, error) {
	where, args := accessLogWhere(siteID, relayName, from, to)
	// #nosec G202 -- accessLogWhere emits only fixed SQL fragments; values are bound via args
	rows, err := d.DB.Query(`
		SELECT l.client_ip, COUNT(*), SUM(l.bytes_out) FROM access_logs l
		WHERE `+where+`
		GROUP BY l.client_ip`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var aggs []AccessLogIPAgg
	for rows.Next() {
		var a AccessLogIPAgg
		if err := rows.Scan(&a.IP, &a.Count, &a.BytesOut); err != nil {
			return nil, err
		}
		aggs = append(aggs, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if aggs == nil {
		aggs = []AccessLogIPAgg{}
	}
	return aggs, nil
}

// QueryAccessLogStats aggregates access logs over the given filters.
func (d *DB) QueryAccessLogStats(siteID int64, relayName string, from, to int64) (*AccessLogStats, error) {
	where, args := accessLogWhere(siteID, relayName, from, to)

	stats := &AccessLogStats{
		Trend:    []TrendPoint{},
		Status:   []StatusCount{},
		TopPaths: []TopPath{},
		TopIPs:   []TopIP{},
	}

	// Hourly trend. Overall latency stats are derived from the same scan
	// (weighted average and max over buckets) to avoid a second full pass.
	// #nosec G202 -- accessLogWhere emits only fixed SQL fragments; values are bound via args
	rows, err := d.DB.Query(`
		SELECT l.ts/3600*3600 AS bucket, COUNT(*), SUM(l.bytes_in), SUM(l.bytes_out),
		       AVG(l.latency_ms), MAX(l.latency_ms)
		FROM access_logs l
		WHERE `+where+`
		GROUP BY bucket
		ORDER BY bucket`, args...)
	if err != nil {
		return nil, err
	}
	var totalCount, latencySum, maxLatency int64
	for rows.Next() {
		var t TrendPoint
		var avg *float64
		var bucketMax *int64
		if err := rows.Scan(&t.Bucket, &t.Requests, &t.BytesIn, &t.BytesOut, &avg, &bucketMax); err != nil {
			rows.Close()
			return nil, err
		}
		if avg != nil {
			t.AvgLatencyMs = int64(*avg)
		}
		if bucketMax != nil && *bucketMax > maxLatency {
			maxLatency = *bucketMax
		}
		totalCount += t.Requests
		latencySum += t.AvgLatencyMs * t.Requests
		stats.Trend = append(stats.Trend, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if totalCount > 0 {
		stats.AvgLatencyMs = latencySum / totalCount
	}
	stats.MaxLatencyMs = maxLatency

	// Status distribution
	// #nosec G202 -- accessLogWhere emits only fixed SQL fragments; values are bound via args
	rows, err = d.DB.Query(`
		SELECT l.status, COUNT(*) FROM access_logs l
		WHERE `+where+`
		GROUP BY l.status
		ORDER BY COUNT(*) DESC`, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var s StatusCount
		if err := rows.Scan(&s.Status, &s.Count); err != nil {
			rows.Close()
			return nil, err
		}
		stats.Status = append(stats.Status, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Top paths: keep the top 10 and roll the rest up into a single
	// "other" bucket so the list stays short even with many distinct paths.
	// Bytes are outbound only (node → user), matching the other traffic columns.
	// #nosec G202 -- accessLogWhere emits only fixed SQL fragments; values are bound via args
	rows, err = d.DB.Query(`
		SELECT l.path, COUNT(*), SUM(l.bytes_out) FROM access_logs l
		WHERE `+where+`
		GROUP BY l.path
		ORDER BY COUNT(*) DESC`, args...)
	if err != nil {
		return nil, err
	}
	var allPaths []TopPath
	for rows.Next() {
		var p TopPath
		if err := rows.Scan(&p.Path, &p.Count, &p.Bytes); err != nil {
			rows.Close()
			return nil, err
		}
		allPaths = append(allPaths, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(allPaths) > topPathCount {
		var otherCount, otherBytes int64
		for _, p := range allPaths[topPathCount:] {
			otherCount += p.Count
			otherBytes += p.Bytes
		}
		stats.TopPaths = allPaths[:topPathCount]
		stats.TopPaths = append(stats.TopPaths, TopPath{Path: "其他", Count: otherCount, Bytes: otherBytes, IsOther: true})
	} else {
		stats.TopPaths = allPaths
	}

	// Top IPs
	// #nosec G202 -- accessLogWhere emits only fixed SQL fragments; values are bound via args
	rows, err = d.DB.Query(`
		SELECT l.client_ip, COUNT(*), SUM(l.bytes_out), AVG(l.latency_ms) FROM access_logs l
		WHERE `+where+`
		GROUP BY l.client_ip
		ORDER BY COUNT(*) DESC
		LIMIT 20`, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var ip TopIP
		var avg *float64
		if err := rows.Scan(&ip.IP, &ip.Count, &ip.Bytes, &avg); err != nil {
			rows.Close()
			return nil, err
		}
		if avg != nil {
			ip.AvgLatencyMs = int64(*avg)
		}
		stats.TopIPs = append(stats.TopIPs, ip)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return stats, nil
}

func (d *DB) DashboardStats() (map[string]interface{}, error) {
	var total, online int
	var totalTraffic int64
	if err := d.DB.QueryRow(`
		SELECT
			COUNT(*),
			COALESCE(SUM(CASE WHEN enabled = 1 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(traffic_used), 0)
		FROM sites
	`).Scan(&total, &online, &totalTraffic); err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"total_sites":   total,
		"online_sites":  online,
		"total_traffic": totalTraffic,
	}, nil
}
