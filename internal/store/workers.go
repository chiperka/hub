package store

import (
	"context"
	"database/sql"
	"encoding/json"
)

// Worker statuses.
const (
	WorkerWaiting = "waiting" // provisioned in the UI, never connected yet
	WorkerOnline  = "online"
	WorkerOffline = "offline"
)

type Worker struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	TokenID       int64  `json:"-"`
	IsLocal       bool   `json:"is_local"`
	Status        string `json:"status"`
	Paused        bool   `json:"paused"`
	Hostname      string `json:"hostname"`
	Version       string `json:"version"`
	Concurrency   int    `json:"concurrency"`
	Capacity      int    `json:"capacity"`
	MaxContainers int    `json:"max_containers"`
	ActiveTests   int    `json:"active_tests"`
	CurrentRunID  string `json:"current_run_id"`
	CreatedAt     int64  `json:"created_at"`
	RegisteredAt  int64  `json:"registered_at"`
	LastHeartbeat int64  `json:"last_heartbeat"`

	// Derived from jobs.
	RunningJobs   int `json:"running_jobs"`
	UsedWeight    int `json:"used_weight"`
	CompletedJobs int `json:"completed_jobs"`
}

const workerCols = `w.id,w.name,COALESCE(w.token_id,0),w.is_local,w.status,w.paused,w.hostname,w.version,w.concurrency,w.capacity,
	w.max_containers,w.active_tests,w.current_run_id,w.created_at,w.registered_at,w.last_heartbeat,
	(SELECT COUNT(*) FROM jobs j WHERE j.worker_id=w.id AND j.status IN ('claimed','running')),
	COALESCE((SELECT SUM(weight) FROM jobs j WHERE j.worker_id=w.id AND j.status IN ('claimed','running')),0),
	(SELECT COUNT(*) FROM jobs j WHERE j.worker_id=w.id AND j.status='completed')`

func scanWorker(sc interface{ Scan(...any) error }) (*Worker, error) {
	var w Worker
	err := sc.Scan(&w.ID, &w.Name, &w.TokenID, &w.IsLocal, &w.Status, &w.Paused, &w.Hostname, &w.Version, &w.Concurrency,
		&w.Capacity, &w.MaxContainers, &w.ActiveTests, &w.CurrentRunID, &w.CreatedAt, &w.RegisteredAt, &w.LastHeartbeat,
		&w.RunningJobs, &w.UsedWeight, &w.CompletedJobs)
	if err != nil {
		return nil, notFound(err)
	}
	return &w, nil
}

func (s *Store) ListWorkers(ctx context.Context) ([]Worker, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+workerCols+` FROM workers w ORDER BY w.is_local DESC, w.created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Worker{}
	for rows.Next() {
		w, err := scanWorker(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *w)
	}
	return out, rows.Err()
}

func (s *Store) GetWorker(ctx context.Context, id string) (*Worker, error) {
	return scanWorker(s.db.QueryRowContext(ctx, `SELECT `+workerCols+` FROM workers w WHERE w.id=?`, id))
}

func (s *Store) GetWorkerByToken(ctx context.Context, tokenID int64) (*Worker, error) {
	return scanWorker(s.db.QueryRowContext(ctx, `SELECT `+workerCols+` FROM workers w WHERE w.token_id=?`, tokenID))
}

// JoinedWorkerPrefix marks the IDs of workers that registered themselves with
// the join token instead of being provisioned in the UI.
const JoinedWorkerPrefix = "wj-"

// GetJoinedWorker finds a self-registered worker by name.
func (s *Store) GetJoinedWorker(ctx context.Context, name string) (*Worker, error) {
	return scanWorker(s.db.QueryRowContext(ctx, `SELECT `+workerCols+` FROM workers w WHERE w.id LIKE ? AND w.name=?
		ORDER BY w.created_at LIMIT 1`, JoinedWorkerPrefix+"%", name))
}

// DeleteStaleJoinedWorkers removes self-registered workers offline since before
// `before`. They rejoin on their next start, so nothing is lost.
func (s *Store) DeleteStaleJoinedWorkers(ctx context.Context, before int64) (int64, error) {
	var n int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		cond := `id LIKE ? AND status=? AND last_heartbeat<?`
		args := []any{JoinedWorkerPrefix + "%", WorkerOffline, before}
		tx.ExecContext(ctx, `DELETE FROM tokens WHERE id IN (SELECT token_id FROM workers WHERE `+cond+`)`, args...)
		res, err := tx.ExecContext(ctx, `DELETE FROM workers WHERE `+cond, args...)
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	return n, err
}

// CreateWorker provisions a worker in "waiting" state bound to tokenID.
func (s *Store) CreateWorker(ctx context.Context, id, name string, tokenID int64, isLocal bool) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO workers(id,name,token_id,is_local,status,created_at) VALUES(?,?,?,?,?,?)`,
		id, name, tokenID, isLocal, WorkerWaiting, Now())
	return err
}

