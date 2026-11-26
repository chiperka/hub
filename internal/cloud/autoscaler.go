package cloud

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"chiperka-hub/internal/store"
)

const (
	settingsKey = "cloud.settings"
	hubIDKey    = "cloud.hub_id"

	tickInterval      = 5 * time.Second
	reconcileInterval = time.Minute
	sampleInterval    = 30 * time.Second
	extendBy          = 30 * time.Minute
	quotaBackoff      = 2 * time.Minute
	errorBackoff      = time.Minute
	historyKeep       = 48 * time.Hour
	workerLostAfter   = 2 * time.Minute // offline worker on a working server → replace the server
	startFailLimit    = 2               // consecutive failed starts before backing off
	startFailBackoff  = 10 * time.Minute
	reconcileGrace    = 5 * time.Minute // providers list new servers with a delay (AWS)
	eventsKeep        = 30 * 24 * time.Hour
	// SelfDestructAfter: every server powers itself off then (AWS terminates it,
	// the janitor deletes it on Hetzner). The janitor uses the same age.
	SelfDestructAfter = 5 * time.Hour
)

// Hooks connect the autoscaler to the HTTP server it lives in.
type Hooks struct {
	RemoveWorker func(id, reason string) // re-queue its jobs and delete it
	Changed      func(what string)       // "cloud" or "workers": push a live update to the UI
}

type Config struct {
	DataDir     string              // the hub's data directory; the SSH key lives in DataDir/cloud
	HubAddr     string              // the hub's HTTP listener as seen from the hub itself, e.g. 127.0.0.1:8080
	WorkerImage string              // default worker image
	TestTimeout time.Duration       // per-test timeout passed to workers
	Providers   map[string]Provider // only providers with credentials
}

type Autoscaler struct {
	cfg   Config
	store *store.Store
	hooks Hooks
	log   *slog.Logger

	hubID  string
	signer ssh.Signer
	hubPub string

	mu           sync.Mutex
	settings     Settings
	tunnels      map[string]*tunnel
	busy         map[string]bool // a provider call for this server is in flight
	queueSince   time.Time       // when the queue first crossed the lowest rule
	backoffUntil time.Time
	backoffWhy   string
	prepared     map[string]string // provider -> SSH CIDR it was prepared with
	prices       map[string]priceEntry
	lastState    string
	ctx          context.Context

	startFails int // consecutive servers whose worker never signed in

	queueDepth func(context.Context) int // tests replace it
}

// fallbackPrice is counted when a price cannot be looked up: deliberately high,
// so an unknown price can only make the budget stricter.
var fallbackPrice = map[string]float64{"hetzner": 1.0, "aws": 3.0}

type priceEntry struct {
	price float64
	at    time.Time
}

// New loads the settings and the hub identity. It fails only on local I/O errors.
func New(ctx context.Context, cfg Config, st *store.Store, hooks Hooks) (*Autoscaler, error) {
	a := &Autoscaler{
		cfg: cfg, store: st, hooks: hooks, log: slog.Default().With("component", "cloud"),
		tunnels: map[string]*tunnel{}, busy: map[string]bool{}, prepared: map[string]string{}, prices: map[string]priceEntry{},
	}
	a.queueDepth = st.CloudQueueDepth
	if a.cfg.TestTimeout <= 0 {
		a.cfg.TestTimeout = 5 * time.Minute
	}
	var err error
	if a.signer, a.hubPub, err = loadOrCreateHubKey(filepath.Join(cfg.DataDir, "cloud")); err != nil {
		return nil, fmt.Errorf("hub SSH key: %w", err)
	}
	a.hubID = st.GetSetting(ctx, hubIDKey)
	if a.hubID == "" {
		a.hubID = randHex(4)
		if err := st.SetSetting(ctx, hubIDKey, a.hubID); err != nil {
			return nil, err
		}
	}
	a.settings = a.loadSettings(ctx)
	return a, nil
}

func (a *Autoscaler) loadSettings(ctx context.Context) Settings {
	s := DefaultSettings()
	if raw := a.store.GetSetting(ctx, settingsKey); raw != "" {
		if err := json.Unmarshal([]byte(raw), &s); err != nil {
			a.log.Error("invalid stored cloud settings, using defaults", "err", err)
			s = DefaultSettings()
		}
	}
	s.Normalize()
	return s
}

// Configured reports whether any provider has credentials. Without one the
// autoscaler does nothing and the hub behaves exactly as before.
func (a *Autoscaler) Configured() bool { return len(a.cfg.Providers) > 0 }

func (a *Autoscaler) HubPublicKey() string { return a.hubPub }

// Settings returns a deep copy: callers may decode into it or modify it freely.
func (a *Autoscaler) Settings() Settings {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.settings.Clone()
}

func (a *Autoscaler) provider(name string) Provider { return a.cfg.Providers[name] }

// ── main loop ───────────────────────────────────────────────────────────────

// Run drives the autoscaler until ctx ends. Servers keep running when the hub
// stops; on the next start it picks them up from the database.
func (a *Autoscaler) Run(ctx context.Context) {
	a.mu.Lock()
	a.ctx = ctx
	a.mu.Unlock()
	a.resume(ctx)
	tick := time.NewTicker(tickInterval)
	defer tick.Stop()
	var lastReconcile, lastSample, lastPurge time.Time
	for {
		now := time.Now()
		a.tick(ctx, now)
		if now.Sub(lastReconcile) >= reconcileInterval {
			lastReconcile = now
			go a.reconcile(ctx)
		}
		if now.Sub(lastSample) >= sampleInterval {
			lastSample = now
			a.sample(ctx, now)
		}
		if now.Sub(lastPurge) >= time.Hour {
			lastPurge = now
			a.store.PurgeCloudHistory(ctx, now.Add(-historyKeep).UnixMilli(), now.Add(-eventsKeep).UnixMilli())
		}
		select {
		case <-ctx.Done():
			a.mu.Lock()
			for _, t := range a.tunnels {
				t.cancel()
			}
			a.mu.Unlock()
			return
		case <-tick.C:
		}
	}
}

// resume handles servers left over from before a restart.
func (a *Autoscaler) resume(ctx context.Context) {
	servers, err := a.store.ListCloudServers(ctx, store.Now())
	if err != nil {
		return
	}
	for _, s := range servers {
		if s.Active() && s.Status == store.CloudRequested && s.ProviderID == "" {
			// The create call died with the old process. If it reached the
			// provider, reconcile deletes the orphan.
			a.finishNow(ctx, s.ID, store.CloudFailed, "hub restarted while the server was being created", false)
		}
	}
	if n := len(servers); n > 0 {
		a.event(ctx, "", "decision", fmt.Sprintf("hub started, picking up %d cloud server(s)", n), nil)
	}
}

