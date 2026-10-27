package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
)

// Test statuses. Terminal: passed, failed, error, skipped, cancelled.
const (
	StatusPending   = "pending"
	StatusRunning   = "running"
	StatusPassed    = "passed"
	StatusFailed    = "failed"
	StatusError     = "error"
	StatusSkipped   = "skipped"
	StatusCancelled = "cancelled"
)

type Run struct {
	ID             string `json:"id"`
	ProjectID      *int64 `json:"project_id"`
	ProjectSlug    string `json:"project"`
	Status         string `json:"status"`
	Source         string `json:"source"`
	Version        string `json:"version"`
	Total          int    `json:"total"`
	Passed         int    `json:"passed"`
	Failed         int    `json:"failed"`
	Errored        int    `json:"errored"`
	Skipped        int    `json:"skipped"`
	Cancelled      int    `json:"cancelled"`
	Pending        int    `json:"pending"`
	Running        int    `json:"running"`
	SnapshotsReady bool   `json:"snapshots_ready"`
	IsCancelled    bool   `json:"is_cancelled"`
	CreatedBy      string `json:"created_by"`
	RerunOf        string `json:"rerun_of"`
	StartedAt      int64  `json:"started_at"`
	CompletedAt    int64  `json:"completed_at"`
}

// Finished reports whether the run reached a terminal state.
func (r *Run) Finished() bool { return r.CompletedAt > 0 }

type Suite struct {
	ID       int64  `json:"id"`
	RunID    string `json:"run_id"`
	Name     string `json:"name"`
	FilePath string `json:"file_path"`
}

type Test struct {
	ID           int64           `json:"id"`
	UUID         string          `json:"uuid"`
	RunID        string          `json:"run_id"`
	SuiteID      int64           `json:"suite_id"`
	SuiteName    string          `json:"suite"`
	SuiteFile    string          `json:"suite_file"`
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	Tags         []string        `json:"tags"`
	Endpoint     string          `json:"endpoint"`
	Status       string          `json:"status"`
	ErrorMessage string          `json:"error_message"`
	DurationMs   int64           `json:"duration_ms"`
	JobID        int64           `json:"job_id"`
	WorkerID     string          `json:"worker_id"`
	StartedAt    int64           `json:"started_at"`
	CompletedAt  int64           `json:"completed_at"`
	Definition   json.RawMessage `json:"definition,omitempty"`
}

// NewTest describes a test to insert. Compile is called with the new test/suite IDs
// and must return the job DAG (nil for skipped tests) plus its scheduling weight.
type NewTest struct {
	UUID        string
	Name        string
	Description string
	Tags        []string
	Endpoint    string
	Skipped     bool
	Definition  []byte
	Compile     func(testID, suiteID int64) (dag []byte, weight, containers int)
}

type NewSuite struct {
	Name     string
	FilePath string
	Tests    []NewTest
}

const runCols = `r.id,r.project_id,COALESCE(p.slug,''),r.status,r.source,r.version,r.total,r.passed,r.failed,r.errored,r.skipped,
	r.cancelled_tests,r.pending,r.running,r.snapshots_ready,r.cancelled,r.created_by,r.rerun_of,r.started_at,r.completed_at`

const runFrom = ` FROM runs r LEFT JOIN projects p ON p.id=r.project_id`

func scanRun(sc interface{ Scan(...any) error }) (*Run, error) {
	var r Run
	var pid sql.NullInt64
	err := sc.Scan(&r.ID, &pid, &r.ProjectSlug, &r.Status, &r.Source, &r.Version, &r.Total, &r.Passed, &r.Failed, &r.Errored,
		&r.Skipped, &r.Cancelled, &r.Pending, &r.Running, &r.SnapshotsReady, &r.IsCancelled, &r.CreatedBy, &r.RerunOf,
		&r.StartedAt, &r.CompletedAt)
	if err != nil {
		return nil, notFound(err)
	}
	if pid.Valid {
		r.ProjectID = &pid.Int64
	}
	return &r, nil
}

