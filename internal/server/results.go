package server

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"chiperka-hub/internal/store"
)

// Results API — mirrors chiperka's internal/result types so the CLI's CloudStore
// (used by MCP tools such as chiperka_read_run / chiperka_read_test) can read
// remote runs. Run UUIDs are "cr-<run id>", test UUIDs start with "ct-".

type resultRunSummary struct {
	UUID      string          `json:"uuid"`
	Status    string          `json:"status"`
	StartedAt time.Time       `json:"started_at"`
	Duration  int64           `json:"duration_ms"`
	Passed    int             `json:"passed"`
	Failed    int             `json:"failed"`
	Errored   int             `json:"errored"`
	Skipped   int             `json:"skipped"`
	Total     int             `json:"total"`
	Tests     []resultTestRef `json:"tests"`
}

type resultTestRef struct {
	UUID     string `json:"uuid"`
	Name     string `json:"name"`
	Suite    string `json:"suite"`
	Status   string `json:"status"`
	Duration int64  `json:"duration_ms"`
}

type resultAssertion struct {
	Passed   bool   `json:"passed"`
	Type     string `json:"type"`
	Expected string `json:"expected"`
	Actual   string `json:"actual"`
	Message  string `json:"message"`
}

type resultHTTPExchange struct {
	Phase          string            `json:"phase"`
	Sequence       int               `json:"sequence"`
	Method         string            `json:"method"`
	URL            string            `json:"url"`
	RequestHeaders map[string]string `json:"request_headers,omitempty"`
	RequestBody    string            `json:"request_body,omitempty"`
	StatusCode     int               `json:"status_code"`
	ResponseBody   string            `json:"response_body,omitempty"`
	Duration       int64             `json:"duration_ms"`
	Error          string            `json:"error,omitempty"`
}

type resultCLIExecution struct {
	Phase      string `json:"phase"`
	Sequence   int    `json:"sequence"`
	Service    string `json:"service"`
	Command    string `json:"command"`
	WorkingDir string `json:"working_dir,omitempty"`
	ExitCode   int    `json:"exit_code"`
	Stdout     string `json:"stdout,omitempty"`
	Stderr     string `json:"stderr,omitempty"`
	Duration   int64  `json:"duration_ms"`
	Error      string `json:"error,omitempty"`
}

type resultService struct {
	Name     string `json:"name"`
	Image    string `json:"image"`
	Duration int64  `json:"duration_ms"`
}

type resultStep struct {
	Type     string `json:"type"`
	Duration int64  `json:"duration_ms"`
	Success  bool   `json:"success"`
	Error    string `json:"error,omitempty"`
}

type resultArtifact struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

type resultPhases struct {
	Network   int64 `json:"network_ms"`
	Services  int64 `json:"services_ms"`
	Setup     int64 `json:"setup_ms"`
	Execution int64 `json:"execution_ms"`
	Assertion int64 `json:"assertion_ms"`
	Teardown  int64 `json:"teardown_ms"`
	Cleanup   int64 `json:"cleanup_ms"`
}

type resultTestDetail struct {
	UUID          string               `json:"uuid"`
	Name          string               `json:"name"`
	Suite         string               `json:"suite"`
	Status        string               `json:"status"`
	Duration      int64                `json:"duration_ms"`
	Error         string               `json:"error,omitempty"`
	Assertions    []resultAssertion    `json:"assertions"`
	HTTPExchanges []resultHTTPExchange `json:"http_exchanges,omitempty"`
	CLIExecutions []resultCLIExecution `json:"cli_executions,omitempty"`
	Services      []resultService      `json:"services,omitempty"`
	Setup         []resultStep         `json:"setup,omitempty"`
	Teardown      []resultStep         `json:"teardown,omitempty"`
	Artifacts     []resultArtifact     `json:"artifacts"`
	Phases        resultPhases         `json:"phases"`
	// Extras (ignored by the CLI, handy for humans and agents).
	RunID  string `json:"run_id"`
	WebURL string `json:"web_url,omitempty"`
}

