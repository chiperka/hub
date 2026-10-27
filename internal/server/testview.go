package server

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"chiperka-hub/internal/compile"
	"chiperka-hub/internal/store"
)

// stepResult is one entry of the worker's step_results map.
type stepResult struct {
	Status   string           `json:"status"`
	Duration int64            `json:"duration"`
	Error    string           `json:"error,omitempty"`
	Data     map[string]any   `json:"data,omitempty"`
	Timing   map[string]int64 `json:"timing,omitempty"`
}

type StepView struct {
	Name        string          `json:"name"`
	Instruction string          `json:"instruction"`
	Run         string          `json:"run,omitempty"`
	DependsOn   []string        `json:"depends_on,omitempty"`
	Status      string          `json:"status"`
	DurationMs  int64           `json:"duration_ms"`
	Error       string          `json:"error,omitempty"`
	QueuedAt    int64           `json:"queued_at,omitempty"`
	StartedAt   int64           `json:"started_at,omitempty"`
	CompletedAt int64           `json:"completed_at,omitempty"`
	Params      json.RawMessage `json:"params,omitempty"`
	Data        map[string]any  `json:"data,omitempty"`
}

type AssertionView struct {
	Passed     bool   `json:"passed"`
	Type       string `json:"type"`
	Expected   string `json:"expected"`
	Actual     string `json:"actual"`
	Message    string `json:"message"`
	DurationMs int64  `json:"duration_ms"`
}

type HTTPExchangeView struct {
	Phase           string            `json:"phase"`
	Sequence        int               `json:"sequence"`
	Step            string            `json:"step"`
	Method          string            `json:"method"`
	URL             string            `json:"url"`
	RequestHeaders  map[string]string `json:"request_headers,omitempty"`
	RequestBody     string            `json:"request_body,omitempty"`
	StatusCode      int               `json:"status_code"`
	ResponseHeaders any               `json:"response_headers,omitempty"`
	ResponseBody    string            `json:"response_body,omitempty"`
	DurationMs      int64             `json:"duration_ms"`
	Error           string            `json:"error,omitempty"`
}

type CLIExecutionView struct {
	Phase      string `json:"phase"`
	Sequence   int    `json:"sequence"`
	Step       string `json:"step"`
	Service    string `json:"service"`
	Command    string `json:"command"`
	WorkingDir string `json:"working_dir,omitempty"`
	ExitCode   int    `json:"exit_code"`
	Stdout     string `json:"stdout,omitempty"`
	Stderr     string `json:"stderr,omitempty"`
	DurationMs int64  `json:"duration_ms"`
	Error      string `json:"error,omitempty"`
}

type ServiceView struct {
	Name             string `json:"name"`
	Image            string `json:"image"`
	DurationMs       int64  `json:"duration_ms"`
	ImageResolveMs   int64  `json:"image_resolve_ms"`
	ContainerStartMs int64  `json:"container_start_ms"`
	HealthCheckMs    int64  `json:"health_check_ms"`
}

type Phases struct {
	Services  int64 `json:"services_ms"`
	Setup     int64 `json:"setup_ms"`
	Execution int64 `json:"execution_ms"`
	Assertion int64 `json:"assertion_ms"`
	Teardown  int64 `json:"teardown_ms"`
	Cleanup   int64 `json:"cleanup_ms"`
}

type TestView struct {
	Test       *store.Test          `json:"test"`
	Run        *store.Run           `json:"run"`
	Job        *store.Job           `json:"job"`
	Attempts   []store.Job          `json:"attempts"`
	Steps      []StepView           `json:"steps"`
	Assertions []AssertionView      `json:"assertions"`
	HTTP       []HTTPExchangeView   `json:"http"`
	CLI        []CLIExecutionView   `json:"cli"`
	Services   []ServiceView        `json:"services"`
	Artifacts  []store.Artifact     `json:"artifacts"`
	Phases     Phases               `json:"phases"`
	Progress   []store.StepProgress `json:"progress"`
}

