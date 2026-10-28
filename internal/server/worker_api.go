package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"chiperka-hub/internal/compile"
	"chiperka-hub/internal/store"
)

const (
	maxClaimBatch   = 8
	maxClaimTimeout = 30 * time.Second
	maxRetries      = 2 // infrastructure errors are retried this many times
)

// ── register / heartbeat ────────────────────────────────────────────────────

func (s *Server) handleWorkerRegister(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name          string `json:"name"`
		Hostname      string `json:"hostname"`
		Concurrency   int    `json:"concurrency"`
		Capacity      int    `json:"capacity"`
		MaxContainers int    `json:"max_containers"`
		Version       string `json:"version"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.Concurrency < 1 {
		req.Concurrency = 1
	}
	// A provisioned worker authenticates with its own token. Otherwise it may
	// join: the hub creates the worker and hands it a token for later calls.
	var issued string
	wk := s.authWorker(r)
	if wk == nil {
		if !s.canJoin(r) {
			writeError(w, http.StatusForbidden, "invalid worker token — provision the worker in the UI (Workers → Add worker) or use the hub's join token")
			return
		}
		name := strings.TrimSpace(req.Name)
		if name == "" {
			name = strings.TrimSpace(req.Hostname)
		}
		var err error
		if wk, issued, err = s.joinWorker(r.Context(), name); err != nil {
			storeError(w, err)
			return
		}
		s.log.Info("worker joined", "worker", wk.ID, "name", wk.Name)
	}
	if err := s.store.ActivateWorker(r.Context(), wk.ID, req.Hostname, req.Version, req.Concurrency, req.Capacity, req.MaxContainers); err != nil {
		storeError(w, err)
		return
	}
	// A restarted worker lost whatever it was running.
	s.failWorkerJobs(wk.ID, "worker restarted while the job was running")
	s.log.Info("worker registered", "worker", wk.ID, "name", wk.Name, "hostname", req.Hostname, "version", req.Version,
		"concurrency", req.Concurrency, "capacity", req.Capacity)
	s.changed("workers")
	s.notifier.Notify()
	resp := map[string]string{"status": "registered", "worker_id": wk.ID}
	if issued != "" {
		resp["token"] = issued
	}
	writeJSON(w, http.StatusOK, resp)
}

// canJoin reports whether an unprovisioned worker may register itself: it
// presents the configured join token, or — in trusted mode without a join
// token — no token at all.
func (s *Server) canJoin(r *http.Request) bool {
	tok := bearer(r)
	if s.cfg.WorkerJoinToken != "" {
		return subtle.ConstantTimeCompare([]byte(tok), []byte(s.cfg.WorkerJoinToken)) == 1
	}
	return s.cfg.AuthDisabled && tok == ""
}

// joinWorker creates the joined worker called name, or takes over the existing
// one (a restarted worker keeps its row), and binds it to a fresh token.
func (s *Server) joinWorker(ctx context.Context, name string) (*store.Worker, string, error) {
	if name == "" {
		name = "worker"
	}
	tokenID, secret, err := s.store.CreateToken(ctx, "worker "+name, store.TokenWorker)
	if err != nil {
		return nil, "", err
	}
	wk, err := s.store.GetJoinedWorker(ctx, name)
	switch {
	case err == store.ErrNotFound:
		id := randomID(store.JoinedWorkerPrefix, 4)
		if err := s.store.CreateWorker(ctx, id, name, tokenID, false); err != nil {
			return nil, "", err
		}
		wk, err = s.store.GetWorker(ctx, id)
	case err == nil:
		err = s.store.SetWorkerToken(ctx, wk.ID, tokenID)
	}
	if err != nil {
		return nil, "", err
	}
	return wk, secret, nil
}

func (s *Server) handleWorkerHeartbeat(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID           string `json:"id"`
		ActiveTests  int    `json:"active_tests"`
		CurrentRunID string `json:"current_run_id"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	wk := workerFrom(r)
	cmds, revived, err := s.store.Heartbeat(r.Context(), wk.ID, req.ActiveTests, req.CurrentRunID)
	if err != nil {
		storeError(w, err)
		return
	}
	if revived || wk.ActiveTests != req.ActiveTests {
		s.changed("workers")
	}
	if cmds == nil {
		cmds = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "commands": cmds})
}

