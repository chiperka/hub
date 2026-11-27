package cloud

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"gopkg.in/yaml.v3"

	"chiperka-hub/internal/store"
)

// fakeProvider keeps instances in memory. unavailable lists "type/location"
// pairs that fail with ErrUnavailable.
type fakeProvider struct {
	mu          sync.Mutex
	name        string
	instances   map[string]*Instance
	seq         int
	unavailable map[string]bool
	quota       bool
	deleteErr   error
	hidden      map[string]bool // created but not yet listed (eventual consistency)
	priceErr    error
	deleted     []string
	created     []CreateRequest
}

func newFake(name string) *fakeProvider {
	return &fakeProvider{name: name, instances: map[string]*Instance{}, unavailable: map[string]bool{}}
}

func (f *fakeProvider) Name() string { return f.name }
func (f *fakeProvider) Currency() string {
	if f.name == "aws" {
		return "USD"
	}
	return "EUR"
}
func (f *fakeProvider) Check(context.Context) error                   { return nil }
func (f *fakeProvider) Prepare(context.Context, PrepareRequest) error { return nil }
func (f *fakeProvider) Create(_ context.Context, r CreateRequest) (*Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.quota {
		return nil, fmt.Errorf("limit: %w", ErrQuota)
	}
	if f.unavailable[r.Type+"/"+r.Location] {
		return nil, fmt.Errorf("%s/%s: %w", r.Type, r.Location, ErrUnavailable)
	}
	f.seq++
	f.created = append(f.created, r)
	in := &Instance{ID: fmt.Sprint(f.seq), Name: r.Name, Type: r.Type, Location: r.Location, State: StateRunning,
		Spot: r.Spot, CreatedAt: time.Now(), Labels: r.Labels}
	f.instances[in.ID] = in
	cp := *in
	return &cp, nil
}
func (f *fakeProvider) Delete(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleteErr != nil {
		return f.deleteErr
	}
	delete(f.instances, id)
	f.deleted = append(f.deleted, id)
	return nil
}
func (f *fakeProvider) List(context.Context) ([]Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []Instance{}
	for id, in := range f.instances {
		if !f.hidden[id] {
			out = append(out, *in)
		}
	}
	return out, nil
}
func (f *fakeProvider) HourlyPrice(context.Context, string, string, bool) (float64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.priceErr != nil {
		return 0, f.priceErr
	}
	return 0.2, nil
}
func (f *fakeProvider) ValidateType(_ context.Context, t string) error {
	if t == "nope" {
		return fmt.Errorf("no such type")
	}
	return nil
}
func (f *fakeProvider) BaseImage(context.Context) (string, error)   { return "ubuntu-24.04", nil }
func (f *fakeProvider) LatestImage(context.Context) (*Image, error) { return nil, nil }
func (f *fakeProvider) ListImages(context.Context) ([]Image, error) { return nil, nil }
func (f *fakeProvider) DeleteImage(context.Context, string) error   { return nil }
func (f *fakeProvider) CreateImage(context.Context, string, string) (*Image, error) {
	return &Image{ID: "img"}, nil
}

func (f *fakeProvider) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.instances)
}

type env struct {
	t       *testing.T
	a       *Autoscaler
	st      *store.Store
	p       *fakeProvider
	queue   int
	removed []string
}

func newEnv(t *testing.T, mutate func(*Settings)) *env {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &env{t: t, st: st, p: newFake("hetzner")}
	ctx := context.Background()
	a, err := New(ctx, Config{DataDir: dir, HubAddr: "127.0.0.1:1", WorkerImage: "w:1", Providers: map[string]Provider{"hetzner": e.p}},
		st, Hooks{RemoveWorker: func(id, _ string) { e.removed = append(e.removed, id); st.DeleteWorker(ctx, id) }})
	if err != nil {
		t.Fatal(err)
	}
	a.queueDepth = func(context.Context) int { return e.queue }
	s := DefaultSettings()
	s.ScaleUpDelaySec = 0
	s.SSHAllowCIDR = "203.0.113.7/32"
	if mutate != nil {
		mutate(&s)
	}
	if errs, err := a.SaveSettings(ctx, s, "test", ""); err != nil || len(errs) > 0 {
		t.Fatalf("settings: %v %v", err, errs)
	}
	e.a = a
	return e
}

// tickWait runs one tick and waits for asynchronous provider calls.
func (e *env) tick(now time.Time) {
	e.a.tick(context.Background(), now)
	e.wait()
}

