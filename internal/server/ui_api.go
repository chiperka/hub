package server

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"chiperka-hub/internal/store"
)

// ── overview ────────────────────────────────────────────────────────────────

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	stats, err := s.store.GetStats(ctx)
	if err != nil {
		storeError(w, err)
		return
	}
	daily, _ := s.store.DailyStats(ctx, 14)
	runs, _, _ := s.store.ListRuns(ctx, store.RunFilter{Limit: 8})
	workers, _ := s.store.ListWorkers(ctx)
	writeJSON(w, http.StatusOK, map[string]any{"stats": stats, "daily": daily, "recent_runs": runs, "workers": workers})
}

func (s *Server) handleConnectInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"server_url":    s.publicURL(r),
		"auth_disabled": s.cfg.AuthDisabled,
		"worker_image":  s.cfg.WorkerImage,
		"version":       s.cfg.Version,
	})
}

// ── runs ────────────────────────────────────────────────────────────────────

func (s *Server) handleRunList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.RunFilter{Status: q.Get("status"), Query: q.Get("q"), Limit: queryInt(r, "limit", 50), Offset: queryInt(r, "offset", 0)}
	if slug := q.Get("project"); slug != "" {
		p, err := s.store.GetProjectBySlug(r.Context(), slug)
		if err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"runs": []store.Run{}, "total": 0})
			return
		}
		f.ProjectID = p.ID
	}
	runs, total, err := s.store.ListRuns(r.Context(), f)
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs, "total": total})
}

func (s *Server) handleRunGet(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	run, err := s.store.GetRun(ctx, r.PathValue("id"))
	if err != nil {
		storeError(w, err)
		return
	}
	suites, _ := s.store.ListSuites(ctx, run.ID)
	tests, _ := s.store.ListRunTests(ctx, run.ID)
	snaps, _ := s.store.ListRunSnapshots(ctx, run.ID)
	writeJSON(w, http.StatusOK, map[string]any{"run": run, "suites": suites, "tests": tests, "snapshots": snaps})
}

func (s *Server) handleRunDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	runID := r.PathValue("id")
	run, err := s.store.GetRun(ctx, runID)
	if err != nil {
		storeError(w, err)
		return
	}
	if !run.Finished() {
		active, _ := s.store.CancelRun(ctx, runID)
		s.cancelJobsOnWorkers(ctx, active)
	}
	if err := s.store.DeleteRun(ctx, runID); err != nil {
		storeError(w, err)
		return
	}
	s.removeRunFiles(runID)
	s.changed("runs")
	s.changed("queue")
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ── tests ───────────────────────────────────────────────────────────────────

func (s *Server) handleTestGet(w http.ResponseWriter, r *http.Request) {
	t, err := s.store.GetTest(r.Context(), pathInt(r, "id"))
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.buildTestView(r.Context(), t))
}

// GET /api/tests/{id}/logs[?job=] — structured worker logs as a JSON array.
func (s *Server) handleTestLogs(w http.ResponseWriter, r *http.Request) {
	t, err := s.store.GetTest(r.Context(), pathInt(r, "id"))
	if err != nil {
		storeError(w, err)
		return
	}
	jobID := t.JobID
	if j := queryInt(r, "job", 0); j > 0 {
		jobID = int64(j)
	}
	logs := []json.RawMessage{}
	if f, err := os.Open(s.jobLogPath(t.RunID, jobID)); err == nil {
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 16<<20)
		for sc.Scan() {
			if line := sc.Bytes(); len(line) > 0 {
				logs = append(logs, json.RawMessage(append([]byte(nil), line...)))
			}
		}
		f.Close()
	}
	writeJSON(w, http.StatusOK, logs)
}

func (s *Server) handleTestArtifact(w http.ResponseWriter, r *http.Request) {
	t, err := s.store.GetTest(r.Context(), pathInt(r, "id"))
	if err != nil {
		storeError(w, err)
		return
	}
	s.serveArtifact(w, r, t, r.PathValue("name"))
}

func (s *Server) serveArtifact(w http.ResponseWriter, r *http.Request, t *store.Test, name string) {
	clean, ok := safeRelPath(name)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid name")
		return
	}
	p := s.artifactPath(t.RunID, t.UUID, clean)
	if _, err := os.Stat(p); err != nil {
		writeError(w, http.StatusNotFound, "artifact not found")
		return
	}
	if r.URL.Query().Get("download") != "" {
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filepath.Base(clean)))
	} else if ct := guessTextType(clean); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	http.ServeFile(w, r, p)
}

func guessTextType(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".log", ".txt", ".diff", ".patch", ".yaml", ".yml", ".csv", ".sql", "":
		return "text/plain; charset=utf-8"
	case ".json":
		return "application/json; charset=utf-8"
	}
	return ""
}

// ── workers ─────────────────────────────────────────────────────────────────

func (s *Server) handleWorkerList(w http.ResponseWriter, r *http.Request) {
	ws, err := s.store.ListWorkers(r.Context())
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ws)
}

