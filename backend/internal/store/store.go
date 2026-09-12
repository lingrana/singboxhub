// Package store implements the panel persistence layer for SQLite and PostgreSQL.
// All timestamps in the database are unix seconds; the API layer converts
// them to RFC 3339 UTC.
package store

import (
	"database/sql"
	"fmt"
	"net/url"
	"strings"

	_ "github.com/lib/pq"
	_ "modernc.org/sqlite"
)

// Driver identifies the supported database engines.
type Driver string

const (
	DriverSQLite   Driver = "sqlite"
	DriverPostgres Driver = "postgres"
)

// Store wraps the selected SQL database.
type Store struct {
	db     *sql.DB
	driver Driver
}

// Open opens (creating if needed) the database with WAL mode enabled and
// runs pending migrations.
func Open(dbPath string) (*Store, error) {
	dsn := "file:" + dbPath +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// Single connection serializes writes; our write volume is tiny and this
	// avoids SQLITE_BUSY handling entirely.
	db.SetMaxOpenConns(1)
	s := &Store{db: db, driver: DriverSQLite}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

// OpenWithConfig opens a SQLite or PostgreSQL database and runs migrations.
func OpenWithConfig(driver, dsn string) (*Store, error) {
	driver = strings.ToLower(strings.TrimSpace(driver))
	if driver != string(DriverSQLite) && driver != string(DriverPostgres) {
		return nil, fmt.Errorf("unsupported database driver %q", driver)
	}
	if strings.TrimSpace(dsn) == "" {
		return nil, fmt.Errorf("database dsn is required")
	}
	driverName := string(driver)
	if driver == string(DriverSQLite) {
		dsn = "file:" + normalizeDSN(driver, dsn) + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	} else {
		dsn = normalizeDSN(driver, dsn)
	}
	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, err
	}
	if driver == string(DriverSQLite) {
		db.SetMaxOpenConns(1)
	}
	s := &Store{db: db, driver: Driver(driver)}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

// normalizeDSN applies driver-specific DSN defaults. PostgreSQL: inject
// sslmode=prefer when the DSN omits one — lib/pq otherwise defaults to
// require, which refuses every TLS-less server (the common self-hosted case)
// right from the first-run setup page. SQLite: strip an accidental leading
// file: scheme, which the file-URI rebuild in OpenWithConfig would otherwise
// duplicate.
func normalizeDSN(driver, dsn string) string {
	switch driver {
	case string(DriverSQLite):
		return strings.TrimPrefix(dsn, "file:")
	case string(DriverPostgres):
		if u, err := url.Parse(dsn); err == nil && (u.Scheme == "postgres" || u.Scheme == "postgresql") {
			q := u.Query()
			if q.Has("sslmode") {
				return dsn
			}
			q.Set("sslmode", "prefer")
			u.RawQuery = q.Encode()
			return u.String()
		}
		for _, tok := range strings.Fields(dsn) {
			if strings.HasPrefix(tok, "sslmode=") {
				return dsn
			}
		}
		return dsn + " sslmode=prefer"
	default:
		return dsn
	}
}

func (s *Store) rebind(query string) string {
	if s.driver != DriverPostgres {
		return query
	}
	var b strings.Builder
	arg := 1
	for _, r := range query {
		if r == '?' {
			fmt.Fprintf(&b, "$%d", arg)
			arg++
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func (s *Store) exec(query string, args ...any) (sql.Result, error) {
	return s.db.Exec(s.rebind(query), args...)
}

func (s *Store) query(query string, args ...any) (*sql.Rows, error) {
	return s.db.Query(s.rebind(query), args...)
}

func (s *Store) queryRow(query string, args ...any) *sql.Row {
	return s.db.QueryRow(s.rebind(query), args...)
}

func (s *Store) begin() (*sql.Tx, error) { return s.db.Begin() }

func (s *Store) txExec(tx *sql.Tx, query string, args ...any) (sql.Result, error) {
	return tx.Exec(s.rebind(query), args...)
}

func (s *Store) txPrepare(tx *sql.Tx, query string) (*sql.Stmt, error) {
	return tx.Prepare(s.rebind(query))
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

var migrations = []string{
	// 1: initial schema
	`
CREATE TABLE IF NOT EXISTS users (
  id            INTEGER PRIMARY KEY,
  username      TEXT NOT NULL UNIQUE,
  password_hash TEXT NOT NULL,
  created_at    INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS refresh_tokens (
  jti        TEXT PRIMARY KEY,
  user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  expires_at INTEGER NOT NULL,
  revoked    INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_refresh_tokens_user ON refresh_tokens(user_id);

CREATE TABLE IF NOT EXISTS nodes (
  id             TEXT PRIMARY KEY,
  name           TEXT NOT NULL UNIQUE,
  api_url        TEXT NOT NULL,
  api_secret_enc TEXT NOT NULL,
  outbound_enc   TEXT NOT NULL DEFAULT '',
  tags           TEXT NOT NULL DEFAULT '',
  enabled        INTEGER NOT NULL DEFAULT 1,
  remark         TEXT NOT NULL DEFAULT '',
  created_at     INTEGER NOT NULL,
  updated_at     INTEGER NOT NULL,
  last_online_at INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS traffic_samples (
  node_id         TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
  ts              INTEGER NOT NULL,
  up_bytes        INTEGER NOT NULL,
  down_bytes      INTEGER NOT NULL,
  peak_up_bps     INTEGER NOT NULL,
  peak_down_bps   INTEGER NOT NULL,
  max_connections INTEGER NOT NULL,
  PRIMARY KEY (node_id, ts)
);

CREATE TABLE IF NOT EXISTS traffic_rollup (
  node_id         TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
  interval        TEXT NOT NULL,
  bucket_start    INTEGER NOT NULL,
  up_bytes        INTEGER NOT NULL,
  down_bytes      INTEGER NOT NULL,
  peak_up_bps     INTEGER NOT NULL,
  peak_down_bps   INTEGER NOT NULL,
  max_connections INTEGER NOT NULL,
  PRIMARY KEY (node_id, interval, bucket_start)
);

CREATE TABLE IF NOT EXISTS ip_stats (
  node_id             TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
  source_ip           TEXT NOT NULL,
  upload_bytes        INTEGER NOT NULL DEFAULT 0,
  download_bytes      INTEGER NOT NULL DEFAULT 0,
  connections_total   INTEGER NOT NULL DEFAULT 0,
  active_connections  INTEGER NOT NULL DEFAULT 0,
  first_seen_at       INTEGER NOT NULL,
  last_seen_at        INTEGER NOT NULL,
  PRIMARY KEY (node_id, source_ip)
);
CREATE INDEX IF NOT EXISTS idx_ip_stats_last_seen ON ip_stats(last_seen_at);

CREATE TABLE IF NOT EXISTS ip_daily (
  node_id    TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
  source_ip  TEXT NOT NULL,
  day        INTEGER NOT NULL,
  up_bytes   INTEGER NOT NULL DEFAULT 0,
  down_bytes INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (node_id, source_ip, day)
);

CREATE TABLE IF NOT EXISTS ip_geo_cache (
  ip         TEXT PRIMARY KEY,
  country    TEXT NOT NULL DEFAULT '',
  region     TEXT NOT NULL DEFAULT '',
  city       TEXT NOT NULL DEFAULT '',
  as_org     TEXT NOT NULL DEFAULT '',
  checked_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS exit_ip_checks (
  node_id    TEXT PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE,
  ip         TEXT NOT NULL,
  country    TEXT NOT NULL DEFAULT '',
  city       TEXT NOT NULL DEFAULT '',
  as_org     TEXT NOT NULL DEFAULT '',
  method     TEXT NOT NULL DEFAULT '',
  checked_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS operations (
  id         TEXT PRIMARY KEY,
  node_id    TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
  type       TEXT NOT NULL,
  status     TEXT NOT NULL,
  result     TEXT,
  error      TEXT,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS subscription (
  id         INTEGER PRIMARY KEY CHECK (id = 1),
  token      TEXT NOT NULL DEFAULT '',
  updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS settings (
  id                       INTEGER PRIMARY KEY CHECK (id = 1),
  sampler_interval_seconds INTEGER NOT NULL DEFAULT 2,
  retention_days           INTEGER NOT NULL DEFAULT 30,
  ip_lookup_enabled        INTEGER NOT NULL DEFAULT 1,
  ip_lookup_provider_url   TEXT NOT NULL DEFAULT '',
  singbox_bin_path         TEXT NOT NULL DEFAULT '',
  updated_at               INTEGER NOT NULL
);
`,
	// 2: add favicon and logo columns to settings
	`ALTER TABLE settings ADD COLUMN favicon BLOB DEFAULT NULL;
ALTER TABLE settings ADD COLUMN logo    BLOB DEFAULT NULL;
`,
	// 3: a monotonic revision makes settings updates atomic across requests.
	`ALTER TABLE settings ADD COLUMN revision INTEGER NOT NULL DEFAULT 1;`,
	// 4: add role to users (admin|user)
	`ALTER TABLE users ADD COLUMN role TEXT NOT NULL DEFAULT 'admin';`,
	// 5: add brand_name to settings
	`ALTER TABLE settings ADD COLUMN brand_name TEXT NOT NULL DEFAULT 'sing-box hub';`,
	// 6: per-user subscription - migrate the old singleton before removing it.
	`CREATE TABLE IF NOT EXISTS user_subscriptions (
  user_id   INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  token     TEXT NOT NULL DEFAULT '',
	updated_at INTEGER NOT NULL
);
INSERT OR IGNORE INTO user_subscriptions (user_id, token, updated_at)
SELECT u.id, old.token, old.updated_at
FROM users u CROSS JOIN subscription old
WHERE old.id = 1 AND old.token != ''
ORDER BY u.id LIMIT 1;
DROP TABLE IF EXISTS subscription;`,
	// 7: add reset_cooldown_minutes to settings
	`ALTER TABLE settings ADD COLUMN reset_cooldown_minutes INTEGER NOT NULL DEFAULT 10;`,
	// 8: add last_reset_at to user_subscriptions (missing from migration 6 on existing DBs)
	`ALTER TABLE user_subscriptions ADD COLUMN last_reset_at INTEGER NOT NULL DEFAULT 0;`,
	// 9: upstream subscription sources + node import provenance
	`ALTER TABLE nodes ADD COLUMN source TEXT NOT NULL DEFAULT '';
ALTER TABLE nodes ADD COLUMN config_url TEXT NOT NULL DEFAULT '';
ALTER TABLE nodes ADD COLUMN kce_key_enc TEXT NOT NULL DEFAULT '';
CREATE TABLE IF NOT EXISTS upstream_subscriptions (
  id            TEXT PRIMARY KEY,
  name          TEXT NOT NULL UNIQUE,
  url           TEXT NOT NULL,
  key_enc       TEXT NOT NULL DEFAULT '',
  enabled       INTEGER NOT NULL DEFAULT 1,
  last_sync_at  INTEGER NOT NULL DEFAULT 0,
  last_sync_ok  INTEGER NOT NULL DEFAULT 0,
  last_sync_msg TEXT NOT NULL DEFAULT '',
  node_count    INTEGER NOT NULL DEFAULT 0,
  created_at    INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL
);`,
	// 10: add mode column to nodes for Clash proxy-group classification
	`ALTER TABLE nodes ADD COLUMN mode TEXT NOT NULL DEFAULT '';`,
	// 11: add ICMP latency monitoring fields to settings
	`ALTER TABLE settings ADD COLUMN icmp_monitor_enabled INTEGER NOT NULL DEFAULT 0;
ALTER TABLE settings ADD COLUMN icmp_monitor_target TEXT NOT NULL DEFAULT '8.8.8.8';
ALTER TABLE settings ADD COLUMN icmp_monitor_interval_seconds INTEGER NOT NULL DEFAULT 60;
ALTER TABLE settings ADD COLUMN icmp_auto_disable_threshold_ms INTEGER NOT NULL DEFAULT 2000;
ALTER TABLE settings ADD COLUMN icmp_auto_disable_consecutive INTEGER NOT NULL DEFAULT 3;`,
	// 12: add latency monitoring state to nodes
	`ALTER TABLE nodes ADD COLUMN latency_ms INTEGER NOT NULL DEFAULT -1;
ALTER TABLE nodes ADD COLUMN latency_checked_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE nodes ADD COLUMN latency_fail_count INTEGER NOT NULL DEFAULT 0;`,
}

func (s *Store) migrate() error {
	if s.driver == DriverPostgres {
		return s.migratePostgres()
	}
	if _, err := s.exec(`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); err != nil {
		return err
	}
	var current int
	row := s.queryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_version`)
	if err := row.Scan(&current); err != nil {
		return err
	}
	for v := current; v < len(migrations); v++ {
		tx, err := s.begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(s.rebind(migrations[v])); err != nil {
			if strings.Contains(err.Error(), "duplicate column") {
				_ = tx.Rollback()
				if _, insertErr := s.exec(`INSERT INTO schema_version (version) VALUES (?)`, v+1); insertErr != nil {
					return insertErr
				}
			} else {
				_ = tx.Rollback()
				return fmt.Errorf("migration %d: %w", v+1, err)
			}
		} else {
			if _, err := tx.Exec(s.rebind(`INSERT INTO schema_version (version) VALUES (?)`), v+1); err != nil {
				_ = tx.Rollback()
				return err
			}
			if err := tx.Commit(); err != nil {
				return err
			}
		}
	}
	return nil
}

const postgresSchema = `
CREATE TABLE IF NOT EXISTS users (
  id BIGINT GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY,
  username TEXT NOT NULL UNIQUE,
  password_hash TEXT NOT NULL,
  role TEXT NOT NULL DEFAULT 'admin',
  created_at BIGINT NOT NULL
);
CREATE TABLE IF NOT EXISTS refresh_tokens (
  jti TEXT PRIMARY KEY, user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  expires_at BIGINT NOT NULL, revoked INTEGER NOT NULL DEFAULT 0, created_at BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_refresh_tokens_user ON refresh_tokens(user_id);
CREATE TABLE IF NOT EXISTS nodes (
  id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE, api_url TEXT NOT NULL,
  api_secret_enc TEXT NOT NULL, outbound_enc TEXT NOT NULL DEFAULT '',
  tags TEXT NOT NULL DEFAULT '', enabled INTEGER NOT NULL DEFAULT 1,
  remark TEXT NOT NULL DEFAULT '', created_at BIGINT NOT NULL, updated_at BIGINT NOT NULL,
  last_online_at BIGINT NOT NULL DEFAULT 0, source TEXT NOT NULL DEFAULT '',
  config_url TEXT NOT NULL DEFAULT '', kce_key_enc TEXT NOT NULL DEFAULT '',
  mode TEXT NOT NULL DEFAULT '',
  latency_ms INTEGER NOT NULL DEFAULT -1,
  latency_checked_at BIGINT NOT NULL DEFAULT 0,
  latency_fail_count INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS traffic_samples (
  node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE, ts BIGINT NOT NULL,
  up_bytes BIGINT NOT NULL, down_bytes BIGINT NOT NULL, peak_up_bps BIGINT NOT NULL,
  peak_down_bps BIGINT NOT NULL, max_connections INTEGER NOT NULL,
  PRIMARY KEY (node_id, ts)
);
CREATE TABLE IF NOT EXISTS traffic_rollup (
  node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE, interval TEXT NOT NULL,
  bucket_start BIGINT NOT NULL, up_bytes BIGINT NOT NULL, down_bytes BIGINT NOT NULL,
  peak_up_bps BIGINT NOT NULL, peak_down_bps BIGINT NOT NULL, max_connections INTEGER NOT NULL,
  PRIMARY KEY (node_id, interval, bucket_start)
);
CREATE TABLE IF NOT EXISTS ip_stats (
  node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE, source_ip TEXT NOT NULL,
  upload_bytes BIGINT NOT NULL DEFAULT 0, download_bytes BIGINT NOT NULL DEFAULT 0,
  connections_total BIGINT NOT NULL DEFAULT 0, active_connections INTEGER NOT NULL DEFAULT 0,
  first_seen_at BIGINT NOT NULL, last_seen_at BIGINT NOT NULL, PRIMARY KEY (node_id, source_ip)
);
CREATE INDEX IF NOT EXISTS idx_ip_stats_last_seen ON ip_stats(last_seen_at);
CREATE TABLE IF NOT EXISTS ip_daily (
  node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE, source_ip TEXT NOT NULL,
  day BIGINT NOT NULL, up_bytes BIGINT NOT NULL DEFAULT 0, down_bytes BIGINT NOT NULL DEFAULT 0,
  PRIMARY KEY (node_id, source_ip, day)
);
CREATE TABLE IF NOT EXISTS ip_geo_cache (
  ip TEXT PRIMARY KEY, country TEXT NOT NULL DEFAULT '', region TEXT NOT NULL DEFAULT '',
  city TEXT NOT NULL DEFAULT '', as_org TEXT NOT NULL DEFAULT '', checked_at BIGINT NOT NULL
);
CREATE TABLE IF NOT EXISTS exit_ip_checks (
  node_id TEXT PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE, ip TEXT NOT NULL,
  country TEXT NOT NULL DEFAULT '', city TEXT NOT NULL DEFAULT '', as_org TEXT NOT NULL DEFAULT '',
  method TEXT NOT NULL DEFAULT '', checked_at BIGINT NOT NULL
);
CREATE TABLE IF NOT EXISTS operations (
  id TEXT PRIMARY KEY, node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
  type TEXT NOT NULL, status TEXT NOT NULL, result TEXT, error TEXT,
  created_at BIGINT NOT NULL, updated_at BIGINT NOT NULL
);
CREATE TABLE IF NOT EXISTS user_subscriptions (
  user_id BIGINT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  token TEXT NOT NULL DEFAULT '', updated_at BIGINT NOT NULL, last_reset_at BIGINT NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS settings (
  id INTEGER PRIMARY KEY CHECK (id = 1), sampler_interval_seconds INTEGER NOT NULL DEFAULT 2,
  retention_days INTEGER NOT NULL DEFAULT 30, ip_lookup_enabled INTEGER NOT NULL DEFAULT 1,
  ip_lookup_provider_url TEXT NOT NULL DEFAULT '', singbox_bin_path TEXT NOT NULL DEFAULT '',
  updated_at BIGINT NOT NULL, favicon BYTEA, logo BYTEA, revision BIGINT NOT NULL DEFAULT 1,
	brand_name TEXT NOT NULL DEFAULT 'sing-box hub',
  reset_cooldown_minutes INTEGER NOT NULL DEFAULT 10,
  icmp_monitor_enabled INTEGER NOT NULL DEFAULT 0,
  icmp_monitor_target TEXT NOT NULL DEFAULT '8.8.8.8',
  icmp_monitor_interval_seconds INTEGER NOT NULL DEFAULT 60,
  icmp_auto_disable_threshold_ms INTEGER NOT NULL DEFAULT 2000,
  icmp_auto_disable_consecutive INTEGER NOT NULL DEFAULT 3
);
CREATE TABLE IF NOT EXISTS upstream_subscriptions (
  id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE, url TEXT NOT NULL, key_enc TEXT NOT NULL DEFAULT '',
  enabled INTEGER NOT NULL DEFAULT 1, last_sync_at BIGINT NOT NULL DEFAULT 0,
  last_sync_ok INTEGER NOT NULL DEFAULT 0, last_sync_msg TEXT NOT NULL DEFAULT '',
  node_count INTEGER NOT NULL DEFAULT 0, created_at BIGINT NOT NULL, updated_at BIGINT NOT NULL
);`

func (s *Store) migratePostgres() error {
	if _, err := s.db.Exec(postgresSchema); err != nil {
		return err
	}
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); err != nil {
		return err
	}
	var current int
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&current); err != nil {
		return err
	}
	if current == 0 {
		_, err := s.db.Exec(`INSERT INTO schema_version (version) VALUES ($1)`, len(migrations))
		return err
	}

	// Apply pending migrations for existing databases
	for v := current; v < len(migrations); v++ {
		tx, err := s.begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[v]); err != nil {
			// Ignore duplicate column errors - the column might already exist from postgresSchema
			if strings.Contains(err.Error(), "already exists") || strings.Contains(err.Error(), "duplicate") {
				_ = tx.Rollback()
				if _, insertErr := s.db.Exec(`INSERT INTO schema_version (version) VALUES ($1)`, v+1); insertErr != nil {
					return insertErr
				}
			} else {
				_ = tx.Rollback()
				return fmt.Errorf("migration %d: %w", v+1, err)
			}
		} else {
			if _, err := tx.Exec(`INSERT INTO schema_version (version) VALUES ($1)`, v+1); err != nil {
				_ = tx.Rollback()
				return err
			}
			if err := tx.Commit(); err != nil {
				return err
			}
		}
	}
	return nil
}
