//go:build integration

// Package integration runs Spoor against real VictoriaMetrics, VictoriaLogs and
// VictoriaTraces. It needs empty backends: start them with `make it-up`.
//
//	go test -tags integration ./test/integration/
//
// With SPOOR_RECORD=1 it also refreshes the offline fixtures used by the golden
// tests in internal/digest.
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/asinha07/spoor/internal/backend"
	"github.com/asinha07/spoor/internal/cases"
	"github.com/asinha07/spoor/internal/config"
	"github.com/asinha07/spoor/internal/digest"
	"github.com/asinha07/spoor/internal/gen"
	"github.com/asinha07/spoor/internal/replay"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

var (
	metricsURL = env("SPOOR_METRICS_URL", "http://localhost:8428")
	logsURL    = env("SPOOR_LOGS_URL", "http://localhost:9428")
	tracesURL  = env("SPOOR_TRACES_URL", "http://localhost:10428")
)

const slot = 5 * time.Hour

func repoRoot(t *testing.T) string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..")
}

func requireHealthy(t *testing.T) {
	for _, u := range []string{metricsURL, logsURL, tracesURL} {
		resp, err := http.Get(u + "/health")
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("backend %s not reachable (start them with `make it-up`): %v", u, err)
		}
		resp.Body.Close()
	}
}

// requireEmpty refuses to run on backends that already hold Spoor test data,
// because overlapping scenarios would corrupt each other's baselines.
func requireEmpty(t *testing.T, ctx context.Context) {
	if os.Getenv("SPOOR_IT_DIRTY_OK") == "1" {
		return
	}
	m := backend.NewMetrics(metricsURL, "", 10*time.Second)
	now := time.Now()
	vals, err := m.LabelValues(ctx, "service_name", now.Add(-40*time.Hour), now)
	if err != nil {
		t.Fatalf("checking backends: %v", err)
	}
	if len(vals) > 0 {
		t.Fatalf("backends already contain data for %v; reset them with `make it-reset` (or set SPOOR_IT_DIRTY_OK=1)", vals)
	}
}

type loaded struct {
	plan  gen.Plan
	truth gen.GroundTruth
}

func generate(t *testing.T, ctx context.Context) []loaded {
	sink := gen.HTTPSink{MetricsURL: metricsURL, LogsURL: logsURL, TracesURL: tracesURL}
	end := time.Now().Add(-2 * time.Minute).UTC().Truncate(time.Minute)
	var out []loaded
	for i, s := range gen.Scenarios {
		fe := end.Add(-time.Duration(i) * slot)
		p := gen.Plan{Scenario: s, FaultEnd: fe, FaultStart: fe.Add(-30 * time.Minute), DataStart: fe.Add(-30 * time.Minute * 8), Seed: 42 + int64(i)}
		if _, err := gen.New(p).Run(ctx, sink); err != nil {
			t.Fatalf("generate %s: %v", s.Name, err)
		}
		out = append(out, loaded{p, p.Truth()})
	}
	sink.Flush(ctx)
	// Give the backends a moment to make the last batches searchable.
	time.Sleep(3 * time.Second)
	return out
}

func newEngine(rec *replay.Recorder) *digest.Engine {
	// Links use the default URLs so recorded golden output does not depend on
	// where the backends ran.
	cfg := config.Default()
	m := backend.NewMetrics(metricsURL, "", time.Minute)
	l := backend.NewLogsQL(logsURL, "", time.Minute)
	tr := backend.NewLogsQL(tracesURL, "", time.Minute)
	return digest.New(cfg, rec.Metrics(m), rec.Logs("logs", l), rec.Logs("traces", tr))
}