func (e *env) wait() {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		e.a.mu.Lock()
		n := len(e.a.busy)
		e.a.mu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	e.t.Fatal("provider calls did not finish")
}

func (e *env) servers() []store.CloudServer {
	s, _ := e.st.ListCloudServers(context.Background(), 0)
	return s
}

func (e *env) active() []store.CloudServer {
	var out []store.CloudServer
	for _, s := range e.servers() {
		if s.Active() {
			out = append(out, s)
		}
	}
	return out
}

func (e *env) setAll(fn func(*store.CloudServer)) {
	for _, s := range e.active() {
		e.a.update(context.Background(), s.ID, func(c *store.CloudServer) bool { fn(c); return true })
	}
}

func TestScaleUpFollowsRules(t *testing.T) {
	e := newEnv(t, nil)
	now := time.Now()

	e.queue = 10 // below the lowest rule: the permanent worker handles it
	e.tick(now)
	if n := e.p.count(); n != 0 {
		t.Fatalf("small queue started %d servers", n)
	}

	e.queue = 100
	e.tick(now)
	if n := e.p.count(); n != 2 {
		t.Fatalf("queue 100: want 2 servers, got %d", n)
	}
	for _, s := range e.active() {
		if s.Status != store.CloudStarting || s.WorkerID == "" || s.HostKey == "" || s.HourlyPrice != 0.2 {
			t.Fatalf("server not set up: %+v", s)
		}
		if w, err := e.st.GetWorker(context.Background(), s.WorkerID); err != nil || w.Name != s.ID {
			t.Fatalf("worker for %s: %v %v", s.ID, w, err)
		}
	}

	e.queue = 1135 // servers already running count: only 2 more
	e.tick(now)
	if n := e.p.count(); n != 4 {
		t.Fatalf("queue 1135: want 4 servers, got %d", n)
	}
	e.tick(now)
	if n := e.p.count(); n != 4 {
		t.Fatalf("no duplicate servers expected, got %d", n)
	}
}

func TestScaleUpDelayIgnoresSpikes(t *testing.T) {
	e := newEnv(t, func(s *Settings) { s.ScaleUpDelaySec = 15 })
	now := time.Now()
	e.queue = 500
	e.tick(now)
	e.tick(now.Add(10 * time.Second))
	if e.p.count() != 0 {
		t.Fatal("started before the delay")
	}
	e.tick(now.Add(16 * time.Second))
	if e.p.count() != 4 {
		t.Fatalf("want 4 after the delay, got %d", e.p.count())
	}
}

func TestMaxServersCapsEverything(t *testing.T) {
	e := newEnv(t, func(s *Settings) {
		s.Hetzner.Rules = []Rule{{MinQueue: 1, Servers: 5}}
		s.Hetzner.MaxServers = 5
	})
	if _, err := e.a.Launch(context.Background(), 2); err != nil {
		t.Fatal(err)
	}
	e.wait()
	e.queue = 100
	e.tick(time.Now())
	if n := e.p.count(); n != 5 {
		t.Fatalf("want 5 (cap incl. manual), got %d", n)
	}
	if _, err := e.a.Launch(context.Background(), 1); err == nil {
		t.Fatal("launch above the cap must fail")
	}
}

