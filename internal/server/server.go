// Package server implements the chiperka-hub HTTP API: the worker protocol,
// the CLI cloud protocol, the results API used by MCP and the UI API.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"chiperka-hub/internal/store"
)

type Config struct {
	DataDir       string
	PublicURL     string // e.g. https://chiperka.example.com — used in generated commands
	AuthDisabled  bool   // trusted single-user mode: UI and CLI need no login/token
	Version       string
	JobTimeout    time.Duration
	RetentionDays int
	WorkerImage   string // image suggested in "add worker" instructions
	LocalWorker   bool   // an embedded worker is managed by this process
}

type Server struct {
	cfg      Config
	store    *store.Store
	hub      *Hub
	notifier *Notifier
	ui       fs.FS
	log      *slog.Logger
}

func New(cfg Config, st *store.Store, ui fs.FS) *Server {
	if cfg.JobTimeout <= 0 {
		cfg.JobTimeout = 5 * time.Minute
	}
	return &Server{cfg: cfg, store: st, hub: NewHub(), notifier: NewNotifier(), ui: ui, log: slog.Default()}
}

func (s *Server) Store() *store.Store { return s.store }

// Handler builds the HTTP routing table.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Public
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /api/auth/state", s.handleAuthState)
	mux.HandleFunc("POST /api/auth/login", s.handleLogin)
	mux.HandleFunc("POST /api/auth/logout", s.handleLogout)
	mux.HandleFunc("POST /api/auth/setup", s.handleSetup)

	// Worker protocol (worker token)
	mux.HandleFunc("POST /api/workers/register", s.workerAuth(s.handleWorkerRegister))
	mux.HandleFunc("POST /api/workers/heartbeat", s.workerAuth(s.handleWorkerHeartbeat))
	mux.HandleFunc("POST /api/jobs/claim", s.workerAuth(s.handleJobClaim))
	mux.HandleFunc("POST /api/jobs/ack", s.workerAuth(s.handleJobAck))
	// Job-scoped (job token or worker token)
	mux.HandleFunc("POST /api/jobs/{id}/progress", s.jobAuth(s.handleJobProgress))
	mux.HandleFunc("POST /api/jobs/{id}/logs", s.jobAuth(s.handleJobLogs))
	mux.HandleFunc("POST /api/jobs/{id}/artifacts", s.jobAuth(s.handleJobArtifact))
	mux.HandleFunc("GET /api/jobs/{id}/files/{path...}", s.jobAuth(s.handleJobFile))
	mux.HandleFunc("POST /api/jobs/{id}/complete", s.jobAuth(s.handleJobComplete))

	// CLI protocol + UI (session or API token)
	u := func(pattern string, h http.HandlerFunc) { mux.HandleFunc(pattern, s.userAuth(h)) }
	u("GET /api/me", s.handleMe)
	u("POST /api/me/password", s.handleChangePassword)
	u("GET /api/overview", s.handleOverview)
	u("GET /api/connect", s.handleConnectInfo)

	u("GET /api/projects/resolve", s.handleProjectResolve)
	u("GET /api/projects", s.handleProjectList)
	u("POST /api/projects", s.handleProjectCreate)
	u("DELETE /api/projects/{id}", s.handleProjectDelete)

	u("POST /api/runs", s.handleRunCreate)
	u("GET /api/runs", s.handleRunList)
	u("GET /api/runs/{id}", s.handleRunGet)
	u("DELETE /api/runs/{id}", s.handleRunDelete)
	u("POST /api/runs/{id}/snapshots", s.handleSnapshotsUpload)
	u("GET /api/runs/{id}/snapshots/{path...}", s.handleSnapshotGet)
	u("POST /api/runs/{id}/stop", s.handleRunStop)
	u("POST /api/runs/{id}/rerun", s.handleRunRerun)
	u("GET /api/runs/{id}/report.xml", s.handleReportJUnit)
	u("GET /api/runs/{id}/report.html", s.handleReportHTML)
	u("GET /api/runs/{id}/report.zip", s.handleReportZip)
	u("GET /api/runs/{id}/artifacts.zip", s.handleArtifactsZip)

	u("GET /api/events/runs", s.handleRunEvents)
	u("GET /api/events/ui", s.handleUIEvents)

	u("GET /api/tests/{id}", s.handleTestGet)
	u("GET /api/tests/{id}/logs", s.handleTestLogs)
	u("GET /api/tests/{id}/artifacts/{name...}", s.handleTestArtifact)
	u("POST /api/tests/{id}/rerun", s.handleTestRerun)

	u("GET /api/workers", s.handleWorkerList)
	u("POST /api/workers", s.handleWorkerProvision)
	u("PATCH /api/workers/{id}", s.handleWorkerUpdate)
	u("POST /api/workers/{id}/pause", s.handleWorkerPause)
	u("POST /api/workers/{id}/unpause", s.handleWorkerUnpause)
	u("POST /api/workers/{id}/token", s.handleWorkerRotateToken)
	u("DELETE /api/workers/{id}", s.handleWorkerDelete)

	u("GET /api/jobs", s.handleJobList)
	u("GET /api/jobs/{id}", s.handleJobGet)

	u("GET /api/tokens", s.handleTokenList)
	u("POST /api/tokens", s.handleTokenCreate)
	u("DELETE /api/tokens/{id}", s.handleTokenDelete)

	u("GET /api/users", s.handleUserList)
	u("POST /api/users", s.handleUserCreate)
	u("DELETE /api/users/{id}", s.handleUserDelete)

	// Results API (CLI MCP drill-down, shapes match chiperka internal/result)
	u("GET /api/results/runs", s.handleResultsRuns)
	u("GET /api/results/runs/{id}", s.handleResultsRun)
	u("GET /api/results/tests/{uuid}", s.handleResultsTest)
	u("GET /api/results/tests/{uuid}/artifacts/{name...}", s.handleResultsArtifact)

	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not found")
	})
	mux.Handle("/", s.spaHandler())

	return s.logRequests(mux)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "version": s.cfg.Version})
}