func (a *Autoscaler) tick(ctx context.Context, now time.Time) {
	st := a.Settings()
	queue := a.queueDepth(ctx)
	servers, err := a.store.ListCloudServers(ctx, now.UnixMilli())
	if err != nil {
		return
	}
	workers := a.workerMap(ctx)
	active := []store.CloudServer{}
	for i := range servers {
		if servers[i].Active() {
			active = append(active, servers[i])
		}
	}
	keep := countKeep(active)
	ids := map[string]bool{}
	for i := range active {
		ids[active[i].ID] = true
		if a.provider(active[i].Provider) == nil {
			continue // credentials gone: never mark it deleted while it may still run (see State)
		}
		a.advance(ctx, st, &active[i], workers[active[i].WorkerID], queue, &keep, now)
	}
	a.sweepTunnels(ids)
	a.scale(ctx, st, queue, active, now)
}

// sweepTunnels stops tunnels of servers that are no longer active.
func (a *Autoscaler) sweepTunnels(active map[string]bool) {
	a.mu.Lock()
	var stale []string
	for id := range a.tunnels {
		if !active[id] {
			stale = append(stale, id)
		}
	}
	a.mu.Unlock()
	for _, id := range stale {
		a.stopTunnel(id)
	}
}

func (a *Autoscaler) workerMap(ctx context.Context) map[string]*store.Worker {
	ws, _ := a.store.ListWorkers(ctx)
	m := map[string]*store.Worker{}
	for i := range ws {
		m[ws[i].ID] = &ws[i]
	}
	return m
}

// advance moves one server through its lifecycle.
// Every update checks that the row is still in the state this tick saw: a
// manual stop or a finished deletion in between must not be overwritten.
func (a *Autoscaler) advance(ctx context.Context, st Settings, s *store.CloudServer, w *store.Worker, queue int, keep *int, now time.Time) {
	ms := now.UnixMilli()
	if s.Status != store.CloudRequested {
		a.ensureTunnel(s)
	}
	switch s.Status {
	case store.CloudRequested:
		if s.ProviderID == "" && !a.isBusy(s.ID) {
			a.goCreate(s.ID)
		}

	case store.CloudStarting:
		if w != nil && w.Status == store.WorkerOnline {
			at := w.RegisteredAt // exact, not rounded to the tick
			if at == 0 {
				at = ms
			}
			if a.update(ctx, s.ID, func(c *store.CloudServer) bool {
				if c.Status != store.CloudStarting {
					return false
				}
				c.Status, c.WorkerAt = store.CloudWorking, at
				return true
			}) {
				a.startSucceeded()
				a.event(ctx, s.ID, "server", fmt.Sprintf("worker signed in, start took %s", since(s.ProviderCreatedAt, at)), nil)
			}
			return
		}
		if s.PublicIP == "" && !a.isBusy(s.ID) {
			go a.reconcile(ctx)
		}
		start := s.ProviderCreatedAt
		if start == 0 {
			start = s.CreatedAt
		}
		if ms-start > int64(st.StartTimeoutSec)*1000 && !a.isBusy(s.ID) {
			why := fmt.Sprintf("worker did not sign in within %s", secs(st.StartTimeoutSec))
			a.startFailed(why)
			a.goDestroy(s.ID, store.CloudFailed, why)
		}

	case store.CloudWorking, store.CloudIdle:
		// A worker that stopped responding (crash, broken tunnel) is replaced
		// instead of blocking a slot until the maximum lifetime.
		if w == nil || (w.Status != store.WorkerOnline && ms-w.LastHeartbeat > workerLostAfter.Milliseconds()) {
			a.goDestroy(s.ID, store.CloudFailed, "the worker stopped responding")
			return
		}
		if s.FirstTestAt == 0 && w != nil && w.CompletedJobs > 0 {
			if ts := a.store.WorkerFirstCompletion(ctx, s.WorkerID); ts > 0 {
				a.update(ctx, s.ID, func(c *store.CloudServer) bool { c.FirstTestAt = ts; return true })
				a.event(ctx, s.ID, "server", fmt.Sprintf("first test finished %s after the server was created", since(s.ProviderCreatedAt, ts)), nil)
			}
		}
		busy := w != nil && w.RunningJobs > 0
		if busy || queue > 0 {
			if s.Status == store.CloudIdle || s.IdleSince != 0 {
				ok := a.update(ctx, s.ID, func(c *store.CloudServer) bool {
					if c.Status != s.Status {
						return false
					}
					c.Status, c.IdleSince = store.CloudWorking, 0
					return true
				})
				if ok && s.Status == store.CloudIdle {
					a.event(ctx, s.ID, "server", "back to work", nil)
				}
			}
		} else if s.IdleSince == 0 {
			if !a.update(ctx, s.ID, func(c *store.CloudServer) bool {
				if c.Status != s.Status {
					return false
				}
				c.Status, c.IdleSince = store.CloudIdle, ms
				return true
			}) {
				return
			}
			s.IdleSince = ms
		}
		if s.ProviderCreatedAt > 0 && ms-s.ProviderCreatedAt > int64(st.MaxLifetimeSec)*1000 {
			a.drain(ctx, s.ID, fmt.Sprintf("maximum lifetime of %s reached", secs(st.MaxLifetimeSec)))
			return
		}
		if s.IdleSince > 0 {
			if stop, why := shouldStopIdle(st, s, *keep, now); stop && a.drain(ctx, s.ID, why) {
				*keep-- // the warm pool check of the next server sees this one gone
			}
		}

	case store.CloudDraining:
		if w != nil && !w.Paused {
			a.store.SetWorkerPaused(ctx, w.ID, true)
			a.notify("workers")
		}
		switch {
		case w == nil || w.Status != store.WorkerOnline || w.RunningJobs == 0:
			a.goDestroy(s.ID, store.CloudDeleted, s.EndReason)
		case s.DrainAt == drainNow:
			a.goDestroy(s.ID, store.CloudDeleted, s.EndReason+"; running tests were re-queued")
		case ms-s.DrainAt > int64(st.DrainTimeoutSec)*1000:
			a.goDestroy(s.ID, store.CloudDeleted, s.EndReason+"; running tests did not finish in time and were re-queued")
		}
	}
}

// countKeep counts servers that will stay (for the warm pool check).
func countKeep(active []store.CloudServer) int {
	n := 0
	for _, s := range active {
		if s.Status != store.CloudDraining {
			n++
		}
	}
	return n
}

