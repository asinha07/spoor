package digest

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/asinha07/spoor/internal/backend"
	"github.com/asinha07/spoor/internal/config"
	"github.com/asinha07/spoor/internal/window"
)

// fakeNRQL answers NRQL queries by the first rule whose substring matches.
type fakeNRQL struct {
	mu      sync.Mutex
	rules   []nrRule
	queries []string
}

type nrRule struct {
	contains string
	rows     []map[string]any
}

func (f *fakeNRQL) NRQL(_ context.Context, q string) ([]map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queries = append(f.queries, q)
	for _, r := range f.rules {
		if strings.Contains(q, r.contains) {
			return r.rows, nil
		}
	}
	return nil, fmt.Errorf("unexpected NRQL: %s", q)
}

var t0 = time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)

func TestNormalizerMatchesPipelineSteps(t *testing.T) {
	n := newNormalizer(config.Default().Logs)
	for raw, want := range map[string]string{
		"charge failed: card processor returned 502 for order ord_7f3k2m9q":                         "charge failed: card processor returned <N> for order <ID>",
		"2026-10-01T10:00:00.123Z  timeout after 3000ms\n\tat com.x.Y.z(Y.java:42)":                 "timeout after <N>ms",
		`{"message":"No matching template found","code":"TEMPLATE_404","requestId":"ses_ab12cd34"}`: "TEMPLATE_404 No matching template found",
		"user 3f2504e0-4f89-11d3-9a0c-0305e82c3301 from 10.1.2.3 denied":                            "user <UUID> from <IP> denied",
	} {
		if got := n.pattern(raw); got != want {
			t.Errorf("pattern(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestNewRelicLogsGroupsMessagesIntoPatterns(t *testing.T) {
	cfg := config.DefaultFor(config.Types{Logs: config.TypeNewRelic})
	cfg.NewRelic.AccountID = 1
	fake := &fakeNRQL{rules: []nrRule{
		{"FACET `entity.name`, message", nil}, // not used: entity.name needs no backticks
		{"FACET entity.name, message", []map[string]any{
			{"facet": []any{"payment", "charge failed for order ord_1a2b3c"}, "n": 40.0, "first": float64(t0.Add(5 * time.Minute).UnixMilli()), "example": "charge failed for order ord_1a2b3c"},
			{"facet": []any{"payment", "charge failed for order ord_9z8y7x"}, "n": 30.0, "first": float64(t0.Add(2 * time.Minute).UnixMilli()), "example": "charge failed for order ord_9z8y7x"},
			{"facet": []any{"cart", "cart 77 not found"}, "n": 5.0, "first": float64(t0.UnixMilli()), "example": "cart 77 not found"},
		}},
		{"TIMESERIES", []map[string]any{
			{"facet": "payment", "beginTimeSeconds": float64(t0.Unix()), "n": 3.0},
			{"facet": "payment", "beginTimeSeconds": float64(t0.Add(30 * time.Minute).Unix()), "n": 4.0},
		}},
		{"FACET entity.name LIMIT MAX", []map[string]any{{"facet": "payment", "n": 70.0}, {"facet": "cart", "n": 5.0}}},
	}}
	e := NewWithStores(cfg, nil, nil, nil)
	store := NewNewRelicLogs(e, fake)
	r := window.Range{Start: t0, End: t0.Add(time.Hour)}
	buckets := []window.Range{{Start: t0, End: t0.Add(30 * time.Minute)}, {Start: t0.Add(30 * time.Minute), End: t0.Add(time.Hour)}}
	res, err := store.Patterns(context.Background(), []string{"payment", "cart"}, r, buckets)
	if err != nil {
		t.Fatal(err)
	}
	p := res.patterns["payment"]["charge failed for order <ID>"]
	if p.n != 70 || !p.first.Equal(t0.Add(2*time.Minute)) {
		t.Errorf("payment pattern = %+v, want 70 lines first seen at +2m", p)
	}
	if res.counts["payment"].total != 70 || res.counts["payment"].buckets[0] != 3 || res.counts["payment"].buckets[1] != 4 {
		t.Errorf("payment counts = %+v", res.counts["payment"])
	}
	for _, q := range fake.queries {
		if !strings.Contains(q, "SINCE ") || !strings.Contains(q, " UNTIL ") {
			t.Errorf("query without an explicit time range: %s", q)
		}
		if !strings.Contains(q, "lower(level) IN (") {
			t.Errorf("query without the error filter: %s", q)
		}
	}
}

func TestNewRelicMetricsWindows(t *testing.T) {
	end := t0.Add(3 * time.Hour)
	fake := &fakeNRQL{rules: []nrRule{
		{"SLIDE BY", []map[string]any{{"endTimeSeconds": float64(end.Unix()), "rate.count": 2.5}}},
		{"TIMESERIES 30 minutes", []map[string]any{
			{"beginTimeSeconds": float64(t0.Unix()), "endTimeSeconds": float64(t0.Add(30 * time.Minute).Unix()), "rate.count": 1.0},
			{"beginTimeSeconds": float64(t0.Add(30 * time.Minute).Unix()), "endTimeSeconds": float64(t0.Add(time.Hour).Unix()), "percentile.duration": map[string]any{"95": 0.25}},
		}},
		{"SELECT", []map[string]any{{"percentile.duration": map[string]any{"95": 0.4}}}},
	}}
	m := newNewRelicMetrics(fake, "SELECT rate(count(*), 1 second) FROM Transaction WHERE appName = 'x'", "appName")
	s, err := m.InstantOver(context.Background(), "SELECT percentile(duration, 95) FROM Transaction", end, 30*time.Minute)
	if err != nil || len(s) != 1 || s[0].Points[0].V != 0.4 {
		t.Fatalf("instant = %+v, %v", s, err)
	}
	if q := fake.queries[0]; !strings.HasSuffix(q, fmt.Sprintf("SINCE %d UNTIL %d", end.Add(-30*time.Minute).UnixMilli(), end.UnixMilli())) {
		t.Errorf("instant must cover [at-lookback, at]: %s", q)
	}
	s, err = m.RangeOver(context.Background(), "SELECT x FROM Transaction", t0.Add(30*time.Minute), t0.Add(time.Hour), 30*time.Minute, 30*time.Minute)
	if err != nil || len(s[0].Points) != 2 || s[0].Points[1].V != 0.25 || !s[0].Points[0].T.Equal(t0.Add(30*time.Minute)) {
		t.Fatalf("range = %+v, %v", s, err)
	}
	if _, err := m.RangeOver(context.Background(), "SELECT x FROM Transaction", end.Add(-time.Hour), end, 3*time.Minute, 6*time.Minute); err != nil {
		t.Fatal(err)
	}
	if q := fake.queries[len(fake.queries)-1]; !strings.Contains(q, "TIMESERIES 6 minutes SLIDE BY 3 minutes") {
		t.Errorf("finer step than lookback needs a sliding window: %s", q)
	}
}

func TestCorroborateSpans(t *testing.T) {
	fs := []Finding{
		{Service: "a", Signal: SignalTraces, Kind: KindSpanSlower},
		{Service: "a", Signal: SignalMetrics, Kind: KindLatencyUp},
		{Service: "b", Signal: SignalTraces, Kind: KindSpanSlower},
		{Service: "b", Signal: SignalTraces, Kind: KindSpanErrorsUp},
		{Service: "b", Signal: SignalTraces, Kind: KindErrorOrigin},
	}
	out, held := corroborateSpans(fs)
	if held != 2 || len(out) != 3 {
		t.Fatalf("held %d, kept %+v", held, out)
	}
	for _, f := range out {
		if f.Service == "b" && f.Kind != KindErrorOrigin {
			t.Errorf("uncorroborated span finding kept: %+v", f)
		}
	}
}

func TestFoldNestedSlower(t *testing.T) {
	fs := []Finding{
		{Service: "a", Kind: KindSpanSlower, Before: "100ms", After: "900ms", Score: 0.8, Title: "Span \"b\""},
		{Service: "a", Kind: KindSpanSlower, Before: "100ms", After: "900ms", Score: 0.9, Title: "Span \"a\""},
		{Service: "a", Kind: KindSpanSlower, Before: "5ms", After: "50ms", Score: 0.7, Title: "Span \"c\""},
	}
	out := foldNestedSlower(fs)
	if len(out) != 2 || out[0].Title != `Span "a" (and 1 nested spans with the same timing)` {
		t.Fatalf("fold = %+v", out)
	}
}

// fakeLoki records LogQL and answers from a function.
type fakeLoki struct {
	mu      sync.Mutex
	queries []string
	instant func(q string) ([]backend.Series, error)
}

func (f *fakeLoki) record(q string) {
	f.mu.Lock()
	f.queries = append(f.queries, q)
	f.mu.Unlock()
}
func (f *fakeLoki) Instant(_ context.Context, q string, _ time.Time) ([]backend.Series, error) {
	f.record(q)
	return f.instant(q)
}
func (f *fakeLoki) Range(_ context.Context, q string, _, _ time.Time, _ time.Duration) ([]backend.Series, error) {
	f.record(q)
	return f.instant(q)
}
func (f *fakeLoki) Lines(_ context.Context, q string, _, _ time.Time, _ int, _ bool) ([]backend.LokiEntry, error) {
	f.record(q)
	return []backend.LokiEntry{{Labels: map[string]string{"service_name": "pay", "spoor_msg": "boom"}, Time: t0, Line: "boom 1"}}, nil
}
func (f *fakeLoki) LabelValues(context.Context, string, time.Time, time.Time) ([]string, error) {
	return []string{"pay"}, nil
}

func TestLokiFallsBackOnSeriesLimit(t *testing.T) {
	cfg := config.DefaultFor(config.Types{Logs: config.TypeLoki})
	fake := &fakeLoki{instant: func(q string) ([]backend.Series, error) {
		if strings.Contains(q, "spoor_msg)") {
			return nil, fmt.Errorf("query returned 400: maximum of series (500) reached for a single query")
		}
		return []backend.Series{{Labels: map[string]string{"service_name": "pay"}, Points: []backend.Point{{T: t0, V: 9}}}}, nil
	}}
	e := NewWithStores(cfg, nil, nil, nil)
	store := NewLokiStore(e, fake)
	res, err := store.Patterns(context.Background(), []string{"pay"}, window.Range{Start: t0, End: t0.Add(30 * time.Minute)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.patterns["pay"]["boom"].n != 1 || res.counts["pay"].total != 9 {
		t.Errorf("sampled fallback = %+v counts %+v", res.patterns, res.counts)
	}
	if !strings.Contains(res.note, "pay") || !strings.Contains(res.note, "counts exact") {
		t.Errorf("fallback must be reported: %q", res.note)
	}
	joined := strings.Join(fake.queries, "\n")
	for _, want := range []string{`{service_name="pay"} | detected_level=~"(?i)(err|`, `| json spoor_m0="message"`, `| drop __error__, __error_details__`, `label_format spoor_msg=`, `regexReplaceAll`} {
		if !strings.Contains(joined, want) {
			t.Errorf("LogQL lacks %s:\n%s", want, joined)
		}
	}
}

// fakeTempo returns fixed search hits and traces.
type fakeTempo struct{ queries []string }

func (f *fakeTempo) Search(_ context.Context, q string, _, _ time.Time, _, _ int) ([]backend.TempoTrace, error) {
	f.queries = append(f.queries, q)
	var out []backend.TempoTrace
	for i := 0; i < 20; i++ {
		out = append(out, backend.TempoTrace{TraceID: fmt.Sprint("t", i), Spans: []backend.TempoSpan{{TraceID: fmt.Sprint("t", i), SpanID: "s", Name: "GET /x", Duration: int64(i+1) * 1e6, Error: i%4 == 0}}})
	}
	return out, nil
}
func (f *fakeTempo) Trace(_ context.Context, id string, _, _ time.Time) ([]backend.TempoSpan, error) {
	return []backend.TempoSpan{{TraceID: id, SpanID: "root", Service: "front", Name: "GET /", Error: true}, {TraceID: id, SpanID: "c", ParentID: "root", Service: "pay", Name: "charge", Error: true}}, nil
}
func (f *fakeTempo) TagValues(context.Context, string, time.Time, time.Time) ([]string, error) {
	return []string{"pay"}, nil
}

func TestTempoStatsAndSpans(t *testing.T) {
	cfg := config.DefaultFor(config.Types{Traces: config.TypeTempo})
	e := NewWithStores(cfg, nil, nil, nil)
	fake := &fakeTempo{}
	store := NewTempoStore(e, fake)
	r := window.Range{Start: t0, End: t0.Add(time.Hour)}
	st, err := store.SpanStats(context.Background(), "pay", "", r)
	if err != nil {
		t.Fatal(err)
	}
	s := st["GET /x"]
	if s.n != 20 || s.errs != 5 || s.p95 != 19e6 {
		t.Errorf("stats = %+v", s)
	}
	if q := fake.queries[0]; q != `{ resource.service.name = "pay" } | select(status, name)` {
		t.Errorf("TraceQL = %s", q)
	}
	rows, err := store.Spans(context.Background(), true, r)
	if err != nil || len(rows) != 40 {
		t.Fatalf("failed spans = %d, %v", len(rows), err)
	}
	if _, ok := store.(sampler); !ok {
		t.Error("Tempo store must report that it samples")
	}
}

func TestPctChangeHasOneSign(t *testing.T) {
	if got := pctChange(99.8, 100); got != "+0%" {
		t.Errorf("pctChange(99.8, 100) = %q, want +0%%", got)
	}
}
