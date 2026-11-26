package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
)

// Cloud server statuses, in lifecycle order.
const (
	CloudRequested = "requested" // the hub asked the provider for a server
	CloudStarting  = "starting"  // the server boots, tunnel and worker connect
	CloudWorking   = "working"   // the worker takes tests
	CloudIdle      = "idle"      // no work, waiting to be shut down
	CloudDraining  = "draining"  // the last tests finish, then the server is deleted
	CloudDeleted   = "deleted"
	CloudFailed    = "failed"
)

// CloudWorkerPrefix marks workers that run on autoscaled cloud servers.
const CloudWorkerPrefix = "wc-"

const cloudSchema = `
CREATE TABLE IF NOT EXISTS cloud_servers (
	id                  TEXT PRIMARY KEY,
	provider            TEXT NOT NULL,
	provider_id         TEXT NOT NULL DEFAULT '',
	type                TEXT NOT NULL DEFAULT '',
	location            TEXT NOT NULL DEFAULT '',
	spot                INTEGER NOT NULL DEFAULT 0,
	public_ip           TEXT NOT NULL DEFAULT '',
	status              TEXT NOT NULL,
	reason              TEXT NOT NULL DEFAULT '',
	end_reason          TEXT NOT NULL DEFAULT '',
	worker_id           TEXT NOT NULL DEFAULT '',
	host_key            TEXT NOT NULL DEFAULT '',
	hourly_price        REAL NOT NULL DEFAULT 0,
	currency            TEXT NOT NULL DEFAULT '',
	manual              INTEGER NOT NULL DEFAULT 0,
	hold_until          INTEGER NOT NULL DEFAULT 0,
	created_at          INTEGER NOT NULL,
	provider_created_at INTEGER NOT NULL DEFAULT 0,
	boot_at             INTEGER NOT NULL DEFAULT 0,
	tunnel_at           INTEGER NOT NULL DEFAULT 0,
	worker_at           INTEGER NOT NULL DEFAULT 0,
	first_test_at       INTEGER NOT NULL DEFAULT 0,
	idle_since          INTEGER NOT NULL DEFAULT 0,
	drain_at            INTEGER NOT NULL DEFAULT 0,
	deleted_at          INTEGER NOT NULL DEFAULT 0,
	completed_jobs      INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_cloud_servers_status ON cloud_servers(status);
CREATE INDEX IF NOT EXISTS idx_cloud_servers_created ON cloud_servers(created_at DESC);
CREATE TABLE IF NOT EXISTS cloud_events (
	id        INTEGER PRIMARY KEY AUTOINCREMENT,
	ts        INTEGER NOT NULL,
	server_id TEXT NOT NULL DEFAULT '',
	kind      TEXT NOT NULL,
	message   TEXT NOT NULL,
	data      TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_cloud_events_ts ON cloud_events(ts DESC);
CREATE TABLE IF NOT EXISTS cloud_samples (
	ts      INTEGER PRIMARY KEY,
	queue   INTEGER NOT NULL,
	servers INTEGER NOT NULL
);
`

type CloudServer struct {
	ID                string  `json:"id"`
	Provider          string  `json:"provider"`
	ProviderID        string  `json:"provider_id"`
	Type              string  `json:"type"`
	Location          string  `json:"location"`
	Spot              bool    `json:"spot"`
	PublicIP          string  `json:"public_ip"`
	Status            string  `json:"status"`
	Reason            string  `json:"reason"`     // why the server exists
	EndReason         string  `json:"end_reason"` // why it is going away / went away / failed
	WorkerID          string  `json:"worker_id"`
	HostKey           string  `json:"-"` // pinned SSH host key (authorized_keys format)
	HourlyPrice       float64 `json:"hourly_price"`
	Currency          string  `json:"currency"`
	Manual            bool    `json:"manual"`
	HoldUntil         int64   `json:"hold_until"`
	CreatedAt         int64   `json:"created_at"`
	ProviderCreatedAt int64   `json:"provider_created_at"`
	BootAt            int64   `json:"boot_at"`
	TunnelAt          int64   `json:"tunnel_at"`
	WorkerAt          int64   `json:"worker_at"`
	FirstTestAt       int64   `json:"first_test_at"`
	IdleSince         int64   `json:"idle_since"`
	DrainAt           int64   `json:"drain_at"`
	DeletedAt         int64   `json:"deleted_at"`
	CompletedJobs     int     `json:"completed_jobs"` // kept when the worker is removed
}

// Active reports whether the server still exists (or is about to) at the provider.
func (c *CloudServer) Active() bool { return c.Status != CloudDeleted && c.Status != CloudFailed }