// CreateRun inserts the run, its suites, tests and pending jobs in one transaction.
func (s *Store) CreateRun(ctx context.Context, run *Run, suites []NewSuite) error {
	err := s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO runs(id,project_id,status,source,version,snapshots_ready,created_by,rerun_of,started_at)
			VALUES(?,?,?,?,?,?,?,?,?)`, run.ID, run.ProjectID, StatusRunning, run.Source, run.Version, run.SnapshotsReady,
			run.CreatedBy, run.RerunOf, run.StartedAt)
		if err != nil {
			return err
		}
		for _, su := range suites {
			res, err := tx.ExecContext(ctx, `INSERT INTO suites(run_id,name,file_path) VALUES(?,?,?)`, run.ID, su.Name, su.FilePath)
			if err != nil {
				return err
			}
			suiteID, _ := res.LastInsertId()
			for _, t := range su.Tests {
				status := StatusPending
				if t.Skipped {
					status = StatusSkipped
				}
				res, err := tx.ExecContext(ctx, `INSERT INTO tests(uuid,run_id,suite_id,name,description,tags,endpoint,status,definition)
					VALUES(?,?,?,?,?,?,?,?,?)`, t.UUID, run.ID, suiteID, t.Name, t.Description, strings.Join(t.Tags, ","),
					t.Endpoint, status, string(t.Definition))
				if err != nil {
					return err
				}
				testID, _ := res.LastInsertId()
				if t.Skipped || t.Compile == nil {
					continue
				}
				dag, weight, containers := t.Compile(testID, suiteID)
				res, err = tx.ExecContext(ctx, `INSERT INTO jobs(run_id,test_id,status,dag,weight,container_count,created_at)
					VALUES(?,?,?,?,?,?,?)`, run.ID, testID, StatusPending, string(dag), weight, containers, Now())
				if err != nil {
					return err
				}
				jobID, _ := res.LastInsertId()
				if _, err := tx.ExecContext(ctx, `UPDATE tests SET job_id=? WHERE id=?`, jobID, testID); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	_, _, err = s.RefreshRun(ctx, run.ID)
	return err
}

// RefreshRun recomputes the run counters from its tests. When nothing is left
// pending/running it finalises the run; justFinished is true only on that transition.
func (s *Store) RefreshRun(ctx context.Context, runID string) (*Run, bool, error) {
	justFinished := false
	err := s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE runs SET
			total=(SELECT COUNT(*) FROM tests WHERE run_id=runs.id),
			passed=(SELECT COUNT(*) FROM tests WHERE run_id=runs.id AND status='passed'),
			failed=(SELECT COUNT(*) FROM tests WHERE run_id=runs.id AND status='failed'),
			errored=(SELECT COUNT(*) FROM tests WHERE run_id=runs.id AND status='error'),
			skipped=(SELECT COUNT(*) FROM tests WHERE run_id=runs.id AND status='skipped'),
			cancelled_tests=(SELECT COUNT(*) FROM tests WHERE run_id=runs.id AND status='cancelled'),
			pending=(SELECT COUNT(*) FROM tests WHERE run_id=runs.id AND status='pending'),
			running=(SELECT COUNT(*) FROM tests WHERE run_id=runs.id AND status='running')
			WHERE id=?`, runID)
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `UPDATE runs SET
			status=CASE WHEN cancelled=1 THEN 'cancelled' WHEN failed+errored>0 THEN 'failed' ELSE 'passed' END,
			completed_at=?
			WHERE id=? AND completed_at=0 AND pending=0 AND running=0`, Now(), runID)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		justFinished = n > 0
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	run, err := s.GetRun(ctx, runID)
	return run, justFinished, err
}

func (s *Store) GetRun(ctx context.Context, id string) (*Run, error) {
	return scanRun(s.db.QueryRowContext(ctx, `SELECT `+runCols+runFrom+` WHERE r.id=?`, id))
}

type RunFilter struct {
	ProjectID int64
	Status    string
	Query     string
	Limit     int
	Offset    int
}

func (s *Store) ListRuns(ctx context.Context, f RunFilter) ([]Run, int, error) {
	where := []string{"1=1"}
	var args []any
	if f.ProjectID > 0 {
		where = append(where, "r.project_id=?")
		args = append(args, f.ProjectID)
	}
	switch f.Status {
	case "":
	case "active":
		where = append(where, "r.completed_at=0")
	default:
		where = append(where, "r.status=?")
		args = append(args, f.Status)
	}
	if f.Query != "" {
		where = append(where, "(r.id LIKE ? OR p.slug LIKE ? OR EXISTS (SELECT 1 FROM tests t WHERE t.run_id=r.id AND t.name LIKE ?))")
		q := "%" + f.Query + "%"
		args = append(args, q, q, q)
	}
	w := " WHERE " + strings.Join(where, " AND ")
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*)`+runFrom+w, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if f.Limit <= 0 {
		f.Limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+runCols+runFrom+w+` ORDER BY r.started_at DESC LIMIT ? OFFSET ?`,
		append(args, f.Limit, f.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []Run{}
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *r)
	}
	return out, total, rows.Err()
}

func (s *Store) DeleteRun(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM runs WHERE id=?`, id)
	return err
}