func TestFallbackToNextTypeAndLocation(t *testing.T) {
	e := newEnv(t, nil)
	e.p.unavailable["cpx62/nbg1"] = true
	e.p.unavailable["cpx62/fsn1"] = true
	if _, err := e.a.Launch(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	e.wait()
	s := e.active()
	if len(s) != 1 || s[0].Type != "ccx33" || s[0].Location != "nbg1" {
		t.Fatalf("want ccx33 in nbg1, got %+v", s)
	}
	ev, _ := e.st.ListCloudEvents(context.Background(), store.CloudEventFilter{Kind: "provider"})
	if len(ev) != 2 {
		t.Fatalf("want 2 'not available' events, got %d", len(ev))
	}
}

func TestQuotaBacksOff(t *testing.T) {
	e := newEnv(t, nil)
	e.p.quota = true
	e.queue = 100
	e.tick(time.Now())
	for _, s := range e.servers() {
		if s.Status != store.CloudFailed || !strings.Contains(s.EndReason, "quota") {
			t.Fatalf("want failed with quota reason, got %+v", s)
		}
		if _, err := e.st.GetWorker(context.Background(), s.WorkerID); err == nil {
			t.Fatal("worker of a failed server must be removed")
		}
	}
	failed := len(e.servers())
	e.p.quota = false
	e.tick(time.Now())
	if len(e.servers()) != failed {
		t.Fatal("must not retry during back-off")
	}
	if st := e.a.State(context.Background()); st.Status != StatusBackoff {
		t.Fatalf("want backoff status, got %s", st.Status)
	}
}

func online(e *env) {
	for _, s := range e.active() {
		e.st.ActivateWorker(context.Background(), s.WorkerID, s.ID, "test", 8, 16, 0)
	}
}

func TestLifecycleIdleDrainDelete(t *testing.T) {
	e := newEnv(t, func(s *Settings) { s.Provider = "hetzner"; s.HetznerHoldHour = false })
	e.queue = 100
	now := time.Now()
	e.tick(now)
	online(e)
	e.tick(now)
	for _, s := range e.active() {
		if s.Status != store.CloudWorking || s.WorkerAt == 0 {
			t.Fatalf("want working, got %s", s.Status)
		}
	}

	e.queue = 0
	e.tick(now) // no work: idle
	for _, s := range e.active() {
		if s.Status != store.CloudIdle {
			t.Fatalf("want idle, got %s", s.Status)
		}
	}
	e.tick(now.Add(2 * time.Minute)) // before the 3 min idle timeout
	if len(e.active()) != 2 || e.active()[0].Status != store.CloudIdle {
		t.Fatal("stopped too early")
	}
	e.tick(now.Add(4 * time.Minute)) // drain
	for _, s := range e.active() {
		if s.Status != store.CloudDraining || !strings.Contains(s.EndReason, "idle") {
			t.Fatalf("want draining, got %+v", s)
		}
		if w, _ := e.st.GetWorker(context.Background(), s.WorkerID); !w.Paused {
			t.Fatal("draining worker must be paused")
		}
	}
	e.tick(now.Add(4 * time.Minute)) // no running jobs: delete
	if len(e.active()) != 0 || e.p.count() != 0 {
		t.Fatalf("servers left: %d at provider %d", len(e.active()), e.p.count())
	}
	if len(e.removed) != 2 {
		t.Fatalf("workers removed: %v", e.removed)
	}
}

func TestIdleServerWakesUp(t *testing.T) {
	e := newEnv(t, nil)
	e.queue = 100
	now := time.Now()
	e.tick(now)
	online(e)
	e.tick(now)
	e.queue = 0
	e.tick(now)
	e.queue = 50
	e.tick(now.Add(time.Minute))
	for _, s := range e.active() {
		if s.Status != store.CloudWorking || s.IdleSince != 0 {
			t.Fatalf("want working again, got %s", s.Status)
		}
	}
}

func TestHetznerHoldsUntilEndOfPaidHour(t *testing.T) {
	st := DefaultSettings()
	created := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	s := &store.CloudServer{Provider: "hetzner", Status: store.CloudIdle, ProviderCreatedAt: created.UnixMilli(),
		IdleSince: created.Add(10 * time.Minute).UnixMilli()}

	if stop, _ := shouldStopIdle(st, s, 1, created.Add(20*time.Minute)); stop {
		t.Fatal("must keep the server for the rest of the paid hour")
	}
	if stop, why := shouldStopIdle(st, s, 1, created.Add(55*time.Minute+time.Second)); !stop || !strings.Contains(why, "paid hour") {
		t.Fatalf("must stop 5 min before the hour ends: %v %q", stop, why)
	}
	if at, _ := PlannedStop(st, s); at != created.Add(55*time.Minute).UnixMilli() {
		t.Fatalf("planned stop %v", time.UnixMilli(at).UTC())
	}
	// Idle timeout reached in the second hour: wait for its end.
	s.IdleSince = created.Add(61 * time.Minute).UnixMilli()
	if at, _ := PlannedStop(st, s); at != created.Add(115*time.Minute).UnixMilli() {
		t.Fatalf("planned stop in second hour %v", time.UnixMilli(at).UTC())
	}

	aws := &store.CloudServer{Provider: "aws", Status: store.CloudIdle, ProviderCreatedAt: created.UnixMilli(),
		IdleSince: created.Add(10 * time.Minute).UnixMilli()}
	if stop, _ := shouldStopIdle(st, aws, 1, created.Add(13*time.Minute+time.Second)); !stop {
		t.Fatal("AWS bills by the second: stop after the idle timeout")
	}
	aws.HoldUntil = created.Add(40 * time.Minute).UnixMilli()
	if stop, _ := shouldStopIdle(st, aws, 1, created.Add(30*time.Minute)); stop {
		t.Fatal("extended server must stay")
	}
}

func TestWarmPoolKeepsAndStarts(t *testing.T) {
	now := time.Date(2026, 11, 25, 9, 0, 0, 0, time.Local) // Wednesday
	e := newEnv(t, func(s *Settings) {
		s.WarmPool = []WarmWindow{{Days: []int{1, 2, 3, 4, 5}, From: "08:00", To: "17:00", Servers: 1}}
	})
	e.tick(now)
	if e.p.count() != 1 {
		t.Fatalf("warm pool should start 1 server, got %d", e.p.count())
	}
	online(e)
	e.tick(now)
	e.tick(now.Add(2 * time.Hour))
	if s := e.active(); len(s) != 1 || s[0].Status == store.CloudDraining {
		t.Fatal("warm server must not be stopped inside the window")
	}
	if st := e.a.Settings(); st.WarmTarget(now.Add(9*time.Hour)) != 0 {
		t.Fatal("window must end at 17:00")
	}
}

func TestStartTimeoutDeletesServer(t *testing.T) {
	e := newEnv(t, nil)
	e.queue = 100
	now := time.Now()
	e.tick(now)
	e.tick(now.Add(6 * time.Minute))
	for _, s := range e.servers() {
		if s.Status != store.CloudFailed || !strings.Contains(s.EndReason, "did not sign in") {
			t.Fatalf("want failed after start timeout, got %+v", s)
		}
	}
	if e.p.count() != 0 {
		t.Fatal("broken servers must be deleted at the provider")
	}
}

func TestReconcile(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	e.queue = 100
	e.tick(time.Now())
	s := e.active()[0]

	// A spot interruption, an orphan of this hub and a server of another hub.
	e.p.mu.Lock()
	in := e.p.instances[s.ProviderID]
	in.State, in.StateReason, in.Spot = StateTerminated, "Server.SpotInstanceTermination", true
	e.p.instances["orphan"] = &Instance{ID: "orphan", Name: "ci-dead", State: StateRunning, CreatedAt: time.Now().Add(-time.Hour), Labels: managedLabels(e.a.hubID)}
	e.p.instances["other"] = &Instance{ID: "other", Name: "ci-else", State: StateRunning, CreatedAt: time.Now().Add(-time.Hour), Labels: managedLabels("other-hub")}
	e.p.mu.Unlock()

	e.a.reconcile(ctx)
	got, _ := e.st.GetCloudServer(ctx, s.ID)
	if got.Status != store.CloudDeleted || !strings.Contains(got.EndReason, "spot") {
		t.Fatalf("spot interruption not detected: %+v", got)
	}
	e.p.mu.Lock()
	_, orphan := e.p.instances["orphan"]
	_, other := e.p.instances["other"]
	e.p.mu.Unlock()
	if orphan || !other {
		t.Fatalf("orphan must be deleted (%v), other hub's server kept (%v)", !orphan, other)
	}
}

func TestBudgetStopsNewServers(t *testing.T) {
	e := newEnv(t, func(s *Settings) { s.MonthlyBudget = 1 })
	ctx := context.Background()
	old := &store.CloudServer{ID: "ci-old", Provider: "hetzner", Status: store.CloudDeleted, HourlyPrice: 0.5,
		CreatedAt: store.Now() - 3_600_000, ProviderCreatedAt: store.Now() - 3_600_000*2 - 1, DeletedAt: store.Now() - 1000}
	e.st.SaveCloudServer(ctx, old) // 3 started hours × 0.5 = 1.5 ≥ 1
	e.queue = 500
	e.tick(time.Now())
	if e.p.count() != 0 {
		t.Fatal("budget exceeded, yet servers started")
	}
	if st := e.a.State(ctx); st.Status != StatusBudget {
		t.Fatalf("want budget status, got %s", st.Status)
	}
	if _, err := e.a.Launch(ctx, 1); err == nil {
		t.Fatal("manual launch must respect the budget")
	}
}

func TestUnknownPriceStopsNewServersUnderBudget(t *testing.T) {
	defer func(d time.Duration) { priceRetryDelay = d }(priceRetryDelay)
	priceRetryDelay = time.Millisecond
	e := newEnv(t, func(s *Settings) { s.MonthlyBudget = 50 })
	ctx := context.Background()
	e.p.mu.Lock()
	e.p.priceErr = fmt.Errorf("price list unavailable")
	e.p.mu.Unlock()

	// Under a budget, a server whose price is unknown does not start at all.
	e.queue = 100
	e.tick(time.Now())
	if n := e.p.count(); n != 0 {
		t.Fatalf("unknown price under a budget, yet %d servers", n)
	}
	// Started without a budget, then a budget is set: servers of unknown price are running.
	st := e.a.Settings()
	st.MonthlyBudget = 0
	e.a.SaveSettings(ctx, st, "test", "")
	e.tick(time.Now())
	st.MonthlyBudget = 50
	e.a.SaveSettings(ctx, st, "test", "")
	if n := e.p.count(); n != 2 {
		t.Fatalf("want 2 servers, got %d", n)
	}
	for _, s := range e.active() {
		if !s.PriceUnknown || s.HourlyPrice != 0 {
			t.Fatalf("no price may be made up: %+v", s)
		}
	}
	if st := e.a.State(ctx); st.Status != StatusPriceUnknown || st.PriceUnknown != 2 || st.SpendMonth != 0 {
		t.Fatalf("state %+v", st)
	}
	e.queue = 1135
	e.tick(time.Now())
	if n := e.p.count(); n != 2 {
		t.Fatalf("unknown price under a budget, yet %d servers", n)
	}
	if _, err := e.a.Launch(ctx, 1); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("manual launch must stop too, got %v", err)
	}

	e.p.mu.Lock()
	e.p.priceErr = nil
	e.p.mu.Unlock()
	e.a.prices = map[string]priceEntry{}
	e.a.reconcile(ctx)
	for _, s := range e.active() {
		if s.PriceUnknown || s.HourlyPrice != 0.2 {
			t.Fatalf("price not filled in later: %+v", s)
		}
	}
	if st := e.a.State(ctx); st.Status != StatusRunning || st.PriceUnknown != 0 {
		t.Fatalf("state after the price came %+v", st)
	}
}

