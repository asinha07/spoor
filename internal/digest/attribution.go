package digest

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/asinha07/spoor/internal/backend"
	"github.com/asinha07/spoor/internal/window"
)

// Root-cause attribution uses trace structure to tell a cause from its echoes.
//
//   - Errors: in each failed trace, a failed span with no failed child is
//     where the failure started (its origin). Callers that fail only because a
//     callee failed are echoes.
//   - Latency: a span's self time is its duration minus its direct children's.
//     The service whose self time grew is where added latency came from.
//
// Findings in services whose errors or latency originate elsewhere are
// demoted and annotated, and an origin finding names where it started.

const (
	attributionSpanLimit = 50000
	originMinTraces      = 5
	originMinShare       = 0.5
	echoMaxShare         = 0.2
	echoDemoteErrors     = 0.7
	echoDemoteLatency    = 0.75
	selfMinDeltaNs       = 10e6 // 10ms of added mean self time
)

// Origin finding kinds.
const (
	KindErrorOrigin   = "error_origin"
	KindLatencyOrigin = "latency_origin"
)

type spanRow struct {
	traceID, spanID, parentID, svc, name string
	dur                                  float64
	start                                int64 // Unix ns; 0 when unknown
}

type opKey struct{ svc, name string }

type attribution struct {
	// failedTraces[S]: failed traces in which service S has a failed span.
	failedTraces map[string]int
	// originBy[S][O]: of those, how many started in service O.
	originBy map[string]map[string]int
	// originOps[O][op]: which operation the failures started in.
	originOps map[string]map[string]int
	// Self time per operation, window and baseline.
	selfW, selfB map[opKey]*selfStat
	// addedSelf[S]: total self time added in service S over the window (ns).
	addedSelf    map[string]float64
	addedSelfSum float64
}

type selfStat struct{ n, total float64 }

func (s *selfStat) mean() float64 {
	if s == nil || s.n == 0 {
		return 0
	}
	return s.total / s.n
}

func (e *Engine) spanRows(ctx context.Context, filter string, r window.Range) ([]spanRow, error) {
	t := e.cfg.Traces
	// A quoted field name inside a fields list misbehaves in some versions, so
	// copy the service field to a plain alias first.
	q := fmt.Sprintf("%s %s | copy %s as spoor_svc, %s as spoor_name, %s as spoor_dur", realSpans, filter,
		backend.Quote(t.ServiceField), backend.Quote(t.NameField), backend.Quote(t.DurationField))
	fields := "trace_id, span_id, parent_span_id, spoor_svc, spoor_name, spoor_dur"
	if t.StartField != "" {
		q += ", " + backend.Quote(t.StartField) + " as spoor_start"
		fields += ", spoor_start"
	}
	q += " | fields " + fields
	rows, err := e.traces.Query(ctx, q, r.Start, r.End, attributionSpanLimit)
	if err != nil {
		return nil, err
	}
	out := make([]spanRow, 0, len(rows))
	for _, row := range rows {
		if e.ignored(row["spoor_svc"]) {
			continue
		}
		out = append(out, spanRow{
			traceID: row["trace_id"], spanID: row["span_id"], parentID: row["parent_span_id"],
			svc: row["spoor_svc"], name: row["spoor_name"], dur: row.Float("spoor_dur"),
			start: parseInt(row["spoor_start"]),
		})
	}
	return out, nil
}