func (s *Server) buildTestView(ctx context.Context, t *store.Test) *TestView {
	v := &TestView{Test: t, Steps: []StepView{}, Assertions: []AssertionView{}, HTTP: []HTTPExchangeView{},
		CLI: []CLIExecutionView{}, Services: []ServiceView{}, Progress: []store.StepProgress{}}
	v.Run, _ = s.store.GetRun(ctx, t.RunID)
	v.Attempts, _ = s.store.ListTestJobs(ctx, t.ID)
	v.Artifacts, _ = s.store.ListArtifacts(ctx, t.ID)
	if v.Artifacts == nil {
		v.Artifacts = []store.Artifact{}
	}
	if t.JobID == 0 {
		return v
	}
	job, err := s.store.GetJob(ctx, t.JobID)
	if err != nil {
		return v
	}
	v.Job = job
	v.Progress, _ = s.store.ListStepProgress(ctx, job.ID)

	var dag compile.Job
	json.Unmarshal(job.DAG, &dag)
	results := map[string]stepResult{}
	if len(job.StepResults) > 0 {
		json.Unmarshal(job.StepResults, &results)
	}
	progress := map[string]store.StepProgress{}
	for _, p := range v.Progress {
		progress[p.Step] = p
	}

	for _, name := range stepOrder(&dag) {
		st := dag.Steps[name]
		sv := StepView{Name: name, Instruction: st.Instruction, Run: st.Run, DependsOn: st.DependsOn, Params: st.Params, Status: "pending"}
		if r, ok := results[name]; ok {
			sv.Status, sv.DurationMs, sv.Error, sv.Data = r.Status, r.Duration, r.Error, r.Data
			sv.QueuedAt, sv.StartedAt, sv.CompletedAt = r.Timing["queued_at"], r.Timing["started_at"], r.Timing["completed_at"]
		} else if p, ok := progress[name]; ok {
			sv.Status, sv.DurationMs, sv.Error, sv.StartedAt = p.Status, p.DurationMs, p.Error, p.StartedAt
			if p.Status != "running" {
				sv.CompletedAt = p.UpdatedAt
			}
		} else if job.Status == store.JobCompleted || job.Status == store.JobCancelled {
			sv.Status = "skipped"
		}
		// Keep huge payloads (HTTP bodies, stdout) out of the step list; they have their own views.
		if sv.Data != nil && (strings.HasPrefix(name, "setup_") || strings.HasPrefix(name, "teardown_") || name == "execute") {
			sv.Data = nil
		}
		if name == "assert" {
			sv.Data = nil
		}
		v.Steps = append(v.Steps, sv)
	}

	if r, ok := results["assert"]; ok {
		if arr, ok := r.Data["results"].([]any); ok {
			for _, it := range arr {
				m, _ := it.(map[string]any)
				v.Assertions = append(v.Assertions, AssertionView{
					Passed: m["passed"] == true, Type: str(m["type"]), Expected: str(m["expected"]), Actual: str(m["actual"]),
					Message: str(m["message"]), DurationMs: num(m["duration_ms"]),
				})
			}
		}
		v.Phases.Assertion = r.Duration
	}
	if r, ok := results["start_services"]; ok {
		v.Phases.Services = r.Duration
		if svcs, ok := r.Data["services"].(map[string]any); ok {
			for name, raw := range svcs {
				m, _ := raw.(map[string]any)
				v.Services = append(v.Services, ServiceView{
					Name: name, Image: str(m["image"]), DurationMs: num(m["duration_ms"]),
					ImageResolveMs: num(m["image_resolve_duration_ms"]), ContainerStartMs: num(m["container_start_duration_ms"]),
					HealthCheckMs: num(m["health_check_duration_ms"]),
				})
			}
			sort.Slice(v.Services, func(i, j int) bool { return v.Services[i].Name < v.Services[j].Name })
		}
	}
	if r, ok := results["execute"]; ok {
		v.Phases.Execution = r.Duration
	}
	if r, ok := results["cleanup"]; ok {
		v.Phases.Cleanup = r.Duration
	}

	for _, name := range stepOrder(&dag) {
		phase, seq := stepPhase(name)
		if phase == "" {
			continue
		}
		r, ok := results[name]
		if !ok {
			continue
		}
		switch phase {
		case "setup":
			v.Phases.Setup += r.Duration
		case "teardown":
			v.Phases.Teardown += r.Duration
		}
		var params map[string]any
		if st := dag.Steps[name]; st != nil {
			json.Unmarshal(st.Params, &params)
		}
		instr := ""
		if st := dag.Steps[name]; st != nil {
			instr = st.Instruction
		}
		if _, isHTTP := r.Data["status_code"]; isHTTP || (instr == "http_request" && r.Data == nil) {
			ex := HTTPExchangeView{
				Phase: phase, Sequence: seq, Step: name, Method: str(params["method"]),
				URL: str(params["target"]) + str(params["url"]), RequestBody: str(params["body"]),
				StatusCode: int(num(r.Data["status_code"])), ResponseHeaders: r.Data["headers"], ResponseBody: str(r.Data["body"]),
				DurationMs: r.Duration, Error: r.Error,
			}
			if h, ok := params["headers"].(map[string]any); ok {
				ex.RequestHeaders = map[string]string{}
				for k, val := range h {
					ex.RequestHeaders[k] = str(val)
				}
			}
			v.HTTP = append(v.HTTP, ex)
			continue
		}
		if _, isCLI := r.Data["exit_code"]; isCLI || (instr == "cli_exec" && r.Data == nil) {
			v.CLI = append(v.CLI, CLIExecutionView{
				Phase: phase, Sequence: seq, Step: name, Service: str(params["service"]), Command: str(params["command"]),
				WorkingDir: str(params["working_dir"]), ExitCode: int(num(r.Data["exit_code"])), Stdout: str(r.Data["stdout"]),
				Stderr: str(r.Data["stderr"]), DurationMs: r.Duration, Error: r.Error,
			})
		}
	}
	return v
}

