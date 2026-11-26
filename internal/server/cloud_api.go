package server

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"chiperka-hub/internal/cloud"
	"chiperka-hub/internal/store"
)

// SetCloud attaches the autoscaler. Without it the cloud API reports "not configured".
func (s *Server) SetCloud(a *cloud.Autoscaler) { s.cloud = a }

// CloudHooks are the callbacks the autoscaler needs from the server.
func (s *Server) CloudHooks() cloud.Hooks {
	return cloud.Hooks{
		RemoveWorker: func(id, reason string) {
			s.failWorkerJobs(id, reason)
			ctx, cancel := bg()
			defer cancel()
			s.store.DeleteWorker(ctx, id)
			s.changed("workers")
		},
		Changed: func(what string) { s.changed(what) },
	}
}

func (s *Server) cloudRoutes(u func(string, http.HandlerFunc)) {
	u("GET /api/cloud", s.handleCloudOverview)
	u("GET /api/cloud/events", s.handleCloudEvents)
	u("GET /api/cloud/timeline", s.handleCloudTimeline)
	u("GET /api/cloud/costs", s.handleCloudCosts)
	u("GET /api/cloud/estimate", s.handleCloudEstimate)
	u("GET /api/cloud/settings", s.handleCloudSettingsGet)
	u("PUT /api/cloud/settings", s.handleCloudSettingsPut)
	u("POST /api/cloud/pause", s.handleCloudPause)
	u("POST /api/cloud/settings/preset", s.handleCloudPreset)
	u("POST /api/cloud/settings/revert/{id}", s.handleCloudRevert)
	u("POST /api/cloud/servers", s.handleCloudLaunch)
	u("POST /api/cloud/servers/{id}/terminate", s.handleCloudTerminate)
	u("POST /api/cloud/servers/{id}/extend", s.handleCloudExtend)
	u("POST /api/cloud/test/{provider}", s.handleCloudTest)
	u("POST /api/cloud/detect-ip", s.handleCloudDetectIP)
	u("GET /api/cloud/runs/{id}", s.handleCloudRunUsage)
}

func (s *Server) requireCloud(w http.ResponseWriter) bool {
	if s.cloud == nil {
		writeError(w, http.StatusServiceUnavailable, "the cloud autoscaler is not available on this hub")
		return false
	}
	return true
}

// requireJSON guards state-changing cloud endpoints against cross-site
// requests: a form on another site cannot send application/json without a
// CORS preflight, which the hub never allows. It matters most with
// CHIPERKA_AUTH=disabled, where no cookie or token is needed.
func requireJSON(w http.ResponseWriter, r *http.Request) bool {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		writeError(w, http.StatusUnsupportedMediaType, "send the request as application/json")
		return false
	}
	return true
}

func (s *Server) handleCloudPause(w http.ResponseWriter, r *http.Request) {
	if !s.requireCloud(w) || !requireJSON(w, r) {
		return
	}
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if err := s.cloud.SetEnabled(r.Context(), req.Enabled, s.principalName(r)); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.changed("cloud")
	writeJSON(w, http.StatusOK, map[string]any{"enabled": req.Enabled})
}

// cloudServerView adds live data from the worker and derived times to a server.
type cloudServerView struct {
	store.CloudServer
	WorkerStatus  string  `json:"worker_status"`
	WorkerPaused  bool    `json:"worker_paused"`
	RunningJobs   int     `json:"running_jobs"`
	Concurrency   int     `json:"concurrency"`
	CompletedJobs int     `json:"completed_jobs"`
	Cost          float64 `json:"cost"`
	PlannedStopAt int64   `json:"planned_stop_at"` // idle: when shutdown starts
	WaitsForHour  bool    `json:"waits_for_hour"`  // …at the end of the paid Hetzner hour
	PaidUntil     int64   `json:"paid_until"`      // Hetzner: end of the current paid hour
	Tunnel        bool    `json:"tunnel"`
}