func TestBudgetHoldsFirstHourOfNewServers(t *testing.T) {
	// 0.5 left, 0.2/h: only two servers fit, though the rules want four.
	e := newEnv(t, func(s *Settings) { s.MonthlyBudget = 0.5 })
	ctx := context.Background()
	e.queue = 1135
	e.tick(time.Now())
	if n := e.p.count(); n != 2 {
		t.Fatalf("want 2 servers within the budget, got %d", n)
	}
	e.tick(time.Now())
	if n := e.p.count(); n != 2 {
		t.Fatalf("budget room counted twice: %d servers", n)
	}
	if st := e.a.State(ctx); st.BudgetLeft < 0.09 || st.BudgetLeft > 0.11 {
		t.Fatalf("budget left %v, want 0.1", st.BudgetLeft)
	}
	if _, err := e.a.Launch(ctx, 1); err == nil || !strings.Contains(err.Error(), "room for 0") {
		t.Fatalf("manual launch must respect the budget, got %v", err)
	}
}

func TestBudgetStopsServersBeforeTheNextHour(t *testing.T) {
	// Two servers, 0.4 spent: 0.65 leaves one more hour for one of them.
	e := newEnv(t, func(s *Settings) { s.MonthlyBudget = 0.65; s.HetznerHoldHour = false })
	e.queue = 100
	e.tick(time.Now())
	online(e)
	e.tick(time.Now())
	if n := len(e.active()); n != 2 {
		t.Fatalf("want 2 servers, got %d", n)
	}
	// 20 minutes into the hour nothing happens; 50 minutes in, the next hour is due.
	e.setAll(func(c *store.CloudServer) { c.ProviderCreatedAt = store.Now() - 20*60_000 })
	e.tick(time.Now())
	if n := countStatus(e, store.CloudDraining); n != 0 {
		t.Fatalf("%d servers stopped mid-hour", n)
	}
	e.setAll(func(c *store.CloudServer) { c.ProviderCreatedAt = store.Now() - 50*60_000 })
	e.tick(time.Now())
	if n := countStatus(e, store.CloudDraining); n != 1 {
		t.Fatalf("want 1 server stopped for the budget, got %d", n)
	}
	for _, s := range e.servers() {
		if s.Status == store.CloudDraining && !strings.Contains(s.EndReason, "monthly budget") {
			t.Fatalf("end reason %q", s.EndReason)
		}
	}
}

