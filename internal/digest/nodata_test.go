package digest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/asinha07/spoor/internal/backend"
	"github.com/asinha07/spoor/internal/config"
)

// fakeMetrics answers request-rate queries from a fixed per-service history.
// window[svc] is the rate during the window (absent = service silent);
// base[svc] is the steady rate during every baseline window.
type fakeMetrics struct {
	window map[string]float64
	base   map[string]float64
	err    error
}

func (f fakeMetrics) svc(q string) string {
	i := strings.Index(q, `service_name="`)
	if i < 0 {
		return ""
	}
	rest := q[i+len(`service_name="`):]
	return rest[:strings.Index(rest, `"`)]
}

func isRequestRate(q string) bool {
	return strings.HasPrefix(q, "sum(rate(http_server_request_duration_seconds_count") && !strings.Contains(q, "/")
}

func (f fakeMetrics) Instant(_ context.Context, q string, at time.Time) ([]backend.Series, error) {
	if f.err != nil {
		return nil, f.err
	}
	if !isRequestRate(q) {
		return nil, nil
	}
	v, ok := f.window[f.svc(q)]
	if !ok {
		return nil, nil
	}
	return []backend.Series{{Points: []backend.Point{{T: at, V: v}}}}, nil
}

func (f fakeMetrics) Range(_ context.Context, q string, start, end time.Time, step time.Duration) ([]backend.Series, error) {
	if f.err != nil {
		return nil, f.err
	}
	v, ok := f.base[f.svc(q)]
	if !isRequestRate(q) || !ok {
		return nil, nil
	}
	var s backend.Series
	for t := start; !t.After(end); t = t.Add(step) {
		s.Points = append(s.Points, backend.Point{T: t, V: v * (1 + 0.01*float64(t.Unix()%3))})
	}
	return []backend.Series{s}, nil
}

func (f fakeMetrics) LabelValues(context.Context, string, time.Time, time.Time) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	var out []string
	for s := range f.base {
		out = append(out, s)
	}
	return out, nil
}

func metricsOnly(m MetricsSource) *Engine {
	e := New(config.Default(), m, nil, nil)
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	e.SetClock(func() time.Time { return now })
	return e
}

func TestSilentServiceIsReported(t *testing.T) {
	e := metricsOnly(fakeMetrics{
		window: map[string]float64{"frontend": 10},
		base:   map[string]float64{"frontend": 10, "ad": 6},
	})
	env, err := e.WhatChanged(context.Background(), Request{BudgetTokens: MaxBudget})
	if err != nil {
		t.Fatal(err)
	}
	if len(env.Findings) != 1 || env.Findings[0].Service != "ad" || env.Findings[0].Kind != KindThroughputDown {
		t.Fatalf("want one throughput_down for ad, got %+v", env.Findings)
	}
	if f := env.Findings[0]; f.After != "0.00/s" || f.Score < 0.9 || !strings.Contains(f.Title, "dropped to zero") {
		t.Errorf("silent service finding = %+v, want a high-scoring drop to zero", f)
	}
}