// attribute computes error origins and self-time attribution for the window.
func (e *Engine) attribute(ctx context.Context, spec window.Spec) (*attribution, error) {
	a := &attribution{
		failedTraces: map[string]int{},
		originBy:     map[string]map[string]int{},
		originOps:    map[string]map[string]int{},
		addedSelf:    map[string]float64{},
	}
	failed, err := e.spanRows(ctx, e.cfg.Traces.ErrorFilter, spec.Window)
	if err != nil {
		return nil, err
	}
	cur, err := e.spanRows(ctx, "", spec.Window)
	if err != nil {
		return nil, err
	}
	// The full span set links failed spans through callers that did not mark
	// their own span as failed.
	a.errorOrigins(failed, cur)

	base, err := e.spanRows(ctx, "", spec.Baseline)
	if err != nil {
		return nil, err
	}
	a.selfW, a.selfB = selfTimes(cur), selfTimes(base)
	for k, w := range a.selfW {
		b := a.selfB[k]
		if b == nil || b.n == 0 || w.n == 0 {
			continue
		}
		if d := (w.mean() - b.mean()) * w.n; d > 0 && w.mean()-b.mean() >= selfMinDeltaNs {
			a.addedSelf[k.svc] += d
			a.addedSelfSum += d
		}
	}
	return a, nil
}

// errorOrigins finds, in each failed trace, the failed spans with no failed
// span anywhere below them: that is where the failure started. Ancestry is
// walked through all spans, not only failed ones, because real services often
// return an error without marking their own span as failed. Looking only at
// direct failed children would then make a caller above such a gap look like a
// second origin (seen on the OpenTelemetry Demo: an Envoy proxy above a
// frontend that did not mark its span, above the real failure in payment).
func (a *attribution) errorOrigins(failed, all []spanRow) {
	parent := make(map[string]string, len(all)+len(failed)) // trace|span -> parent span
	for _, s := range all {
		parent[s.traceID+"|"+s.spanID] = s.parentID
	}
	for _, s := range failed {
		parent[s.traceID+"|"+s.spanID] = s.parentID
	}
	byTrace := map[string][]spanRow{}
	for _, s := range failed {
		byTrace[s.traceID] = append(byTrace[s.traceID], s)
	}
	for trace, spans := range byTrace {
		isFailed := make(map[string]bool, len(spans))
		for _, s := range spans {
			isFailed[s.spanID] = true
		}
		hasFailedBelow := map[string]bool{}
		for _, s := range spans {
			p := s.parentID
			for hops := 0; p != "" && hops < maxTreeDepth; hops++ {
				if isFailed[p] {
					hasFailedBelow[p] = true
				}
				p = parent[trace+"|"+p]
			}
		}
		origins := map[string]string{} // svc -> op
		involved := map[string]bool{}
		for _, s := range spans {
			involved[s.svc] = true
			if !hasFailedBelow[s.spanID] {
				origins[s.svc] = s.name
			}
		}
		for svc := range involved {
			a.failedTraces[svc]++
			if a.originBy[svc] == nil {
				a.originBy[svc] = map[string]int{}
			}
			for o := range origins {
				a.originBy[svc][o]++
			}
		}
		for o, op := range origins {
			if a.originOps[o] == nil {
				a.originOps[o] = map[string]int{}
			}
			a.originOps[o][op]++
		}
	}
}

// maxTreeDepth bounds the ancestor walk (and breaks any parent-ID cycle).
const maxTreeDepth = 256

// selfTimes computes each operation's self time: its duration minus the
// time covered by its direct children. With start times, children that run
// in parallel are counted once (the union of their intervals, clipped to the
// parent); without them, child durations are summed.
func selfTimes(rows []spanRow) map[opKey]*selfStat {
	children := map[string][]spanRow{} // trace|span -> direct children
	for _, s := range rows {
		if s.parentID != "" {
			k := s.traceID + "|" + s.parentID
			children[k] = append(children[k], s)
		}
	}
	out := map[opKey]*selfStat{}
	for _, s := range rows {
		self := s.dur - childTime(s, children[s.traceID+"|"+s.spanID])
		if self < 0 {
			self = 0
		}
		k := opKey{s.svc, s.name}
		st := out[k]
		if st == nil {
			st = &selfStat{}
			out[k] = st
		}
		st.n++
		st.total += self
	}
	return out
}