// ── claim / ack ─────────────────────────────────────────────────────────────

func (s *Server) handleJobClaim(w http.ResponseWriter, r *http.Request) {
	var req struct {
		WorkerID          string `json:"worker_id"`
		Count             int    `json:"count"`
		TimeoutMs         int    `json:"timeout_ms"`
		AvailableCapacity int    `json:"available_capacity"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	wk := workerFrom(r)
	count := min(max(req.Count, 1), maxClaimBatch)
	timeout := time.Duration(req.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = 25 * time.Second
	}
	timeout = min(timeout, maxClaimTimeout)
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()

	for {
		if s.store.IsWorkerPaused(r.Context(), wk.ID) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		wake := s.notifier.Wait()
		jobs, err := s.store.ClaimJobs(r.Context(), wk.ID, count, req.AvailableCapacity, func() string { return store.RandomToken("cjt_") })
		if err != nil {
			if r.Context().Err() != nil {
				return
			}
			s.log.Error("claim failed", "err", err)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if len(jobs) > 0 {
			out := make([]*compile.Job, 0, len(jobs))
			for _, j := range jobs {
				var cj compile.Job
				if err := json.Unmarshal(j.DAG, &cj); err != nil {
					s.log.Error("corrupt job DAG", "job", j.ID, "err", err)
					continue
				}
				cj.JobID = strconv.FormatInt(j.ID, 10)
				cj.Token = j.Token
				if cj.Meta != nil {
					cj.Meta.JobID = j.ID
					cj.RegistryAuth = s.registry.Get(cj.Meta.RunID)
				}
				out = append(out, &cj)
			}
			s.log.Info("jobs claimed", "worker", wk.ID, "count", len(out))
			s.changed("queue")
			writeJSON(w, http.StatusOK, map[string]any{"jobs": out})
			return
		}
		select {
		case <-wake:
		case <-deadline.C:
			w.WriteHeader(http.StatusNoContent)
			return
		case <-r.Context().Done():
			return
		}
	}
}

func (s *Server) handleJobAck(w http.ResponseWriter, r *http.Request) {
	var req struct {
		WorkerID string   `json:"worker_id"`
		JobIDs   []string `json:"job_ids"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	var ids []int64
	for _, s := range req.JobIDs {
		if id, err := strconv.ParseInt(s, 10, 64); err == nil {
			ids = append(ids, id)
		}
	}
	wk := workerFrom(r)
	acked, err := s.store.AckJobs(r.Context(), wk.ID, ids)
	if err != nil {
		storeError(w, err)
		return
	}
	ackedIDs := make([]string, 0, len(acked))
	runs := map[string]bool{}
	for _, j := range acked {
		ackedIDs = append(ackedIDs, strconv.FormatInt(j.ID, 10))
		os.Remove(s.jobLogPath(j.RunID, j.ID))
		s.hub.Publish(runTopic(j.RunID), "test_update", map[string]any{"test_id": j.TestID, "status": store.StatusRunning, "worker_id": wk.ID})
		runs[j.RunID] = true
	}
	for runID := range runs {
		s.refreshRun(runID)
	}
	writeJSON(w, http.StatusOK, map[string]any{"acked": len(acked), "job_ids": ackedIDs})
}

// ── progress / logs / artifacts / files ─────────────────────────────────────

func (s *Server) handleJobProgress(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Step     string `json:"step"`
		Status   string `json:"status"`
		Duration int64  `json:"duration_ms"`
		Error    string `json:"error"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	job := jobFrom(r)
	if err := s.store.RecordProgress(r.Context(), job.ID, req.Step, req.Status, req.Duration, req.Error); err != nil {
		storeError(w, err)
		return
	}
	s.hub.Publish(runTopic(job.RunID), "step_update", map[string]any{
		"test_id": job.TestID, "job_id": job.ID, "step": req.Step, "status": req.Status,
		"duration_ms": req.Duration, "error": req.Error,
	})
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleJobLogs(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Logs []json.RawMessage `json:"logs"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	job := jobFrom(r)
	if len(req.Logs) > 0 {
		p := s.jobLogPath(job.RunID, job.ID)
		os.MkdirAll(filepath.Dir(p), 0o755)
		f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		bw := bufio.NewWriter(f)
		for _, l := range req.Logs {
			bw.Write(compactJSON(l))
			bw.WriteByte('\n')
		}
		bw.Flush()
		f.Close()
		s.store.TouchJob(r.Context(), job.ID)
		s.hub.Publish(runTopic(job.RunID), "test_log", map[string]any{"test_id": job.TestID, "job_id": job.ID, "logs": req.Logs})
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func compactJSON(raw json.RawMessage) []byte {
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		return raw
	}
	return b.Bytes()
}

func (s *Server) handleJobArtifact(w http.ResponseWriter, r *http.Request) {
	job := jobFrom(r)
	r.Body = http.MaxBytesReader(w, r.Body, 512<<20)

	var name string
	var src io.Reader
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		mr, err := r.MultipartReader()
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid multipart: "+err.Error())
			return
		}
		// Fields arrive in order: "name" then "file" (see worker apiclient), stream the file part.
		for {
			part, err := mr.NextPart()
			if err != nil {
				break
			}
			if part.FormName() == "name" {
				b, _ := io.ReadAll(io.LimitReader(part, 4096))
				name = string(b)
				continue
			}
			if part.FormName() == "file" {
				if name == "" {
					name = part.FileName()
				}
				src = part
				break
			}
		}
	} else {
		var req struct {
			Name    string `json:"name"`
			Content string `json:"content"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		name, src = req.Name, strings.NewReader(req.Content)
	}
	clean, ok := safeRelPath(name)
	if !ok || src == nil {
		writeError(w, http.StatusBadRequest, "artifact name and file are required")
		return
	}
	test, err := s.store.GetTest(r.Context(), job.TestID)
	if err != nil {
		storeError(w, err)
		return
	}
	p := s.artifactPath(job.RunID, test.UUID, clean)
	os.MkdirAll(filepath.Dir(p), 0o755)
	f, err := os.Create(p)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	n, err := io.Copy(f, src)
	f.Close()
	if err != nil {
		os.Remove(p)
		writeError(w, http.StatusBadRequest, "upload failed: "+err.Error())
		return
	}
	if err := s.store.UpsertArtifact(r.Context(), job.TestID, job.ID, clean, n); err != nil {
		storeError(w, err)
		return
	}
	s.store.TouchJob(r.Context(), job.ID)
	s.hub.Publish(runTopic(job.RunID), "artifact", map[string]any{"test_id": job.TestID, "name": clean, "size": n})
	writeJSON(w, http.StatusCreated, map[string]any{"status": "ok", "name": clean, "size": n})
}

func (s *Server) handleJobFile(w http.ResponseWriter, r *http.Request) {
	job := jobFrom(r)
	clean, ok := safeRelPath(r.PathValue("path"))
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid path")
		return
	}
	p := filepath.Join(s.snapshotDir(job.RunID), filepath.FromSlash(clean))
	if _, err := os.Stat(p); err != nil {
		writeError(w, http.StatusNotFound, "file not found: "+clean)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeFile(w, r, p)
}

// ── complete ────────────────────────────────────────────────────────────────

func (s *Server) handleJobComplete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		WorkerID     string                     `json:"worker_id"`
		Status       string                     `json:"status"`
		ErrorMessage string                     `json:"error_message"`
		DurationMs   int64                      `json:"duration_ms"`
		StepResults  map[string]json.RawMessage `json:"step_results"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	job := jobFrom(r)
	idem := r.Header.Get("X-Idempotency-Key")
	if idem != "" && job.IdempotencyKey == idem {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "idempotent": "true"})
		return
	}
	switch req.Status {
	case store.StatusPassed, store.StatusFailed, store.StatusError:
	default:
		req.Status = store.StatusError
	}
	stepResults, _ := json.Marshal(req.StepResults)
	ok, err := s.store.CompleteJob(r.Context(), job.ID, req.Status, req.ErrorMessage, req.DurationMs, stepResults, idem)
	if err != nil {
		storeError(w, err)
		return
	}
	if !ok {
		// Cancelled or reassigned meanwhile: accept but ignore the result.
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "ignored": "job no longer active"})
		return
	}
	s.log.Info("job completed", "job", job.ID, "test", job.TestName, "status", req.Status, "ms", req.DurationMs)
	s.finishJob(job, req.Status, req.ErrorMessage, req.DurationMs)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// finishJob applies a final job result to its test: retries infrastructure
