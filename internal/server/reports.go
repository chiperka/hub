package server

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"chiperka-hub/internal/store"
)

type reportTest struct {
	store.Test
	Failures []AssertionView
}

type reportSuite struct {
	Name     string
	FilePath string
	Tests    []reportTest
	Failed   int
	Errored  int
	Skipped  int
	TimeMs   int64
}

func (s *Server) collectReport(r *http.Request) (*store.Run, []*reportSuite, bool) {
	run, err := s.store.GetRun(r.Context(), r.PathValue("id"))
	if err != nil {
		return nil, nil, false
	}
	tests, _ := s.store.ListRunTests(r.Context(), run.ID)
	var suites []*reportSuite
	idx := map[int64]*reportSuite{}
	for i := range tests {
		t := tests[i]
		su := idx[t.SuiteID]
		if su == nil {
			su = &reportSuite{Name: t.SuiteName, FilePath: t.SuiteFile}
			idx[t.SuiteID] = su
			suites = append(suites, su)
		}
		rt := reportTest{Test: t}
		if t.Status == store.StatusFailed {
			v := s.buildTestView(r.Context(), &t)
			for _, a := range v.Assertions {
				if !a.Passed {
					rt.Failures = append(rt.Failures, a)
				}
			}
		}
		switch t.Status {
		case store.StatusFailed:
			su.Failed++
		case store.StatusError:
			su.Errored++
		case store.StatusSkipped, store.StatusCancelled, store.StatusPending, store.StatusRunning:
			su.Skipped++
		}
		su.TimeMs += t.DurationMs
		su.Tests = append(su.Tests, rt)
	}
	return run, suites, true
}

// ── JUnit ───────────────────────────────────────────────────────────────────

type junitSuites struct {
	XMLName  xml.Name     `xml:"testsuites"`
	Name     string       `xml:"name,attr"`
	Tests    int          `xml:"tests,attr"`
	Failures int          `xml:"failures,attr"`
	Errors   int          `xml:"errors,attr"`
	Skipped  int          `xml:"skipped,attr"`
	Time     string       `xml:"time,attr"`
	Suites   []junitSuite `xml:"testsuite"`
}

type junitSuite struct {
	Name      string      `xml:"name,attr"`
	Tests     int         `xml:"tests,attr"`
	Failures  int         `xml:"failures,attr"`
	Errors    int         `xml:"errors,attr"`
	Skipped   int         `xml:"skipped,attr"`
	Time      string      `xml:"time,attr"`
	Timestamp string      `xml:"timestamp,attr"`
	File      string      `xml:"file,attr,omitempty"`
	Cases     []junitCase `xml:"testcase"`
}

type junitCase struct {
	Name      string        `xml:"name,attr"`
	Classname string        `xml:"classname,attr"`
	Time      string        `xml:"time,attr"`
	Failure   *junitMessage `xml:"failure,omitempty"`
	Error     *junitMessage `xml:"error,omitempty"`
	Skipped   *junitMessage `xml:"skipped,omitempty"`
}

type junitMessage struct {
	Message string `xml:"message,attr,omitempty"`
	Body    string `xml:",chardata"`
}

func secs(ms int64) string { return fmt.Sprintf("%.3f", float64(ms)/1000) }