// shouldStopIdle decides whether an idle server goes, and explains why.
func shouldStopIdle(st Settings, s *store.CloudServer, keep int, now time.Time) (bool, string) {
	ms := now.UnixMilli()
	idle := time.Duration(ms-s.IdleSince) * time.Millisecond
	if idle < secs(st.IdleTimeoutSec) {
		return false, ""
	}
	if s.HoldUntil > ms {
		return false, ""
	}
	if warm := st.WarmTarget(now); warm > 0 && keep <= warm {
		return false, ""
	}
	if s.Provider == "hetzner" && st.HetznerHoldHour && s.ProviderCreatedAt > 0 {
		into := time.Duration(ms-s.ProviderCreatedAt) * time.Millisecond % time.Hour
		if into < time.Hour-secs(st.HetznerHourReserveSec) {
			return false, ""
		}
		return true, fmt.Sprintf("idle for %s, the paid hour is ending", roundDur(idle))
	}
	return true, fmt.Sprintf("idle for %s", roundDur(idle))
}

// PlannedStop returns when an idle server will start shutting down (0 = not
// planned) and whether it waits for the end of a paid Hetzner hour.
func PlannedStop(st Settings, s *store.CloudServer) (int64, bool) {
	if s.Status != store.CloudIdle || s.IdleSince == 0 {
		return 0, false
	}
	at := s.IdleSince + int64(st.IdleTimeoutSec)*1000
	if s.HoldUntil > at {
		at = s.HoldUntil
	}
	if s.Provider == "hetzner" && st.HetznerHoldHour && s.ProviderCreatedAt > 0 {
		hour := int64(time.Hour / time.Millisecond)
		reserve := int64(st.HetznerHourReserveSec) * 1000
		into := (at - s.ProviderCreatedAt) % hour
		if into < hour-reserve {
			at += hour - reserve - into
		}
		return at, true
	}
	return at, false
}

// ── scaling ─────────────────────────────────────────────────────────────────

// Status values shown at the top of the Cloud page.
const (
	StatusRunning       = "running"
	StatusPaused        = "paused"
	StatusBudget        = "budget"
	StatusNotConfigured = "not_configured"
	StatusBackoff       = "backoff"
)

func (a *Autoscaler) scale(ctx context.Context, st Settings, queue int, active []store.CloudServer, now time.Time) {
	state := a.computeState(ctx, st, now)
	a.mu.Lock()
	changed := state != a.lastState
	a.lastState = state
	a.mu.Unlock()
	if changed {
		switch state {
		case StatusBudget:
			a.event(ctx, "", "decision", fmt.Sprintf("monthly budget of %.2f %s reached, no new servers this month", st.MonthlyBudget, a.currency(st)), nil)
		case StatusPaused:
			a.event(ctx, "", "decision", "autoscaler paused, no new servers", nil)
		case StatusRunning:
			a.notify("cloud")
		}
	}
	if state != StatusRunning {
		a.mu.Lock()
		a.queueSince = time.Time{}
		a.mu.Unlock()
		return
	}

	ps := st.Active()
	if len(ps.Types) == 0 || len(ps.Locations) == 0 {
		return // invalid settings never pass Validate; only a hand-edited database gets here
	}
	want := ps.TargetFor(queue)
	a.mu.Lock()
	if want == 0 {
		a.queueSince = time.Time{}
	} else if a.queueSince.IsZero() {
		a.queueSince = now
	}
	held := now.Sub(a.queueSince)
	a.mu.Unlock()
	if want > 0 && held < secs(st.ScaleUpDelaySec) {
		want = 0 // short spikes don't start servers
	}
	warm := st.WarmTarget(now)
	target := max(want, warm)

	counted, total := 0, len(active)
	for _, s := range active {
		if s.Provider == st.Provider && s.Status != store.CloudDraining {
			counted++
		}
	}
	n := target - counted
	if room := ps.MaxServers - total; n > room {
		n = room
	}
	if n <= 0 {
		return
	}
	reason := fmt.Sprintf("started for a queue of %d tests", queue)
	msg := fmt.Sprintf("queue %d tests → %d server(s) wanted, %d running; starting %d× %s", queue, target, counted, n, ps.Types[0])
	if want < warm {
		reason = "warm pool"
		msg = fmt.Sprintf("warm pool wants %d server(s), %d running; starting %d", warm, counted, n)
	}
	a.event(ctx, "", "decision", msg, nil)
	for i := 0; i < n; i++ {
		a.request(ctx, st, reason, false)
	}
}