func TestBudgetIgnoresOtherCurrency(t *testing.T) {
	e := newEnv(t, func(s *Settings) { s.MonthlyBudget = 1 })
	ctx := context.Background()
	aws := &store.CloudServer{ID: "ci-aws", Provider: "aws", Status: store.CloudDeleted, HourlyPrice: 3, Currency: "USD",
		CreatedAt: store.Now() - 3_600_000, ProviderCreatedAt: store.Now() - 3_600_000, DeletedAt: store.Now() - 1000}
	e.st.SaveCloudServer(ctx, aws)
	e.queue = 100
	e.tick(time.Now())
	if n := e.p.count(); n != 2 {
		t.Fatalf("dollars spent on AWS must not use up a euro budget: %d servers", n)
	}
	st := e.a.State(ctx)
	if st.SpendMonth > 0.41 || st.SpendOther["USD"] < 2.9 {
		t.Fatalf("spend %v, other %v", st.SpendMonth, st.SpendOther)
	}
}

func countStatus(e *env, status string) int {
	n := 0
	for _, s := range e.servers() {
		if s.Status == status {
			n++
		}
	}
	return n
}

func TestUnknownPriceWithoutBudget(t *testing.T) {
	defer func(d time.Duration) { priceRetryDelay = d }(priceRetryDelay)
	priceRetryDelay = time.Millisecond
	e := newEnv(t, func(s *Settings) { s.MonthlyBudget = 0 })
	e.p.mu.Lock()
	e.p.priceErr = fmt.Errorf("price list unavailable")
	e.p.mu.Unlock()
	e.queue = 100
	e.tick(time.Now())
	e.queue = 1135
	e.tick(time.Now())
	if n := e.p.count(); n != 4 {
		t.Fatalf("no budget to protect: want 4 servers, got %d", n)
	}
}