func randomID(prefix string, n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

func (s *Server) handleWorkerProvision(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	decodeJSON(r, &req)
	req.Name = strings.TrimSpace(req.Name)
	id := randomID("w-", 4)
	if req.Name == "" {
		req.Name = id
	}
	tokenID, secret, err := s.store.CreateToken(r.Context(), "worker "+req.Name, store.TokenWorker)
	if err != nil {
		storeError(w, err)
		return
	}
	if err := s.store.CreateWorker(r.Context(), id, req.Name, tokenID, false); err != nil {
		storeError(w, err)
		return
	}
	wk, _ := s.store.GetWorker(r.Context(), id)
	s.changed("workers")
	writeJSON(w, http.StatusCreated, s.workerCredentials(r, wk, secret))
}

func (s *Server) handleWorkerRotateToken(w http.ResponseWriter, r *http.Request) {
	wk, err := s.store.GetWorker(r.Context(), r.PathValue("id"))
	if err != nil {
		storeError(w, err)
		return
	}
	if wk.IsLocal {
		writeError(w, http.StatusBadRequest, "the embedded local worker manages its own token")
		return
	}
	tokenID, secret, err := s.store.CreateToken(r.Context(), "worker "+wk.Name, store.TokenWorker)
	if err != nil {
		storeError(w, err)
		return
	}
	if err := s.store.SetWorkerToken(r.Context(), wk.ID, tokenID); err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.workerCredentials(r, wk, secret))
}

func (s *Server) workerCredentials(r *http.Request, wk *store.Worker, secret string) map[string]any {
	api := s.publicURL(r)
	name := "chiperka-worker-" + strings.TrimPrefix(wk.ID, "w-")
	image := s.cfg.WorkerImage
	if image == "" {
		image = "<chiperka-worker image>" // set CHIPERKA_WORKER_IMAGE to fill this in
	}
	docker := strings.Join([]string{
		"docker run -d --name " + name + " --restart unless-stopped \\",
		"  -v /var/run/docker.sock:/var/run/docker.sock \\",
		"  -e CHIPERKA_WORKER_TOKEN=" + secret + " \\",
		"  " + image + " \\",
		"  --api " + api,
	}, "\n")
	binary := "CHIPERKA_WORKER_TOKEN=" + secret + " chiperka-worker worker --api " + api
	return map[string]any{
		"worker": wk, "token": secret, "api_url": api,
		"commands": map[string]string{"docker": docker, "binary": binary},
	}
}

func (s *Server) handleWorkerUpdate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := decodeJSON(r, &req); err != nil || strings.TrimSpace(req.Name) == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if err := s.store.RenameWorker(r.Context(), r.PathValue("id"), strings.TrimSpace(req.Name)); err != nil {
		storeError(w, err)
		return
	}
	s.changed("workers")
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleWorkerPause(w http.ResponseWriter, r *http.Request) {
	s.setPaused(w, r, true)
}

func (s *Server) handleWorkerUnpause(w http.ResponseWriter, r *http.Request) {
	s.setPaused(w, r, false)
}

func (s *Server) setPaused(w http.ResponseWriter, r *http.Request, paused bool) {
	if err := s.store.SetWorkerPaused(r.Context(), r.PathValue("id"), paused); err != nil {
		storeError(w, err)
		return
	}
	if !paused {
		s.notifier.Notify()
	}
	s.changed("workers")
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleWorkerDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	wk, err := s.store.GetWorker(r.Context(), id)
	if err != nil {
		storeError(w, err)
		return
	}
	if wk.IsLocal {
		writeError(w, http.StatusBadRequest, "the embedded local worker cannot be deleted; pause it instead")
		return
	}
	s.failWorkerJobs(id, "worker was deleted")
	if err := s.store.DeleteWorker(r.Context(), id); err != nil {
		storeError(w, err)
		return
	}
	s.changed("workers")
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ── queue ───────────────────────────────────────────────────────────────────

func (s *Server) handleJobList(w http.ResponseWriter, r *http.Request) {
	f := store.JobFilter{WorkerID: r.URL.Query().Get("worker"), Limit: queryInt(r, "limit", 200)}
	if st := r.URL.Query().Get("status"); st != "" {
		f.Statuses = strings.Split(st, ",")
	}
	jobs, err := s.store.ListJobs(r.Context(), f)
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, jobs)
}

func (s *Server) handleJobGet(w http.ResponseWriter, r *http.Request) {
	j, err := s.store.GetJob(r.Context(), pathInt(r, "id"))
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, j)
}

// ── tokens & users ──────────────────────────────────────────────────────────

func (s *Server) handleTokenList(w http.ResponseWriter, r *http.Request) {
	ts, err := s.store.ListTokens(r.Context(), store.TokenAPI)
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ts)
}

func (s *Server) handleTokenCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	decodeJSON(r, &req)
	if strings.TrimSpace(req.Name) == "" {
		req.Name = "cli"
	}
	id, secret, err := s.store.CreateToken(r.Context(), strings.TrimSpace(req.Name), store.TokenAPI)
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "name": req.Name, "token": secret})
}

func (s *Server) handleTokenDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteToken(r.Context(), pathInt(r, "id")); err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleUserList(w http.ResponseWriter, r *http.Request) {
	us, err := s.store.ListUsers(r.Context())
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, us)
}

func (s *Server) handleUserCreate(w http.ResponseWriter, r *http.Request) {
	var req struct{ Email, Password string }
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.createUser(r.Context(), req.Email, req.Password); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"status": "ok"})
}

func (s *Server) handleUserDelete(w http.ResponseWriter, r *http.Request) {
	id := pathInt(r, "id")
	users, _ := s.store.ListUsers(r.Context())
	if len(users) <= 1 {
		writeError(w, http.StatusBadRequest, "cannot delete the last user")
		return
	}
	for _, u := range users {
		if u.ID == id && u.Email == principal(r).Email {
			writeError(w, http.StatusBadRequest, "you cannot delete yourself")
			return
		}
	}
	if err := s.store.DeleteUser(r.Context(), id); err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
