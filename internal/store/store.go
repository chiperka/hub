// Package store is the SQLite persistence layer of chiperka-hub.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS settings (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS users (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	email         TEXT NOT NULL UNIQUE,
	password_hash TEXT NOT NULL,
	created_at    INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS sessions (
	token_hash TEXT PRIMARY KEY,
	user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	expires_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS tokens (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	name         TEXT NOT NULL,
	kind         TEXT NOT NULL,
	token_hash   TEXT NOT NULL UNIQUE,
	prefix       TEXT NOT NULL,
	created_at   INTEGER NOT NULL,
	last_used_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS projects (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	slug       TEXT NOT NULL UNIQUE,
	name       TEXT NOT NULL,
	created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS workers (
	id             TEXT PRIMARY KEY,
	name           TEXT NOT NULL,
	token_id       INTEGER,
	is_local       INTEGER NOT NULL DEFAULT 0,
	status         TEXT NOT NULL DEFAULT 'waiting',
	paused         INTEGER NOT NULL DEFAULT 0,
	hostname       TEXT NOT NULL DEFAULT '',
	version        TEXT NOT NULL DEFAULT '',
	concurrency    INTEGER NOT NULL DEFAULT 0,
	capacity       INTEGER NOT NULL DEFAULT 0,
	max_containers INTEGER NOT NULL DEFAULT 0,
	active_tests   INTEGER NOT NULL DEFAULT 0,
	current_run_id TEXT NOT NULL DEFAULT '',
	commands       TEXT NOT NULL DEFAULT '[]',
	created_at     INTEGER NOT NULL,
	registered_at  INTEGER NOT NULL DEFAULT 0,
	last_heartbeat INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS runs (
	id              TEXT PRIMARY KEY,
	project_id      INTEGER,
	status          TEXT NOT NULL,
	source          TEXT NOT NULL DEFAULT 'cli',
	version         TEXT NOT NULL DEFAULT '',
	total           INTEGER NOT NULL DEFAULT 0,
	passed          INTEGER NOT NULL DEFAULT 0,
	failed          INTEGER NOT NULL DEFAULT 0,
	errored         INTEGER NOT NULL DEFAULT 0,
	skipped         INTEGER NOT NULL DEFAULT 0,
	cancelled_tests INTEGER NOT NULL DEFAULT 0,
	pending         INTEGER NOT NULL DEFAULT 0,
	running         INTEGER NOT NULL DEFAULT 0,
	snapshots_ready INTEGER NOT NULL DEFAULT 1,
	cancelled       INTEGER NOT NULL DEFAULT 0,
	created_by      TEXT NOT NULL DEFAULT '',
	rerun_of        TEXT NOT NULL DEFAULT '',
	started_at      INTEGER NOT NULL,
	completed_at    INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_runs_started ON runs(started_at DESC);
CREATE TABLE IF NOT EXISTS suites (
	id        INTEGER PRIMARY KEY AUTOINCREMENT,
	run_id    TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
	name      TEXT NOT NULL,
	file_path TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_suites_run ON suites(run_id);
CREATE TABLE IF NOT EXISTS tests (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	uuid          TEXT NOT NULL UNIQUE,
	run_id        TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
	suite_id      INTEGER NOT NULL REFERENCES suites(id) ON DELETE CASCADE,
	name          TEXT NOT NULL,
	description   TEXT NOT NULL DEFAULT '',
	tags          TEXT NOT NULL DEFAULT '',
	endpoint      TEXT NOT NULL DEFAULT '',
	status        TEXT NOT NULL,
	error_message TEXT NOT NULL DEFAULT '',
	duration_ms   INTEGER NOT NULL DEFAULT 0,
	definition    TEXT NOT NULL,
	job_id        INTEGER NOT NULL DEFAULT 0,
	worker_id     TEXT NOT NULL DEFAULT '',
	started_at    INTEGER NOT NULL DEFAULT 0,
	completed_at  INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_tests_run ON tests(run_id);
CREATE TABLE IF NOT EXISTS jobs (
	id               INTEGER PRIMARY KEY AUTOINCREMENT,
	run_id           TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
	test_id          INTEGER NOT NULL REFERENCES tests(id) ON DELETE CASCADE,
	status           TEXT NOT NULL,
	result           TEXT NOT NULL DEFAULT '',
	dag              TEXT NOT NULL,
	weight           INTEGER NOT NULL DEFAULT 1,
	container_count  INTEGER NOT NULL DEFAULT 1,
	worker_id        TEXT NOT NULL DEFAULT '',
	token            TEXT NOT NULL DEFAULT '',
	retry_count      INTEGER NOT NULL DEFAULT 0,
	step_results     TEXT NOT NULL DEFAULT '',
	error_message    TEXT NOT NULL DEFAULT '',
	duration_ms      INTEGER NOT NULL DEFAULT 0,
	idempotency_key  TEXT NOT NULL DEFAULT '',
	created_at       INTEGER NOT NULL,
	claimed_at       INTEGER NOT NULL DEFAULT 0,
	acked_at         INTEGER NOT NULL DEFAULT 0,
	last_progress_at INTEGER NOT NULL DEFAULT 0,
	completed_at     INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_jobs_status ON jobs(status, id);
CREATE INDEX IF NOT EXISTS idx_jobs_test ON jobs(test_id);
CREATE INDEX IF NOT EXISTS idx_jobs_token ON jobs(token);
CREATE TABLE IF NOT EXISTS step_progress (
	job_id      INTEGER NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
	step        TEXT NOT NULL,
	status      TEXT NOT NULL,
	duration_ms INTEGER NOT NULL DEFAULT 0,
	error       TEXT NOT NULL DEFAULT '',
	started_at  INTEGER NOT NULL DEFAULT 0,
	updated_at  INTEGER NOT NULL,
	PRIMARY KEY (job_id, step)
);
CREATE TABLE IF NOT EXISTS artifacts (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	test_id    INTEGER NOT NULL REFERENCES tests(id) ON DELETE CASCADE,
	job_id     INTEGER NOT NULL DEFAULT 0,
	name       TEXT NOT NULL,
	size       INTEGER NOT NULL,
	created_at INTEGER NOT NULL,
	UNIQUE (test_id, name)
);
CREATE TABLE IF NOT EXISTS run_snapshots (
	run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
	path   TEXT NOT NULL,
	size   INTEGER NOT NULL,
	PRIMARY KEY (run_id, path)
);
`

// Open opens (and migrates) the SQLite database at path.
func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// A single connection serialises all writes and avoids SQLITE_BUSY entirely.
	// The workload (a handful of workers + UI) is far below what this can serve.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Now returns the current time in unix milliseconds — the time unit of every column.
func Now() int64 { return time.Now().UnixMilli() }

func (s *Store) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// GetSetting returns a setting value or "" when unset.
func (s *Store) GetSetting(ctx context.Context, key string) string {
	var v string
	s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, key).Scan(&v)
	return v
}

func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

func notFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}
