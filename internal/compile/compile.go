// Package compile turns a CLI test definition into the job DAG executed by chiperka-worker.
//
// The output is wire-compatible with the job format Chiperka Cloud sends to
// workers, so the stock chiperka-worker runs it unchanged.
package compile

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"

	"chiperka-hub/internal/model"
)

// Job is a unit of work sent to a worker.
type Job struct {
	JobID          string           `json:"job_id"`
	Token          string           `json:"token,omitempty"`
	Timeout        string           `json:"timeout,omitempty"`
	Steps          map[string]*Step `json:"steps"`
	Finally        []string         `json:"finally,omitempty"`
	Meta           *Meta            `json:"meta,omitempty"`
	Weight         int              `json:"weight"`
	ContainerCount int              `json:"container_count"`
	// RegistryAuth is attached at claim time from the run's in-memory credentials, never stored in the DAG.
	RegistryAuth []RegistryAuth `json:"registry_auth,omitempty"`
}

// RegistryAuth holds credentials the worker uses to pull images from Host.
type RegistryAuth struct {
	Host     string `json:"host"`
	Username string `json:"username"`
	Password string `json:"password"`
}

// Step is a single instruction in the job DAG.
type Step struct {
	Instruction string          `json:"instruction"`
	DependsOn   []string        `json:"depends_on,omitempty"`
	Params      json.RawMessage `json:"params"`
	Run         string          `json:"run,omitempty"`
	Retry       *RetryPolicy    `json:"retry,omitempty"`
	Timeout     string          `json:"timeout,omitempty"`
}

type RetryPolicy struct {
	Max      int    `json:"max"`
	Interval string `json:"interval,omitempty"`
}

// Meta carries identifiers that map job results back to tests.
type Meta struct {
	Type          string `json:"type"`
	JobID         int64  `json:"job_id,omitempty"`
	TestID        int64  `json:"test_id,omitempty"`
	RunID         string `json:"run_id,omitempty"`
	SuiteID       int64  `json:"suite_id,omitempty"`
	UUID          string `json:"uuid,omitempty"`
	Name          string `json:"name,omitempty"`
	SuiteName     string `json:"suite_name,omitempty"`
	SuiteFilePath string `json:"suite_file_path,omitempty"`
	ProjectID     int64  `json:"project_id,omitempty"`
}

// NormalizeSnapshotPaths rewrites suite-relative snapshot paths to project-root
// relative ones ("./__snapshots__/a.json" in "tests/x.chiperka" → "tests/__snapshots__/a.json").
// These match the zip entry names the CLI uploads.
func NormalizeSnapshotPaths(test *model.Test, suiteFilePath string) {
	dir := filepath.Dir(suiteFilePath)
	norm := func(p *string) {
		if *p != "" {
			*p = filepath.ToSlash(filepath.Join(dir, *p))
		}
	}
	for i := range test.Assertions {
		a := &test.Assertions[i]
		if a.Response != nil && a.Response.Body != nil {
			norm(&a.Response.Body.Snapshot)
		}
		if a.CLI != nil {
			if a.CLI.Stdout != nil {
				norm(&a.CLI.Stdout.Snapshot)
			}
			if a.CLI.Stderr != nil {
				norm(&a.CLI.Stderr.Snapshot)
			}
		}
		if a.Artifact != nil {
			norm(&a.Artifact.Snapshot)
		}
	}
}