func TestTerminateGracefulAndNow(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	e.a.Launch(ctx, 2)
	e.wait()
	online(e)
	e.tick(time.Now())
	s := e.active()
	if err := e.a.Terminate(ctx, s[0].ID, false); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.st.GetCloudServer(ctx, s[0].ID); got.Status != store.CloudDraining {
		t.Fatalf("graceful stop must drain, got %s", got.Status)
	}
	if err := e.a.Terminate(ctx, s[1].ID, true); err != nil {
		t.Fatal(err)
	}
	e.tick(time.Now())
	if got, _ := e.st.GetCloudServer(ctx, s[1].ID); got.Status != store.CloudDeleted {
		t.Fatalf("immediate stop must delete, got %s", got.Status)
	}
}

func TestCosts(t *testing.T) {
	h := time.Hour.Milliseconds()
	hz := &store.CloudServer{Provider: "hetzner", HourlyPrice: 0.208, ProviderCreatedAt: 0 + 1, DeletedAt: 1 + h + 60_000}
	if c := ServerCost(hz, 0); c < 0.415 || c > 0.417 {
		t.Fatalf("hetzner 61 min = 2 started hours, got %v", c)
	}
	aw := &store.CloudServer{Provider: "aws", HourlyPrice: 0.36, ProviderCreatedAt: 1, DeletedAt: 1 + h/6}
	if c := ServerCost(aw, 0); c < 0.0599 || c > 0.0601 {
		t.Fatalf("aws 10 min at 0.36/h, got %v", c)
	}
	if c := costBetween(aw, 1, 1+h/12, 0); c < 0.0299 || c > 0.0301 {
		t.Fatalf("half of the lifetime, got %v", c)
	}
}

func TestSettingsValidation(t *testing.T) {
	s := DefaultSettings()
	if errs := s.Validate(); len(errs) != 0 {
		t.Fatalf("defaults invalid: %v", errs)
	}
	s.Hetzner.Rules = []Rule{{MinQueue: 300, Servers: 4}, {MinQueue: 40, Servers: 2}}
	s.Hetzner.MaxServers = 3
	s.IdleTimeoutSec = 0
	errs := strings.Join(s.Validate(), "\n")
	for _, want := range []string{"thresholds must increase", "lower than the highest rule", "idle time before shutdown must be positive"} {
		if !strings.Contains(errs, want) {
			t.Errorf("missing %q in %s", want, errs)
		}
	}
	s = DefaultSettings()
	s.SSHAllowCIDR = "203.0.113.7"
	s.Normalize()
	if s.SSHAllowCIDR != "203.0.113.7/32" {
		t.Fatalf("plain IP should become /32, got %s", s.SSHAllowCIDR)
	}
	if got := s.Hetzner.TargetFor(299); got != 2 {
		t.Fatalf("TargetFor(299)=%d", got)
	}
}

