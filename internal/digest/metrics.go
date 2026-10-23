package digest

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/asinha07/spoor/internal/backend"
	"github.com/asinha07/spoor/internal/window"
)

// metricSpec is one metric the scorer evaluates for a service.
type metricSpec struct {
	name      string // human name used in titles
	tmpl      string
	unit      string // ratio, seconds, reqps, bytes or a free label
	relFloor  float64
	absFloor  float64
	direction int // +1: only increases matter, -1: only decreases, 0: both
	kindUp    string
	kindDown  string
	needsLoad bool // skipped for services below MinRequestRate
}

func (e *Engine) metricSpecs() []metricSpec {
	m := e.cfg.Metrics
	specs := []metricSpec{
		{name: "request rate", tmpl: m.RequestRate, unit: "reqps", relFloor: 0.3, absFloor: 0.05, direction: 0, kindUp: KindThroughputUp, kindDown: KindThroughputDown},
		{name: "error rate", tmpl: m.ErrorRate, unit: "ratio", relFloor: 0.5, absFloor: 0.02, direction: +1, kindUp: KindErrorRateUp, needsLoad: true},
		{name: "p95 latency", tmpl: m.LatencyP95, unit: "seconds", relFloor: 0.25, absFloor: 0.02, direction: +1, kindUp: KindLatencyUp, needsLoad: true},
	}
	for _, r := range m.Resources {
		abs := 1e-9
		switch r.Unit {
		case "cores":
			abs = 0.05
		case "bytes":
			abs = 16 << 20
		case "messages":
			abs = 50
		case "ratio":
			abs = 0.05 // five percentage points: 0.1% -> 0.6% CPU is not news
		}
		specs = append(specs, metricSpec{name: r.Name, tmpl: r.Query, unit: r.Unit, relFloor: 0.25, absFloor: abs, direction: +1, kindUp: KindResourceUp})
	}
	return specs
}

// scoreMetrics compares each metric's value over the window with its values
// over the baseline sub-windows.
func (e *Engine) scoreMetrics(ctx context.Context, svc string, spec window.Spec) ([]Finding, error) {
	w := spec.Window.Duration()
	var out []Finding
	var lastErr error
	ok := 0
	loadOK := true
	for i, ms := range e.metricSpecs() {
		if ms.needsLoad && !loadOK {
			continue
		}
		q, err := e.cfg.Metrics.Render(ms.tmpl, svc, w)
		if err != nil {
			return nil, err
		}
		cur, has, err := e.instant(ctx, q, spec.Window.End, w)
		if err != nil {
			lastErr = err
			continue
		}
		ok++
		if i == 0 && !has {
			// No request series in the window means the service went silent:
			// its request rate is zero, which is exactly what must be flagged.
			cur, has = 0, true
		}
		hist, err := e.history(ctx, q, spec)
		if err != nil {
			lastErr = err
			continue
		}
		if i == 0 { // request rate gates error-rate and latency scoring
			if math.Max(cur, median(hist)) < e.cfg.Scoring.MinRequestRate {
				loadOK = false
			}
		}
		if !has || len(hist) < 2 {
			continue
		}
		z, med := robustZ(cur, hist, ms.relFloor, ms.absFloor)
		kind := ""
		switch {
		case z > 0 && ms.direction >= 0:
			kind = ms.kindUp
		case z < 0 && ms.direction <= 0:
			kind = ms.kindDown
			z = -z
		}
		score := zScore(z)
		stopped := i == 0 && cur == 0 && med >= e.cfg.Scoring.MinRequestRate
		if stopped {
			// Traffic stopped entirely: the relative noise floor caps z for a
			// 100% drop, so score a full stop directly. It is rarely noise.
			kind, score = KindThroughputDown, 0.9
		}
		if kind == "" || score < e.cfg.Scoring.ReportThreshold {
			continue
		}
		f := Finding{
			Service: svc,
			Signal:  SignalMetrics,
			Kind:    kind,
			Score:   round2(score),
			Before:  humanValue(med, ms.unit),
			After:   humanValue(cur, ms.unit),
		}
		verb := "up"
		if kind == KindThroughputDown {
			verb = "down"
		}
		f.Title = fmt.Sprintf("%s %s: %s → %s", displayName(ms.name), verb, f.Before, f.After)
		if stopped {
			f.Title = fmt.Sprintf("Request rate dropped to zero (was %s): service stopped receiving or reporting traffic", f.Before)
		}
		f.onset = e.metricOnset(ctx, ms, svc, spec, med, cur)
		f.Onset = fmtTime(f.onset)
		f.EvidenceLink = e.metricsLink(q, spec.SubWindows[0].Start, spec.Window.End)
		out = append(out, f)
	}
	if ok == 0 && lastErr != nil {
		return nil, lastErr
	}
	if lastErr != nil {
		// Some queries answered and some failed: keep the findings, but say
		// that the picture is incomplete.
		return out, partialError{svc: svc, err: lastErr}
	}
	return out, nil
}