func (a *Autoscaler) computeState(ctx context.Context, st Settings, now time.Time) string {
	if a.provider(st.Provider) == nil {
		return StatusNotConfigured
	}
	if !st.Enabled {
		return StatusPaused
	}
	if st.MonthlyBudget > 0 && a.MonthSpend(ctx, now) >= st.MonthlyBudget {
		return StatusBudget
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if now.Before(a.backoffUntil) {
		return StatusBackoff
	}
	return StatusRunning
}

// request records a new server and starts creating it.
func (a *Autoscaler) request(ctx context.Context, st Settings, reason string, manual bool) (*store.CloudServer, error) {
	s := &store.CloudServer{
		ID: "ci-" + randHex(4), Provider: st.Provider, Status: store.CloudRequested, Reason: reason,
		Manual: manual, CreatedAt: store.Now(), Currency: a.currency(st),
	}
	if err := a.store.InsertCloudServer(ctx, s); err != nil {
		return nil, err
	}
	a.event(ctx, s.ID, "server", "requested: "+reason, nil)
	a.goCreate(s.ID)
	return s, nil
}

func (a *Autoscaler) currency(st Settings) string {
	if p := a.provider(st.Provider); p != nil {
		return p.Currency()
	}
	if st.Provider == "aws" {
		return "USD"
	}
	return "EUR"
}

// ── provider calls (asynchronous, one at a time per server) ────────────────

func (a *Autoscaler) isBusy(id string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.busy[id]
}

func (a *Autoscaler) claim(id string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.busy[id] {
		return false
	}
	a.busy[id] = true
	return true
}

func (a *Autoscaler) release(id string) {
	a.mu.Lock()
	delete(a.busy, id)
	a.mu.Unlock()
}

func (a *Autoscaler) bg() context.Context {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ctx != nil {
		return a.ctx
	}
	return context.Background()
}

func (a *Autoscaler) goCreate(id string) {
	if !a.claim(id) {
		return
	}
	go func() {
		defer a.release(id)
		ctx, cancel := context.WithTimeout(a.bg(), 5*time.Minute)
		defer cancel()
		a.create(ctx, id)
	}()
}

func (a *Autoscaler) create(ctx context.Context, id string) {
	st := a.Settings()
	s, err := a.store.GetCloudServer(ctx, id)
	if err != nil || s.Status != store.CloudRequested {
		return
	}
	p := a.provider(s.Provider)
	if p == nil {
		a.finish(ctx, id, store.CloudFailed, s.Provider+" is not configured", false)
		return
	}
	if err := a.prepare(ctx, p, st); err != nil {
		a.backoff(errorBackoff, err.Error())
		a.finish(ctx, id, store.CloudFailed, "cannot prepare "+p.Name()+": "+err.Error(), false)
		return
	}

	// The worker gets its own token; its name is the server name.
	workerID := store.CloudWorkerPrefix + strings.TrimPrefix(id, "ci-")
	tokenID, token, err := a.store.CreateToken(ctx, "cloud worker "+id, store.TokenWorker)
	if err == nil {
		err = a.store.CreateWorker(ctx, workerID, id, tokenID, false)
	}
	if err != nil {
		a.finish(ctx, id, store.CloudFailed, "cannot create the worker: "+err.Error(), false)
		return
	}
	a.update(ctx, id, func(c *store.CloudServer) bool { c.WorkerID = workerID; return true })
	a.notify("workers")

	hostPriv, hostPub, _, err := newKey(id)
	if err != nil {
		a.finish(ctx, id, store.CloudFailed, err.Error(), false)
		return
	}
	image := st.WorkerImage
	if image == "" {
		image = a.cfg.WorkerImage
	}
	ud, err := WorkerUserData{
		Provider: p.Name(), HostKeyPrivate: string(hostPriv), HostKeyPublic: hostPub, HubPublicKey: a.hubPub,
		WorkerImage: image, WorkerName: id, WorkerToken: token, Concurrency: st.WorkerConcurrency,
		TimeoutSec: int(a.cfg.TestTimeout.Seconds()), SelfDestructMin: int(SelfDestructAfter.Minutes()),
	}.Render()
	if err != nil {
		a.finish(ctx, id, store.CloudFailed, err.Error(), false)
		return
	}

	ps := st.Active()
	if s.Provider == "aws" {
		ps = &st.AWS
	} else {
		ps = &st.Hetzner
	}
	labels := managedLabels(a.hubID)
	var inst *Instance
	var lastErr error
	started := time.Now()
try:
	for _, typ := range ps.Types {
		for _, loc := range ps.Locations {
			inst, lastErr = p.Create(ctx, CreateRequest{
				Name: id, Type: typ, Location: loc, Image: st.ServerImage, UserData: ud, DiskGB: st.DiskGB, Labels: labels,
				Spot: st.AWSSpot && p.Name() == "aws", SpotFallback: st.AWSSpotToOnDemand,
			})
			if lastErr == nil {
				break try
			}
			if errors.Is(lastErr, ErrUnavailable) {
				a.event(ctx, id, "provider", fmt.Sprintf("%s in %s not available, trying the next option", typ, loc), errData(lastErr))
				continue
			}
			break try
		}
	}
	if lastErr != nil {
		why := "provider error: " + lastErr.Error()
		switch {
		case errors.Is(lastErr, ErrQuota):
			a.backoff(quotaBackoff, "provider quota reached: "+lastErr.Error())
			why = "provider quota reached: " + lastErr.Error()
		case errors.Is(lastErr, ErrUnavailable):
			a.backoff(errorBackoff, "no configured server type is available")
			why = "no configured server type or location is available"
		default:
			a.backoff(errorBackoff, lastErr.Error())
		}
		a.finish(ctx, id, store.CloudFailed, why, false)
		return
	}
	price, err := a.price(ctx, p, inst.Type, inst.Location, inst.Spot)
	for i := 0; err != nil && i < 2; i++ {
		sleep(ctx, 2*time.Second)
		price, err = a.price(ctx, p, inst.Type, inst.Location, inst.Spot)
	}
	if err != nil {
		// Never count a server as free: that would bypass the budget.
		price = fallbackPrice[p.Name()]
		a.event(ctx, id, "provider", fmt.Sprintf("price unknown (%v), counting %.2f %s/h for the budget", err, price, p.Currency()), nil)
	}
	created := inst.CreatedAt.UnixMilli()
	if inst.CreatedAt.IsZero() {
		created = store.Now()
	}
	a.update(ctx, id, func(c *store.CloudServer) bool {
		c.ProviderID, c.Type, c.Location, c.Spot, c.PublicIP = inst.ID, inst.Type, inst.Location, inst.Spot, inst.PublicIP
		c.HostKey, c.HourlyPrice, c.Currency, c.ProviderCreatedAt = hostPub, price, p.Currency(), created
		if c.Status == store.CloudRequested { // stays draining when stopped while being created
			c.Status = store.CloudStarting
		}
		return true
	})
	kind := ""
	if inst.Spot {
		kind = " (spot)"
	} else if p.Name() == "aws" {
		kind = " (on-demand)"
	}
	a.event(ctx, id, "server", fmt.Sprintf("created %s%s in %s in %s", inst.Type, kind, inst.Location, roundDur(time.Since(started))), nil)
}

func (a *Autoscaler) prepare(ctx context.Context, p Provider, st Settings) error {
	if st.SSHAllowCIDR == "" {
		return fmt.Errorf("set the IP address SSH may come from (Cloud → Settings → Network)")
	}
	a.mu.Lock()
	done := a.prepared[p.Name()] == st.SSHAllowCIDR
	a.mu.Unlock()
	if done {
		return nil
	}
	if err := p.Prepare(ctx, PrepareRequest{HubID: a.hubID, SSHAllowCIDR: st.SSHAllowCIDR, HubPublicKey: a.hubPub}); err != nil {
		return err
	}
	a.mu.Lock()
	a.prepared[p.Name()] = st.SSHAllowCIDR
	a.mu.Unlock()
	return nil
}

// startFailed counts servers whose worker never signed in. Several in a row
// mean something systematic (image, firewall, SSH source IP): stop creating
// servers for a while instead of paying for one broken server after another.
func (a *Autoscaler) startFailed(why string) {
	a.mu.Lock()
	a.startFails++
	n := a.startFails
	a.mu.Unlock()
	if n >= startFailLimit {
		a.backoff(startFailBackoff, fmt.Sprintf("%d servers in a row did not start (%s); check the worker image, the SSH source IP and the provider firewall", n, why))
	}
}

func (a *Autoscaler) startSucceeded() {
	a.mu.Lock()
	a.startFails = 0
	a.mu.Unlock()
}

func (a *Autoscaler) backoff(d time.Duration, why string) {
	a.mu.Lock()
	a.backoffUntil = time.Now().Add(d)
	a.backoffWhy = why
	a.mu.Unlock()
}

// drain stops the worker from taking new tests; the server is deleted once
// its running tests finish.
func (a *Autoscaler) drain(ctx context.Context, id, why string) bool {
	return a.stop(ctx, id, why, false)
}

// drainNow marks a server to be deleted on the next tick without waiting for
// its tests. It is stored, so a failed delete is retried.
const drainNow = 1

func (a *Autoscaler) stop(ctx context.Context, id, why string, now bool) bool {
	ok := a.update(ctx, id, func(c *store.CloudServer) bool {
		if !c.Active() || (c.Status == store.CloudDraining && (!now || c.DrainAt == drainNow)) {
			return false
		}
		c.Status, c.EndReason = store.CloudDraining, why
		c.DrainAt = store.Now()
		if now {
			c.DrainAt = drainNow
		}
		return true
	})
	if !ok {
		return false
	}
	if s, err := a.store.GetCloudServer(ctx, id); err == nil && s.WorkerID != "" {
		a.store.SetWorkerPaused(ctx, s.WorkerID, true)
		a.notify("workers")
	}
	a.event(ctx, id, "server", "shutting down: "+why, nil)
	return true
}

func (a *Autoscaler) goDestroy(id, status, why string) {
	if !a.claim(id) {
		return
	}
	go func() {
		defer a.release(id)
		ctx, cancel := context.WithTimeout(a.bg(), 2*time.Minute)
		defer cancel()
		a.finish(ctx, id, status, why, true)
	}()
}

// finishNow runs finish unless another provider call for the server is in
// flight (that call decides the server's fate). It reports whether it ran.
func (a *Autoscaler) finishNow(ctx context.Context, id, status, why string, deleteAtProvider bool) bool {
	if !a.claim(id) {
		return false
	}
	defer a.release(id)
	return a.finish(ctx, id, status, why, deleteAtProvider)
}

// finish deletes the server at the provider (when deleteAtProvider), removes its
// worker and records the final state.
func (a *Autoscaler) finish(ctx context.Context, id, status, why string, deleteAtProvider bool) bool {
	s, err := a.store.GetCloudServer(ctx, id)
	if err != nil || !s.Active() {
		return false
	}
	if deleteAtProvider && s.ProviderID != "" {
		p := a.provider(s.Provider)
		if p == nil {
			a.event(ctx, id, "error", s.Provider+" is no longer configured: delete the server at the provider by hand", nil)
			return false
		}
		if err := p.Delete(ctx, s.ProviderID); err != nil {
			a.event(ctx, id, "error", "delete failed, retrying: "+err.Error(), nil)
			return false // the next tick retries
		}
	}
	a.stopTunnel(id)
	done := 0
	if s.WorkerID != "" {
		if w, err := a.store.GetWorker(ctx, s.WorkerID); err == nil {
			done = w.CompletedJobs
		}
	}
	if s.WorkerID != "" && a.hooks.RemoveWorker != nil {
		a.hooks.RemoveWorker(s.WorkerID, "cloud server "+id+" was removed")
	}
	a.update(ctx, id, func(c *store.CloudServer) bool {
		c.Status, c.EndReason, c.DeletedAt = status, why, store.Now()
		if done > 0 {
			c.CompletedJobs = done
		}
		return true
	})
	verb := "deleted"
	if status == store.CloudFailed {
		verb = "failed"
	}
	a.event(ctx, id, "server", verb+": "+why, nil)
	a.notify("workers")
	return true
}

// update applies fn to the stored server under the lock and saves it when fn
// returns true. It reports whether the row was changed.
func (a *Autoscaler) update(ctx context.Context, id string, fn func(*store.CloudServer) bool) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	s, err := a.store.GetCloudServer(ctx, id)
	if err != nil || !fn(s) {
		return false
	}
	if err := a.store.SaveCloudServer(ctx, s); err != nil {
		a.log.Error("save cloud server", "server", id, "err", err)
		return false
	}
	go a.notify("cloud")
	return true
}