// SnapshotPaths lists the (normalized) snapshot files a test's assertions reference.
func SnapshotPaths(test model.Test) []string {
	var out []string
	seen := map[string]bool{}
	add := func(p string) {
		if p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, a := range test.Assertions {
		if a.Response != nil && a.Response.Body != nil {
			add(a.Response.Body.Snapshot)
		}
		if a.CLI != nil {
			if a.CLI.Stdout != nil {
				add(a.CLI.Stdout.Snapshot)
			}
			if a.CLI.Stderr != nil {
				add(a.CLI.Stderr.Snapshot)
			}
		}
		if a.Artifact != nil {
			add(a.Artifact.Snapshot)
		}
	}
	return out
}

// Test translates a test definition into a job DAG.
func Test(test model.Test, meta *Meta, timeout string) *Job {
	snapshotPaths := SnapshotPaths(test)
	job := &Job{Timeout: timeout, Steps: map[string]*Step{}, Meta: meta}

	var lastDep string
	chain := func(name string, step *Step) {
		if step == nil {
			return
		}
		if lastDep != "" {
			step.DependsOn = []string{lastDep}
		}
		job.Steps[name] = step
		lastDep = name
	}

	if len(snapshotPaths) > 0 {
		chain("download_files", &Step{Instruction: "download_files", Params: mustJSON(map[string]any{
			"paths":           snapshotPaths,
			"suite_file_path": meta.SuiteFilePath,
		})})
	}

	if len(test.Services) > 0 {
		chain("start_services", &Step{Instruction: "start_services", Params: mustJSON(map[string]any{
			"services":  test.Services,
			"test_name": test.Name,
		})})
	}

	for i, setup := range test.Setup {
		chain(fmt.Sprintf("setup_%d", i), instructionStep(setup, meta.UUID, ""))
	}

	for i, h := range collectHooks(test, "beforeExecution") {
		chain(fmt.Sprintf("hook_before_%d", i), hookStep(h))
	}

	execStep := executionStep(test.Execution, meta.UUID)
	chain("execute", execStep)

	for i, td := range test.Teardown {
		chain(fmt.Sprintf("teardown_%d", i), instructionStep(td, meta.UUID, "always"))
	}

	// afterExecution CLI hooks write files into containers before extraction.
	afterHooks := collectHooks(test, "afterExecution")
	hookIdx := 0
	for _, h := range afterHooks {
		if h.CLI == nil {
			continue
		}
		chain(fmt.Sprintf("hook_after_%d", hookIdx), hookStep(h))
		hookIdx++
	}

	chain("extract_artifacts", &Step{Instruction: "extract_artifacts", Run: "always", Params: json.RawMessage(`{}`)})

	// collect_logs hangs off extract_artifacts but does not become the chain tail.
	job.Steps["collect_logs"] = &Step{Instruction: "collect_logs", Run: "always", DependsOn: []string{lastDep}, Params: json.RawMessage(`{}`)}

	// Diff hooks read extracted artifacts from disk.
	for _, h := range afterHooks {
		if h.Diff == nil {
			continue
		}
		chain(fmt.Sprintf("hook_after_%d", hookIdx), hookStep(h))
		hookIdx++
	}

	assertParams := map[string]any{"assertions": test.Assertions}
	if execStep != nil {
		if test.Execution.Executor == model.ExecutorCLI {
			assertParams["cli_response_step"] = "execute"
		} else {
			assertParams["http_response_step"] = "execute"
		}
		assertParams["execution_duration_step"] = "execute"
	}
	if meta.SuiteFilePath != "" {
		assertParams["suite_file_path"] = meta.SuiteFilePath
	}
	if len(snapshotPaths) > 0 {
		assertParams["download_step"] = "download_files"
	}
	assertParams["artifact_infos_step"] = "extract_artifacts"
	assertDeps := []string{lastDep}
	if execStep != nil && lastDep != "execute" {
		assertDeps = append(assertDeps, "execute")
	}
	job.Steps["assert"] = &Step{Instruction: "assert", DependsOn: assertDeps, Params: mustJSON(assertParams)}

	job.Steps["upload_files"] = &Step{
		Instruction: "upload_files", DependsOn: []string{"assert"}, Run: "always",
		Params: mustJSON(map[string]any{"collect_logs_step": "extract_artifacts"}),
	}

	job.Steps["cleanup"] = &Step{Instruction: "cleanup", Params: json.RawMessage(`{}`)}
	job.Finally = []string{"cleanup"}

	job.Weight = test.Weight()
	job.ContainerCount = test.ContainerCount()
	return job
}

func instructionStep(in model.SetupInstruction, uuid, run string) *Step {
	switch {
	case in.HTTP != nil:
		return &Step{Instruction: "http_request", Run: run, Params: mustJSON(map[string]any{
			"target": in.HTTP.Target, "method": in.HTTP.Request.Method, "url": in.HTTP.Request.URL,
			"headers": in.HTTP.Request.Headers, "body": in.HTTP.Request.Body.Raw, "uuid": uuid,
		})}
	case in.CLI != nil:
		return &Step{Instruction: "cli_exec", Run: run, Params: mustJSON(map[string]any{
			"service": in.CLI.Service, "command": in.CLI.Command, "working_dir": in.CLI.WorkingDir,
		})}
	}
	return nil
}

func executionStep(exec model.Execution, uuid string) *Step {
	switch exec.Executor {
	case model.ExecutorCLI:
		if exec.CLI == nil {
			return nil
		}
		return &Step{Instruction: "cli_exec", Params: mustJSON(map[string]any{
			"service": exec.CLI.Service, "command": exec.CLI.Command, "working_dir": exec.CLI.WorkingDir,
		})}
	default:
		return &Step{Instruction: "http_request", Params: mustJSON(map[string]any{
			"target": exec.Target, "method": exec.Request.Method, "url": exec.Request.URL,
			"headers": exec.Request.Headers, "body": exec.Request.Body.Raw, "uuid": uuid,
		})}
	}
}

// collectHooks gathers hooks of a slot from all services, sorted by priority (default 50).
func collectHooks(test model.Test, slot string) []model.Hook {
	var hooks []model.Hook
	for _, svc := range test.Services {
		for _, h := range svc.Hooks {
			if h.Slot == slot {
				h.ServiceName = svc.Name
				if h.ServiceName == "" {
					h.ServiceName = svc.Ref
				}
				hooks = append(hooks, h)
			}
		}
	}
	prio := func(p int) int {
		if p == 0 {
			return 50
		}
		return p
	}
	sort.SliceStable(hooks, func(i, j int) bool { return prio(hooks[i].Priority) < prio(hooks[j].Priority) })
	return hooks
}

func hookStep(h model.Hook) *Step {
	switch {
	case h.CLI != nil:
		return &Step{Instruction: "cli_exec", Params: mustJSON(map[string]any{
			"service": h.ServiceName, "command": h.CLI.Command, "working_dir": h.CLI.WorkingDir, "fail_on_error": true,
		})}
	case h.Diff != nil:
		return &Step{Instruction: "diff", Params: mustJSON(map[string]any{
			"source": h.Diff.Source, "target": h.Diff.Target, "output": h.Diff.Output,
			"optional": true, "collect_logs_step": "extract_artifacts",
		})}
	}
	return nil
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