func (s *Server) cloudServers(ctx context.Context, since int64) []cloudServerView {
	servers, _ := s.store.ListCloudServers(ctx, since)
	workers, _ := s.store.ListWorkers(ctx)
	wm := map[string]*store.Worker{}
	for i := range workers {
		wm[workers[i].ID] = &workers[i]
	}
	st := s.cloud.Settings()
	now := store.Now()
	out := make([]cloudServerView, 0, len(servers))
	for i := len(servers) - 1; i >= 0; i-- { // newest first
		c := servers[i]
		v := cloudServerView{CloudServer: c, Cost: cloud.ServerCost(&c, now), Tunnel: c.Active() && s.cloud.TunnelUp(c.ID), CompletedJobs: c.CompletedJobs}
		if w := wm[c.WorkerID]; w != nil {
			v.WorkerStatus, v.WorkerPaused, v.RunningJobs, v.Concurrency, v.CompletedJobs = w.Status, w.Paused, w.RunningJobs, w.Concurrency, w.CompletedJobs
		}
		if c.Active() {
			v.PlannedStopAt, v.WaitsForHour = cloud.PlannedStop(st, &c)
			if c.Provider == "hetzner" && c.ProviderCreatedAt > 0 {
				hour := int64(time.Hour / time.Millisecond)
				v.PaidUntil = c.ProviderCreatedAt + ((now-c.ProviderCreatedAt)/hour+1)*hour
			}
		}
		out = append(out, v)
	}
	return out
}

func (s *Server) handleCloudOverview(w http.ResponseWriter, r *http.Request) {
	if !s.requireCloud(w) {
		return
	}
	since := time.Now().Add(-24 * time.Hour).UnixMilli()
	writeJSON(w, http.StatusOK, map[string]any{
		"state":   s.cloud.State(r.Context()),
		"servers": s.cloudServers(r.Context(), since),
	})
}

func (s *Server) handleCloudEvents(w http.ResponseWriter, r *http.Request) {
	if !s.requireCloud(w) {
		return
	}
	q := r.URL.Query()
	before, _ := strconv.ParseInt(q.Get("before"), 10, 64)
	ev, err := s.store.ListCloudEvents(r.Context(), store.CloudEventFilter{
		ServerID: q.Get("server"), Kind: q.Get("kind"), BeforeID: before, Limit: queryInt(r, "limit", 100),
	})
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ev)
}

type timelineSegment struct {
	Status string `json:"status"`
	From   int64  `json:"from"`
	To     int64  `json:"to"`
}

// handleCloudTimeline returns the queue curve and one row of lifecycle segments
// per server for the last `hours` (default 24).
func (s *Server) handleCloudTimeline(w http.ResponseWriter, r *http.Request) {
	if !s.requireCloud(w) {
		return
	}
	hours := queryInt(r, "hours", 24)
	if hours < 1 || hours > 48 {
		hours = 24
	}
	now := store.Now()
	from := now - int64(hours)*int64(time.Hour/time.Millisecond)
	samples, _ := s.store.ListCloudSamples(r.Context(), from)
	servers, _ := s.store.ListCloudServersBetween(r.Context(), from, now)
	type row struct {
		ID       string            `json:"id"`
		Type     string            `json:"type"`
		Status   string            `json:"status"`
		Segments []timelineSegment `json:"segments"`
	}
	rows := []row{}
	for _, c := range servers {
		end := c.DeletedAt
		if end == 0 {
			end = now
		}
		marks := []struct {
			status string
			at     int64
		}{
			{store.CloudRequested, c.CreatedAt},
			{store.CloudStarting, c.ProviderCreatedAt},
			{store.CloudWorking, c.WorkerAt},
			{store.CloudIdle, c.IdleSince},
			{store.CloudDraining, c.DrainAt},
		}
		var segs []timelineSegment
		for i, m := range marks {
			if m.at == 0 {
				continue
			}
			to := end
			for _, n := range marks[i+1:] {
				if n.at > m.at {
					to = n.at
					break
				}
			}
			if to > m.at {
				segs = append(segs, timelineSegment{Status: m.status, From: m.at, To: to})
			}
		}
		rows = append(rows, row{ID: c.ID, Type: c.Type, Status: c.Status, Segments: segs})
	}
	writeJSON(w, http.StatusOK, map[string]any{"from": from, "to": now, "samples": samples, "servers": rows})
}

