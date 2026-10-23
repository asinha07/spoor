package digest

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/asinha07/spoor/internal/backend"
	"github.com/asinha07/spoor/internal/window"
)

// TempoAPI is the subset of the Tempo client the Tempo store uses.
type TempoAPI interface {
	Search(ctx context.Context, q string, start, end time.Time, limit, spss int) ([]backend.TempoTrace, error)
	Trace(ctx context.Context, traceID string, start, end time.Time) ([]backend.TempoSpan, error)
	TagValues(ctx context.Context, tag string, start, end time.Time) ([]string, error)
}

const (
	tempoStatTraces   = 500 // traces sampled per service and range for span statistics
	tempoStatSpans    = 50  // matched spans kept per sampled trace
	tempoAttrFailed   = 40  // failed traces fetched whole for error attribution
	tempoAttrSample   = 60  // other traces fetched whole for latency attribution
	tempoFetchWorkers = 4
	tempoCacheTraces  = 2000
)

// tempoTraces implements TraceStore with Tempo's search API. Tempo has no
// aggregation that every deployment enables, so span statistics come from a
// sample of traces per service and range, and attribution from sampled
// traces fetched whole. The engine reports this in coverage.
type tempoTraces struct {
	e   *Engine
	api TempoAPI

	mu    sync.Mutex
	cache map[string][]backend.TempoSpan
}

// NewTempoStore returns a TraceStore backed by Tempo.
func NewTempoStore(e *Engine, api TempoAPI) TraceStore {
	return &tempoTraces{e: e, api: api, cache: map[string][]backend.TempoSpan{}}
}

// SampleNote is reported in coverage when traces are scored.
func (t *tempoTraces) SampleNote() string {
	return fmt.Sprintf("span statistics from a sample of up to %d traces per service and range; attribution from up to %d failed and %d other traces", tempoStatTraces, tempoAttrFailed, tempoAttrSample)
}

func (t *tempoTraces) svcCond(svc string) string {
	return t.e.cfg.Traces.ServiceField + " = " + strconv.Quote(svc)
}

func (t *tempoTraces) nameField() string {
	if f := t.e.cfg.Traces.NameField; f != "" {
		return f
	}
	return "name"
}

func (t *tempoTraces) filter(svc, op, name string, failed bool) string {
	q := "{ " + t.svcCond(svc)
	if op != "" {
		q += " && " + t.nameField() + " = " + strconv.Quote(op)
	}
	if name != "" && name != op {
		q += " && " + t.nameField() + " = " + strconv.Quote(name)
	}
	if failed {
		q += " && " + t.e.cfg.Traces.ErrorFilter
	}
	return q + " }"
}

func (t *tempoTraces) Services(ctx context.Context, r window.Range) ([]string, error) {
	return t.api.TagValues(ctx, t.e.cfg.Traces.ServiceField, r.Start, r.End)
}

func (t *tempoTraces) Evidence(svc, op string) string { return t.filter(svc, op, "", false) }

func (t *tempoTraces) SpanStats(ctx context.Context, svc, op string, r window.Range) (map[string]spanStat, error) {
	traces, err := t.api.Search(ctx, t.filter(svc, op, "", false)+" | select(status, name)", r.Start, r.End, tempoStatTraces, tempoStatSpans)
	if err != nil {
		return nil, err
	}
	durs := map[string][]float64{}
	out := map[string]spanStat{}
	for _, tr := range traces {
		for _, s := range tr.Spans {
			st := out[s.Name]
			st.n++
			if s.Error {
				st.errs++
			}
			st.total += float64(s.Duration)
			out[s.Name] = st
			durs[s.Name] = append(durs[s.Name], float64(s.Duration))
		}
	}
	for name, d := range durs {
		st := out[name]
		st.p95 = quantile(d, 0.95)
		out[name] = st
	}
	return topSpanNames(out, spanLimit), nil
}