// ── tunnels ─────────────────────────────────────────────────────────────────

func (a *Autoscaler) ensureTunnel(s *store.CloudServer) {
	if s.PublicIP == "" || s.HostKey == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.tunnels[s.ID]; ok || a.ctx == nil {
		return
	}
	key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(s.HostKey))
	if err != nil {
		a.log.Error("invalid host key", "server", s.ID, "err", err)
		return
	}
	id := s.ID
	t := &tunnel{
		serverID: id, addr: joinHostPort(s.PublicIP, "22"), hostKey: key, signer: a.signer, hubAddr: a.cfg.HubAddr,
		logf: func(f string, args ...any) { a.log.Warn(fmt.Sprintf(f, args...)) },
		onBoot: func() {
			ctx := a.bg()
			var first bool
			var created int64
			a.update(ctx, id, func(c *store.CloudServer) bool {
				if c.BootAt != 0 || !c.Active() {
					return false
				}
				first, created, c.BootAt = true, c.ProviderCreatedAt, store.Now()
				return true
			})
			if first {
				a.event(ctx, id, "server", fmt.Sprintf("system booted (%s)", since(created, store.Now())), nil)
			}
		},
		onUp: func(first bool) {
			ctx := a.bg()
			if !first {
				a.event(ctx, id, "server", "tunnel restored", nil)
				return
			}
			var created int64
			a.update(ctx, id, func(c *store.CloudServer) bool {
				created = c.ProviderCreatedAt
				if c.TunnelAt != 0 || !c.Active() {
					return false
				}
				c.TunnelAt = store.Now()
				return true
			})
			a.event(ctx, id, "server", fmt.Sprintf("tunnel connected (%s)", since(created, store.Now())), nil)
		},
		onDown: func(err error) {
			msg := "tunnel dropped, reconnecting"
			if err != nil {
				msg += ": " + err.Error()
			}
			a.event(a.bg(), id, "server", msg, nil)
		},
	}
	a.tunnels[id] = t
	t.start(a.ctx)
}

func (a *Autoscaler) stopTunnel(id string) {
	a.mu.Lock()
	t := a.tunnels[id]
	delete(a.tunnels, id)
	a.mu.Unlock()
	if t != nil {
		t.stop()
	}
}

// TunnelUp reports whether the hub currently holds a tunnel to the server.
func (a *Autoscaler) TunnelUp(id string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, ok := a.tunnels[id]
	return ok
}

func joinHostPort(host, port string) string {
	if strings.Contains(host, ":") {
		return "[" + host + "]:" + port
	}
	return host + ":" + port
}

// ── reconcile with the provider ─────────────────────────────────────────────

