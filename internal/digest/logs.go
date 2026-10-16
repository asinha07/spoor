package digest

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/asinha07/spoor/internal/backend"
	"github.com/asinha07/spoor/internal/links"
	"github.com/asinha07/spoor/internal/window"
)

const (
	patternLimit   = 200
	maxPatternLen  = 140
	maxExampleLen  = 200
	spikeMinRatio  = 4.0
	logSpikeMinZ   = 3.0
	newPatternBase = 0.5
	spikeMaxP      = 1e-3 // family-wise false-alarm rate for spike findings per call
	steadyMaxRatio = 2.0  // errors above twice the usual rate are not "steady"...
	steadyMinP     = 0.01 // ...unless the excess is plausibly chance
	maxSteady      = 3    // steady services listed per response
)

// poissonSF returns P(X >= k) for X ~ Poisson(lambda).
func poissonSF(k, lambda float64) float64 {
	if k <= 0 {
		return 1
	}
	n := math.Ceil(k)
	sum := 0.0
	for i := n; i < n+2000; i++ {
		lg, _ := math.Lgamma(i + 1)
		term := math.Exp(-lambda + i*math.Log(lambda) - lg)
		sum += term
		if i > lambda && term < sum*1e-12 {
			break
		}
	}
	return math.Min(sum, 1)
}

// errorPatternQuery groups a service's error-level lines into patterns
// server-side: numbers, UUIDs and IPs are collapsed by VictoriaLogs.
func (e *Engine) errorPatternQuery(svc string) string {
	return e.logFilter(svc) + " | copy _msg as raw | collapse_nums prettify" +
		" | stats by (_msg) count() n, min(_time) first, any(raw) example" +
		fmt.Sprintf(" | sort by (n desc) | limit %d", patternLimit)
}

func (e *Engine) logFilter(svc string) string {
	f := backend.Eq(e.cfg.Logs.ServiceField, svc)
	if e.cfg.Logs.ErrorFilter != "" {
		f += " " + e.cfg.Logs.ErrorFilter
	}
	return f
}

type patternStat struct {
	n       float64
	first   time.Time
	example string
}

func (e *Engine) patterns(ctx context.Context, q string, r window.Range) (map[string]patternStat, error) {
	rows, err := e.logs.Query(ctx, q, r.Start, r.End, 0)
	if err != nil {
		return nil, err
	}
	out := make(map[string]patternStat, len(rows))
	for _, row := range rows {
		out[row["_msg"]] = patternStat{n: row.Float("n"), first: row.Time("first"), example: row["example"]}
	}
	return out, nil
}

// scoreLogs flags error patterns that are new in the window, or whose rate
// jumped against the baseline history. When the service's errors as a whole
// are running at their usual rate, it also returns a SteadyError: positive
// evidence that those errors predate the window, which stops an agent from
// blaming chronic background errors for a new problem.
func (e *Engine) scoreLogs(ctx context.Context, svc string, spec window.Spec) ([]Finding, *SteadyError, error) {
	q := e.errorPatternQuery(svc)
	cur, err := e.patterns(ctx, q, spec.Window)
	if err != nil {
		return nil, nil, err
	}
	if len(cur) == 0 {
		return nil, nil, nil
	}
	hist := window.Range{Start: spec.SubWindows[0].Start, End: spec.Baseline.End}
	base, err := e.patterns(ctx, q, hist)
	if err != nil {
		return nil, nil, err
	}
	k := float64(len(spec.SubWindows))
	minCount := float64(e.cfg.Scoring.MinPatternCount)
	link := links.LogsQL(e.uiBase(e.cfg.Logs.Backend), e.logFilter(svc), hist.Start, spec.Window.End)

	keys := make([]string, 0, len(cur))
	for p := range cur {
		keys = append(keys, p)
	}
	sort.Strings(keys)

	var out []Finding
	for _, pat := range keys {
		c := cur[pat]
		if c.n < minCount {
			continue
		}
		b, seen := base[pat]
		expected := b.n / k
		var kind string
		var score float64
		switch {
		case !seen || b.n == 0:
			kind = KindNewLogPattern
			// A new error pattern is noteworthy on its own; volume raises it further.
			score = newPatternBase + 0.4*(1-1/(1+c.n/20))
		default:
			ratio := (c.n + 1) / (expected + 1)
			z := (c.n - expected) / math.Sqrt(expected+1)
			if ratio < spikeMinRatio || z < logSpikeMinZ {
				continue
			}
			// Many patterns are checked per window, so a chance blip in one of
			// them (a background error seen 7 times instead of once) must not
			// count. Require the Poisson tail probability, corrected for the
			// number of patterns checked, to be small.
			if poissonSF(c.n, math.Max(expected, 1))*float64(len(keys)) > spikeMaxP {
				continue
			}
			kind = KindSpikingLogPattern
			score = zScore(z)
		}
		if score < e.cfg.Scoring.ReportThreshold {
			continue
		}
		f := Finding{
			Service:      svc,
			Signal:       SignalLogs,
			Kind:         kind,
			Score:        round2(score),
			After:        fmt.Sprintf("%.0f in window", c.n),
			Example:      Redact(c.example, maxExampleLen),
			EvidenceLink: link,
		}
		short := truncate(pat, maxPatternLen)
		if kind == KindNewLogPattern {
			f.Before = "0"
			f.Title = fmt.Sprintf("New error log pattern: %q", short)
		} else {
			f.Before = fmt.Sprintf("%s per window", trimFloat(expected))
			f.Title = fmt.Sprintf("Error log pattern spiking: %q", short)
		}
		f.onset = c.first
		f.Onset = fmtTime(c.first)
		out = append(out, f)
	}
	return out, steadyErrors(svc, cur, base, k, minCount, len(out) > 0), nil
}

// steadyErrors summarizes a service's error lines when, taken together, they
// run at their baseline rate. Patterns are totalled per service because
// chronic errors often fragment into many patterns (one per product ID, say),
// each too small to judge alone.
func steadyErrors(svc string, cur, base map[string]patternStat, k, minCount float64, hasFindings bool) *SteadyError {
	if hasFindings || k <= 0 {
		return nil
	}
	var now, before float64
	top, topN := "", -1.0
	for pat, c := range cur {
		now += c.n
		if c.n > topN || (c.n == topN && pat < top) {
			top, topN = pat, c.n
		}
	}
	for _, b := range base {
		before += b.n
	}
	usual := before / k
	if now < minCount || usual <= 0 {
		return nil
	}
	// Steady: not significantly above the usual rate.
	if (now+1)/(usual+1) > steadyMaxRatio && poissonSF(now, math.Max(usual, 1)) < steadyMinP {
		return nil
	}
	return &SteadyError{
		Service:     svc,
		ErrorsNow:   math.Round(now),
		ErrorsUsual: math.Round(usual*10) / 10,
		Example:     Redact(truncate(top, maxPatternLen), maxPatternLen),
	}
}