func (t *tempoTraces) Example(ctx context.Context, svc, op, name string, failed bool, r window.Range) (string, time.Time) {
	traces, err := t.api.Search(ctx, t.filter(svc, op, name, failed), r.Start, r.End, 50, 5)
	if err != nil || len(traces) == 0 {
		return "", time.Time{}
	}
	var best backend.TempoSpan
	for _, tr := range traces {
		for _, s := range tr.Spans {
			switch {
			case best.TraceID == "":
				best = s
			case failed && s.Start < best.Start:
				best = s
			case !failed && s.Duration > best.Duration:
				best = s
			}
		}
	}
	if !failed {
		return best.TraceID, time.Time{}
	}
	return best.TraceID, time.Unix(0, best.Start).UTC()
}

func (t *tempoTraces) Spans(ctx context.Context, failedOnly bool, r window.Range) ([]spanRow, error) {
	failed, err := t.api.Search(ctx, "{ "+t.e.cfg.Traces.ErrorFilter+" }", r.Start, r.End, tempoAttrFailed, 1)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(failed))
	for _, tr := range failed {
		ids = append(ids, tr.TraceID)
	}
	if !failedOnly {
		// Failed traces are included so failed spans link through callers
		// that did not mark their own span as failed.
		other, err := t.api.Search(ctx, "{ }", r.Start, r.End, tempoAttrSample, 1)
		if err != nil {
			return nil, err
		}
		for _, tr := range other {
			ids = append(ids, tr.TraceID)
		}
	}
	spans, err := t.fetch(ctx, dedupe(ids), r)
	if err != nil {
		return nil, err
	}
	out := make([]spanRow, 0, len(spans))
	for _, s := range spans {
		if failedOnly && !s.Error {
			continue
		}
		out = append(out, spanRow{traceID: s.TraceID, spanID: s.SpanID, parentID: s.ParentID, svc: s.Service, name: s.Name, dur: float64(s.Duration), start: s.Start})
	}
	return out, nil
}

// fetch loads whole traces, a few at a time, with a small cache: the same
// failed traces are read for the failed and the full span sets.
func (t *tempoTraces) fetch(ctx context.Context, ids []string, r window.Range) ([]backend.TempoSpan, error) {
	results := make([][]backend.TempoSpan, len(ids))
	errs := make([]error, len(ids))
	var wg sync.WaitGroup
	sem := make(chan struct{}, tempoFetchWorkers)
	for i, id := range ids {
		t.mu.Lock()
		cached, ok := t.cache[id]
		t.mu.Unlock()
		if ok {
			results[i] = cached
			continue
		}
		i, id := i, id
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			spans, err := t.api.Trace(ctx, id, r.Start, r.End)
			results[i], errs[i] = spans, err
			if err == nil {
				t.mu.Lock()
				if len(t.cache) >= tempoCacheTraces {
					t.cache = map[string][]backend.TempoSpan{}
				}
				t.cache[id] = spans
				t.mu.Unlock()
			}
		}()
	}
	wg.Wait()
	var out []backend.TempoSpan
	failures := 0
	var lastErr error
	for i := range ids {
		if errs[i] != nil {
			failures++
			lastErr = errs[i]
			continue
		}
		out = append(out, results[i]...)
	}
	if failures > 0 && failures == len(ids) {
		return nil, lastErr
	}
	return out, nil
}

func dedupe(ids []string) []string {
	seen := map[string]bool{}
	out := ids[:0:0]
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// quantile returns the q-quantile of vs (nearest rank).
func quantile(vs []float64, q float64) float64 {
	if len(vs) == 0 {
		return 0
	}
	s := append([]float64(nil), vs...)
	sort.Float64s(s)
	i := int(math.Ceil(q*float64(len(s)))) - 1
	if i < 0 {
		i = 0
	}
	return s[i]
}

// topSpanNames keeps the n busiest span names, ties broken by name.
func topSpanNames(m map[string]spanStat, n int) map[string]spanStat {
	if len(m) <= n {
		return m
	}
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Slice(names, func(i, j int) bool {
		if m[names[i]].n != m[names[j]].n {
			return m[names[i]].n > m[names[j]].n
		}
		return names[i] < names[j]
	})
	out := make(map[string]spanStat, n)
	for _, k := range names[:n] {
		out[k] = m[k]
	}
	return out
}
