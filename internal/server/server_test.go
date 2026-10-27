package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"chiperka-hub/internal/store"
)

type harness struct {
	t   *testing.T
	srv *httptest.Server
	s   *Server
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	s := New(Config{DataDir: dir, AuthDisabled: true, Version: "test", WorkerImage: "img"}, st, nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return &harness{t: t, srv: srv, s: s}
}

func (h *harness) do(method, path, token string, body any, out any) int {
	h.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, h.srv.URL+path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer res.Body.Close()
	if out != nil && res.StatusCode != http.StatusNoContent {
		json.NewDecoder(res.Body).Decode(out)
	}
	return res.StatusCode
}

const suiteJSON = `{"suites":[{"name":"s","file_path":"tests/a.chiperka","tests":[
 {"kind":"Test","name":"ok","endpoint":"e","services":[{"ref":"web","name":"web","image":"nginx","weight":2}],
  "execution":{"executor":"http","target":"http://web","request":{"method":"GET","url":"/"}},
  "assertions":[{"response":{"status_code":200}}]},
 {"kind":"Test","name":"skip","endpoint":"e","skipped":true,"execution":{"executor":"http"},"assertions":[]}
]}],"config":{"version":"1.0"}}`

func TestRunLifecycle(t *testing.T) {
	h := newHarness(t)

	var proj struct{ ID int64 }
	if code := h.do("GET", "/api/projects/resolve?slug=demo", "", nil, &proj); code != 200 || proj.ID == 0 {
		t.Fatalf("resolve: %d %+v", code, proj)
	}
	var req map[string]any
	json.Unmarshal([]byte(suiteJSON), &req)
	req["project_id"] = proj.ID
	var created struct{ ID string }
	if code := h.do("POST", "/api/runs", "", req, &created); code != 201 {
		t.Fatalf("create run: %d", code)
	}

	// SSE snapshot for the CLI.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	sreq, _ := http.NewRequestWithContext(ctx, "GET", h.srv.URL+"/api/events/runs?id="+created.ID, nil)
	sres, err := http.DefaultClient.Do(sreq)
	if err != nil {
		t.Fatal(err)
	}
	events := make(chan string, 32)
	go func() {
		sc := bufio.NewScanner(sres.Body)
		for sc.Scan() {
			if l := sc.Text(); strings.HasPrefix(l, "event: ") {
				events <- strings.TrimPrefix(l, "event: ")
			}
		}
		close(events)
	}()
	if ev := <-events; ev != "snapshot" {
		t.Fatalf("first event %q", ev)
	}

	// Provision + register a worker.
	var prov struct{ Token string }
	h.do("POST", "/api/workers", "", map[string]string{"name": "w"}, &prov)
	var reg struct {
		WorkerID string `json:"worker_id"`
	}
	if code := h.do("POST", "/api/workers/register", prov.Token, map[string]any{"hostname": "h", "concurrency": 1, "capacity": 4}, &reg); code != 200 {
		t.Fatalf("register: %d", code)
	}
	if code := h.do("POST", "/api/workers/register", "spt_bogus", map[string]any{}, nil); code != 403 {
		t.Fatalf("bogus token register: %d", code)
	}

	claim := func() map[string]any {
		var res struct{ Jobs []map[string]any }
		code := h.do("POST", "/api/jobs/claim", prov.Token, map[string]any{"worker_id": reg.WorkerID, "count": 4, "timeout_ms": 200, "available_capacity": 4}, &res)
		if code != 200 || len(res.Jobs) != 1 {
			t.Fatalf("claim: %d %d jobs", code, len(res.Jobs))
		}
		j := res.Jobs[0]
		h.do("POST", "/api/jobs/ack", prov.Token, map[string]any{"worker_id": reg.WorkerID, "job_ids": []string{j["job_id"].(string)}}, nil)
		return j
	}
	complete := func(j map[string]any, status string) {
		code := h.do("POST", "/api/jobs/"+j["job_id"].(string)+"/complete", j["token"].(string),
			map[string]any{"status": status, "duration_ms": 5, "step_results": map[string]any{}}, nil)
		if code != 200 {
			t.Fatalf("complete: %d", code)
		}
	}

	j := claim()
	if j["weight"].(float64) != 2 || j["steps"].(map[string]any)["execute"] == nil {
		t.Fatalf("unexpected job: %v", j)
	}
	complete(j, "error") // infrastructure error → retried
	j2 := claim()
	if j2["job_id"] == j["job_id"] {
		t.Fatal("expected a new job for the retry")
	}
	// Old job token must not complete the new job.
	if code := h.do("POST", "/api/jobs/"+j2["job_id"].(string)+"/complete", j["token"].(string), map[string]any{"status": "passed"}, nil); code != 403 {
		t.Fatalf("foreign job token accepted: %d", code)
	}
	complete(j2, "passed")

	var nothing struct{ Jobs []any }
	if code := h.do("POST", "/api/jobs/claim", prov.Token, map[string]any{"worker_id": reg.WorkerID, "timeout_ms": 100}, &nothing); code != 204 {
		t.Fatalf("empty queue claim: %d", code)
	}

	got := map[string]bool{}
	for ev := range events {
		got[ev] = true
		if ev == "run_completed" {
			break
		}
	}
	if !got["test_update"] || !got["run_completed"] {
		t.Fatalf("events: %v", got)
	}

	var run struct {
		Run store.Run
	}
	h.do("GET", "/api/runs/"+created.ID, "", nil, &run)
	if run.Run.Status != "passed" || run.Run.Passed != 1 || run.Run.Skipped != 1 {
		t.Fatalf("run: %+v", run.Run)
	}

	var summary resultRunSummary
	h.do("GET", "/api/results/runs/cr-"+created.ID, "", nil, &summary)
	if summary.Total != 2 || len(summary.Tests) != 2 || !strings.HasPrefix(summary.Tests[0].UUID, "ct-") {
		t.Fatalf("results summary: %+v", summary)
	}
}

func TestStopRun(t *testing.T) {
	h := newHarness(t)
	var req map[string]any
	json.Unmarshal([]byte(suiteJSON), &req)
	var created struct{ ID string }
	h.do("POST", "/api/runs", "", req, &created)
	if code := h.do("POST", "/api/runs/"+created.ID+"/stop", "", nil, nil); code != 200 {
		t.Fatalf("stop: %d", code)
	}
	var run struct{ Run store.Run }
	h.do("GET", "/api/runs/"+created.ID, "", nil, &run)
	if run.Run.Status != "cancelled" || run.Run.CompletedAt == 0 {
		t.Fatalf("run after stop: %+v", run.Run)
	}
}

func TestSafeRelPath(t *testing.T) {
	for in, ok := range map[string]bool{"a/b.json": true, "./a": true, "../x": false, "a/../../x": false, "": false} {
		if _, got := safeRelPath(in); got != ok {
			t.Errorf("safeRelPath(%q) = %v", in, got)
		}
	}
}

func TestWorkerJoin(t *testing.T) {
	h := newHarness(t)
	type regResp struct {
		WorkerID string `json:"worker_id"`
		Token    string `json:"token"`
	}
	hb := func(tok string) int {
		return h.do("POST", "/api/workers/heartbeat", tok, map[string]any{}, nil)
	}

	// Trusted mode without a join token: a worker with no token joins.
	var a regResp
	if code := h.do("POST", "/api/workers/register", "", map[string]any{"name": "ci-1", "hostname": "x"}, &a); code != 200 || a.Token == "" {
		t.Fatalf("open join: %d %+v", code, a)
	}
	if hb(a.Token) != 200 {
		t.Fatal("issued token rejected")
	}

	// Restart: same name → same worker, the old token stops working.
	var b regResp
	h.do("POST", "/api/workers/register", "", map[string]any{"name": "ci-1"}, &b)
	if b.WorkerID != a.WorkerID || b.Token == a.Token {
		t.Fatalf("rejoin: %+v vs %+v", b, a)
	}
	if hb(a.Token) != 403 || hb(b.Token) != 200 {
		t.Fatal("token not rotated on rejoin")
	}

	// With a join token configured, only that token joins.
	h.s.cfg.WorkerJoinToken = "secret-join"
	if code := h.do("POST", "/api/workers/register", "", map[string]any{"name": "ci-2"}, nil); code != 403 {
		t.Fatalf("join without token: %d", code)
	}
	if code := h.do("POST", "/api/workers/register", "wrong", map[string]any{"name": "ci-2"}, nil); code != 403 {
		t.Fatalf("join with wrong token: %d", code)
	}
	var c regResp
	if code := h.do("POST", "/api/workers/register", "secret-join", map[string]any{"hostname": "host-2"}, &c); code != 200 || c.WorkerID == a.WorkerID {
		t.Fatalf("join token: %d %+v", code, c)
	}
	if wk, _ := h.s.store.GetWorker(context.Background(), c.WorkerID); wk.Name != "host-2" {
		t.Fatalf("name defaults to hostname, got %q", wk.Name)
	}
	// The join token itself is not a worker credential.
	if hb("secret-join") != 403 {
		t.Fatal("join token accepted as worker token")
	}

	// Long-offline joined workers are cleaned up.
	h.s.store.MarkStaleWorkersOffline(context.Background(), store.Now()+1)
	if n, _ := h.s.store.DeleteStaleJoinedWorkers(context.Background(), store.Now()+1); n != 2 {
		t.Fatalf("pruned %d joined workers", n)
	}
}