// stepPhase maps DAG step names to report phases (execution/setup/teardown).
func stepPhase(name string) (string, int) {
	switch {
	case name == "execute":
		return "execution", 0
	case strings.HasPrefix(name, "setup_"):
		return "setup", atoiSuffix(name)
	case strings.HasPrefix(name, "teardown_"):
		return "teardown", atoiSuffix(name)
	}
	return "", 0
}

func atoiSuffix(name string) int {
	i := strings.LastIndexByte(name, '_')
	n := 0
	for _, c := range name[i+1:] {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// stepOrder returns DAG steps in topological order (finally steps last).
func stepOrder(dag *compile.Job) []string {
	finally := map[string]bool{}
	for _, f := range dag.Finally {
		finally[f] = true
	}
	indeg := map[string]int{}
	children := map[string][]string{}
	for name, st := range dag.Steps {
		if _, ok := indeg[name]; !ok {
			indeg[name] = 0
		}
		for _, d := range st.DependsOn {
			indeg[name]++
			children[d] = append(children[d], name)
		}
	}
	var ready, out []string
	for name, d := range indeg {
		if d == 0 && !finally[name] {
			ready = append(ready, name)
		}
	}
	sort.Strings(ready)
	for len(ready) > 0 {
		n := ready[0]
		ready = ready[1:]
		out = append(out, n)
		kids := children[n]
		sort.Strings(kids)
		for _, c := range kids {
			indeg[c]--
			if indeg[c] == 0 && !finally[c] {
				ready = append(ready, c)
			}
		}
	}
	seen := map[string]bool{}
	for _, n := range out {
		seen[n] = true
	}
	var rest []string
	for name := range dag.Steps {
		if !seen[name] {
			rest = append(rest, name)
		}
	}
	sort.Strings(rest)
	return append(out, rest...)
}

func str(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case nil:
		return ""
	default:
		b, _ := json.Marshal(x)
		return string(b)
	}
}

func num(v any) int64 {
	switch x := v.(type) {
	case float64:
		return int64(x)
	case int64:
		return x
	case json.Number:
		n, _ := x.Int64()
		return n
	}
	return 0
}