// reconcile compares the database with what the providers report: it notices
// servers that vanished (spot interruptions, manual deletion), fills in public
// IPs, and deletes managed servers this hub does not know.
func (a *Autoscaler) reconcile(ctx context.Context) {
	if !a.claim("~reconcile") {
		return
	}
	defer a.release("~reconcile")
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	servers, err := a.store.ListCloudServers(ctx, store.Now())
	if err != nil {
		return
	}
	for name, p := range a.cfg.Providers {
		insts, err := p.List(ctx)
		if err != nil {
			if !errors.Is(err, ErrNotConfigured) {
				a.log.Warn("cloud reconcile failed", "provider", name, "err", err)
			}
			continue
		}
		byID := map[string]*Instance{}
		for i := range insts {
			byID[insts[i].ID] = &insts[i]
		}
		known := map[string]bool{}
		creating := map[string]bool{}
		for _, s := range servers {
			if s.Provider != name || !s.Active() {
				continue
			}
			if s.ProviderID == "" {
				creating[s.ID] = true
				continue
			}
			known[s.ProviderID] = true
			inst := byID[s.ProviderID]
			switch {
			case inst != nil && inst.SpotInterrupted():
				if a.finishNow(ctx, s.ID, store.CloudDeleted, "taken back by AWS (spot)", false) {
					a.event(ctx, s.ID, "spot", "AWS took the spot server back, its tests are re-queued", nil)
				}
			case inst == nil || inst.State == StateTerminated:
				// Providers list new servers with a delay: right after creation a
				// missing server is not a vanished one.
				if time.Since(time.UnixMilli(s.ProviderCreatedAt)) < reconcileGrace {
					continue
				}
				a.finishNow(ctx, s.ID, store.CloudDeleted, "server disappeared at the provider", false)
			case inst.State == StateStopped:
				a.goDestroy(s.ID, store.CloudDeleted, "server powered off")
			case inst.PublicIP != "" && inst.PublicIP != s.PublicIP:
				a.update(ctx, s.ID, func(c *store.CloudServer) bool { c.PublicIP = inst.PublicIP; return c.Active() })
			}
		}
		for _, inst := range insts {
			if inst.Labels[LabelHub] != a.hubID || known[inst.ID] || creating[inst.Name] || inst.State == StateTerminated {
				continue
			}
			if time.Since(inst.CreatedAt) < 2*time.Minute {
				continue // a create call may still be returning
			}
			if err := p.Delete(ctx, inst.ID); err == nil {
				a.event(ctx, "", "decision", fmt.Sprintf("deleted %s (%s): the hub has no record of it", inst.Name, inst.ID), nil)
			}
		}
	}
}

// ── manual actions ──────────────────────────────────────────────────────────

// Launch starts n servers by hand, e.g. before a demo. They go away on their own when idle.
func (a *Autoscaler) Launch(ctx context.Context, n int) ([]store.CloudServer, error) {
	st := a.Settings()
	if a.provider(st.Provider) == nil {
		return nil, fmt.Errorf("%s is not configured on this hub", st.Provider)
	}
	if n < 1 {
		n = 1
	}
	servers, err := a.store.ListCloudServers(ctx, store.Now())
	if err != nil {
		return nil, err
	}
	total := 0
	for _, s := range servers {
		if s.Active() {
			total++
		}
	}
	if total+n > st.Active().MaxServers {
		return nil, fmt.Errorf("that would exceed the maximum of %d servers (%d running)", st.Active().MaxServers, total)
	}
	if st.MonthlyBudget > 0 && a.MonthSpend(ctx, time.Now()) >= st.MonthlyBudget {
		return nil, fmt.Errorf("the monthly budget is used up")
	}
	out := []store.CloudServer{}
	for i := 0; i < n; i++ {
		s, err := a.request(ctx, st, "started manually", true)
		if err != nil {
			return out, err
		}
		out = append(out, *s)
	}
	return out, nil
}

// Terminate shuts a server down: gracefully (running tests finish) or at once
// (running tests are re-queued).
func (a *Autoscaler) Terminate(ctx context.Context, id string, now bool) error {
	s, err := a.store.GetCloudServer(ctx, id)
	if err != nil {
		return err
	}
	if !s.Active() {
		return fmt.Errorf("the server is already gone")
	}
	// Stored as a state, so a delete that fails is retried on the next tick.
	// A server still being created is removed right after its create call.
	now = now || s.Status == store.CloudRequested || s.Status == store.CloudStarting
	a.stop(ctx, id, "stopped manually", now)
	return nil
}

// Extend keeps an idle server for another 30 minutes, e.g. while debugging.
func (a *Autoscaler) Extend(ctx context.Context, id string) error {
	s, err := a.store.GetCloudServer(ctx, id)
	if err != nil {
		return err
	}
	if !s.Active() || s.Status == store.CloudDraining {
		return fmt.Errorf("the server is shutting down")
	}
	until := time.Now().Add(extendBy).UnixMilli()
	if !a.update(ctx, id, func(c *store.CloudServer) bool {
		if !c.Active() || c.Status == store.CloudDraining {
			return false
		}
		c.HoldUntil = until
		return true
	}) {
		return fmt.Errorf("the server is shutting down")
	}
	a.event(ctx, id, "server", "kept for another 30 minutes", nil)
	return nil
}

// TestConnection checks a provider's credentials.
func (a *Autoscaler) TestConnection(ctx context.Context, name string) error {
	p := a.provider(name)
	if p == nil {
		return ErrNotConfigured
	}
	return p.Check(ctx)
}

// ── settings ────────────────────────────────────────────────────────────────

// SaveSettings validates and stores new settings; they apply on the next tick
// (within 5 seconds). Every change is logged and can be reverted.
func (a *Autoscaler) SaveSettings(ctx context.Context, next Settings, who, note string) ([]string, error) {
	return a.saveSettings(ctx, next, who, note, true)
}

// SetEnabled pauses or resumes the autoscaler. It never calls a provider, so
// the emergency brake works even when a provider API is down.
func (a *Autoscaler) SetEnabled(ctx context.Context, enabled bool, who string) error {
	next := a.Settings()
	next.Enabled = enabled
	note := "paused"
	if enabled {
		note = "resumed"
	}
	errs, err := a.saveSettings(ctx, next, who, note, false)
	if err == nil && len(errs) > 0 {
		err = fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return err
}

// SettingsVersion identifies the stored settings, so a save from a stale
// browser tab can be refused instead of silently undoing newer changes.
func (a *Autoscaler) SettingsVersion() string {
	raw, _ := json.Marshal(a.Settings())
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:8])
}