const cloudCols = `id,provider,provider_id,type,location,spot,public_ip,status,reason,end_reason,worker_id,host_key,hourly_price,currency,
	manual,hold_until,created_at,provider_created_at,boot_at,tunnel_at,worker_at,first_test_at,idle_since,drain_at,deleted_at,completed_jobs`

func (c *CloudServer) fields() []any {
	return []any{&c.ID, &c.Provider, &c.ProviderID, &c.Type, &c.Location, &c.Spot, &c.PublicIP, &c.Status, &c.Reason, &c.EndReason,
		&c.WorkerID, &c.HostKey, &c.HourlyPrice, &c.Currency, &c.Manual, &c.HoldUntil, &c.CreatedAt, &c.ProviderCreatedAt,
		&c.BootAt, &c.TunnelAt, &c.WorkerAt, &c.FirstTestAt, &c.IdleSince, &c.DrainAt, &c.DeletedAt, &c.CompletedJobs}
}

func (c *CloudServer) values() []any {
	f := c.fields()
	out := make([]any, len(f))
	for i, p := range f {
		switch v := p.(type) {
		case *string:
			out[i] = *v
		case *bool:
			out[i] = *v
		case *int64:
			out[i] = *v
		case *float64:
			out[i] = *v
		case *int:
			out[i] = *v
		}
	}
	return out
}

// InsertCloudServer adds a new server; it fails on an ID collision instead of
// overwriting another server's history.
func (s *Store) InsertCloudServer(ctx context.Context, c *CloudServer) error {
	ph := strings.TrimSuffix(strings.Repeat("?,", len(c.fields())), ",")
	_, err := s.db.ExecContext(ctx, `INSERT INTO cloud_servers(`+cloudCols+`) VALUES(`+ph+`)`, c.values()...)
	return err
}

// SaveCloudServer inserts or fully replaces a server row.
func (s *Store) SaveCloudServer(ctx context.Context, c *CloudServer) error {
	ph := strings.TrimSuffix(strings.Repeat("?,", len(c.fields())), ",")
	_, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO cloud_servers(`+cloudCols+`) VALUES(`+ph+`)`, c.values()...)
	return err
}

func (s *Store) GetCloudServer(ctx context.Context, id string) (*CloudServer, error) {
	var c CloudServer
	if err := s.db.QueryRowContext(ctx, `SELECT `+cloudCols+` FROM cloud_servers WHERE id=?`, id).Scan(c.fields()...); err != nil {
		return nil, notFound(err)
	}
	return &c, nil
}

// ListCloudServers returns servers that are active, or ended after `since` (unix ms).
func (s *Store) ListCloudServers(ctx context.Context, since int64) ([]CloudServer, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+cloudCols+` FROM cloud_servers
		WHERE status NOT IN (?,?) OR deleted_at>=? OR created_at>=? ORDER BY created_at`, CloudDeleted, CloudFailed, since, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CloudServer{}
	for rows.Next() {
		var c CloudServer
		if err := rows.Scan(c.fields()...); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ListCloudServersBetween returns every server alive at some point in [from, to).
func (s *Store) ListCloudServersBetween(ctx context.Context, from, to int64) ([]CloudServer, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+cloudCols+` FROM cloud_servers
		WHERE created_at<? AND (deleted_at=0 OR deleted_at>=?) ORDER BY created_at`, to, from)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CloudServer{}
	for rows.Next() {
		var c CloudServer
		if err := rows.Scan(c.fields()...); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CloudQueueDepth counts tests waiting to run: pending jobs of live runs.
func (s *Store) CloudQueueDepth(ctx context.Context) int {
	var n int
	s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs j JOIN runs r ON r.id=j.run_id
		WHERE j.status='pending' AND r.cancelled=0 AND r.snapshots_ready=1`).Scan(&n)
	return n
}

// WorkerFirstCompletion returns when the worker finished its first job (0 = never).
func (s *Store) WorkerFirstCompletion(ctx context.Context, workerID string) int64 {
	var ts sql.NullInt64
	s.db.QueryRowContext(ctx, `SELECT MIN(completed_at) FROM jobs WHERE worker_id=? AND completed_at>0`, workerID).Scan(&ts)
	return ts.Int64
}

// ── events ──────────────────────────────────────────────────────────────────

type CloudEvent struct {
	ID       int64           `json:"id"`
	TS       int64           `json:"ts"`
	ServerID string          `json:"server_id"`
	Kind     string          `json:"kind"` // decision | server | provider | spot | settings | error
	Message  string          `json:"message"`
	Data     json.RawMessage `json:"data,omitempty"`
}

func (s *Store) AddCloudEvent(ctx context.Context, e *CloudEvent) error {
	if e.TS == 0 {
		e.TS = Now()
	}
	data := string(e.Data)
	res, err := s.db.ExecContext(ctx, `INSERT INTO cloud_events(ts,server_id,kind,message,data) VALUES(?,?,?,?,?)`,
		e.TS, e.ServerID, e.Kind, e.Message, data)
	if err == nil {
		e.ID, _ = res.LastInsertId()
	}
	return err
}