// windowedMetrics is implemented by metrics sources whose queries carry no
// lookback of their own (NRQL): the engine passes it, so a value at t
// aggregates [t-lookback, t] as a PromQL [window] range does.
type windowedMetrics interface {
	InstantOver(ctx context.Context, q string, at time.Time, lookback time.Duration) ([]backend.Series, error)
	RangeOver(ctx context.Context, q string, start, end time.Time, step, lookback time.Duration) ([]backend.Series, error)
}

func (e *Engine) metricInstant(ctx context.Context, q string, at time.Time, lookback time.Duration) ([]backend.Series, error) {
	if w, ok := e.metrics.(windowedMetrics); ok {
		return w.InstantOver(ctx, q, at, lookback)
	}
	return e.metrics.Instant(ctx, q, at)
}

func (e *Engine) metricRange(ctx context.Context, q string, start, end time.Time, step, lookback time.Duration) ([]backend.Series, error) {
	if w, ok := e.metrics.(windowedMetrics); ok {
		return w.RangeOver(ctx, q, start, end, step, lookback)
	}
	return e.metrics.Range(ctx, q, start, end, step)
}

func (e *Engine) instant(ctx context.Context, q string, at time.Time, lookback time.Duration) (float64, bool, error) {
	s, err := e.metricInstant(ctx, q, at, lookback)
	if err != nil {
		return 0, false, err
	}
	v, ok := backend.Scalar(s)
	return v, ok, nil
}

// history evaluates q at the end of every baseline sub-window. Because the
// template's range equals the window length, each value aggregates exactly one
// sub-window.
func (e *Engine) history(ctx context.Context, q string, spec window.Spec) ([]float64, error) {
	subs := spec.SubWindows
	w := spec.Window.Duration()
	series, err := e.metricRange(ctx, q, subs[0].End, subs[len(subs)-1].End, w, w)
	if err != nil {
		return nil, err
	}
	if len(series) == 0 {
		return nil, nil
	}
	vals := make([]float64, 0, len(series[0].Points))
	for _, p := range series[0].Points {
		vals = append(vals, p.V)
	}
	return vals, nil
}

// metricOnset finds when the metric first moved past the midpoint between its
// baseline and its window value, using a finer range query over the window.
func (e *Engine) metricOnset(ctx context.Context, ms metricSpec, svc string, spec window.Spec, before, after float64) time.Time {
	w := spec.Window.Duration()
	fine := w / 10
	if fine < time.Minute {
		fine = time.Minute
	}
	q, err := e.cfg.Metrics.Render(ms.tmpl, svc, fine)
	if err != nil {
		return time.Time{}
	}
	series, err := e.metricRange(ctx, q, spec.Window.Start, spec.Window.End, fine/2, fine)
	if err != nil || len(series) == 0 {
		return time.Time{}
	}
	mid := (before + after) / 2
	rising := after > before
	for _, p := range series[0].Points {
		if (rising && p.V >= mid) || (!rising && p.V <= mid) {
			// The value at t aggregates [t-fine, t]; report the start of that slice.
			t := p.T.Add(-fine)
			if t.Before(spec.Window.Start) {
				t = spec.Window.Start
			}
			return t
		}
	}
	return time.Time{}
}

// displayName turns a metric name into title words: "consumer_lag" ->
// "Consumer lag", "cpu" -> "CPU".
func displayName(s string) string {
	s = strings.ReplaceAll(s, "_", " ")
	if len(s) <= 3 {
		return strings.ToUpper(s)
	}
	return capFirst(s)
}

func capFirst(s string) string {
	if s == "" {
		return s
	}
	b := []byte(s)
	if b[0] >= 'a' && b[0] <= 'z' {
		b[0] -= 'a' - 'A'
	}
	return string(b)
}