func (a *Autoscaler) saveSettings(ctx context.Context, next Settings, who, note string, checkTypes bool) ([]string, error) {
	next.Normalize()
	if errs := next.Validate(); len(errs) > 0 {
		return errs, nil
	}
	if checkTypes {
		if errs := a.validateTypes(ctx, next); len(errs) > 0 {
			return errs, nil
		}
	}
	prev := a.Settings()
	changes := diffSettings(prev, next)
	if len(changes) == 0 {
		return nil, nil
	}
	raw, _ := json.Marshal(next)
	if err := a.store.SetSetting(ctx, settingsKey, string(raw)); err != nil {
		return nil, err
	}
	a.mu.Lock()
	a.settings = next.Clone()
	a.backoffUntil = time.Time{}
	a.mu.Unlock()
	data, _ := json.Marshal(map[string]any{"before": prev, "after": next, "changes": changes, "by": who})
	msg := "settings changed: " + summarizeChanges(changes)
	if note != "" {
		msg = note + ": " + summarizeChanges(changes)
	}
	a.event(ctx, "", "settings", msg, data)
	return nil, nil
}

// RevertSettings restores the settings from before the given change.
func (a *Autoscaler) RevertSettings(ctx context.Context, eventID int64, who string) ([]string, error) {
	e, err := a.store.GetCloudEvent(ctx, eventID)
	if err != nil {
		return nil, err
	}
	var d struct {
		Before *Settings `json:"before"`
	}
	if e.Kind != "settings" || json.Unmarshal(e.Data, &d) != nil || d.Before == nil {
		return nil, fmt.Errorf("event %d is not a settings change", eventID)
	}
	return a.SaveSettings(ctx, *d.Before, who, fmt.Sprintf("reverted change #%d", eventID))
}

func (a *Autoscaler) validateTypes(ctx context.Context, s Settings) []string {
	var errs []string
	for name, ps := range map[string]*ProviderSettings{"hetzner": &s.Hetzner, "aws": &s.AWS} {
		p := a.provider(name)
		if p == nil {
			continue
		}
		ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		for _, t := range ps.Types {
			if err := p.ValidateType(ctx, t); err != nil {
				if errors.Is(err, ErrNotConfigured) || ctx.Err() != nil {
					break
				}
				errs = append(errs, fmt.Sprintf("%s does not offer server type %q", name, t))
			}
		}
		cancel()
	}
	return errs
}

type change struct {
	Field string `json:"field"`
	From  any    `json:"from"`
	To    any    `json:"to"`
}

// diffSettings lists changed top-level and per-provider fields.
func diffSettings(a, b Settings) []change {
	flat := func(s Settings) map[string]any {
		raw, _ := json.Marshal(s)
		var m map[string]any
		json.Unmarshal(raw, &m)
		out := map[string]any{}
		for k, v := range m {
			if sub, ok := v.(map[string]any); ok && (k == "hetzner" || k == "aws") {
				for sk, sv := range sub {
					out[k+"."+sk] = sv
				}
				continue
			}
			out[k] = v
		}
		return out
	}
	fa, fb := flat(a), flat(b)
	var out []change
	for k, vb := range fb {
		va := fa[k]
		ja, _ := json.Marshal(va)
		jb, _ := json.Marshal(vb)
		if string(ja) != string(jb) {
			out = append(out, change{Field: k, From: va, To: vb})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Field < out[j].Field })
	return out
}

func summarizeChanges(cs []change) string {
	parts := make([]string, 0, len(cs))
	for _, c := range cs {
		ja, _ := json.Marshal(c.From)
		jb, _ := json.Marshal(c.To)
		parts = append(parts, fmt.Sprintf("%s %s → %s", c.Field, ja, jb))
	}
	s := strings.Join(parts, ", ")
	if len(s) > 400 {
		s = s[:400] + "…"
	}
	return s
}

// ── prices & costs ──────────────────────────────────────────────────────────

func (a *Autoscaler) price(ctx context.Context, p Provider, typ, loc string, spot bool) (float64, error) {
	key := fmt.Sprintf("%s/%s/%s/%v", p.Name(), typ, loc, spot)
	a.mu.Lock()
	e, ok := a.prices[key]
	a.mu.Unlock()
	if ok && time.Since(e.at) < time.Hour {
		return e.price, nil
	}
	v, err := p.HourlyPrice(ctx, typ, loc, spot)
	if err != nil {
		return 0, err
	}
	a.mu.Lock()
	a.prices[key] = priceEntry{v, time.Now()}
	a.mu.Unlock()
	return v, nil
}

// Price is the hourly price of a type at the first configured location.
func (a *Autoscaler) Price(ctx context.Context, provider, typ string) (float64, error) {
	p := a.provider(provider)
	if p == nil {
		return 0, ErrNotConfigured
	}
	st := a.Settings()
	ps := st.Hetzner
	spot := false
	if provider == "aws" {
		ps, spot = st.AWS, st.AWSSpot
	}
	loc := ""
	if len(ps.Locations) > 0 {
		loc = ps.Locations[0]
	}
	return a.price(ctx, p, typ, loc, spot)
}

// ServerCost estimates what a server has cost so far (or in total once gone).
// Hetzner bills every started hour, AWS by the second.
func ServerCost(s *store.CloudServer, now int64) float64 {
	if s.ProviderCreatedAt == 0 {
		return 0
	}
	end := s.DeletedAt
	if end == 0 {
		end = now
	}
	hours := float64(end-s.ProviderCreatedAt) / float64(time.Hour/time.Millisecond)
	if hours < 0 {
		hours = 0
	}
	if s.Provider == "hetzner" {
		hours = math.Max(1, math.Ceil(hours))
	}
	return hours * s.HourlyPrice
}

// costBetween spreads a server's cost evenly over its lifetime and returns the
// part that falls into [from, to).
func costBetween(s *store.CloudServer, from, to, now int64) float64 {
	if s.ProviderCreatedAt == 0 {
		return 0
	}
	start, end := s.ProviderCreatedAt, s.DeletedAt
	if end == 0 {
		end = now
	}
	if end <= start {
		return ServerCost(s, now)
	}
	lo, hi := max(start, from), min(end, to)
	if hi <= lo {
		return 0
	}
	return ServerCost(s, now) * float64(hi-lo) / float64(end-start)
}

// SpendBetween estimates the spend over all providers in [from, to).
func (a *Autoscaler) SpendBetween(ctx context.Context, from, to time.Time) float64 {
	return a.spend(ctx, from.UnixMilli(), to.UnixMilli())
}

// MonthSpend estimates this calendar month's spend over all providers.
func (a *Autoscaler) MonthSpend(ctx context.Context, now time.Time) float64 {
	from := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location()).UnixMilli()
	return a.spend(ctx, from, now.UnixMilli())
}

func (a *Autoscaler) spend(ctx context.Context, from, to int64) float64 {
	servers, err := a.store.ListCloudServersBetween(ctx, from, to)
	if err != nil {
		return 0
	}
	sum := 0.0
	now := store.Now()
	for i := range servers {
		sum += costBetween(&servers[i], from, to, now)
	}
	return sum
}