// topOrigin returns the service where most of S's failed traces started.
func (a *attribution) topOrigin(svc string) (origin string, share float64, n int) {
	n = a.failedTraces[svc]
	if n == 0 {
		return "", 0, 0
	}
	best, bestN := "", 0
	for o, c := range a.originBy[svc] {
		if c > bestN || (c == bestN && o < best) {
			best, bestN = o, c
		}
	}
	return best, float64(bestN) / float64(n), n
}

func (a *attribution) topOp(m map[string]int) string {
	best, bestN := "", -1
	for op, c := range m {
		if c > bestN || (c == bestN && op < best) {
			best, bestN = op, c
		}
	}
	return best
}

// selfShare returns S's share of all added self time.
func (a *attribution) selfShare(svc string) float64 {
	if a.addedSelfSum == 0 {
		return 0
	}
	return a.addedSelf[svc] / a.addedSelfSum
}

// topSelfOrigin returns the service that added the most self time.
func (a *attribution) topSelfOrigin() (string, float64) {
	best, bestV := "", 0.0
	for s, v := range a.addedSelf {
		if v > bestV || (v == bestV && s < best) {
			best, bestV = s, v
		}
	}
	if a.addedSelfSum == 0 {
		return "", 0
	}
	return best, bestV / a.addedSelfSum
}

// topSelfOp returns the operation in svc whose self time grew the most.
func (a *attribution) topSelfOp(svc string) (opKey, *selfStat, *selfStat) {
	var best opKey
	bestV := 0.0
	keys := make([]opKey, 0)
	for k := range a.selfW {
		if k.svc == svc {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].name < keys[j].name })
	for _, k := range keys {
		w, b := a.selfW[k], a.selfB[k]
		if b == nil {
			continue
		}
		if d := (w.mean() - b.mean()) * w.n; d > bestV {
			best, bestV = k, d
		}
	}
	return best, a.selfW[best], a.selfB[best]
}

func isErrorKind(k string) bool {
	switch k {
	case KindErrorRateUp, KindNewLogPattern, KindSpikingLogPattern, KindSpanErrorsUp:
		return true
	}
	return false
}

func isLatencyKind(k string) bool { return k == KindLatencyUp || k == KindSpanSlower }

