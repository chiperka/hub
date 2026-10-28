package server

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"chiperka-hub/internal/compile"
	"chiperka-hub/internal/model"
	"chiperka-hub/internal/store"
)

type suiteInput struct {
	Name     string       `json:"name"`
	FilePath string       `json:"file_path"`
	Tests    []model.Test `json:"tests"`
}

type runInput struct {
	ProjectID      *int64
	Version        string
	Source         string
	CreatedBy      string
	RerunOf        string
	Suites         []suiteInput
	Normalize      bool // rewrite suite-relative snapshot paths (fresh CLI submissions only)
	SnapshotsReady bool
	RegistryAuth   []compile.RegistryAuth
}

func newRunID() string {
	b := make([]byte, 2)
	rand.Read(b)
	return time.Now().Format("20060102-150405") + "-" + hex.EncodeToString(b)
}

func newTestUUID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return "ct-" + hex.EncodeToString(b)
}

// createRun persists a run with compiled jobs and wakes the workers.
func (s *Server) createRun(ctx context.Context, in runInput) (*store.Run, error) {
	run := &store.Run{
		ID: newRunID(), ProjectID: in.ProjectID, Source: in.Source, Version: in.Version, CreatedBy: in.CreatedBy,
		RerunOf: in.RerunOf, StartedAt: store.Now(), SnapshotsReady: in.SnapshotsReady,
	}
	var projectID int64
	if in.ProjectID != nil {
		projectID = *in.ProjectID
	}
	hasSnapshots := false
	suites := make([]store.NewSuite, 0, len(in.Suites))
	for _, su := range in.Suites {
		ns := store.NewSuite{Name: su.Name, FilePath: su.FilePath}
		for _, t := range su.Tests {
			t := t
			if in.Normalize {
				compile.NormalizeSnapshotPaths(&t, su.FilePath)
			}
			if len(compile.SnapshotPaths(t)) > 0 {
				hasSnapshots = true
			}
			def, err := json.Marshal(t)
			if err != nil {
				return nil, fmt.Errorf("test %q: %w", t.Name, err)
			}
			uuid := newTestUUID()
			suiteName, suiteFile := su.Name, su.FilePath
			ns.Tests = append(ns.Tests, store.NewTest{
				UUID: uuid, Name: t.Name, Description: t.Description, Tags: t.Tags, Endpoint: t.Endpoint,
				Skipped: t.Skipped, Definition: def,
				Compile: func(testID, suiteID int64) ([]byte, int, int) {
					job := compile.Test(t, &compile.Meta{
						Type: "test", TestID: testID, RunID: run.ID, SuiteID: suiteID, UUID: uuid, Name: t.Name,
						SuiteName: suiteName, SuiteFilePath: suiteFile, ProjectID: projectID,
					}, fmt.Sprintf("%ds", int(s.cfg.JobTimeout.Seconds())))
					dag, _ := json.Marshal(job)
					return dag, job.Weight, job.ContainerCount
				},
			})
		}
		suites = append(suites, ns)
	}
	if !in.SnapshotsReady && !hasSnapshots {
		run.SnapshotsReady = true
	}
	// Registered before the run is stored so no job can be claimed without them.
	s.registry.Set(run.ID, in.RegistryAuth)
	if err := s.store.CreateRun(ctx, run, suites); err != nil {
		s.registry.Delete(run.ID)
		return nil, err
	}
	created, finished, err := s.store.RefreshRun(ctx, run.ID)
	if err != nil {
		return nil, err
	}
	s.log.Info("run created", "run", run.ID, "tests", created.Total, "source", in.Source, "snapshots_pending", !run.SnapshotsReady)
	if run.SnapshotsReady {
		s.notifier.Notify()
	}
	s.changed("runs")
	s.changed("queue")
	if finished {
		s.publishRunEnd(created)
	}
	return created, nil
}

