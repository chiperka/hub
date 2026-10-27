package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
)

// Job statuses.
const (
	JobPending   = "pending"   // waiting in the queue
	JobClaimed   = "claimed"   // handed to a worker, not yet acknowledged
	JobRunning   = "running"   // acknowledged, worker is executing
	JobCompleted = "completed" // worker reported a result (see Job.Result)
	JobCancelled = "cancelled"
)

type Job struct {
	ID             int64           `json:"id"`
	RunID          string          `json:"run_id"`
	TestID         int64           `json:"test_id"`
	TestName       string          `json:"test_name"`
	ProjectSlug    string          `json:"project"`
	Status         string          `json:"status"`
	Result         string          `json:"result"`
	DAG            json.RawMessage `json:"dag,omitempty"`
	Weight         int             `json:"weight"`
	ContainerCount int             `json:"container_count"`
	WorkerID       string          `json:"worker_id"`
	Token          string          `json:"-"`
	RetryCount     int             `json:"retry_count"`
	StepResults    json.RawMessage `json:"step_results,omitempty"`
	ErrorMessage   string          `json:"error_message"`
	DurationMs     int64           `json:"duration_ms"`
	IdempotencyKey string          `json:"-"`
	CreatedAt      int64           `json:"created_at"`
	ClaimedAt      int64           `json:"claimed_at"`
	AckedAt        int64           `json:"acked_at"`
	LastProgressAt int64           `json:"last_progress_at"`
	CompletedAt    int64           `json:"completed_at"`
}

const jobCols = `j.id,j.run_id,j.test_id,COALESCE(t.name,''),COALESCE(p.slug,''),j.status,j.result,j.weight,j.container_count,
	j.worker_id,j.token,j.retry_count,j.error_message,j.duration_ms,j.idempotency_key,j.created_at,j.claimed_at,j.acked_at,
	j.last_progress_at,j.completed_at`

const jobFrom = ` FROM jobs j LEFT JOIN tests t ON t.id=j.test_id LEFT JOIN runs r ON r.id=j.run_id LEFT JOIN projects p ON p.id=r.project_id`

func scanJob(sc interface{ Scan(...any) error }, full bool) (*Job, error) {
	var j Job
	dest := []any{&j.ID, &j.RunID, &j.TestID, &j.TestName, &j.ProjectSlug, &j.Status, &j.Result, &j.Weight, &j.ContainerCount,
		&j.WorkerID, &j.Token, &j.RetryCount, &j.ErrorMessage, &j.DurationMs, &j.IdempotencyKey, &j.CreatedAt, &j.ClaimedAt,
		&j.AckedAt, &j.LastProgressAt, &j.CompletedAt}
	var dag, sr string
	if full {
		dest = append(dest, &dag, &sr)
	}
	if err := sc.Scan(dest...); err != nil {
		return nil, notFound(err)
	}
	if full {
		j.DAG = json.RawMessage(dag)
		if sr != "" {
			j.StepResults = json.RawMessage(sr)
		}
	}
	return &j, nil
}

func (s *Store) GetJob(ctx context.Context, id int64) (*Job, error) {
	return scanJob(s.db.QueryRowContext(ctx, `SELECT `+jobCols+`,j.dag,j.step_results`+jobFrom+` WHERE j.id=?`, id), true)
}

// ListTestJobs returns every attempt of a test, newest first.
func (s *Store) ListTestJobs(ctx context.Context, testID int64) ([]Job, error) {
	return s.queryJobs(ctx, ` WHERE j.test_id=? ORDER BY j.id DESC`, testID)
}

type JobFilter struct {
	Statuses []string
	WorkerID string
	Limit    int
}

func (s *Store) ListJobs(ctx context.Context, f JobFilter) ([]Job, error) {
	where := []string{"1=1"}
	var args []any
	if len(f.Statuses) > 0 {
		where = append(where, "j.status IN (?"+strings.Repeat(",?", len(f.Statuses)-1)+")")
		for _, st := range f.Statuses {
			args = append(args, st)
		}
	}
	if f.WorkerID != "" {
		where = append(where, "j.worker_id=?")
		args = append(args, f.WorkerID)
	}
	if f.Limit <= 0 {
		f.Limit = 200
	}
	order := " ORDER BY j.id DESC"
	if len(f.Statuses) > 0 && f.Statuses[0] == JobPending {
		order = " ORDER BY j.id"
	}
	return s.queryJobs(ctx, " WHERE "+strings.Join(where, " AND ")+order+" LIMIT ?", append(args, f.Limit)...)
}