// ── UI static files ─────────────────────────────────────────────────────────

func (s *Server) spaHandler() http.Handler {
	if s.ui == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "UI not built (run `make ui`)", http.StatusNotFound)
		})
	}
	files := http.FileServer(http.FS(s.ui))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if p != "" {
			if f, err := s.ui.Open(p); err == nil {
				f.Close()
				if strings.HasPrefix(p, "assets/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				files.ServeHTTP(w, r)
				return
			}
		}
		// SPA fallback: every unknown path renders index.html.
		index, err := fs.ReadFile(s.ui, "index.html")
		if err != nil {
			http.Error(w, "UI not built", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(index)
	})
}

// ── helpers ─────────────────────────────────────────────────────────────────

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(rec, r)
		// Keep the log readable: skip the chatty, healthy worker traffic and static files.
		if rec.status < 400 && (r.URL.Path == "/api/jobs/claim" || r.URL.Path == "/api/workers/heartbeat" ||
			strings.HasSuffix(r.URL.Path, "/logs") || strings.HasSuffix(r.URL.Path, "/progress") ||
			!strings.HasPrefix(r.URL.Path, "/api/")) {
			return
		}
		s.log.Info("http", "method", r.Method, "path", r.URL.Path, "status", rec.status, "ms", time.Since(start).Milliseconds())
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func storeError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	writeError(w, http.StatusInternalServerError, err.Error())
}

func decodeJSON(r *http.Request, v any) error {
	return json.NewDecoder(http.MaxBytesReader(nil, r.Body, 64<<20)).Decode(v)
}

func pathInt(r *http.Request, name string) int64 {
	n, _ := strconv.ParseInt(r.PathValue(name), 10, 64)
	return n
}

func queryInt(r *http.Request, name string, def int) int {
	if v, err := strconv.Atoi(r.URL.Query().Get(name)); err == nil {
		return v
	}
	return def
}

// safeRelPath validates a client supplied relative path (zip entries, artifact names).
func safeRelPath(p string) (string, bool) {
	p = strings.ReplaceAll(p, "\\", "/")
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return "", false
		}
	}
	clean := path.Clean("/" + p)[1:]
	if clean == "" || clean == "." {
		return "", false
	}
	return clean, true
}

// ── blob layout ─────────────────────────────────────────────────────────────

func (s *Server) snapshotDir(runID string) string {
	return filepath.Join(s.cfg.DataDir, "runs", runID, "snapshots")
}

func (s *Server) artifactPath(runID, testUUID, name string) string {
	return filepath.Join(s.cfg.DataDir, "runs", runID, "tests", testUUID, filepath.FromSlash(name))
}

func (s *Server) jobLogPath(runID string, jobID int64) string {
	return filepath.Join(s.cfg.DataDir, "runs", runID, "logs", strconv.FormatInt(jobID, 10)+".jsonl")
}

func (s *Server) removeRunFiles(runID string) {
	os.RemoveAll(filepath.Join(s.cfg.DataDir, "runs", runID))
}

// publicURL returns the externally reachable base URL of this server.
func (s *Server) publicURL(r *http.Request) string {
	if s.cfg.PublicURL != "" {
		return strings.TrimRight(s.cfg.PublicURL, "/")
	}
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	host := r.Header.Get("X-Forwarded-Host")
	if host == "" {
		host = r.Host
	}
	return scheme + "://" + host
}

// ── background helpers shared by handlers and monitor ───────────────────────

func bg() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 30*time.Second)
}

func (s *Server) changed(what string) {
	s.hub.Publish(topicGlobal, what, map[string]any{"at": store.Now()})
}