// POST /api/runs — CLI `chiperka test --cloud`.
func (s *Server) handleRunCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Suites []suiteInput `json:"suites"`
		Config *struct {
			Version string `json:"version"`
		} `json:"config"`
		ProjectID    *int64                 `json:"project_id"`
		RegistryAuth []compile.RegistryAuth `json:"registry_auth"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	total := 0
	for _, su := range req.Suites {
		total += len(su.Tests)
	}
	if total == 0 {
		writeError(w, http.StatusBadRequest, "no tests found in suites")
		return
	}
	if req.ProjectID != nil {
		if _, err := s.store.GetProject(r.Context(), *req.ProjectID); err != nil {
			writeError(w, http.StatusBadRequest, "unknown project_id")
			return
		}
	}
	in := runInput{ProjectID: req.ProjectID, Source: "cli", CreatedBy: principal(r).Name, Suites: req.Suites, Normalize: true, RegistryAuth: req.RegistryAuth}
	if req.Config != nil {
		in.Version = req.Config.Version
	}
	run, err := s.createRun(r.Context(), in)
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": run.ID})
}

// POST /api/runs/{id}/snapshots — zip with project-root relative snapshot files.
func (s *Server) handleSnapshotsUpload(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	if _, err := s.store.GetRun(r.Context(), runID); err != nil {
		storeError(w, err)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 512<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read body: "+err.Error())
		return
	}
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid zip: "+err.Error())
		return
	}
	dir := s.snapshotDir(runID)
	count := 0
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		clean, ok := safeRelPath(f.Name)
		if !ok {
			continue
		}
		n, err := extractZipFile(f, filepath.Join(dir, filepath.FromSlash(clean)))
		if err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("store %s: %v", clean, err))
			return
		}
		s.store.AddRunSnapshot(r.Context(), runID, clean, n)
		count++
	}
	if err := s.store.SetSnapshotsReady(r.Context(), runID); err != nil {
		storeError(w, err)
		return
	}
	s.notifier.Notify()
	s.changed("queue")
	writeJSON(w, http.StatusCreated, map[string]any{"count": count})
}

func extractZipFile(f *zip.File, dst string) (int64, error) {
	rc, err := f.Open()
	if err != nil {
		return 0, err
	}
	defer rc.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return 0, err
	}
	out, err := os.Create(dst)
	if err != nil {
		return 0, err
	}
	defer out.Close()
	return io.Copy(out, io.LimitReader(rc, 256<<20))
}

func (s *Server) handleSnapshotGet(w http.ResponseWriter, r *http.Request) {
	clean, ok := safeRelPath(r.PathValue("path"))
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid path")
		return
	}
	p := filepath.Join(s.snapshotDir(r.PathValue("id")), filepath.FromSlash(clean))
	if _, err := os.Stat(p); err != nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	http.ServeFile(w, r, p)
}

// POST /api/runs/{id}/stop
func (s *Server) handleRunStop(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	run, err := s.store.GetRun(r.Context(), runID)
	if err != nil {
		storeError(w, err)
		return
	}
	if run.Finished() {
		writeJSON(w, http.StatusOK, map[string]string{"status": run.Status})
		return
	}
	active, err := s.store.CancelRun(r.Context(), runID)
	if err != nil {
		storeError(w, err)
		return
	}
	s.cancelJobsOnWorkers(r.Context(), active)
	s.log.Info("run stopped", "run", runID, "cancelled_on_workers", len(active), "by", principal(r).Name)
	s.refreshRun(runID)
	s.changed("queue")
	writeJSON(w, http.StatusOK, map[string]string{"status": "cancelled"})
}

// POST /api/runs/{id}/rerun  {"failed_only": bool, "test_ids": [..]}
func (s *Server) handleRunRerun(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FailedOnly bool    `json:"failed_only"`
		TestIDs    []int64 `json:"test_ids"`
	}
	if r.ContentLength != 0 {
		decodeJSON(r, &req)
	}
	s.rerun(w, r, r.PathValue("id"), req.FailedOnly, req.TestIDs)
}

func (s *Server) handleTestRerun(w http.ResponseWriter, r *http.Request) {
	t, err := s.store.GetTest(r.Context(), pathInt(r, "id"))
	if err != nil {
		storeError(w, err)
		return
	}
	s.rerun(w, r, t.RunID, false, []int64{t.ID})
}

func (s *Server) rerun(w http.ResponseWriter, r *http.Request, runID string, failedOnly bool, testIDs []int64) {
	ctx := r.Context()
	old, err := s.store.GetRun(ctx, runID)
	if err != nil {
		storeError(w, err)
		return
	}
	tests, err := s.store.ListRunTestsWithDefinitions(ctx, runID)
	if err != nil {
		storeError(w, err)
		return
	}
	want := map[int64]bool{}
	for _, id := range testIDs {
		want[id] = true
	}
	var order []int64
	bySuite := map[int64]*suiteInput{}
	for _, t := range tests {
		if len(want) > 0 && !want[t.ID] {
			continue
		}
		if failedOnly && t.Status != store.StatusFailed && t.Status != store.StatusError && t.Status != store.StatusCancelled {
			continue
		}
		var def model.Test
		if err := json.Unmarshal(t.Definition, &def); err != nil {
			continue
		}
		su := bySuite[t.SuiteID]
		if su == nil {
			su = &suiteInput{Name: t.SuiteName, FilePath: t.SuiteFile}
			bySuite[t.SuiteID] = su
			order = append(order, t.SuiteID)
		}
		su.Tests = append(su.Tests, def)
	}
	if len(order) == 0 {
		writeError(w, http.StatusBadRequest, "no tests to rerun")
		return
	}
	suites := make([]suiteInput, 0, len(order))
	for _, id := range order {
		suites = append(suites, *bySuite[id])
	}
	// Definitions are already normalized; snapshots are copied from the original run below.
	run, err := s.createRun(ctx, runInput{
		ProjectID: old.ProjectID, Version: old.Version, Source: "rerun", CreatedBy: principal(r).Name, RerunOf: old.ID,
		Suites: suites, SnapshotsReady: false,
	})
	if err != nil {
		storeError(w, err)
		return
	}
	if !run.SnapshotsReady {
		snaps, _ := s.store.ListRunSnapshots(ctx, old.ID)
		for _, sn := range snaps {
			src := filepath.Join(s.snapshotDir(old.ID), filepath.FromSlash(sn.Path))
			dst := filepath.Join(s.snapshotDir(run.ID), filepath.FromSlash(sn.Path))
			if err := copyFile(src, dst); err == nil {
				s.store.AddRunSnapshot(ctx, run.ID, sn.Path, sn.Size)
			}
		}
		s.store.SetSnapshotsReady(ctx, run.ID)
		s.notifier.Notify()
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": run.ID})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// ── SSE ─────────────────────────────────────────────────────────────────────

func sseStart(w http.ResponseWriter) (http.Flusher, bool) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return nil, false
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fl.Flush()
	return fl, true
}

func sseSend(w io.Writer, name string, data any) error {
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, b)
	return err
}

// GET /api/events/runs?id=<run> — the stream consumed by `chiperka test --cloud`.
// Without id it streams global change notifications.
func (s *Server) handleRunEvents(w http.ResponseWriter, r *http.Request) {
	runID := r.URL.Query().Get("id")
	if runID == "" {
		s.streamTopics(w, r, nil, topicGlobal)
		return
	}
	if _, err := s.store.GetRun(r.Context(), runID); err != nil {
		storeError(w, err)
		return
	}
	s.streamTopics(w, r, func(w io.Writer) bool {
		snap, run := s.runSnapshot(r.Context(), runID)
		sseSend(w, "snapshot", snap)
		if run != nil && run.Finished() {
			// The run already ended before the client connected — tell it right away.
			if run.IsCancelled {
				sseSend(w, "run_cancelled", map[string]any{"run_id": run.ID, "passed": run.Passed, "failed": run.Failed + run.Errored, "cancelled": run.Cancelled + run.Skipped})
			} else {
				sseSend(w, "run_completed", map[string]any{"run_id": run.ID, "passed": run.Passed, "failed": run.Failed + run.Errored, "skipped": run.Skipped + run.Cancelled})
			}
		}
		return true
	}, runTopic(runID))
}

// GET /api/events/ui?run=<id> — UI stream: global notifications plus one run's live events.
func (s *Server) handleUIEvents(w http.ResponseWriter, r *http.Request) {
	topics := []string{topicGlobal}
	if id := r.URL.Query().Get("run"); id != "" {
		topics = append(topics, runTopic(id))
	}
	s.streamTopics(w, r, func(w io.Writer) bool {
		sseSend(w, "hello", map[string]any{"at": store.Now()})
		return true
	}, topics...)
}

func (s *Server) streamTopics(w http.ResponseWriter, r *http.Request, initial func(io.Writer) bool, topics ...string) {
	// Subscribe before sending the initial state so no event slips in between.
	sub := s.hub.Subscribe(topics...)
	defer s.hub.Unsubscribe(sub)
	fl, ok := sseStart(w)
	if !ok {
		return
	}
	if initial != nil {
		initial(w)
		fl.Flush()
	}
	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
				return
			}
			fl.Flush()
		case ev := <-sub.ch:
			if err := sseSend(w, ev.Name, ev.Data); err != nil {
				return
			}
			// Drain whatever else is queued before flushing.
			for more := true; more; {
				select {
				case ev := <-sub.ch:
					sseSend(w, ev.Name, ev.Data)
				default:
					more = false
				}
			}
			fl.Flush()
		}
	}
}

type snapTest struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

type snapSuite struct {
	Name  string     `json:"name"`
	Tests []snapTest `json:"tests"`
}

func (s *Server) runSnapshot(ctx context.Context, runID string) (map[string]any, *store.Run) {
	run, _ := s.store.GetRun(ctx, runID)
	tests, _ := s.store.ListRunTests(ctx, runID)
	var suites []*snapSuite
	idx := map[int64]*snapSuite{}
	for _, t := range tests {
		su := idx[t.SuiteID]
		if su == nil {
			su = &snapSuite{Name: t.SuiteName}
			idx[t.SuiteID] = su
			suites = append(suites, su)
		}
		su.Tests = append(su.Tests, snapTest{ID: t.ID, Name: t.Name, Status: t.Status})
	}
	out := map[string]any{"id": runID, "suites": suites}
	if run != nil {
		out["status"] = run.Status
	}
	return out, run
}

// ── projects (CLI resolve + UI) ─────────────────────────────────────────────

// GET /api/projects/resolve?slug= — unknown projects are created on first use.
func (s *Server) handleProjectResolve(w http.ResponseWriter, r *http.Request) {
	slug := strings.TrimSpace(r.URL.Query().Get("slug"))
	if slug == "" {
		writeError(w, http.StatusBadRequest, "slug is required")
		return
	}
	p, err := s.store.EnsureProject(r.Context(), slug, "")
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) handleProjectList(w http.ResponseWriter, r *http.Request) {
	ps, err := s.store.ListProjects(r.Context())
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ps)
}

func (s *Server) handleProjectCreate(w http.ResponseWriter, r *http.Request) {
	var req struct{ Slug, Name string }
	if err := decodeJSON(r, &req); err != nil || strings.TrimSpace(req.Slug) == "" {
		writeError(w, http.StatusBadRequest, "slug is required")
		return
	}
	p, err := s.store.EnsureProject(r.Context(), strings.TrimSpace(req.Slug), strings.TrimSpace(req.Name))
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

func (s *Server) handleProjectDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteProject(r.Context(), pathInt(r, "id")); err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