func (s *Store) queryJobs(ctx context.Context, where string, args ...any) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+jobCols+jobFrom+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Job{}
	for rows.Next() {
		j, err := scanJob(rows, false)
		if err != nil {
			return nil, err
		}
		out = append(out, *j)
	}
	return out, rows.Err()
}

// ClaimJobs atomically assigns up to count pending jobs to the worker.
//
// Fair scheduling: jobs are interleaved across runs (the n-th job of every run
// before the n+1-th of any), so a huge run cannot starve a small one. When
// maxWeight > 0 only jobs fitting the remaining weight budget are taken; if that
// yields nothing and the worker is idle, a single overweight job is claimed so
// heavy tests still run (alone).
func (s *Store) ClaimJobs(ctx context.Context, workerID string, count, maxWeight int, tokenFn func() string) ([]Job, error) {
	var claimed []Job
	err := s.tx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT id,weight FROM (
				SELECT j.id,j.weight,j.run_id,ROW_NUMBER() OVER (PARTITION BY j.run_id ORDER BY j.id) rn
				FROM jobs j JOIN runs r ON r.id=j.run_id
				WHERE j.status='pending' AND r.snapshots_ready=1 AND r.cancelled=0
			) ORDER BY rn, id LIMIT 500`)
		if err != nil {
			return err
		}
		type cand struct {
			id     int64
			weight int
		}
		var cands []cand
		for rows.Next() {
			var c cand
			rows.Scan(&c.id, &c.weight)
			cands = append(cands, c)
		}
		rows.Close()
		if len(cands) == 0 {
			return nil
		}

		var picked []int64
		budget := maxWeight
		for _, c := range cands {
			if len(picked) >= count {
				break
			}
			if maxWeight > 0 {
				if c.weight > budget {
					continue
				}
				budget -= c.weight
			}
			picked = append(picked, c.id)
		}
		if len(picked) == 0 {
			var active int
			tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE worker_id=? AND status IN ('claimed','running')`, workerID).Scan(&active)
			if active > 0 {
				return nil
			}
			picked = []int64{cands[0].id}
		}

		now := Now()
		for _, id := range picked {
			if _, err := tx.ExecContext(ctx, `UPDATE jobs SET status=?,worker_id=?,token=?,claimed_at=? WHERE id=?`,
				JobClaimed, workerID, tokenFn(), now, id); err != nil {
				return err
			}
			j, err := scanJob(tx.QueryRowContext(ctx, `SELECT `+jobCols+`,j.dag,j.step_results`+jobFrom+` WHERE j.id=?`, id), true)
			if err != nil {
				return err
			}
			claimed = append(claimed, *j)
		}
		return nil
	})
	return claimed, err
}

// AckJobs moves claimed jobs to running and marks their tests as running.
func (s *Store) AckJobs(ctx context.Context, workerID string, ids []int64) ([]Job, error) {
	var acked []Job
	err := s.tx(ctx, func(tx *sql.Tx) error {
		now := Now()
		for _, id := range ids {
			res, err := tx.ExecContext(ctx, `UPDATE jobs SET status=?,acked_at=?,last_progress_at=? WHERE id=? AND worker_id=? AND status=?`,
				JobRunning, now, now, id, workerID, JobClaimed)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				continue
			}
			var j Job
			tx.QueryRowContext(ctx, `SELECT id,run_id,test_id FROM jobs WHERE id=?`, id).Scan(&j.ID, &j.RunID, &j.TestID)
			if _, err := tx.ExecContext(ctx, `UPDATE tests SET status=?,worker_id=?,started_at=?,error_message='' WHERE id=? AND job_id=?`,
				StatusRunning, workerID, now, j.TestID, id); err != nil {
				return err
			}
			acked = append(acked, j)
		}
		return nil
	})
	return acked, err
}

// RecordProgress upserts the live state of one DAG step.
func (s *Store) RecordProgress(ctx context.Context, jobID int64, step, status string, durationMs int64, errMsg string) error {
	now := Now()
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO step_progress(job_id,step,status,duration_ms,error,started_at,updated_at)
			VALUES(?,?,?,?,?,?,?) ON CONFLICT(job_id,step) DO UPDATE SET status=excluded.status,duration_ms=excluded.duration_ms,
			error=excluded.error,updated_at=excluded.updated_at`, jobID, step, status, durationMs, errMsg, now, now)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE jobs SET last_progress_at=? WHERE id=?`, now, jobID)
		return err
	})
}

// TouchJob bumps last_progress_at (logs/artifacts count as liveness).
func (s *Store) TouchJob(ctx context.Context, jobID int64) {
	s.db.ExecContext(ctx, `UPDATE jobs SET last_progress_at=? WHERE id=?`, Now(), jobID)
}

