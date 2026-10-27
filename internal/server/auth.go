package server

import (
	"context"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"chiperka-hub/internal/store"
)

const sessionCookie = "chiperka_session"
const sessionTTL = 30 * 24 * time.Hour

type ctxKey int

const (
	ctxPrincipal ctxKey = iota
	ctxWorker
)

// Principal identifies who is calling the user-facing API.
type Principal struct {
	Kind  string `json:"kind"` // "user", "token", "anonymous"
	Name  string `json:"name"`
	Email string `json:"email,omitempty"`
}

func principal(r *http.Request) Principal {
	if p, ok := r.Context().Value(ctxPrincipal).(Principal); ok {
		return p
	}
	return Principal{Kind: "anonymous"}
}

func workerFrom(r *http.Request) *store.Worker {
	w, _ := r.Context().Value(ctxWorker).(*store.Worker)
	return w
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

// resolvePrincipal authenticates a user-facing request (session cookie or API token).
func (s *Server) resolvePrincipal(r *http.Request) (Principal, bool) {
	if tok := bearer(r); tok != "" {
		t, err := s.store.LookupToken(r.Context(), tok)
		if err == nil && t.Kind == store.TokenAPI {
			return Principal{Kind: "token", Name: t.Name}, true
		}
		if !s.cfg.AuthDisabled {
			return Principal{}, false
		}
	}
	if c, err := r.Cookie(sessionCookie); err == nil {
		if u, err := s.store.GetSessionUser(r.Context(), c.Value); err == nil {
			return Principal{Kind: "user", Name: u.Email, Email: u.Email}, true
		}
	}
	if s.cfg.AuthDisabled {
		return Principal{Kind: "anonymous", Name: "local"}, true
	}
	return Principal{}, false
}

func (s *Server) userAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := s.resolvePrincipal(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "authentication required (log in, or pass an API token as Bearer / CHIPERKA_TOKEN)")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), ctxPrincipal, p)))
	}
}

// workerAuth requires a worker (spt_) token bound to a provisioned worker.
func (s *Server) workerAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		wk := s.authWorker(r)
		if wk == nil {
			writeError(w, http.StatusForbidden, "invalid worker token — provision the worker in the UI (Workers → Add worker)")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), ctxWorker, wk)))
	}
}

func (s *Server) authWorker(r *http.Request) *store.Worker {
	tok := bearer(r)
	if !strings.HasPrefix(tok, "spt_") {
		return nil
	}
	t, err := s.store.LookupToken(r.Context(), tok)
	if err != nil || t.Kind != store.TokenWorker {
		return nil
	}
	wk, err := s.store.GetWorkerByToken(r.Context(), t.ID)
	if err != nil {
		return nil
	}
	return wk
}

// jobAuth accepts the job-scoped token issued at claim time, or any worker token.
func (s *Server) jobAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		job, err := s.store.GetJob(r.Context(), pathInt(r, "id"))
		if err != nil {
			storeError(w, err)
			return
		}
		tok := bearer(r)
		if tok == "" || (tok != job.Token && s.authWorker(r) == nil) {
			writeError(w, http.StatusForbidden, "invalid job token")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), ctxJob, job)))
	}
}

const ctxJob ctxKey = 100

func jobFrom(r *http.Request) *store.Job {
	j, _ := r.Context().Value(ctxJob).(*store.Job)
	return j
}

// ── auth endpoints ──────────────────────────────────────────────────────────

func (s *Server) handleAuthState(w http.ResponseWriter, r *http.Request) {
	p, ok := s.resolvePrincipal(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"auth_disabled":  s.cfg.AuthDisabled,
		"setup_required": !s.cfg.AuthDisabled && s.store.CountUsers(r.Context()) == 0,
		"authenticated":  ok,
		"principal":      p,
		"version":        s.cfg.Version,
	})
}

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	if s.store.CountUsers(r.Context()) > 0 {
		writeError(w, http.StatusConflict, "already set up")
		return
	}
	var req struct{ Email, Password string }
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.createUser(r.Context(), req.Email, req.Password); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.login(w, r, req.Email, req.Password)
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct{ Email, Password string }
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.login(w, r, req.Email, req.Password)
}

func (s *Server) login(w http.ResponseWriter, r *http.Request, email, password string) {
	u, hash, err := s.store.GetUserByEmail(r.Context(), strings.ToLower(strings.TrimSpace(email)))
	if err != nil || bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		time.Sleep(300 * time.Millisecond)
		writeError(w, http.StatusUnauthorized, "invalid email or password")
		return
	}
	token := store.RandomToken("ses_")
	if err := s.store.CreateSession(r.Context(), u.ID, token, time.Now().Add(sessionTTL).UnixMilli()); err != nil {
		storeError(w, err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https", MaxAge: int(sessionTTL.Seconds()),
	})
	writeJSON(w, http.StatusOK, map[string]any{"user": u})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.store.DeleteSession(r.Context(), c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) createUser(ctx context.Context, email, password string) error {
	email = strings.ToLower(strings.TrimSpace(email))
	if !strings.Contains(email, "@") {
		return errString("a valid email is required")
	}
	if len(password) < 8 {
		return errString("password must have at least 8 characters")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	_, err = s.store.CreateUser(ctx, email, string(hash))
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return errString("user already exists")
	}
	return err
}

// EnsureAdmin creates the bootstrap admin from env when no users exist yet.
func (s *Server) EnsureAdmin(ctx context.Context, email, password string) error {
	if email == "" || password == "" || s.store.CountUsers(ctx) > 0 {
		return nil
	}
	return s.createUser(ctx, email, password)
}

type errString string

func (e errString) Error() string { return string(e) }

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"principal":     principal(r),
		"auth_disabled": s.cfg.AuthDisabled,
		"version":       s.cfg.Version,
	})
}

func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if p.Kind != "user" {
		writeError(w, http.StatusBadRequest, "only logged-in users can change a password")
		return
	}
	var req struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	u, hash, err := s.store.GetUserByEmail(r.Context(), p.Email)
	if err != nil || bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Current)) != nil {
		writeError(w, http.StatusBadRequest, "current password is wrong")
		return
	}
	if len(req.New) < 8 {
		writeError(w, http.StatusBadRequest, "password must have at least 8 characters")
		return
	}
	nh, _ := bcrypt.GenerateFromPassword([]byte(req.New), bcrypt.DefaultCost)
	if err := s.store.UpdateUserPassword(r.Context(), u.ID, string(nh)); err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