func TestUnknownServiceSaysNoData(t *testing.T) {
	e := metricsOnly(fakeMetrics{window: map[string]float64{"frontend": 10}, base: map[string]float64{"frontend": 10}})
	env, err := e.WhatChanged(context.Background(), Request{Service: "nosuchsvc"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(env.Summary, `No data for service "nosuchsvc"`) {
		t.Errorf("summary = %q", env.Summary)
	}
	if got := env.Coverage[SignalMetrics]; got != `no data for service "nosuchsvc"` {
		t.Errorf("coverage = %q", got)
	}
	if env.Coverage[SignalLogs] != "not configured" {
		t.Errorf("logs coverage = %q", env.Coverage[SignalLogs])
	}
}

func TestEmptyWindowSaysNoData(t *testing.T) {
	e := metricsOnly(fakeMetrics{})
	env, err := e.WhatChanged(context.Background(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(env.Summary, "No data") || env.Coverage[SignalMetrics] != "no data in window" {
		t.Errorf("summary %q coverage %v", env.Summary, env.Coverage)
	}
}

func TestBackendsDownAreNotOK(t *testing.T) {
	e := metricsOnly(fakeMetrics{err: errors.New("dial tcp: connection refused")})
	env, err := e.WhatChanged(context.Background(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(env.Coverage[SignalMetrics], "error") {
		t.Errorf("coverage = %q, want an error", env.Coverage[SignalMetrics])
	}
	if !strings.HasPrefix(env.Summary, "No data") {
		t.Errorf("summary = %q", env.Summary)
	}
}

func TestScanCapIsReported(t *testing.T) {
	base := map[string]float64{}
	for _, s := range []string{"a", "b", "c", "d"} {
		base[s] = 1
	}
	e := metricsOnly(fakeMetrics{window: base, base: base})
	e.cfg.MaxServices = 2
	env, err := e.WhatChanged(context.Background(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(env.Coverage["scope"], "first 2 of 4") {
		t.Errorf("scope note = %q", env.Coverage["scope"])
	}
}

func TestFitTrimsLongCoverage(t *testing.T) {
	long := "error: unavailable: /select/logsql/query returned 400: " + strings.Repeat("<bad> ", 30)
	env := Envelope{Summary: strings.Repeat("s", 300), Findings: []Finding{},
		Coverage: map[string]string{"metrics": long, "logs": long, "traces": long, "attribution": long}}
	out := Fit(env, MinBudget)
	b, _ := MarshalCompact(out)
	if EstimateTokens(b) > MinBudget {
		t.Errorf("%d tokens over the %d budget: %s", EstimateTokens(b), MinBudget, b)
	}
	if out.Coverage["metrics"] != "error" {
		t.Errorf("trimmed coverage = %q, want its first word", out.Coverage["metrics"])
	}
}

func TestMarshalCompactKeepsAngleBrackets(t *testing.T) {
	b, err := MarshalCompact(Finding{Title: "pattern <N>"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "<N>") {
		t.Errorf("escaped: %s", b)
	}
	var f Finding
	if err := json.Unmarshal(b, &f); err != nil || f.Title != "pattern <N>" {
		t.Errorf("round trip failed: %v %+v", err, f)
	}
}

func TestIgnoredServicesSkipped(t *testing.T) {
	e := metricsOnly(fakeMetrics{
		window: map[string]float64{"frontend": 10},
		base:   map[string]float64{"frontend": 10, "load-generator": 6},
	})
	e.cfg.IgnoreServices = []string{"load-generator"}
	env, err := e.WhatChanged(context.Background(), Request{BudgetTokens: MaxBudget})
	if err != nil {
		t.Fatal(err)
	}
	if len(env.Findings) != 0 {
		t.Errorf("ignored service still reported: %+v", env.Findings)
	}
	// Asking for it by name still works.
	env, err = e.WhatChanged(context.Background(), Request{Service: "load-generator", BudgetTokens: MaxBudget})
	if err != nil {
		t.Fatal(err)
	}
	if len(env.Findings) != 1 {
		t.Errorf("named ignored service should be scored, got %+v", env.Findings)
	}
}

func TestRatioResourceFloor(t *testing.T) {
	cfg := config.Default()
	cfg.Metrics.Resources = []config.ResourceQuery{{Name: "cpu", Unit: "ratio", Query: "x"}}
	e := New(cfg, nil, nil, nil)
	specs := e.metricSpecs()
	if got := specs[len(specs)-1].absFloor; got != 0.05 {
		t.Errorf("cpu ratio floor = %v, want 0.05", got)
	}
	z, _ := robustZ(0.006, []float64{0.001, 0.001, 0.0012, 0.0009}, 0.25, 0.05)
	if zScore(z) >= 0.5 {
		t.Errorf("0.1%% -> 0.6%% CPU scored %v, want below threshold", zScore(z))
	}
}

func TestCheckBaselineEmptyDropsFindings(t *testing.T) {
	fs := []Finding{
		{Service: "payment", Signal: SignalLogs, Kind: KindNewLogPattern, Title: "New error log pattern"},
		{Service: "payment", Signal: SignalMetrics, Kind: KindErrorRateUp, Title: "Error rate up"},
	}
	base := ServiceList{
		Services: []ServiceInfo{{Name: "payment", Signals: []string{SignalMetrics}}},
		Coverage: map[string]string{SignalMetrics: "ok", SignalLogs: "ok", SignalTraces: "ok"},
	}
	wanted := map[string]string{SignalMetrics: "", SignalLogs: "", SignalTraces: "not configured"}
	out, notes := checkBaseline(fs, base, wanted)
	if len(out) != 1 || out[0].Signal != SignalMetrics {
		t.Fatalf("want only the metrics finding kept, got %+v", out)
	}
	if notes[SignalLogs] != noBaselineNote {
		t.Errorf("logs note = %q", notes[SignalLogs])
	}
	if _, ok := notes[SignalTraces]; ok {
		t.Error("an unconfigured signal must not get a baseline note")
	}
}

func TestCheckBaselineMarksNewService(t *testing.T) {
	fs := []Finding{{Service: "newsvc", Signal: SignalLogs, Kind: KindNewLogPattern, Title: "New error log pattern"}}
	base := ServiceList{
		Services: []ServiceInfo{{Name: "payment", Signals: []string{SignalLogs}}},
		Coverage: map[string]string{SignalLogs: "ok"},
	}
	out, notes := checkBaseline(fs, base, map[string]string{SignalLogs: ""})
	if len(notes) != 0 || len(out) != 1 || !strings.Contains(out[0].Title, "no baseline data for this service") {
		t.Errorf("want the finding kept and marked, got %+v %v", out, notes)
	}
}

func TestCheckBaselineError(t *testing.T) {
	base := ServiceList{Coverage: map[string]string{SignalLogs: "error: unavailable: boom"}}
	_, notes := checkBaseline(nil, base, map[string]string{SignalLogs: ""})
	if !strings.HasPrefix(notes[SignalLogs], "baseline check failed") {
		t.Errorf("note = %q", notes[SignalLogs])
	}
}

func TestSteadyErrorsAggregatesFragments(t *testing.T) {
	// Ten product IDs, each a separate pattern of about one error per window.
	cur, base := map[string]patternStat{}, map[string]patternStat{}
	for i := 0; i < 10; i++ {
		p := fmt.Sprintf("failed to load product ID%d: database query returned error", i)
		cur[p] = patternStat{n: 1}
		base[p] = patternStat{n: 6}
	}
	cur["failed to load product ID0: database query returned error"] = patternStat{n: 3}
	st := steadyFromPatterns("product-catalog", cur, base, 6, 3)
	if st == nil {
		t.Fatal("ten small patterns at their usual rate should add up to a steady entry")
	}
	if st.ErrorsNow != 12 || st.ErrorsUsual != 10 || !strings.Contains(st.Example, "ID0") {
		t.Errorf("steady = %+v", st)
	}
}

func TestSteadyFromPatternsRejectsRising(t *testing.T) {
	base := map[string]patternStat{"x": {n: 60}} // 10 per window over 6 windows
	if st := steadyFromPatterns("svc", map[string]patternStat{"x": {n: 60}}, base, 6, 3); st != nil {
		t.Errorf("six times the usual rate is not steady: %+v", st)
	}
	if st := steadyFromPatterns("svc", map[string]patternStat{"x": {n: 2}}, base, 6, 3); st != nil {
		t.Error("below the minimum count nothing is reported")
	}
	if st := steadyFromPatterns("svc", map[string]patternStat{"x": {n: 5}}, map[string]patternStat{}, 6, 3); st != nil {
		t.Error("with no baseline errors nothing can be called steady")
	}
}

func TestFitDropsSteadyBeforeFindings(t *testing.T) {
	env := randomEnvelope(rand.New(rand.NewSource(3)), 3)
	for i := 0; i < 3; i++ {
		env.SteadyErrors = append(env.SteadyErrors, SteadyError{Service: fmt.Sprintf("s%d", i), ErrorsNow: 9, ErrorsUsual: 9, Example: strings.Repeat("e", 120)})
	}
	withSteady, _ := MarshalCompact(env)
	noSteady := env
	noSteady.SteadyErrors = nil
	for i := range noSteady.Findings {
		noSteady.Findings[i].EvidenceLink, noSteady.Findings[i].Example = "", ""
	}
	bare, _ := MarshalCompact(noSteady)
	budget := EstimateTokens(bare) + 5
	if EstimateTokens(withSteady) <= budget {
		t.Skip("envelope too small to exercise trimming")
	}
	out := Fit(env, budget)
	if len(out.Findings) != 3 || out.SteadyErrors != nil {
		t.Errorf("want all findings kept and steady errors dropped, got %d findings, %d steady", len(out.Findings), len(out.SteadyErrors))
	}
}

func TestSteadySentence(t *testing.T) {
	st := []SteadyError{{Service: "product-catalog", ErrorsNow: 8, ErrorsUsual: 10.2, Change: "-22%", LinesChange: "+3%", Example: "db error"}}
	if s := steadySentence(st, true); !strings.Contains(s, "not a new problem") || !strings.Contains(s, "10.2 usual, -22%, all log lines +3%") {
		t.Errorf("no-findings sentence = %q", s)
	}
	if s := steadySentence(st, false); s != " Errors not significantly up (not new) in: product-catalog." {
		t.Errorf("with-findings sentence = %q", s)
	}
	if steadySentence(nil, true) != "" {
		t.Error("no steady errors, no sentence")
	}
}