type StepProgress struct {
	Step       string `json:"step"`
	Status     string `json:"status"`
	DurationMs int64  `json:"duration_ms"`
	Error      string `json:"error"`
	StartedAt  int64  `json:"started_at"`
	UpdatedAt  int64  `json:"updated_at"`
}

func (s *Store) ListStepProgress(ctx context.Context, jobID int64) ([]StepProgress, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT step,status,duration_ms,error,started_at,updated_at FROM step_progress WHERE job_id=? ORDER BY started_at`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []StepProgress{}
	for rows.Next() {
		var p StepProgress
		rows.Scan(&p.Step, &p.Status, &p.DurationMs, &p.Error, &p.StartedAt, &p.UpdatedAt)
		out = append(out, p)
	}
	return out, rows.Err()
}

// CompleteJob stores the worker's final report. It returns false when the job
// was not in an active state (already completed, cancelled, or reassigned).
func (s *Store) CompleteJob(ctx context.Context, jobID int64, result, errMsg string, durationMs int64, stepResults []byte, idemKey string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE jobs SET status=?,result=?,error_message=?,duration_ms=?,step_results=?,
		idempotency_key=?,completed_at=? WHERE id=? AND status IN ('claimed','running')`,
		JobCompleted, result, errMsg, durationMs, string(stepResults), idemKey, Now(), jobID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// FailJob marks an active job as completed with result=error (monitor recovery).
func (s *Store) FailJob(ctx context.Context, jobID int64, errMsg string) (bool, error) {
	return s.CompleteJob(ctx, jobID, StatusError, errMsg, 0, nil, "")
}

// RetryJob clones a job into a new pending attempt and points the test at it.
func (s *Store) RetryJob(ctx context.Context, old *Job) (int64, error) {
	var newID int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO jobs(run_id,test_id,status,dag,weight,container_count,retry_count,created_at)
			SELECT run_id,test_id,?,dag,weight,container_count,retry_count+1,? FROM jobs WHERE id=?`, JobPending, Now(), old.ID)
		if err != nil {
			return err
		}
		newID, _ = res.LastInsertId()
		_, err = tx.ExecContext(ctx, `UPDATE tests SET job_id=?,status=?,worker_id='' WHERE id=?`, newID, StatusPending, old.TestID)
		return err
	})
	return newID, err
}

// ResetStaleClaims returns jobs that were claimed but never acknowledged before `before` to the queue.
func (s *Store) ResetStaleClaims(ctx context.Context, before int64) (int64, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE jobs SET status=?,worker_id='',token='',claimed_at=0 WHERE status=? AND claimed_at<?`,
		JobPending, JobClaimed, before)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// StaleRunningJobs returns running jobs with no sign of life since `before`.
func (s *Store) StaleRunningJobs(ctx context.Context, before int64) ([]Job, error) {
	return s.queryJobs(ctx, ` WHERE j.status=? AND j.last_progress_at<?`, JobRunning, before)
}

func (s *Store) WorkerActiveJobs(ctx context.Context, workerID string) ([]Job, error) {
	return s.queryJobs(ctx, ` WHERE j.worker_id=? AND j.status IN ('claimed','running')`, workerID)
}

// CancelRun marks the run cancelled, cancels queued jobs and unfinished tests.
// It returns the jobs that were already on a worker so the caller can send
// "cancel:<id>" commands.
func (s *Store) CancelRun(ctx context.Context, runID string) ([]Job, error) {
	active, err := s.queryJobs(ctx, ` WHERE j.run_id=? AND j.status IN ('claimed','running')`, runID)
	if err != nil {
		return nil, err
	}
	err = s.tx(ctx, func(tx *sql.Tx) error {
		now := Now()
		stmts := []struct {
			q    string
			args []any
		}{
			{`UPDATE runs SET cancelled=1 WHERE id=?`, []any{runID}},
			{`UPDATE jobs SET status=?,completed_at=? WHERE run_id=? AND status IN ('pending','claimed','running')`, []any{JobCancelled, now, runID}},
			{`UPDATE tests SET status=?,completed_at=? WHERE run_id=? AND status IN ('pending','running')`, []any{StatusCancelled, now, runID}},
		}
		for _, st := range stmts {
			if _, err := tx.ExecContext(ctx, st.q, st.args...); err != nil {
				return err
			}
		}
		return nil
	})
	return active, err
}

// PendingJobCount is used to decide whether to wake waiting workers.
func (s *Store) PendingJobCount(ctx context.Context) int {
	var n int
	s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs j JOIN runs r ON r.id=j.run_id WHERE j.status='pending' AND r.snapshots_ready=1`).Scan(&n)
	return n
}