type CloudEventFilter struct {
	ServerID string
	Kind     string
	BeforeID int64
	Limit    int
}

func (s *Store) ListCloudEvents(ctx context.Context, f CloudEventFilter) ([]CloudEvent, error) {
	q := `SELECT id,ts,server_id,kind,message,data FROM cloud_events WHERE 1=1`
	var args []any
	if f.ServerID != "" {
		q += ` AND server_id=?`
		args = append(args, f.ServerID)
	}
	if f.Kind != "" {
		q += ` AND kind=?`
		args = append(args, f.Kind)
	}
	if f.BeforeID > 0 {
		q += ` AND id<?`
		args = append(args, f.BeforeID)
	}
	if f.Limit <= 0 || f.Limit > 1000 {
		f.Limit = 200
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, f.Limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CloudEvent{}
	for rows.Next() {
		var e CloudEvent
		var data string
		if err := rows.Scan(&e.ID, &e.TS, &e.ServerID, &e.Kind, &e.Message, &data); err != nil {
			return nil, err
		}
		if data != "" {
			e.Data = json.RawMessage(data)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) GetCloudEvent(ctx context.Context, id int64) (*CloudEvent, error) {
	var e CloudEvent
	var data string
	err := s.db.QueryRowContext(ctx, `SELECT id,ts,server_id,kind,message,data FROM cloud_events WHERE id=?`, id).
		Scan(&e.ID, &e.TS, &e.ServerID, &e.Kind, &e.Message, &data)
	if err != nil {
		return nil, notFound(err)
	}
	if data != "" {
		e.Data = json.RawMessage(data)
	}
	return &e, nil
}

// ── queue samples (timeline) ────────────────────────────────────────────────

type CloudSample struct {
	TS      int64 `json:"ts"`
	Queue   int   `json:"queue"`
	Servers int   `json:"servers"`
}

func (s *Store) AddCloudSample(ctx context.Context, smp CloudSample) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO cloud_samples(ts,queue,servers) VALUES(?,?,?)`, smp.TS, smp.Queue, smp.Servers)
	return err
}

func (s *Store) ListCloudSamples(ctx context.Context, since int64) ([]CloudSample, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT ts,queue,servers FROM cloud_samples WHERE ts>=? ORDER BY ts`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CloudSample{}
	for rows.Next() {
		var c CloudSample
		rows.Scan(&c.TS, &c.Queue, &c.Servers)
		out = append(out, c)
	}
	return out, rows.Err()
}

// PurgeCloudHistory drops timeline samples and events older than `before`.
func (s *Store) PurgeCloudHistory(ctx context.Context, samplesBefore, eventsBefore int64) {
	s.db.ExecContext(ctx, `DELETE FROM cloud_samples WHERE ts<?`, samplesBefore)
	s.db.ExecContext(ctx, `DELETE FROM cloud_events WHERE ts<? AND kind<>'settings'`, eventsBefore)
}

// ── per-run usage ───────────────────────────────────────────────────────────

// CloudJobUsage is the busy time a run spent on one cloud worker.
type CloudJobUsage struct {
	WorkerID   string `json:"worker_id"`
	Jobs       int    `json:"jobs"`
	DurationMs int64  `json:"duration_ms"`
}

func (s *Store) RunCloudUsage(ctx context.Context, runID string) ([]CloudJobUsage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT worker_id,COUNT(*),COALESCE(SUM(duration_ms),0) FROM jobs
		WHERE run_id=? AND worker_id LIKE ? AND completed_at>0 GROUP BY worker_id`, runID, CloudWorkerPrefix+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CloudJobUsage{}
	for rows.Next() {
		var u CloudJobUsage
		rows.Scan(&u.WorkerID, &u.Jobs, &u.DurationMs)
		out = append(out, u)
	}
	return out, rows.Err()
}

// RecentRunTestTime returns the summed test time and the longest test of the
// largest finished run among the last `n` runs: the base for run-time estimates.
func (s *Store) RecentRunTestTime(ctx context.Context, n int) (tests int, sumMs, maxMs int64) {
	s.db.QueryRowContext(ctx, `SELECT COUNT(t.id),COALESCE(SUM(t.duration_ms),0),COALESCE(MAX(t.duration_ms),0)
		FROM tests t WHERE t.run_id=(SELECT r.id FROM (SELECT id,total FROM runs WHERE completed_at>0 AND cancelled=0
		ORDER BY started_at DESC LIMIT ?) r ORDER BY r.total DESC LIMIT 1) AND t.completed_at>0`, n).Scan(&tests, &sumMs, &maxMs)
	return
}