// DailyCost is spend per day for the cost history.
type DailyCost struct {
	Day     string  `json:"day"` // YYYY-MM-DD
	Cost    float64 `json:"cost"`
	Servers int     `json:"servers"`
	Hours   float64 `json:"hours"`
}

func (a *Autoscaler) DailyCosts(ctx context.Context, days int, now time.Time) []DailyCost {
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	first := today.AddDate(0, 0, -(days - 1))
	servers, _ := a.store.ListCloudServersBetween(ctx, first.UnixMilli(), now.UnixMilli())
	out := make([]DailyCost, days)
	nowMs := now.UnixMilli()
	for d := 0; d < days; d++ {
		from := first.AddDate(0, 0, d)
		to := from.AddDate(0, 0, 1)
		dc := DailyCost{Day: from.Format("2006-01-02")}
		for i := range servers {
			s := &servers[i]
			c := costBetween(s, from.UnixMilli(), to.UnixMilli(), nowMs)
			if c > 0 || (s.ProviderCreatedAt > 0 && s.ProviderCreatedAt < to.UnixMilli() && (s.DeletedAt == 0 || s.DeletedAt >= from.UnixMilli())) {
				dc.Servers++
				end := s.DeletedAt
				if end == 0 {
					end = nowMs
				}
				lo, hi := max(s.ProviderCreatedAt, from.UnixMilli()), min(end, to.UnixMilli())
				if hi > lo {
					dc.Hours += float64(hi-lo) / float64(time.Hour/time.Millisecond)
				}
			}
			dc.Cost += c
		}
		out[d] = dc
	}
	return out
}

// ── state for the UI ────────────────────────────────────────────────────────

type State struct {
	Configured    map[string]bool `json:"configured"`
	Enabled       bool            `json:"enabled"`
	Provider      string          `json:"provider"`
	Status        string          `json:"status"`
	StatusMessage string          `json:"status_message"`
	Queue         int             `json:"queue"`
	QueueTrend    int             `json:"queue_trend"` // -1, 0, 1 against a minute ago
	Servers       int             `json:"servers"`
	MaxServers    int             `json:"max_servers"`
	SpendToday    float64         `json:"spend_today"`
	SpendMonth    float64         `json:"spend_month"`
	Budget        float64         `json:"budget"`
	Currency      string          `json:"currency"`
	HubPublicKey  string          `json:"hub_public_key"`
	SSHAllowCIDR  string          `json:"ssh_allow_cidr"`
}

func (a *Autoscaler) State(ctx context.Context) State {
	now := time.Now()
	st := a.Settings()
	s := State{
		Configured: map[string]bool{}, Enabled: st.Enabled, Provider: st.Provider, Budget: st.MonthlyBudget,
		MaxServers: st.Active().MaxServers, Currency: a.currency(st), HubPublicKey: a.hubPub, SSHAllowCIDR: st.SSHAllowCIDR,
	}
	for _, n := range []string{"hetzner", "aws"} {
		s.Configured[n] = a.provider(n) != nil
	}
	s.Queue = a.queueDepth(ctx)
	if smp, _ := a.store.ListCloudSamples(ctx, now.Add(-90*time.Second).UnixMilli()); len(smp) > 0 {
		switch prev := smp[0].Queue; {
		case s.Queue > prev:
			s.QueueTrend = 1
		case s.Queue < prev:
			s.QueueTrend = -1
		}
	}
	servers, _ := a.store.ListCloudServers(ctx, now.UnixMilli())
	orphaned := map[string]int{}
	for _, sv := range servers {
		if sv.Active() {
			s.Servers++
			if a.provider(sv.Provider) == nil {
				orphaned[sv.Provider]++
			}
		}
	}
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	s.SpendToday = a.spend(ctx, today.UnixMilli(), now.UnixMilli())
	s.SpendMonth = a.MonthSpend(ctx, now)
	s.Status = a.computeState(ctx, st, now)
	switch s.Status {
	case StatusNotConfigured:
		s.StatusMessage = fmt.Sprintf("No credentials for %s on this hub. Set them in the server configuration.", st.Provider)
	case StatusPaused:
		s.StatusMessage = "Paused: no new servers are started. Running servers finish and shut down as usual."
	case StatusBudget:
		s.StatusMessage = "Monthly budget reached: tests run on the permanent workers only."
	case StatusBackoff:
		a.mu.Lock()
		s.StatusMessage = fmt.Sprintf("Waiting %s after a provider error: %s", roundDur(time.Until(a.backoffUntil)), a.backoffWhy)
		a.mu.Unlock()
	}
	for prov, n := range orphaned {
		s.StatusMessage = fmt.Sprintf("%d server(s) at %s, but %s is no longer configured: the hub cannot manage or delete them. Restore the credentials or delete them at the provider.", n, prov, prov)
	}
	if st.SSHAllowCIDR == "" && s.Status != StatusNotConfigured && len(orphaned) == 0 {
		s.StatusMessage = "Set the IP address SSH may come from before servers can start (Settings → Network)."
	}
	return s
}

func (a *Autoscaler) sample(ctx context.Context, now time.Time) {
	servers, _ := a.store.ListCloudServers(ctx, now.UnixMilli())
	n := 0
	for _, s := range servers {
		if s.Active() {
			n++
		}
	}
	a.store.AddCloudSample(ctx, store.CloudSample{TS: now.UnixMilli(), Queue: a.queueDepth(ctx), Servers: n})
}

// ── events ──────────────────────────────────────────────────────────────────

func (a *Autoscaler) event(ctx context.Context, serverID, kind, msg string, data json.RawMessage) {
	e := &store.CloudEvent{ServerID: serverID, Kind: kind, Message: msg, Data: data}
	if err := a.store.AddCloudEvent(ctx, e); err != nil {
		a.log.Error("cloud event", "err", err)
	}
	lvl := slog.LevelInfo
	if kind == "error" || kind == "spot" {
		lvl = slog.LevelWarn
	}
	a.log.Log(ctx, lvl, msg, "server", serverID, "kind", kind)
	a.notify("cloud")
}

func (a *Autoscaler) notify(what string) {
	if a.hooks.Changed != nil {
		a.hooks.Changed(what)
	}
}

func errData(err error) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"error": err.Error()})
	return b
}

// ── helpers ─────────────────────────────────────────────────────────────────

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func since(from, to int64) string {
	if from == 0 || to < from {
		return "—"
	}
	return roundDur(time.Duration(to-from) * time.Millisecond)
}

func roundDur(d time.Duration) string {
	if d < time.Minute {
		return d.Round(time.Second).String()
	}
	return d.Round(time.Second).String()
}