// errors, otherwise records the outcome and refreshes the run.
func (s *Server) finishJob(job *store.Job, result, errMsg string, durationMs int64) {
	ctx, cancel := bg()
	defer cancel()
	run, err := s.store.GetRun(ctx, job.RunID)
	if err != nil {
		return
	}
	if result == store.StatusError && job.RetryCount < maxRetries && !run.IsCancelled {
		newID, err := s.store.RetryJob(ctx, job)
		if err == nil {
			s.log.Warn("retrying job after infrastructure error", "job", job.ID, "retry_job", newID,
				"attempt", job.RetryCount+1, "error", errMsg)
			s.hub.Publish(runTopic(job.RunID), "test_update", map[string]any{
				"test_id": job.TestID, "status": store.StatusPending, "retry": job.RetryCount + 1, "message": errMsg,
			})
			s.notifier.Notify()
			s.refreshRun(job.RunID)
			return
		}
		s.log.Error("retry failed", "job", job.ID, "err", err)
	}
	if err := s.store.FinishTest(ctx, job.TestID, result, errMsg, durationMs); err != nil {
		s.log.Error("finish test", "test", job.TestID, "err", err)
	}
	s.hub.Publish(runTopic(job.RunID), "test_update", map[string]any{
		"test_id": job.TestID, "status": result, "duration": durationMs, "message": errMsg,
	})
	s.refreshRun(job.RunID)
}