func (s *Server) handleCloudCosts(w http.ResponseWriter, r *http.Request) {
	if !s.requireCloud(w) {
		return
	}
	days := queryInt(r, "days", 30)
	if days < 1 || days > 400 {
		days = 30
	}
	now := time.Now()
	daily := s.cloud.DailyCosts(r.Context(), days, now)
	type month struct {
		Month string  `json:"month"`
		Cost  float64 `json:"cost"`
	}
	months := []month{}
	for i := 5; i >= 0; i-- {
		m := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location()).AddDate(0, -i, 0)
		end := m.AddDate(0, 1, 0)
		if end.After(now) {
			end = now
		}
		months = append(months, month{Month: m.Format("2006-01"), Cost: s.cloud.SpendBetween(r.Context(), m, end)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"daily": daily, "monthly": months, "currency": s.cloud.State(r.Context()).Currency})
}

// handleCloudEstimate returns what the UI needs to estimate run time and cost
// for a settings draft: test time of the last full run, start-up times and prices.
func (s *Server) handleCloudEstimate(w http.ResponseWriter, r *http.Request) {
	if !s.requireCloud(w) {
		return
	}
	ctx := r.Context()
	tests, sum, longest := s.store.RecentRunTestTime(ctx, 20)
	st := s.cloud.Settings()
	prices := map[string]map[string]float64{"hetzner": {}, "aws": {}}
	for name, ps := range map[string]cloud.ProviderSettings{"hetzner": st.Hetzner, "aws": st.AWS} {
		for _, t := range ps.Types {
			if p, err := s.cloud.Price(ctx, name, t); err == nil {
				prices[name][t] = p
			}
		}
	}
	// Measured start-up (server created → worker signed in), else the specification's estimates.
	startup := map[string]int64{"hetzner": 120_000, "aws": 180_000}
	measured := map[string][]int64{}
	servers, _ := s.store.ListCloudServersBetween(ctx, time.Now().AddDate(0, 0, -30).UnixMilli(), store.Now())
	for _, c := range servers {
		if c.WorkerAt > 0 && c.ProviderCreatedAt > 0 {
			measured[c.Provider] = append(measured[c.Provider], c.WorkerAt-c.ProviderCreatedAt)
		}
	}
	for p, v := range measured {
		var t int64
		for _, x := range v {
			t += x
		}
		startup[p] = t / int64(len(v))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"tests": tests, "test_time_ms": sum, "longest_test_ms": longest,
		"startup_ms": startup, "startup_measured": map[string]int{"hetzner": len(measured["hetzner"]), "aws": len(measured["aws"])},
		"prices": prices, "currency": map[string]string{"hetzner": "EUR", "aws": "USD"},
	})
}

func (s *Server) handleCloudSettingsGet(w http.ResponseWriter, r *http.Request) {
	if !s.requireCloud(w) {
		return
	}
	presets := []string{"economical", "balanced", "fast"}
	writeJSON(w, http.StatusOK, map[string]any{"settings": s.cloud.Settings(), "version": s.cloud.SettingsVersion(), "defaults": cloud.DefaultSettings(), "presets": presets})
}

func (s *Server) principalName(r *http.Request) string {
	if p := principal(r); p.Name != "" {
		return p.Name
	}
	return "ui"
}

func (s *Server) saveCloudSettings(w http.ResponseWriter, r *http.Request, next cloud.Settings, note string) {
	errs, err := s.cloud.SaveSettings(r.Context(), next, s.principalName(r), note)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(errs) > 0 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "invalid settings", "errors": errs})
		return
	}
	s.changed("cloud")
	writeJSON(w, http.StatusOK, map[string]any{"settings": s.cloud.Settings(), "version": s.cloud.SettingsVersion()})
}

func (s *Server) handleCloudSettingsPut(w http.ResponseWriter, r *http.Request) {
	if !s.requireCloud(w) || !requireJSON(w, r) {
		return
	}
	if v := r.Header.Get("If-Match"); v != "" && v != s.cloud.SettingsVersion() {
		writeError(w, http.StatusConflict, "the settings were changed meanwhile (e.g. paused from the Cloud page); reload and apply your edits again")
		return
	}
	next := s.cloud.Settings() // a deep copy: decoding must not touch the live settings
	if err := decodeJSON(r, &next); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	s.saveCloudSettings(w, r, next, "")
}

func (s *Server) handleCloudPreset(w http.ResponseWriter, r *http.Request) {
	if !s.requireCloud(w) || !requireJSON(w, r) {
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	decodeJSON(r, &req)
	apply, ok := cloud.Presets[req.Name]
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown preset")
		return
	}
	next := s.cloud.Settings()
	apply(&next)
	s.saveCloudSettings(w, r, next, "preset "+req.Name)
}