// RunIDsOlderThan returns finished runs started before ts (for retention cleanup).
func (s *Store) RunIDsOlderThan(ctx context.Context, ts int64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM runs WHERE started_at<? AND completed_at>0`, ts)
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

func (s *Store) ListSuites(ctx context.Context, runID string) ([]Suite, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,run_id,name,file_path FROM suites WHERE run_id=? ORDER BY id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Suite{}
	for rows.Next() {
		var su Suite
		if err := rows.Scan(&su.ID, &su.RunID, &su.Name, &su.FilePath); err != nil {
			return nil, err
		}
		out = append(out, su)
	}
	return out, rows.Err()
}

// ── tests ───────────────────────────────────────────────────────────────────

const testCols = `t.id,t.uuid,t.run_id,t.suite_id,s.name,s.file_path,t.name,t.description,t.tags,t.endpoint,t.status,t.error_message,
	t.duration_ms,t.job_id,t.worker_id,t.started_at,t.completed_at`

const testFrom = ` FROM tests t JOIN suites s ON s.id=t.suite_id`

func scanTest(sc interface{ Scan(...any) error }, withDef bool) (*Test, error) {
	var t Test
	var tags, def string
	dest := []any{&t.ID, &t.UUID, &t.RunID, &t.SuiteID, &t.SuiteName, &t.SuiteFile, &t.Name, &t.Description, &tags, &t.Endpoint,
		&t.Status, &t.ErrorMessage, &t.DurationMs, &t.JobID, &t.WorkerID, &t.StartedAt, &t.CompletedAt}
	if withDef {
		dest = append(dest, &def)
	}
	if err := sc.Scan(dest...); err != nil {
		return nil, notFound(err)
	}
	t.Tags = []string{}
	if tags != "" {
		t.Tags = strings.Split(tags, ",")
	}
	if withDef {
		t.Definition = json.RawMessage(def)
	}
	return &t, nil
}

func (s *Store) ListRunTests(ctx context.Context, runID string) ([]Test, error) {
	return s.queryTests(ctx, false, ` WHERE t.run_id=? ORDER BY t.id`, runID)
}

// ListRunTestsWithDefinitions is used by reruns and reports.
func (s *Store) ListRunTestsWithDefinitions(ctx context.Context, runID string) ([]Test, error) {
	return s.queryTests(ctx, true, ` WHERE t.run_id=? ORDER BY t.id`, runID)
}

func (s *Store) queryTests(ctx context.Context, withDef bool, where string, args ...any) ([]Test, error) {
	cols := testCols
	if withDef {
		cols += ",t.definition"
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+cols+testFrom+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Test{}
	for rows.Next() {
		t, err := scanTest(rows, withDef)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

func (s *Store) GetTest(ctx context.Context, id int64) (*Test, error) {
	return scanTest(s.db.QueryRowContext(ctx, `SELECT `+testCols+`,t.definition`+testFrom+` WHERE t.id=?`, id), true)
}

func (s *Store) GetTestByUUID(ctx context.Context, uuid string) (*Test, error) {
	return scanTest(s.db.QueryRowContext(ctx, `SELECT `+testCols+`,t.definition`+testFrom+` WHERE t.uuid=?`, uuid), true)
}

// FinishTest stores the terminal result of a test.
func (s *Store) FinishTest(ctx context.Context, testID int64, status, errMsg string, durationMs int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE tests SET status=?,error_message=?,duration_ms=?,completed_at=? WHERE id=?`,
		status, errMsg, durationMs, Now(), testID)
	return err
}

// ── snapshots & artifacts ───────────────────────────────────────────────────

func (s *Store) AddRunSnapshot(ctx context.Context, runID, path string, size int64) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO run_snapshots(run_id,path,size) VALUES(?,?,?)
		ON CONFLICT(run_id,path) DO UPDATE SET size=excluded.size`, runID, path, size)
	return err
}

type Snapshot struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

func (s *Store) ListRunSnapshots(ctx context.Context, runID string) ([]Snapshot, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT path,size FROM run_snapshots WHERE run_id=? ORDER BY path`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Snapshot{}
	for rows.Next() {
		var sn Snapshot
		rows.Scan(&sn.Path, &sn.Size)
		out = append(out, sn)
	}
	return out, rows.Err()
}

func (s *Store) SetSnapshotsReady(ctx context.Context, runID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE runs SET snapshots_ready=1 WHERE id=?`, runID)
	return err
}

type Artifact struct {
	TestID    int64  `json:"test_id"`
	TestUUID  string `json:"test_uuid,omitempty"`
	JobID     int64  `json:"job_id"`
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	CreatedAt int64  `json:"created_at"`
}

func (s *Store) UpsertArtifact(ctx context.Context, testID, jobID int64, name string, size int64) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO artifacts(test_id,job_id,name,size,created_at) VALUES(?,?,?,?,?)
		ON CONFLICT(test_id,name) DO UPDATE SET job_id=excluded.job_id,size=excluded.size,created_at=excluded.created_at`,
		testID, jobID, name, size, Now())
	return err
}