func TestSettingsHistoryAndRevert(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	s := e.a.Settings()
	s.IdleTimeoutSec = 600
	if errs, err := e.a.SaveSettings(ctx, s, "test", ""); err != nil || len(errs) > 0 {
		t.Fatal(err, errs)
	}
	s.Hetzner.Types = []string{"nope"}
	if errs, _ := e.a.SaveSettings(ctx, s, "test", ""); len(errs) == 0 {
		t.Fatal("unknown server type must be rejected")
	}
	ev, _ := e.st.ListCloudEvents(ctx, store.CloudEventFilter{Kind: "settings", Limit: 1})
	if len(ev) != 1 || !strings.Contains(ev[0].Message, "idle_timeout_sec 180 → 600") {
		t.Fatalf("change not logged: %+v", ev)
	}
	if _, err := e.a.RevertSettings(ctx, ev[0].ID, "test"); err != nil {
		t.Fatal(err)
	}
	if e.a.Settings().IdleTimeoutSec != 180 {
		t.Fatal("revert did not restore the old value")
	}
}

func TestWorkerUserData(t *testing.T) {
	priv, pub, _, err := newKey("ci-1")
	if err != nil {
		t.Fatal(err)
	}
	ud, err := WorkerUserData{Provider: "aws", HostKeyPrivate: string(priv), HostKeyPublic: pub, HubPublicKey: "ssh-ed25519 AAAA hub",
		WorkerImage: "w:1", WorkerName: "ci-1", WorkerToken: "spt_x", Concurrency: 8, TimeoutSec: 600, SelfDestructMin: 300}.Render()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ud, "#cloud-config\n") {
		t.Fatal("missing cloud-config header")
	}
	var cc map[string]any
	if err := yaml.Unmarshal([]byte(ud), &cc); err != nil {
		t.Fatalf("invalid YAML: %v", err)
	}
	keys := cc["ssh_keys"].(map[string]any)
	if _, err := ssh.ParsePrivateKey([]byte(keys["ed25519_private"].(string))); err != nil {
		t.Fatalf("host key does not survive YAML: %v", err)
	}
	// Workers up to 2.9 remove every container whose name contains "chiperka-", including themselves.
	if strings.Contains(ud, "--name chiperka-") {
		t.Error("the worker container must not be named chiperka-*")
	}
	for _, want := range []string{`permitlisten="127.0.0.1:18080"`, "CHIPERKA_WORKER_TOKEN=spt_x", "CHIPERKA_API_URL=http://127.0.0.1:18080", "AllowUsers chiperka-tunnel", "169.254.169.254"} {
		if !strings.Contains(ud, want) {
			t.Errorf("user data lacks %q", want)
		}
	}
}

func TestSettingsCopiesAreIndependent(t *testing.T) {
	e := newEnv(t, nil)
	c := e.a.Settings()
	c.Hetzner.Rules[0].Servers = 100
	c.Hetzner.Types[0] = "changed"
	if got := e.a.Settings(); got.Hetzner.Rules[0].Servers != 2 || got.Hetzner.Types[0] != "cpx62" {
		t.Fatal("editing a copy changed the live settings")
	}
}

func TestReconcileWaitsForNewServers(t *testing.T) {
	e := newEnv(t, nil)
	e.queue = 100
	e.tick(time.Now())
	s := e.active()[0]
	e.p.mu.Lock()
	e.p.hidden = map[string]bool{s.ProviderID: true} // AWS: not listed yet right after RunInstances
	e.p.mu.Unlock()
	e.a.reconcile(context.Background())
	if got, _ := e.st.GetCloudServer(context.Background(), s.ID); !got.Active() {
		t.Fatal("a just-created server missing from the list must not be written off")
	}
}

func TestLostWorkerIsReplaced(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	e.queue = 100
	now := time.Now()
	e.tick(now)
	online(e)
	e.tick(now)
	s := e.active()[0]
	e.st.MarkStaleWorkersOffline(ctx, store.Now()+1) // all workers stop sending heartbeats
	e.tick(now.Add(3 * time.Minute))
	if got, _ := e.st.GetCloudServer(ctx, s.ID); got.Status != store.CloudFailed || !strings.Contains(got.EndReason, "stopped responding") {
		t.Fatalf("server with a lost worker must be replaced, got %s %q", got.Status, got.EndReason)
	}
}

func TestRepeatedStartFailuresBackOff(t *testing.T) {
	e := newEnv(t, nil)
	e.queue = 100
	now := time.Now()
	e.tick(now)
	e.tick(now.Add(6 * time.Minute)) // both servers time out
	created := len(e.servers())
	e.tick(now.Add(6 * time.Minute))
	if len(e.servers()) != created {
		t.Fatal("must not keep creating servers that never start")
	}
	if st := e.a.State(context.Background()); st.Status != StatusBackoff || !strings.Contains(st.StatusMessage, "did not start") {
		t.Fatalf("want back-off with an explanation, got %s %q", st.Status, st.StatusMessage)
	}
}

