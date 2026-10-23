package digest

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/asinha07/spoor/internal/backend"
	"github.com/asinha07/spoor/internal/config"
	"github.com/asinha07/spoor/internal/window"
)

// NRQLRunner runs one NRQL query (the NerdGraph client, or a fake in tests).
type NRQLRunner interface {
	NRQL(ctx context.Context, nrql string) ([]map[string]any, error)
}

// nrFacetLimit is NRQL's LIMIT MAX for facets.
const nrFacetLimit = 5000

func newRelicClient(cfg config.Config) NRQLRunner {
	if !cfg.UsesNewRelic() {
		return nil
	}
	return backend.NewNewRelic(cfg.NewRelic.Endpoint(), cfg.NewRelic.APIKey, cfg.NewRelic.AccountID, cfg.Timeout)
}

var nrMeta = map[string]bool{"beginTimeSeconds": true, "endTimeSeconds": true, "facet": true, "timestamp": true}

// nrValue reads the one aggregate value of a result row.
func nrValue(row map[string]any) (float64, bool) {
	for _, k := range sortedRowKeys(row) {
		if nrMeta[k] {
			continue
		}
		if v, ok := backend.NRQLNumber(row[k]); ok {
			return v, true
		}
	}
	return 0, false
}

func sortedRowKeys(row map[string]any) []string {
	ks := make([]string, 0, len(row))
	for k := range row {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func nrNum(row map[string]any, key string) float64 {
	v, _ := backend.NRQLNumber(row[key])
	return v
}

// --- Metrics ---------------------------------------------------------------

// newRelicMetrics evaluates NRQL metric templates. NRQL has no lookback
// inside the query, so the engine passes it: a value at t aggregates
// [t-lookback, t], as a PromQL [window] range does.
type newRelicMetrics struct {
	api   NRQLRunner
	event string // event type services are listed from (Transaction)
	label string // service attribute (appName)
}

func newNewRelicMetrics(api NRQLRunner, requestRate, label string) newRelicMetrics {
	event := "Transaction"
	if mm := nrFrom.FindStringSubmatch(requestRate); mm != nil {
		event = mm[1]
	}
	return newRelicMetrics{api: api, event: event, label: label}
}

// Triage scores every service by how far its request rate, error rate and
// p95 moved in the window against the baseline sub-windows, in two batched
// queries, so a capped scan examines the most changed services. It reads
// Transaction-style events (error, duration attributes).
func (m newRelicMetrics) Triage(ctx context.Context, spec window.Spec) (map[string]float64, error) {
	lbl := config.NRQLField(m.label)
	sel := fmt.Sprintf("SELECT count(*) AS n, filter(count(*), WHERE error IS true) AS errs, percentile(duration, 95) AS p95 FROM %s FACET %s LIMIT MAX", m.event, lbl)
	cur, err := m.api.NRQL(ctx, sel+backend.NRQLRange(spec.Window.Start, spec.Window.End))
	if err != nil {
		return nil, err
	}
	subs := spec.SubWindows
	w := spec.Window.Duration()
	hist := map[string][][3]float64{}
	regular := true
	for i := 1; i < len(subs); i++ {
		regular = regular && subs[i].Start.Equal(subs[i-1].End)
	}
	if !regular {
		return nil, fmt.Errorf("triage needs contiguous baseline windows")
	}
	rows, err := m.api.NRQL(ctx, sel+backend.NRQLRange(subs[0].Start, subs[len(subs)-1].End)+" TIMESERIES "+nrDuration(w))
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		svc := nrFacet(r, 0)
		hist[svc] = append(hist[svc], [3]float64{nrNum(r, "n"), nrNum(r, "errs"), nrNum(r, "p95")})
	}
	out := map[string]float64{}
	for _, r := range cur {
		svc := nrFacet(r, 0)
		n, errs, p95 := nrNum(r, "n"), nrNum(r, "errs"), nrNum(r, "p95")
		var rates, errRates, p95s []float64
		for _, h := range hist[svc] {
			rates = append(rates, h[0])
			if h[0] > 0 {
				errRates = append(errRates, h[1]/h[0])
				p95s = append(p95s, h[2])
			}
		}
		if len(rates) < 2 {
			out[svc] = 1 // new or sparse: worth a look
			continue
		}
		zr, _ := robustZ(n, rates, 0.3, 5)
		score := math.Abs(zr)
		if n > 0 && len(errRates) >= 2 {
			ze, _ := robustZ(errs/n, errRates, 0.5, 0.02)
			score = math.Max(score, ze)
			zp, _ := robustZ(p95, p95s, 0.25, 0.02)
			score = math.Max(score, zp)
		}
		out[svc] = score
	}
	return out, nil
}

func (m newRelicMetrics) Instant(ctx context.Context, q string, at time.Time) ([]backend.Series, error) {
	return m.InstantOver(ctx, q, at, 0)
}

func (m newRelicMetrics) Range(ctx context.Context, q string, start, end time.Time, step time.Duration) ([]backend.Series, error) {
	return m.RangeOver(ctx, q, start, end, step, step)
}

func (m newRelicMetrics) InstantOver(ctx context.Context, q string, at time.Time, lookback time.Duration) ([]backend.Series, error) {
	if lookback <= 0 {
		lookback = 30 * time.Minute
	}
	rows, err := m.api.NRQL(ctx, q+backend.NRQLRange(at.Add(-lookback), at))
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	v, ok := nrValue(rows[0])
	if !ok {
		return nil, nil
	}
	return []backend.Series{{Points: []backend.Point{{T: at, V: v}}}}, nil
}

func (m newRelicMetrics) RangeOver(ctx context.Context, q string, start, end time.Time, step, lookback time.Duration) ([]backend.Series, error) {
	if step <= 0 {
		step = lookback
	}
	ts := " TIMESERIES " + nrDuration(lookback)
	if step != lookback {
		ts += " SLIDE BY " + nrDuration(step)
	}
	rows, err := m.api.NRQL(ctx, q+backend.NRQLRange(start.Add(-lookback), end)+ts)
	if err != nil {
		return nil, err
	}
	s := backend.Series{}
	for _, row := range rows {
		v, ok := nrValue(row)
		if !ok {
			continue
		}
		t := time.Unix(int64(nrNum(row, "endTimeSeconds")), 0).UTC()
		if t.Before(start) || t.After(end) {
			continue
		}
		s.Points = append(s.Points, backend.Point{T: t, V: v})
	}
	if len(s.Points) == 0 {
		return nil, nil
	}
	return []backend.Series{s}, nil
}

var nrFrom = regexp.MustCompile(`(?i)\bFROM\s+([A-Za-z_][A-Za-z0-9_]*)`)

// LabelValues lists services from the event type the request-rate
// template reads (Transaction by default).
func (m newRelicMetrics) LabelValues(ctx context.Context, label string, start, end time.Time) ([]string, error) {
	return nrUniques(ctx, m.api, m.event, label, start, end)
}

func nrUniques(ctx context.Context, api NRQLRunner, event, attr string, start, end time.Time) ([]string, error) {
	f := config.NRQLField(attr)
	rows, err := api.NRQL(ctx, fmt.Sprintf("SELECT uniques(%s, 10000) FROM %s WHERE %s IS NOT NULL", f, event, f)+backend.NRQLRange(start, end))
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	var out []string
	for _, k := range sortedRowKeys(rows[0]) {
		if list, ok := rows[0][k].([]any); ok {
			for _, v := range list {
				if s := backend.NRQLString(v); s != "" {
					out = append(out, s)
				}
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

// nrDuration formats d for TIMESERIES / SLIDE BY.
func nrDuration(d time.Duration) string {
	s := int64(d / time.Second)
	switch {
	case s <= 0:
		return "1 minute"
	case s%3600 == 0:
		return fmt.Sprintf("%d hours", s/3600)
	case s%60 == 0:
		return fmt.Sprintf("%d minutes", s/60)
	}
	return fmt.Sprintf("%d seconds", s)
}

// --- Logs ------------------------------------------------------------------

// newRelicLogs implements LogStore on New Relic Log events. NRQL cannot
// rewrite text, so the most frequent distinct messages are fetched (FACET,
// up to 5000) and grouped into patterns in Go; error counts are exact.
type newRelicLogs struct {
	e    *Engine
	api  NRQLRunner
	norm *normalizer
}

// NewNewRelicLogs returns a LogStore backed by New Relic.
func NewNewRelicLogs(e *Engine, api NRQLRunner) LogStore {
	return &newRelicLogs{e: e, api: api, norm: newNormalizer(e.cfg.Logs)}
}

func (n *newRelicLogs) svc() string { return config.NRQLField(n.e.cfg.Logs.ServiceField) }

func (n *newRelicLogs) where(services []string, all bool, errorsOnly bool) string {
	var conds []string
	if !all && len(services) > 0 {
		qs := make([]string, len(services))
		for i, s := range services {
			qs[i] = config.NRQLString(s)
		}
		conds = append(conds, n.svc()+" IN ("+strings.Join(qs, ", ")+")")
	}
	if errorsOnly {
		conds = append(conds, n.e.cfg.Logs.EffectiveErrorFilter())
	}
	if len(conds) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(conds, " AND ")
}

func (n *newRelicLogs) Services(ctx context.Context, r window.Range) ([]string, error) {
	return nrUniques(ctx, n.api, "Log", n.e.cfg.Logs.ServiceField, r.Start, r.End)
}

func (n *newRelicLogs) Evidence(svc string) string {
	return "SELECT * FROM Log" + n.where([]string{svc}, false, true)
}

func (n *newRelicLogs) Patterns(ctx context.Context, services []string, r window.Range, buckets []window.Range) (patternResult, error) {
	res := patternResult{patterns: servicePatterns{}, counts: map[string]errorCounts{}}
	if len(services) == 0 {
		return res, nil
	}
	field := "message"
	grouped := false
	if pf := n.e.cfg.Logs.PatternField; pf != "" {
		field, grouped = pf, true
	}
	q := fmt.Sprintf("SELECT count(*) AS n, min(timestamp) AS first, latest(message) AS example FROM Log%s FACET %s, %s LIMIT MAX",
		n.where(services, false, true), n.svc(), config.NRQLField(field)) + backend.NRQLRange(r.Start, r.End)
	rows, err := n.api.NRQL(ctx, q)
	if err != nil {
		return res, err
	}
	for _, row := range rows {
		svc, text := nrFacet(row, 0), nrFacet(row, 1)
		pat := text
		if !grouped {
			pat = n.norm.pattern(text)
		}
		if res.patterns[svc] == nil {
			res.patterns[svc] = map[string]patternStat{}
		}
		st := res.patterns[svc][pat]
		st.n += nrNum(row, "n")
		if ms := nrNum(row, "first"); ms > 0 {
			t := time.UnixMilli(int64(ms)).UTC()
			if st.first.IsZero() || t.Before(st.first) {
				st.first = t
			}
		}
		if st.example == "" {
			st.example = backend.NRQLString(row["example"])
		}
		res.patterns[svc][pat] = st
	}
	for svc, pats := range res.patterns {
		res.patterns[svc] = topPatterns(pats, patternLimit)
	}
	if len(rows) >= nrFacetLimit {
		res.note = fmt.Sprintf("more than %d distinct error messages; patterns grouped from the %d most frequent (counts per service exact)", nrFacetLimit, nrFacetLimit)
	}
	res.counts, err = n.errorCounts(ctx, services, r, buckets)
	return res, err
}

// nrFacet reads the i-th facet value of a multi-facet row.
func nrFacet(row map[string]any, i int) string {
	switch f := row["facet"].(type) {
	case []any:
		if i < len(f) {
			return backend.NRQLString(f[i])
		}
	case string:
		if i == 0 {
			return f
		}
	}
	return ""
}

func (n *newRelicLogs) errorCounts(ctx context.Context, services []string, r window.Range, buckets []window.Range) (map[string]errorCounts, error) {
	out := map[string]errorCounts{}
	get := func(svc string) errorCounts {
		c, ok := out[svc]
		if !ok {
			c = errorCounts{buckets: make([]float64, len(buckets))}
		}
		return c
	}
	rows, err := n.api.NRQL(ctx, "SELECT count(*) AS n FROM Log"+n.where(services, false, true)+" FACET "+n.svc()+" LIMIT MAX"+backend.NRQLRange(r.Start, r.End))
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		svc := nrFacet(row, 0)
		c := get(svc)
		c.total = nrNum(row, "n")
		out[svc] = c
	}
	if len(buckets) == 0 {
		return out, nil
	}
	per, err := n.bucketed(ctx, "SELECT count(*) AS n FROM Log"+n.where(services, false, true), buckets, "n")
	if err != nil {
		return nil, err
	}
	for svc, vals := range per {
		c := get(svc)
		copy(c.buckets, vals)
		out[svc] = c
	}
	return out, nil
}

// bucketed runs "<q> FACET service" per bucket: one TIMESERIES query when
// the buckets are contiguous and equal, else one query each.
func (n *newRelicLogs) bucketed(ctx context.Context, q string, buckets []window.Range, field string) (map[string][]float64, error) {
	out := map[string][]float64{}
	add := func(svc string, i int, v float64) {
		if out[svc] == nil {
			out[svc] = make([]float64, len(buckets))
		}
		out[svc][i] += v
	}
	step := buckets[0].Duration()
	regular := len(buckets) > 1
	for i := 1; i < len(buckets); i++ {
		regular = regular && buckets[i].Start.Equal(buckets[i-1].End) && buckets[i].Duration() == step
	}
	if regular {
		rows, err := n.api.NRQL(ctx, q+" FACET "+n.svc()+" LIMIT MAX"+backend.NRQLRange(buckets[0].Start, buckets[len(buckets)-1].End)+" TIMESERIES "+nrDuration(step))
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			begin := time.Unix(int64(nrNum(row, "beginTimeSeconds")), 0)
			i := int(math.Round(float64(begin.Sub(buckets[0].Start)) / float64(step)))
			if i >= 0 && i < len(buckets) {
				add(nrFacet(row, 0), i, nrNum(row, field))
			}
		}
		return out, nil
	}
	for i, b := range buckets {
		rows, err := n.api.NRQL(ctx, q+" FACET "+n.svc()+" LIMIT MAX"+backend.NRQLRange(b.Start, b.End))
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			add(nrFacet(row, 0), i, nrNum(row, field))
		}
	}
	return out, nil
}

func (n *newRelicLogs) Totals(ctx context.Context, services []string, all bool, r window.Range, step time.Duration, num int) (map[string][]volume, error) {
	var buckets []window.Range
	if step == 0 {
		buckets = []window.Range{r}
	} else {
		for i := 0; i < num; i++ {
			buckets = append(buckets, window.Range{Start: r.Start.Add(time.Duration(i) * step), End: r.Start.Add(time.Duration(i+1) * step)})
		}
	}
	sel := "count(*) AS total"
	if u := n.e.cfg.Logs.UnclassifiedFilter(); u != "" {
		sel += ", filter(count(*), WHERE " + u + ") AS unclassified"
	}
	q := "SELECT " + sel + " FROM Log" + n.where(services, all, false)
	totals, err := n.bucketed(ctx, q, buckets, "total")
	if err != nil {
		return nil, err
	}
	var uncl map[string][]float64
	if n.e.cfg.Logs.UnclassifiedFilter() != "" {
		if uncl, err = n.bucketed(ctx, q, buckets, "unclassified"); err != nil {
			return nil, err
		}
	}
	out := map[string][]volume{}
	for svc, vals := range totals {
		vs := make([]volume, len(buckets))
		for i, v := range vals {
			vs[i].total = v
			if uncl[svc] != nil {
				vs[i].unclassified = uncl[svc][i]
			}
		}
		out[svc] = vs
	}
	return out, nil
}

func (n *newRelicLogs) CountFamilies(ctx context.Context, services []string, r window.Range, checks []familyCheck) ([]float64, error) {
	parts := make([]string, len(checks))
	for i, c := range checks {
		cond := n.svc() + " = " + config.NRQLString(c.svc)
		for _, w := range c.words {
			cond += " AND lower(message) LIKE " + config.NRQLString("%"+strings.ToLower(w)+"%")
		}
		parts[i] = fmt.Sprintf("filter(count(*), WHERE %s) AS c%d", cond, i)
	}
	rows, err := n.api.NRQL(ctx, "SELECT "+strings.Join(parts, ", ")+" FROM Log"+n.where(services, false, true)+backend.NRQLRange(r.Start, r.End))
	out := make([]float64, len(checks))
	if err != nil || len(rows) == 0 {
		return out, err
	}
	for i := range checks {
		out[i] = nrNum(rows[0], fmt.Sprintf("c%d", i))
	}
	return out, nil
}

func (n *newRelicLogs) CountLines(ctx context.Context, r window.Range) (float64, error) {
	rows, err := n.api.NRQL(ctx, "SELECT count(*) AS n FROM Log"+backend.NRQLRange(r.Start, r.End))
	if err != nil || len(rows) == 0 {
		return 0, err
	}
	return nrNum(rows[0], "n"), nil
}

// --- Traces ----------------------------------------------------------------

const (
	nrAttrFailed = 50 // failed traces read whole for attribution
	nrAttrSample = 50 // other traces read whole for latency attribution
)

// newRelicTraces implements TraceStore on New Relic Span events. Span
// statistics are exact; attribution reads a sample of whole traces.
type newRelicTraces struct {
	e   *Engine
	api NRQLRunner
}

// NewNewRelicTraces returns a TraceStore backed by New Relic.
func NewNewRelicTraces(e *Engine, api NRQLRunner) TraceStore { return &newRelicTraces{e: e, api: api} }

// BiasedSpans: New Relic samples spans, keeping failed and slow traces
// preferentially, so span-level changes need the service's metrics to agree.
func (n *newRelicTraces) BiasedSpans() bool { return true }

// SampleNote is reported in coverage when traces are scored.
func (n *newRelicTraces) SampleNote() string {
	return fmt.Sprintf("spans are New Relic's sample of traces; attribution reads up to %d failed and %d other traces", nrAttrFailed, nrAttrSample)
}

func (n *newRelicTraces) cfg() config.TracesConfig { return n.e.cfg.Traces }

// durScale converts the duration field to nanoseconds: *.ms fields are
// milliseconds, anything else seconds (New Relic's convention).
func (n *newRelicTraces) durScale() float64 {
	if strings.HasSuffix(n.cfg().DurationField, ".ms") {
		return 1e6
	}
	return 1e9
}

func (n *newRelicTraces) where(svc, op, name string, failed bool) string {
	c := n.cfg()
	conds := []string{config.NRQLField(c.ServiceField) + " = " + config.NRQLString(svc)}
	if op != "" {
		conds = append(conds, config.NRQLField(c.NameField)+" = "+config.NRQLString(op))
	}
	if name != "" && name != op {
		conds = append(conds, config.NRQLField(c.NameField)+" = "+config.NRQLString(name))
	}
	if failed {
		conds = append(conds, c.ErrorFilter)
	}
	return " WHERE " + strings.Join(conds, " AND ")
}

func (n *newRelicTraces) Services(ctx context.Context, r window.Range) ([]string, error) {
	return nrUniques(ctx, n.api, "Span", n.cfg().ServiceField, r.Start, r.End)
}

func (n *newRelicTraces) Evidence(svc, op string) string {
	return "SELECT * FROM Span" + n.where(svc, op, "", false)
}

func (n *newRelicTraces) SpanStats(ctx context.Context, svc, op string, r window.Range) (map[string]spanStat, error) {
	c := n.cfg()
	dur := config.NRQLField(c.DurationField)
	q := fmt.Sprintf("SELECT count(*) AS n, filter(count(*), WHERE %s) AS errs, percentile(%s, 95) AS p95, sum(%s) AS total FROM Span%s FACET %s LIMIT %d",
		c.ErrorFilter, dur, dur, n.where(svc, op, "", false), config.NRQLField(c.NameField), spanLimit) + backend.NRQLRange(r.Start, r.End)
	rows, err := n.api.NRQL(ctx, q)
	if err != nil {
		return nil, err
	}
	scale := n.durScale()
	out := make(map[string]spanStat, len(rows))
	for _, row := range rows {
		out[nrFacet(row, 0)] = spanStat{n: nrNum(row, "n"), errs: nrNum(row, "errs"), p95: nrNum(row, "p95") * scale, total: nrNum(row, "total") * scale}
	}
	return out, nil
}

func (n *newRelicTraces) Example(ctx context.Context, svc, op, name string, failed bool, r window.Range) (string, time.Time) {
	c := n.cfg()
	q := fmt.Sprintf("SELECT trace.id, timestamp, %s FROM Span%s LIMIT 100", config.NRQLField(c.DurationField), n.where(svc, op, name, failed)) + backend.NRQLRange(r.Start, r.End)
	rows, err := n.api.NRQL(ctx, q)
	if err != nil || len(rows) == 0 {
		return "", time.Time{}
	}
	best := rows[0]
	for _, row := range rows[1:] {
		if failed && nrNum(row, "timestamp") < nrNum(best, "timestamp") {
			best = row
		}
		if !failed && nrNum(row, c.DurationField) > nrNum(best, c.DurationField) {
			best = row
		}
	}
	tid := backend.NRQLString(best["trace.id"])
	if !failed {
		return tid, time.Time{}
	}
	return tid, time.UnixMilli(int64(nrNum(best, "timestamp"))).UTC()
}

func (n *newRelicTraces) Spans(ctx context.Context, failedOnly bool, r window.Range) ([]spanRow, error) {
	c := n.cfg()
	rng := backend.NRQLRange(r.Start, r.End)
	in := fmt.Sprintf("trace.id IN (SELECT uniques(trace.id, %d) FROM Span WHERE %s%s)", nrAttrFailed, c.ErrorFilter, rng)
	where := " WHERE " + in
	if failedOnly {
		where += " AND " + c.ErrorFilter
	} else {
		where = fmt.Sprintf(" WHERE (%s OR trace.id IN (SELECT uniques(trace.id, %d) FROM Span%s))", in, nrAttrSample, rng)
	}
	q := fmt.Sprintf("SELECT trace.id, id, parent.id, %s, %s, %s, timestamp FROM Span%s LIMIT MAX",
		config.NRQLField(c.ServiceField), config.NRQLField(c.NameField), config.NRQLField(c.DurationField), where) + rng
	rows, err := n.api.NRQL(ctx, q)
	if err != nil {
		return nil, err
	}
	scale := n.durScale()
	out := make([]spanRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, spanRow{
			traceID: backend.NRQLString(row["trace.id"]), spanID: backend.NRQLString(row["id"]), parentID: backend.NRQLString(row["parent.id"]),
			svc: backend.NRQLString(row[c.ServiceField]), name: backend.NRQLString(row[c.NameField]),
			dur: nrNum(row, c.DurationField) * scale, start: int64(nrNum(row, "timestamp")) * 1e6,
		})
	}
	return out, nil
}
