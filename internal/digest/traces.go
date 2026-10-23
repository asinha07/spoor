package digest

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/asinha07/spoor/internal/backend"
	"github.com/asinha07/spoor/internal/window"
)

const (
	spanLimit        = 200
	slowerMinRatio   = 1.5
	slowerMinDeltaNs = 20e6 // 20ms
	spanErrMinDelta  = 0.05
	spanErrMinCount  = 3
)

type spanStat struct {
	n, errs, p95, total float64
}

func (s spanStat) mean() float64 {
	if s.n == 0 {
		return 0
	}
	return s.total / s.n
}

func (s spanStat) errRate() float64 {
	if s.n == 0 {
		return 0
	}
	return s.errs / s.n
}

// realSpans excludes VictoriaTraces' internal trace-index rows, which carry a
// duration field but are not spans.
const realSpans = "span_id:*"

func (e *Engine) spanFilter(svc, op string) string {
	f := realSpans + " " + backend.Eq(e.cfg.Traces.ServiceField, svc)
	if op != "" {
		f += " " + backend.Eq(e.cfg.Traces.NameField, op)
	}
	return f
}

func (e *Engine) spanStatsQuery(filter string) string {
	t := e.cfg.Traces
	name, dur := backend.Quote(t.NameField), backend.Quote(t.DurationField)
	// The name breaks ties, so the cut at the limit is the same on every run.
	return fmt.Sprintf("%s | stats by (%s) count() n, count() if (%s) errs, quantile(0.95, %s) p95, sum(%s) total | sort by (n desc, %s) | limit %d",
		filter, name, t.ErrorFilter, dur, dur, name, spanLimit)
}

func (e *Engine) spanStats(ctx context.Context, src LogsSource, q string, r window.Range) (map[string]spanStat, error) {
	rows, err := src.Query(ctx, q, r.Start, r.End, 0)
	if err != nil {
		return nil, err
	}
	out := make(map[string]spanStat, len(rows))
	for _, row := range rows {
		out[row[e.cfg.Traces.NameField]] = spanStat{n: row.Float("n"), errs: row.Float("errs"), p95: row.Float("p95"), total: row.Float("total")}
	}
	return out, nil
}

// scoreTraces compares span error rate and p95 per span name, and attributes
// each slower span's share of the total added latency.
func (e *Engine) scoreTraces(ctx context.Context, svc, op string, spec window.Spec) ([]Finding, error) {
	cur, err := e.traces.SpanStats(ctx, svc, op, spec.Window)
	if err != nil {
		return nil, err
	}
	if len(cur) == 0 {
		return nil, nil
	}
	hist := window.Range{Start: spec.SubWindows[0].Start, End: spec.Baseline.End}
	base, err := e.traces.SpanStats(ctx, svc, op, hist)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(cur))
	for n := range cur {
		names = append(names, n)
	}
	sort.Strings(names)

	// Total latency added across all span names, for share attribution.
	added := map[string]float64{}
	var addedSum float64
	for _, n := range names {
		c, b := cur[n], base[n]
		if b.n == 0 {
			continue
		}
		if d := (c.mean() - b.mean()) * c.n; d > 0 {
			added[n] = d
			addedSum += d
		}
	}

	link := e.tracesLink(e.traces.Evidence(svc, op), hist.Start, spec.Window.End)
	minSpans := float64(e.cfg.Scoring.MinSpans)
	var out []Finding
	for _, n := range names {
		c, b := cur[n], base[n]
		if c.n < minSpans || b.n < minSpans {
			continue
		}
		// Errors: a binomial z-score against the baseline error rate.
		er, br := c.errRate(), b.errRate()
		if er-br >= spanErrMinDelta && c.errs >= spanErrMinCount {
			p := math.Max(br, 0.005)
			z := (c.errs - c.n*p) / math.Sqrt(c.n*p*(1-p))
			if s := zScore(z); s >= e.cfg.Scoring.ReportThreshold {
				f := Finding{
					Service: svc, Signal: SignalTraces, Kind: KindSpanErrorsUp, Score: round2(s),
					Before: humanValue(br, "ratio"), After: humanValue(er, "ratio"),
					Title:        fmt.Sprintf("Span %q errors %s → %s (%d spans)", n, humanValue(br, "ratio"), humanValue(er, "ratio"), int(c.n)),
					EvidenceLink: link,
				}
				f.TraceID, f.onset = e.traces.Example(ctx, svc, op, n, true, spec.Window)
				f.Onset = fmtTime(f.onset)
				out = append(out, f)
			}
		}
		// Latency: p95 ratio with an absolute floor.
		if b.p95 > 0 && c.p95/b.p95 >= slowerMinRatio && c.p95-b.p95 >= slowerMinDeltaNs {
			ratio := c.p95 / b.p95
			s := 1 - 0.75/ratio
			if s >= e.cfg.Scoring.ReportThreshold {
				title := fmt.Sprintf("Span %q p95 %s → %s", n, humanNanos(b.p95), humanNanos(c.p95))
				if addedSum > 0 && added[n] > 0 {
					title += fmt.Sprintf(" (%.0f%% of added latency)", 100*added[n]/addedSum)
				}
				f := Finding{
					Service: svc, Signal: SignalTraces, Kind: KindSpanSlower, Score: round2(s),
					Before: humanNanos(b.p95), After: humanNanos(c.p95), Title: title, EvidenceLink: link,
				}
				f.TraceID, _ = e.traces.Example(ctx, svc, op, n, false, spec.Window)
				out = append(out, f)
			}
		}
	}
	return out, nil
}

// exampleSpan returns one trace ID for a span name (the slowest, or the
// earliest failed one) and, for failures, when they started.
func (e *Engine) exampleSpan(ctx context.Context, src LogsSource, filter, name string, failed bool, r window.Range) (string, time.Time) {
	t := e.cfg.Traces
	q := filter + " " + backend.Eq(t.NameField, name)
	if failed {
		q += " " + t.ErrorFilter + " | sort by (_time) | limit 1 | fields trace_id, _time"
	} else {
		q += fmt.Sprintf(" | sort by (%s desc) | limit 1 | fields trace_id, _time", backend.Quote(t.DurationField))
	}
	rows, err := src.Query(ctx, q, r.Start, r.End, 1)
	if err != nil || len(rows) == 0 {
		return "", time.Time{}
	}
	var onset time.Time
	if failed {
		onset = rows[0].Time("_time")
	}
	return rows[0]["trace_id"], onset
}