// applyAttribution adds origin findings and demotes echoes. scope lists the
// services the caller asked about; origins outside it are still reported,
// because "the cause is downstream in X" is the most useful thing to say.
func (e *Engine) applyAttribution(a *attribution, scope []string, fs []Finding, spec window.Spec) []Finding {
	if a == nil {
		return fs
	}
	inScope := map[string]bool{}
	for _, s := range scope {
		inScope[s] = true
	}

	hasKind := func(svc string, pred func(string) bool) bool {
		for _, f := range fs {
			if f.Service == svc && pred(f.Kind) {
				return true
			}
		}
		return false
	}

	// Errors. Only explain services that actually show an error change: a
	// healthy system still has a few failed traces, and those are not news.
	added := map[string]bool{}
	for _, svc := range scope {
		if !hasKind(svc, isErrorKind) {
			continue
		}
		origin, share, n := a.topOrigin(svc)
		if n < originMinTraces || origin == "" || share < originMinShare {
			continue
		}
		if origin != svc {
			for i := range fs {
				if fs[i].Service == svc && isErrorKind(fs[i].Kind) {
					fs[i].Score = round2(fs[i].Score * echoDemoteErrors)
					if !strings.Contains(fs[i].Title, "originates in") {
						fs[i].Title += " — originates in " + origin
					}
				}
			}
		}
		if added[origin] {
			continue
		}
		oShare, oN := share, n
		if origin != svc {
			// Describe the origin with its own numbers when we have them.
			if o2, s2, n2 := a.topOrigin(origin); o2 == origin && n2 >= originMinTraces {
				oShare, oN = s2, n2
			}
		}
		op := a.topOp(a.originOps[origin])
		f := Finding{
			Service: origin, Signal: SignalTraces, Kind: KindErrorOrigin,
			Score:        round2(0.5 + 0.49*oShare),
			Title:        fmt.Sprintf("Failures start here, in %q (%.0f%% of %d failed traces)", op, 100*oShare, oN),
			EvidenceLink: linkTraces(e, realSpans+" "+e.cfg.Traces.ErrorFilter+" "+backend.Eq(e.cfg.Traces.ServiceField, origin), spec),
		}
		if !inScope[origin] {
			f.Title = fmt.Sprintf("Failures in %s start downstream in %s, %q (%.0f%% of %d failed traces)", svc, origin, op, 100*share, n)
		}
		fs = append(fs, f)
		added[origin] = true
	}

	// Latency.
	origin, share := a.topSelfOrigin()
	if origin == "" || share < originMinShare {
		return fs
	}
	k, w, b := a.topSelfOp(origin)
	if w == nil || b == nil || w.mean() < b.mean()*slowerMinRatio {
		return fs
	}
	isEcho := func(f Finding) bool {
		return isLatencyKind(f.Kind) && f.Service != origin && a.selfShare(f.Service) < echoMaxShare
	}
	echoed := false
	for _, f := range fs {
		echoed = echoed || isEcho(f)
	}
	if echoed || hasKind(origin, isLatencyKind) {
		for i := range fs {
			if isEcho(fs[i]) {
				fs[i].Score = round2(fs[i].Score * echoDemoteLatency)
				if !strings.Contains(fs[i].Title, "originates in") {
					fs[i].Title += " — added latency originates in " + origin
				}
			}
		}
		{
			title := fmt.Sprintf("Added latency starts here, in %q: self time %s → %s (%.0f%% of added self time)",
				k.name, humanNanos(b.mean()), humanNanos(w.mean()), 100*share)
			if !inScope[origin] {
				title = fmt.Sprintf("Added latency starts downstream in %s, %q: self time %s → %s (%.0f%% of added self time)",
					origin, k.name, humanNanos(b.mean()), humanNanos(w.mean()), 100*share)
			}
			fs = append(fs, Finding{
				Service: origin, Signal: SignalTraces, Kind: KindLatencyOrigin,
				Score: round2(0.5 + 0.49*share), Title: title,
				Before: humanNanos(b.mean()), After: humanNanos(w.mean()),
				EvidenceLink: linkTraces(e, realSpans+" "+backend.Eq(e.cfg.Traces.ServiceField, origin)+" "+backend.Eq(e.cfg.Traces.NameField, k.name), spec),
			})
		}
	}
	return fs
}

func linkTraces(e *Engine, filter string, spec window.Spec) string {
	return e.tracesLink(filter, spec.Window.Start, spec.Window.End)
}

// childTime is the part of parent's duration covered by its children.
func childTime(parent spanRow, kids []spanRow) float64 {
	if len(kids) == 0 {
		return 0
	}
	known := parent.start > 0
	for _, k := range kids {
		known = known && k.start > 0
	}
	if !known {
		sum := 0.0
		for _, k := range kids {
			sum += k.dur
		}
		return sum
	}
	type iv struct{ a, b float64 }
	pa := float64(parent.start)
	pb := pa + parent.dur
	ivs := make([]iv, 0, len(kids))
	for _, k := range kids {
		a := math.Max(float64(k.start), pa)
		b := math.Min(float64(k.start)+k.dur, pb)
		if b > a {
			ivs = append(ivs, iv{a, b})
		}
	}
	sort.Slice(ivs, func(i, j int) bool { return ivs[i].a < ivs[j].a })
	covered := 0.0
	for i := 0; i < len(ivs); {
		a, b := ivs[i].a, ivs[i].b
		j := i + 1
		for j < len(ivs) && ivs[j].a <= b {
			b = math.Max(b, ivs[j].b)
			j++
		}
		covered += b - a
		i = j
	}
	return covered
}

func parseInt(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}