func (s *Store) ListArtifacts(ctx context.Context, testID int64) ([]Artifact, error) {
	return s.queryArtifacts(ctx, `WHERE a.test_id=?`, testID)
}

func (s *Store) ListRunArtifacts(ctx context.Context, runID string) ([]Artifact, error) {
	return s.queryArtifacts(ctx, `WHERE t.run_id=?`, runID)
}

func (s *Store) queryArtifacts(ctx context.Context, where string, arg any) ([]Artifact, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT a.test_id,t.uuid,a.job_id,a.name,a.size,a.created_at FROM artifacts a
		JOIN tests t ON t.id=a.test_id `+where+` ORDER BY a.test_id,a.name`, arg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Artifact{}
	for rows.Next() {
		var a Artifact
		if err := rows.Scan(&a.TestID, &a.TestUUID, &a.JobID, &a.Name, &a.Size, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// DeleteArtifacts removes artifact rows of a test (used when a retry starts afresh).
func (s *Store) DeleteArtifacts(ctx context.Context, testID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM artifacts WHERE test_id=?`, testID)
	return err
}

// ── overview ────────────────────────────────────────────────────────────────

type Stats struct {
	Runs24h        int     `json:"runs_24h"`
	Tests24h       int     `json:"tests_24h"`
	PassRate24h    float64 `json:"pass_rate_24h"`
	ActiveRuns     int     `json:"active_runs"`
	QueuePending   int     `json:"queue_pending"`
	QueueRunning   int     `json:"queue_running"`
	WorkersOnline  int     `json:"workers_online"`
	WorkersTotal   int     `json:"workers_total"`
	CapacityTotal  int     `json:"capacity_total"`
	CapacityUsed   int     `json:"capacity_used"`
	AvgTestMs24h   int64   `json:"avg_test_ms_24h"`
	FailedTests24h int     `json:"failed_tests_24h"`
}

func (s *Store) GetStats(ctx context.Context) (*Stats, error) {
	var st Stats
	since := Now() - 24*3600*1000
	var passed, finished int
	err := s.db.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM runs WHERE started_at>?),
		(SELECT COUNT(*) FROM runs WHERE completed_at=0),
		(SELECT COUNT(*) FROM jobs WHERE status='pending'),
		(SELECT COUNT(*) FROM jobs WHERE status IN ('claimed','running')),
		(SELECT COUNT(*) FROM workers WHERE status='online'),
		(SELECT COUNT(*) FROM workers),
		(SELECT COALESCE(SUM(capacity),0) FROM workers WHERE status='online' AND paused=0),
		(SELECT COALESCE(SUM(weight),0) FROM jobs WHERE status IN ('claimed','running'))`, since).
		Scan(&st.Runs24h, &st.ActiveRuns, &st.QueuePending, &st.QueueRunning, &st.WorkersOnline, &st.WorkersTotal,
			&st.CapacityTotal, &st.CapacityUsed)
	if err != nil {
		return nil, err
	}
	s.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(t.status='passed'),0), COALESCE(SUM(t.status IN ('failed','error')),0),
		COALESCE(CAST(AVG(CASE WHEN t.duration_ms>0 THEN t.duration_ms END) AS INTEGER),0)
		FROM tests t JOIN runs r ON r.id=t.run_id
		WHERE r.started_at>? AND t.status IN ('passed','failed','error')`, since).
		Scan(&finished, &passed, &st.FailedTests24h, &st.AvgTestMs24h)
	st.Tests24h = finished
	if finished > 0 {
		st.PassRate24h = float64(passed) / float64(finished)
	}
	return &st, nil
}

// DailyStat is one day bucket of the activity chart.
type DailyStat struct {
	Day    string `json:"day"`
	Passed int    `json:"passed"`
	Failed int    `json:"failed"`
}

func (s *Store) DailyStats(ctx context.Context, days int) ([]DailyStat, error) {
	since := Now() - int64(days)*24*3600*1000
	rows, err := s.db.QueryContext(ctx, `SELECT date(r.started_at/1000,'unixepoch','localtime') d,
		SUM(t.status='passed'), SUM(t.status IN ('failed','error'))
		FROM tests t JOIN runs r ON r.id=t.run_id WHERE r.started_at>? GROUP BY d ORDER BY d`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DailyStat{}
	for rows.Next() {
		var d DailyStat
		rows.Scan(&d.Day, &d.Passed, &d.Failed)
		out = append(out, d)
	}
	return out, rows.Err()
}