func (s *Server) handleReportJUnit(w http.ResponseWriter, r *http.Request) {
	run, suites, ok := s.collectReport(r)
	if !ok {
		writeError(w, http.StatusNotFound, "run not found")
		return
	}
	out := junitSuites{Name: "chiperka " + run.ID}
	var total int64
	for _, su := range suites {
		js := junitSuite{Name: su.Name, Tests: len(su.Tests), Failures: su.Failed, Errors: su.Errored, Skipped: su.Skipped,
			Time: secs(su.TimeMs), Timestamp: time.UnixMilli(run.StartedAt).UTC().Format(time.RFC3339), File: su.FilePath}
		for _, t := range su.Tests {
			jc := junitCase{Name: t.Name, Classname: su.Name, Time: secs(t.DurationMs)}
			switch t.Status {
			case store.StatusFailed:
				var b strings.Builder
				for _, f := range t.Failures {
					fmt.Fprintf(&b, "[%s] %s\n  expected: %s\n  actual:   %s\n", f.Type, f.Message, f.Expected, f.Actual)
				}
				jc.Failure = &junitMessage{Message: firstNonEmpty(t.ErrorMessage, "assertion failed"), Body: b.String()}
			case store.StatusError:
				jc.Error = &junitMessage{Message: t.ErrorMessage, Body: t.ErrorMessage}
			case store.StatusSkipped, store.StatusCancelled, store.StatusPending, store.StatusRunning:
				jc.Skipped = &junitMessage{Message: t.Status}
			}
			js.Cases = append(js.Cases, jc)
		}
		out.Tests += js.Tests
		out.Failures += js.Failures
		out.Errors += js.Errors
		out.Skipped += js.Skipped
		total += su.TimeMs
		out.Suites = append(out.Suites, js)
	}
	out.Time = secs(total)
	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="chiperka-%s.xml"`, run.ID))
	io.WriteString(w, xml.Header)
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	enc.Encode(out)
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

// ── HTML ────────────────────────────────────────────────────────────────────

var reportTmpl = template.Must(template.New("report").Funcs(template.FuncMap{
	"dur": func(ms int64) string {
		if ms < 1000 {
			return fmt.Sprintf("%dms", ms)
		}
		return fmt.Sprintf("%.2fs", float64(ms)/1000)
	},
	"ts": func(ms int64) string { return time.UnixMilli(ms).Format("2006-01-02 15:04:05") },
}).Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Chiperka report {{.Run.ID}}</title>
<style>
:root{--bg:#0b0d12;--panel:#12151c;--line:#232835;--text:#e6e8ee;--muted:#8b93a7;--ok:#22c55e;--bad:#ef4444;--warn:#f59e0b;--skip:#64748b}
@media (prefers-color-scheme: light){:root{--bg:#f6f7f9;--panel:#fff;--line:#e3e6ec;--text:#141821;--muted:#5d667a}}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--text);font:14px/1.5 ui-sans-serif,system-ui,-apple-system,Segoe UI,Roboto,sans-serif}
main{max-width:1100px;margin:0 auto;padding:32px 16px}
h1{font-size:20px;margin:0 0 4px}.muted{color:var(--muted)}
.cards{display:grid;grid-template-columns:repeat(auto-fit,minmax(130px,1fr));gap:12px;margin:24px 0}
.card{background:var(--panel);border:1px solid var(--line);border-radius:10px;padding:14px}
.card b{display:block;font-size:22px}
.suite{background:var(--panel);border:1px solid var(--line);border-radius:10px;margin:14px 0;overflow:hidden}
.suite h2{font-size:14px;margin:0;padding:12px 16px;border-bottom:1px solid var(--line);display:flex;justify-content:space-between}
.t{padding:10px 16px;border-bottom:1px solid var(--line)}.t:last-child{border-bottom:0}
.row{display:flex;gap:10px;align-items:center}.name{flex:1}
.pill{font-size:11px;font-weight:600;text-transform:uppercase;padding:2px 8px;border-radius:99px;color:#fff}
.passed{background:var(--ok)}.failed{background:var(--bad)}.error{background:var(--warn)}.skipped,.cancelled,.pending,.running{background:var(--skip)}
pre{background:var(--bg);border:1px solid var(--line);border-radius:8px;padding:10px;white-space:pre-wrap;word-break:break-word;font-size:12px;margin:8px 0 0}
</style></head><body><main>
<h1>Run {{.Run.ID}}</h1>
<div class="muted">{{if .Run.ProjectSlug}}{{.Run.ProjectSlug}} · {{end}}started {{ts .Run.StartedAt}} · <span class="pill {{.Run.Status}}">{{.Run.Status}}</span></div>
<div class="cards">
<div class="card"><span class="muted">Total</span><b>{{.Run.Total}}</b></div>
<div class="card"><span class="muted">Passed</span><b style="color:var(--ok)">{{.Run.Passed}}</b></div>
<div class="card"><span class="muted">Failed</span><b style="color:var(--bad)">{{.Run.Failed}}</b></div>
<div class="card"><span class="muted">Errors</span><b style="color:var(--warn)">{{.Run.Errored}}</b></div>
<div class="card"><span class="muted">Skipped</span><b>{{.Skipped}}</b></div>
</div>
{{range .Suites}}<section class="suite"><h2><span>{{.Name}}</span><span class="muted">{{.FilePath}}</span></h2>
{{range .Tests}}<div class="t"><div class="row"><span class="pill {{.Status}}">{{.Status}}</span><span class="name">{{.Name}}</span><span class="muted">{{dur .DurationMs}}</span></div>
{{if .ErrorMessage}}<pre>{{.ErrorMessage}}</pre>{{end}}
{{range .Failures}}<pre>[{{.Type}}] {{.Message}}
expected: {{.Expected}}
actual:   {{.Actual}}</pre>{{end}}
</div>{{end}}</section>{{end}}
<p class="muted">Generated by chiperka-hub {{.Version}}</p>
</main></body></html>`))

func (s *Server) renderHTML(w io.Writer, r *http.Request) bool {
	run, suites, ok := s.collectReport(r)
	if !ok {
		return false
	}
	reportTmpl.Execute(w, map[string]any{"Run": run, "Suites": suites, "Skipped": run.Skipped + run.Cancelled, "Version": s.cfg.Version})
	return true
}

func (s *Server) handleReportHTML(w http.ResponseWriter, r *http.Request) {
	var buf bytes.Buffer
	if !s.renderHTML(&buf, r) {
		writeError(w, http.StatusNotFound, "run not found")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(buf.Bytes())
}

// GET /api/runs/{id}/report.zip — the CLI extracts it into its --html directory.
func (s *Server) handleReportZip(w http.ResponseWriter, r *http.Request) {
	var html bytes.Buffer
	if !s.renderHTML(&html, r) {
		writeError(w, http.StatusNotFound, "run not found")
		return
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, _ := zw.Create("index.html")
	f.Write(html.Bytes())
	zw.Close()
	w.Header().Set("Content-Type", "application/zip")
	w.Write(buf.Bytes())
}

// GET /api/runs/{id}/artifacts.zip — entries are "<test uuid>/<artifact name>"; 404 when empty.
func (s *Server) handleArtifactsZip(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	arts, err := s.store.ListRunArtifacts(r.Context(), runID)
	if err != nil {
		storeError(w, err)
		return
	}
	if len(arts) == 0 {
		writeError(w, http.StatusNotFound, "no artifacts")
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="artifacts-%s.zip"`, runID))
	zw := zip.NewWriter(w)
	for _, a := range arts {
		src, err := os.Open(s.artifactPath(runID, a.TestUUID, a.Name))
		if err != nil {
			continue
		}
		dst, err := zw.Create(a.TestUUID + "/" + a.Name)
		if err == nil {
			io.Copy(dst, src)
		}
		src.Close()
	}
	zw.Close()
}