func TestWarmPoolDrainsOnlyTheSurplus(t *testing.T) {
	now := time.Date(2026, 11, 25, 9, 0, 0, 0, time.Local) // Wednesday
	e := newEnv(t, func(s *Settings) {
		s.WarmPool = []WarmWindow{{Days: []int{3}, From: "08:00", To: "17:00", Servers: 2}}
		s.HetznerHoldHour = false
	})
	e.a.Launch(context.Background(), 3)
	e.wait()
	online(e)
	e.tick(now)                       // working
	e.tick(now)                       // idle
	e.tick(now.Add(10 * time.Minute)) // past the idle timeout
	draining := 0
	for _, s := range e.active() {
		if s.Status == store.CloudDraining {
			draining++
		}
	}
	if draining != 1 {
		t.Fatalf("warm pool of 2 with 3 idle servers: want 1 draining, got %d", draining)
	}
}

func TestStopNowIsRetried(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	e.a.Launch(ctx, 1)
	e.wait()
	s := e.active()[0]
	e.p.mu.Lock()
	e.p.deleteErr = fmt.Errorf("server is locked")
	e.p.mu.Unlock()
	e.a.Terminate(ctx, s.ID, true)
	e.tick(time.Now())
	if got, _ := e.st.GetCloudServer(ctx, s.ID); !got.Active() {
		t.Fatal("a failed delete must not mark the server deleted")
	}
	e.p.mu.Lock()
	e.p.deleteErr = nil
	e.p.mu.Unlock()
	e.tick(time.Now())
	if got, _ := e.st.GetCloudServer(ctx, s.ID); got.Status != store.CloudDeleted || e.p.count() != 0 {
		t.Fatalf("the stop must be retried, got %s", got.Status)
	}
}

func TestLifetimeMustEndBeforeSelfDestruct(t *testing.T) {
	s := DefaultSettings()
	s.MaxLifetimeSec = 5 * 3600
	if !strings.Contains(strings.Join(s.Validate(), "\n"), "power themselves off") {
		t.Fatal("a lifetime past the self-destruct must be rejected")
	}
}

func TestImageScriptRemovesRegistryPassword(t *testing.T) {
	ud, _ := ImageUserData{WorkerImage: "w", Registry: "r", RegistryUser: "u", RegistryPassword: "secret"}.Render()
	if !strings.Contains(ud, `rm -f "$0"`) {
		t.Fatal("the build script with the registry password must delete itself before the snapshot")
	}
}

func TestFinishedServerKeepsItsTestCount(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	e.a.Launch(ctx, 1)
	e.wait()
	s := e.active()[0]
	completeJobs(t, e.st, s.WorkerID, 3)
	e.a.Terminate(ctx, s.ID, true)
	e.tick(time.Now())
	if got, _ := e.st.GetCloudServer(ctx, s.ID); got.Status != store.CloudDeleted || got.CompletedJobs != 3 {
		t.Fatalf("want deleted with 3 tests, got %s %d", got.Status, got.CompletedJobs)
	}
}

// completeJobs runs n trivial jobs on the worker through the store's public API.
func completeJobs(t *testing.T, st *store.Store, workerID string, n int) {
	t.Helper()
	ctx := context.Background()
	tests := make([]store.NewTest, n)
	for i := range tests {
		tests[i] = store.NewTest{UUID: fmt.Sprintf("u-%s-%d", workerID, i), Name: fmt.Sprint("t", i), Definition: []byte("{}"),
			Compile: func(int64, int64) ([]byte, int, int) { return []byte("{}"), 1, 1 }}
	}
	run := &store.Run{ID: "r-" + workerID, Status: "running", SnapshotsReady: true, StartedAt: store.Now()}
	if err := st.CreateRun(ctx, run, []store.NewSuite{{Name: "s", Tests: tests}}); err != nil {
		t.Fatal(err)
	}
	jobs, err := st.ClaimJobs(ctx, workerID, n, 100, func() string { return "tok" })
	if err != nil || len(jobs) != n {
		t.Fatalf("claim: %v (%d jobs)", err, len(jobs))
	}
	for _, j := range jobs {
		if _, err := st.CompleteJob(ctx, j.ID, "passed", "", 10, nil, ""); err != nil {
			t.Fatal(err)
		}
	}
}