func TestScenarios(t *testing.T) {
	ctx := context.Background()
	requireHealthy(t)
	requireEmpty(t, ctx)
	data := generate(t, ctx)

	record := os.Getenv("SPOOR_RECORD") == "1"
	dir := filepath.Join(repoRoot(t), "internal", "digest", "testdata", "scenarios")
	if record {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	for _, d := range data {
		d := d
		t.Run(d.plan.Scenario.Name, func(t *testing.T) {
			clock := d.plan.FaultEnd.Add(time.Minute)
			rec := replay.NewRecorder(clock)
			eng := newEngine(rec)
			eng.SetClock(func() time.Time { return clock })
			results := map[string]any{}
			for _, c := range cases.For(d.truth) {
				out, err := cases.Run(ctx, eng, c)
				if err != nil {
					t.Fatalf("%s: %v", c.Name, err)
				}
				results[c.Name] = out
			}
			all := results["what_changed_all"].(digest.Envelope)
			for _, sig := range []string{digest.SignalMetrics, digest.SignalLogs, digest.SignalTraces} {
				if cov := all.Coverage[sig]; cov != "ok" {
					t.Errorf("coverage %s = %q, want ok", sig, cov)
				}
			}
			if !strings.Contains(all.Coverage["logs_lines"], "error lines in") {
				t.Errorf("coverage should count log lines: %v", all.Coverage)
			}
			logTop(t, all)
			if d.truth.Service == "" {
				if len(all.Findings) != 0 {
					t.Errorf("control scenario: want no findings, got %d (top: %s)", len(all.Findings), all.Findings[0].Title)
				}
				if len(all.SteadyErrors) == 0 || !strings.Contains(all.Summary, "not a new problem") {
					t.Errorf("control scenario: want routine errors named as steady, got summary %q", all.Summary)
				}
			} else {
				rank := rankOf(all.Findings, d.truth.Service)
				if rank == 0 || rank > 3 {
					t.Errorf("ground-truth service %s ranked %d, want top 3", d.truth.Service, rank)
				} else {
					t.Logf("ground truth %s ranked #%d", d.truth.Service, rank)
				}
			}
			// A baseline with no data must say so, never report changes.
			week := results["what_changed_all_empty_7d_baseline"].(digest.Envelope)
			if len(week.Findings) != 0 {
				t.Errorf("7d baseline has no data, yet %d findings were reported (first: %s)", len(week.Findings), week.Findings[0].Title)
			}
			if !strings.HasPrefix(week.Summary, "No baseline data") {
				t.Errorf("7d baseline summary = %q, want a no-baseline-data message", week.Summary)
			}
			small := results["what_changed_all_default_budget"].(digest.Envelope)
			b, _ := digest.MarshalCompact(small)
			if got := digest.EstimateTokens(b); got > digest.DefaultBudget {
				t.Errorf("default-budget response is %d tokens, want <= %d", got, digest.DefaultBudget)
			}
			if record {
				if err := rec.Save(filepath.Join(dir, d.truth.Scenario+".fixture.json.gz"), "recorded by test/integration; regenerate with `make record`"); err != nil {
					t.Fatal(err)
				}
				tb, _ := json.MarshalIndent(d.truth, "", "  ")
				if err := os.WriteFile(filepath.Join(dir, d.truth.Scenario+".truth.json"), append(tb, '\n'), 0o644); err != nil {
					t.Fatal(err)
				}
			}
		})
	}

	t.Run("cli", func(t *testing.T) {
		bin := filepath.Join(t.TempDir(), "spoor")
		build := exec.Command("go", "build", "-o", bin, "./cmd/spoor")
		build.Dir = repoRoot(t)
		if out, err := build.CombinedOutput(); err != nil {
			t.Fatalf("build: %v\n%s", err, out)
		}
		tr := data[0].truth
		cmd := exec.Command(bin, "what-changed", "--since", tr.Since, "--until", tr.Until)
		cmd.Env = append(os.Environ(), "SPOOR_METRICS_URL="+metricsURL, "SPOOR_LOGS_URL="+logsURL, "SPOOR_TRACES_URL="+tracesURL)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("spoor what-changed: %v", err)
		}
		var env digest.Envelope
		if err := json.Unmarshal(out, &env); err != nil {
			t.Fatalf("CLI output is not an envelope: %v\n%s", err, out)
		}
		if len(env.Findings) == 0 || env.Findings[0].Service != tr.Service {
			t.Errorf("CLI top finding = %+v, want service %s", env.Findings, tr.Service)
		}
	})
}

func rankOf(fs []digest.Finding, svc string) int {
	for i, f := range fs {
		if f.Service == svc {
			return i + 1
		}
	}
	return 0
}

func logTop(t *testing.T, env digest.Envelope) {
	var b strings.Builder
	for i, f := range env.Findings {
		if i == 5 {
			break
		}
		fmt.Fprintf(&b, "\n  %.2f %-16s %-20s %s", f.Score, f.Service, f.Kind, f.Title)
	}
	t.Logf("summary: %s%s", env.Summary, b.String())
}
