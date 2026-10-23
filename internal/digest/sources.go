package digest

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/asinha07/spoor/internal/backend"
	"github.com/asinha07/spoor/internal/window"
)

// LogStore is what the log scorer needs from a logs backend. Each backend
// (VictoriaLogs, Loki, New Relic) answers these in its own query language;
// scoring, grouping into error families and attribution are shared.
type LogStore interface {
	// Services lists the values of the service field seen in r.
	Services(ctx context.Context, r window.Range) ([]string, error)
	// Patterns groups the error lines of services in r into normalized
	// patterns (top patternLimit per service), with each service's exact
	// error-line count over r and per bucket.
	Patterns(ctx context.Context, services []string, r window.Range, buckets []window.Range) (patternResult, error)
	// Totals counts all lines (and lines with no severity field) per service
	// in n buckets of step from r.Start, or over all of r when step is 0.
	// With all set, lines without a service field are counted under "".
	Totals(ctx context.Context, services []string, all bool, r window.Range, step time.Duration, n int) (map[string][]volume, error)
	// CountFamilies counts, for each check, the error lines of a service in
	// r that contain all of its words (case-insensitive).
	CountFamilies(ctx context.Context, services []string, r window.Range, checks []familyCheck) ([]float64, error)
	// CountLines counts every line in r, service field or not.
	CountLines(ctx context.Context, r window.Range) (float64, error)
	// Evidence is the query that selects one service's error lines, for links.
	Evidence(svc string) string
}

// patternResult is one Patterns answer.
type patternResult struct {
	patterns servicePatterns
	counts   map[string]errorCounts
	// note, when set, says the patterns are incomplete (sampled, or limited
	// by the backend); it is reported in coverage.
	note string
}

// familyCheck asks whether an error family occurs in a service.
type familyCheck struct {
	svc   string
	words []string
}

// TraceStore is what the trace scorer and root-cause attribution need from a
// traces backend.
type TraceStore interface {
	Services(ctx context.Context, r window.Range) ([]string, error)
	// SpanStats returns per-span-name counts, errors, p95 and total duration
	// (nanoseconds) for one service (and one operation, if set), top spanLimit.
	SpanStats(ctx context.Context, svc, op string, r window.Range) (map[string]spanStat, error)
	// Example returns one trace ID for a span name: the earliest failed one
	// (with its time) when failed is set, else the slowest.
	Example(ctx context.Context, svc, op, name string, failed bool, r window.Range) (string, time.Time)
	// Spans returns span rows for attribution: failed spans only, or all.
	Spans(ctx context.Context, failedOnly bool, r window.Range) ([]spanRow, error)
	// Evidence is the query that selects one service's spans, for links.
	Evidence(svc, op string) string
}

// victoriaLogs implements LogStore with LogsQL against VictoriaLogs. Its
// queries are built by the Engine's LogsQL helpers.
type victoriaLogs struct {
	e   *Engine
	src LogsSource
}

func (v *victoriaLogs) Services(ctx context.Context, r window.Range) ([]string, error) {
	return fieldValues(ctx, v.src, v.e.cfg.Logs.ServiceField, r)
}

func (v *victoriaLogs) Patterns(ctx context.Context, services []string, r window.Range, buckets []window.Range) (patternResult, error) {
	pats, counts, err := v.e.patternsIn(ctx, v.src, v.e.logScope(services, false), r, buckets)
	return patternResult{patterns: pats, counts: counts}, err
}

func (v *victoriaLogs) Totals(ctx context.Context, services []string, all bool, r window.Range, step time.Duration, n int) (map[string][]volume, error) {
	return v.e.totals(ctx, v.src, v.e.logScope(services, all), r, step, n)
}

func (v *victoriaLogs) CountFamilies(ctx context.Context, services []string, r window.Range, checks []familyCheck) ([]float64, error) {
	conds := make([]string, 0, len(checks))
	for i, c := range checks {
		cond := backend.Eq("spoor_svc", c.svc)
		for _, w := range c.words {
			cond += " i(" + strconv.Quote(w) + ")"
		}
		conds = append(conds, fmt.Sprintf("count() if (%s) c%d", cond, i))
	}
	l := v.e.cfg.Logs
	q := v.e.logScope(services, false) + " " + l.EffectiveErrorFilter() + " | copy " + backend.Quote(l.ServiceField) +
		" as spoor_svc | stats " + strings.Join(conds, ", ")
	rows, err := v.src.Query(ctx, q, r.Start, r.End, 0)
	if err != nil {
		return nil, err
	}
	out := make([]float64, len(checks))
	if len(rows) > 0 {
		for i := range checks {
			out[i] = rows[0].Float(fmt.Sprintf("c%d", i))
		}
	}
	return out, nil
}

func (v *victoriaLogs) CountLines(ctx context.Context, r window.Range) (float64, error) {
	rows, err := v.src.Query(ctx, "* | stats count() n", r.Start, r.End, 0)
	if err != nil || len(rows) == 0 {
		return 0, err
	}
	return rows[0].Float("n"), nil
}

func (v *victoriaLogs) Evidence(svc string) string { return v.e.logFilter(svc) }

// victoriaTraces implements TraceStore with LogsQL against VictoriaTraces.
type victoriaTraces struct {
	e   *Engine
	src LogsSource
}

func (v *victoriaTraces) Services(ctx context.Context, r window.Range) ([]string, error) {
	return fieldValues(ctx, v.src, v.e.cfg.Traces.ServiceField, r)
}

func (v *victoriaTraces) SpanStats(ctx context.Context, svc, op string, r window.Range) (map[string]spanStat, error) {
	return v.e.spanStats(ctx, v.src, v.e.spanStatsQuery(v.e.spanFilter(svc, op)), r)
}

func (v *victoriaTraces) Example(ctx context.Context, svc, op, name string, failed bool, r window.Range) (string, time.Time) {
	return v.e.exampleSpan(ctx, v.src, v.e.spanFilter(svc, op), name, failed, r)
}

func (v *victoriaTraces) Spans(ctx context.Context, failedOnly bool, r window.Range) ([]spanRow, error) {
	filter := ""
	if failedOnly {
		filter = v.e.cfg.Traces.ErrorFilter
	}
	return v.e.spanRows(ctx, v.src, filter, r)
}

func (v *victoriaTraces) Evidence(svc, op string) string { return v.e.spanFilter(svc, op) }

func fieldValues(ctx context.Context, src LogsSource, field string, r window.Range) ([]string, error) {
	rows, err := src.Query(ctx, "* | field_values "+backend.Quote(field), r.Start, r.End, 0)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, row[field])
	}
	return out, nil
}