// failJob marks an active job as errored (monitor / worker loss) and runs the retry logic.
func (s *Server) failJob(job *store.Job, msg string) {
	ctx, cancel := bg()
	defer cancel()
	ok, err := s.store.FailJob(ctx, job.ID, msg)
	if err != nil || !ok {
		return
	}
	s.log.Warn("job failed", "job", job.ID, "worker", job.WorkerID, "reason", msg)
	if job.WorkerID != "" {
		s.store.QueueWorkerCommand(ctx, job.WorkerID, "cancel:"+strconv.FormatInt(job.ID, 10))
	}
	s.finishJob(job, store.StatusError, msg, 0)
}

func (s *Server) failWorkerJobs(workerID, msg string) {
	ctx, cancel := bg()
	defer cancel()
	jobs, _ := s.store.WorkerActiveJobs(ctx, workerID)
	for i := range jobs {
		s.failJob(&jobs[i], msg)
	}
}

// refreshRun recomputes counters and emits completion events.
func (s *Server) refreshRun(runID string) {
	ctx, cancel := bg()
	defer cancel()
	run, finished, err := s.store.RefreshRun(ctx, runID)
	if err != nil {
		return
	}
	s.hub.Publish(runTopic(runID), "run_update", run)
	s.changed("runs")
	if finished {
		s.log.Info("run finished", "run", runID, "status", run.Status, "passed", run.Passed, "failed", run.Failed+run.Errored)
		s.publishRunEnd(run)
	}
}

func (s *Server) publishRunEnd(run *store.Run) {
	s.registry.Delete(run.ID)
	if run.IsCancelled {
		s.hub.Publish(runTopic(run.ID), "run_cancelled", map[string]any{
			"run_id": run.ID, "passed": run.Passed, "failed": run.Failed + run.Errored, "cancelled": run.Cancelled + run.Skipped,
		})
		return
	}
	s.hub.Publish(runTopic(run.ID), "run_completed", map[string]any{
		"run_id": run.ID, "passed": run.Passed, "failed": run.Failed + run.Errored, "skipped": run.Skipped + run.Cancelled,
	})
}

// cancelJobsOnWorkers sends cancel commands for jobs already handed to workers.
func (s *Server) cancelJobsOnWorkers(ctx context.Context, jobs []store.Job) {
	for _, j := range jobs {
		if j.WorkerID != "" {
			s.store.QueueWorkerCommand(ctx, j.WorkerID, fmt.Sprintf("cancel:%d", j.ID))
		}
	}
}