// SetWorkerToken rebinds a worker to a new token (old token is deleted).
func (s *Store) SetWorkerToken(ctx context.Context, id string, tokenID int64) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		tx.ExecContext(ctx, `DELETE FROM tokens WHERE id=(SELECT token_id FROM workers WHERE id=?)`, id)
		_, err := tx.ExecContext(ctx, `UPDATE workers SET token_id=? WHERE id=?`, tokenID, id)
		return err
	})
}

func (s *Store) ActivateWorker(ctx context.Context, id, hostname, version string, concurrency, capacity, maxContainers int) error {
	now := Now()
	_, err := s.db.ExecContext(ctx, `UPDATE workers SET status=?,hostname=?,version=?,concurrency=?,capacity=?,max_containers=?,
		registered_at=?,last_heartbeat=?,active_tests=0 WHERE id=?`,
		WorkerOnline, hostname, version, concurrency, capacity, maxContainers, now, now, id)
	return err
}

// Heartbeat records liveness and returns (and clears) pending commands.
// The second return value reports whether the worker came back from offline.
func (s *Store) Heartbeat(ctx context.Context, id string, activeTests int, currentRunID string) ([]string, bool, error) {
	var cmds []string
	revived := false
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var raw, status string
		if err := tx.QueryRowContext(ctx, `SELECT commands,status FROM workers WHERE id=?`, id).Scan(&raw, &status); err != nil {
			return notFound(err)
		}
		revived = status != WorkerOnline
		json.Unmarshal([]byte(raw), &cmds)
		_, err := tx.ExecContext(ctx, `UPDATE workers SET last_heartbeat=?,active_tests=?,current_run_id=?,status=?,commands='[]' WHERE id=?`,
			Now(), activeTests, currentRunID, WorkerOnline, id)
		return err
	})
	return cmds, revived, err
}

// QueueWorkerCommand appends a command (e.g. "cancel:42") delivered on the next heartbeat.
func (s *Store) QueueWorkerCommand(ctx context.Context, id, cmd string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE workers SET commands=json_insert(commands,'$[#]',?) WHERE id=?`, cmd, id)
	return err
}

func (s *Store) SetWorkerPaused(ctx context.Context, id string, paused bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE workers SET paused=? WHERE id=?`, paused, id)
	return err
}

func (s *Store) RenameWorker(ctx context.Context, id, name string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE workers SET name=? WHERE id=?`, name, id)
	return err
}

func (s *Store) IsWorkerPaused(ctx context.Context, id string) bool {
	var p bool
	s.db.QueryRowContext(ctx, `SELECT paused FROM workers WHERE id=?`, id).Scan(&p)
	return p
}

// DeleteWorker removes the worker and its token.
func (s *Store) DeleteWorker(ctx context.Context, id string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		tx.ExecContext(ctx, `DELETE FROM tokens WHERE id=(SELECT token_id FROM workers WHERE id=?)`, id)
		_, err := tx.ExecContext(ctx, `DELETE FROM workers WHERE id=?`, id)
		return err
	})
}

// MarkStaleWorkersOffline flips online workers without a heartbeat since `before` to offline.
func (s *Store) MarkStaleWorkersOffline(ctx context.Context, before int64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `UPDATE workers SET status=?,active_tests=0 WHERE status=? AND last_heartbeat<? RETURNING id`,
		WorkerOffline, WorkerOnline, before)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