func (s *Server) handleCloudRevert(w http.ResponseWriter, r *http.Request) {
	if !s.requireCloud(w) || !requireJSON(w, r) {
		return
	}
	errs, err := s.cloud.RevertSettings(r.Context(), pathInt(r, "id"), s.principalName(r))
	if err != nil {
		storeError(w, err)
		return
	}
	if len(errs) > 0 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "the old settings are no longer valid", "errors": errs})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"settings": s.cloud.Settings()})
}

func (s *Server) handleCloudLaunch(w http.ResponseWriter, r *http.Request) {
	if !s.requireCloud(w) || !requireJSON(w, r) {
		return
	}
	var req struct {
		Count int `json:"count"`
	}
	decodeJSON(r, &req)
	servers, err := s.cloud.Launch(r.Context(), req.Count)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, servers)
}

func (s *Server) handleCloudTerminate(w http.ResponseWriter, r *http.Request) {
	if !s.requireCloud(w) || !requireJSON(w, r) {
		return
	}
	var req struct {
		Now bool `json:"now"`
	}
	decodeJSON(r, &req)
	if err := s.cloud.Terminate(r.Context(), r.PathValue("id"), req.Now); err != nil {
		if err == store.ErrNotFound {
			storeError(w, err)
			return
		}
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleCloudExtend(w http.ResponseWriter, r *http.Request) {
	if !s.requireCloud(w) || !requireJSON(w, r) {
		return
	}
	if err := s.cloud.Extend(r.Context(), r.PathValue("id")); err != nil {
		if err == store.ErrNotFound {
			storeError(w, err)
			return
		}
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleCloudTest(w http.ResponseWriter, r *http.Request) {
	if !s.requireCloud(w) || !requireJSON(w, r) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if err := s.cloud.TestConnection(ctx, r.PathValue("provider")); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleCloudDetectIP asks a public echo service which address the hub's
// outbound traffic comes from: the cloud firewall allows SSH only from there.
func (s *Server) handleCloudDetectIP(w http.ResponseWriter, r *http.Request) {
	if !requireJSON(w, r) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://checkip.amazonaws.com", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		writeError(w, http.StatusBadGateway, "cannot detect the public IP: "+err.Error())
		return
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 100))
	writeJSON(w, http.StatusOK, map[string]string{"ip": strings.TrimSpace(string(b))})
}

// handleCloudRunUsage tells on how many cloud servers a run ran and roughly
// what that cost: each test's share of its server's time.
func (s *Server) handleCloudRunUsage(w http.ResponseWriter, r *http.Request) {
	if !s.requireCloud(w) {
		return
	}
	ctx := r.Context()
	usage, err := s.store.RunCloudUsage(ctx, r.PathValue("id"))
	if err != nil {
		storeError(w, err)
		return
	}
	type srv struct {
		ID         string  `json:"id"`
		Type       string  `json:"type"`
		Provider   string  `json:"provider"`
		Jobs       int     `json:"jobs"`
		DurationMs int64   `json:"duration_ms"`
		Cost       float64 `json:"cost"`
	}
	out := struct {
		Servers  []srv   `json:"servers"`
		Jobs     int     `json:"jobs"`
		Cost     float64 `json:"cost"`
		Currency string  `json:"currency"`
	}{Servers: []srv{}}
	conc := s.cloud.Settings().WorkerConcurrency
	for _, u := range usage {
		id := "ci-" + strings.TrimPrefix(u.WorkerID, store.CloudWorkerPrefix)
		c, err := s.store.GetCloudServer(ctx, id)
		if err != nil {
			continue
		}
		cost := float64(u.DurationMs) / float64(time.Hour/time.Millisecond) / float64(max(conc, 1)) * c.HourlyPrice
		out.Servers = append(out.Servers, srv{ID: c.ID, Type: c.Type, Provider: c.Provider, Jobs: u.Jobs, DurationMs: u.DurationMs, Cost: cost})
		out.Jobs += u.Jobs
		out.Cost += cost
		out.Currency = c.Currency
	}
	writeJSON(w, http.StatusOK, out)
}