func (s *Server) runSummary(ctx context.Context, run *store.Run, withTests bool) resultRunSummary {
	end := run.CompletedAt
	if end == 0 {
		end = store.Now()
	}
	status := run.Status
	if status == "cancelled" {
		status = "error"
	}
	rs := resultRunSummary{
		UUID: "cr-" + run.ID, Status: status, StartedAt: time.UnixMilli(run.StartedAt), Duration: end - run.StartedAt,
		Passed: run.Passed, Failed: run.Failed, Errored: run.Errored, Skipped: run.Skipped + run.Cancelled, Total: run.Total,
		Tests: []resultTestRef{},
	}
	if withTests {
		tests, _ := s.store.ListRunTests(ctx, run.ID)
		for _, t := range tests {
			rs.Tests = append(rs.Tests, resultTestRef{UUID: t.UUID, Name: t.Name, Suite: t.SuiteName, Status: t.Status, Duration: t.DurationMs})
		}
	}
	return rs
}

func (s *Server) handleResultsRuns(w http.ResponseWriter, r *http.Request) {
	runs, _, err := s.store.ListRuns(r.Context(), store.RunFilter{Limit: queryInt(r, "limit", 20), Query: r.URL.Query().Get("project")})
	if err != nil {
		storeError(w, err)
		return
	}
	out := make([]resultRunSummary, 0, len(runs))
	for i := range runs {
		out = append(out, s.runSummary(r.Context(), &runs[i], false))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleResultsRun(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.PathValue("id"), "cr-")
	run, err := s.store.GetRun(r.Context(), id)
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.runSummary(r.Context(), run, true))
}

func (s *Server) handleResultsTest(w http.ResponseWriter, r *http.Request) {
	t, err := s.store.GetTestByUUID(r.Context(), r.PathValue("uuid"))
	if err != nil {
		storeError(w, err)
		return
	}
	v := s.buildTestView(r.Context(), t)
	d := resultTestDetail{
		UUID: t.UUID, Name: t.Name, Suite: t.SuiteName, Status: t.Status, Duration: t.DurationMs, Error: t.ErrorMessage,
		Assertions: []resultAssertion{}, Artifacts: []resultArtifact{}, RunID: t.RunID,
		WebURL: s.publicURL(r) + "/tests/" + itoa(t.ID),
		Phases: resultPhases{Services: v.Phases.Services, Setup: v.Phases.Setup, Execution: v.Phases.Execution,
			Assertion: v.Phases.Assertion, Teardown: v.Phases.Teardown, Cleanup: v.Phases.Cleanup},
	}
	for _, a := range v.Assertions {
		d.Assertions = append(d.Assertions, resultAssertion{Passed: a.Passed, Type: a.Type, Expected: a.Expected, Actual: a.Actual, Message: a.Message})
	}
	for _, h := range v.HTTP {
		d.HTTPExchanges = append(d.HTTPExchanges, resultHTTPExchange{Phase: h.Phase, Sequence: h.Sequence, Method: h.Method,
			URL: h.URL, RequestHeaders: h.RequestHeaders, RequestBody: h.RequestBody, StatusCode: h.StatusCode,
			ResponseBody: h.ResponseBody, Duration: h.DurationMs, Error: h.Error})
	}
	for _, c := range v.CLI {
		d.CLIExecutions = append(d.CLIExecutions, resultCLIExecution{Phase: c.Phase, Sequence: c.Sequence, Service: c.Service,
			Command: c.Command, WorkingDir: c.WorkingDir, ExitCode: c.ExitCode, Stdout: c.Stdout, Stderr: c.Stderr,
			Duration: c.DurationMs, Error: c.Error})
	}
	for _, sv := range v.Services {
		d.Services = append(d.Services, resultService{Name: sv.Name, Image: sv.Image, Duration: sv.DurationMs})
	}
	for _, st := range v.Steps {
		phase, _ := stepPhase(st.Name)
		if phase != "setup" && phase != "teardown" {
			continue
		}
		typ := "cli"
		if st.Instruction == "http_request" {
			typ = "http"
		}
		rs := resultStep{Type: typ, Duration: st.DurationMs, Success: st.Status == "completed", Error: st.Error}
		if phase == "setup" {
			d.Setup = append(d.Setup, rs)
		} else {
			d.Teardown = append(d.Teardown, rs)
		}
	}
	for _, a := range v.Artifacts {
		d.Artifacts = append(d.Artifacts, resultArtifact{Name: a.Name, Size: a.Size})
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) handleResultsArtifact(w http.ResponseWriter, r *http.Request) {
	t, err := s.store.GetTestByUUID(r.Context(), r.PathValue("uuid"))
	if err != nil {
		storeError(w, err)
		return
	}
	s.serveArtifact(w, r, t, r.PathValue("name"))
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
