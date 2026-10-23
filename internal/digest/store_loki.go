package digest

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/asinha07/spoor/internal/backend"
	"github.com/asinha07/spoor/internal/config"
	"github.com/asinha07/spoor/internal/window"
)

// LokiAPI is the subset of the Loki client the Loki store uses.
type LokiAPI interface {
	Instant(ctx context.Context, q string, at time.Time) ([]backend.Series, error)
	Range(ctx context.Context, q string, start, end time.Time, step time.Duration) ([]backend.Series, error)
	Lines(ctx context.Context, q string, start, end time.Time, limit int, forward bool) ([]backend.LokiEntry, error)
	LabelValues(ctx context.Context, label string, start, end time.Time) ([]string, error)
}

const (
	// lokiSampleLines is how many error lines are grouped in Go when a
	// service has more distinct patterns than Loki returns series.
	lokiSampleLines = 5000
	// lokiExampleLines are read to find an example line per pattern.
	lokiExampleLines = 1000
	// lokiOnsetSteps splits the window to find when each pattern started.
	lokiOnsetSteps = 20
)

// lokiLogs implements LogStore with LogQL. Error lines are normalized into
// patterns on the server (json, then label_format with regexReplaceAll),
// mirroring the VictoriaLogs pipeline, and counted with count_over_time.
type lokiLogs struct {
	e   *Engine
	api LokiAPI
}

// NewLokiStore returns a LogStore backed by Loki.
func NewLokiStore(e *Engine, api LokiAPI) LogStore { return &lokiLogs{e: e, api: api} }

func (l *lokiLogs) cfg() config.LogsConfig { return l.e.cfg.Logs }
func (l *lokiLogs) svcLabel() string       { return config.LokiLabel(l.cfg().ServiceField) }

// selector selects the streams of the given services, or of any service.
func (l *lokiLogs) selector(services []string, all bool) string {
	lbl := l.svcLabel()
	switch {
	case all || len(services) == 0:
		return "{" + lbl + `=~".+"}`
	case len(services) == 1:
		return "{" + lbl + "=" + strconv.Quote(services[0]) + "}"
	}
	qs := make([]string, len(services))
	for i, s := range services {
		qs[i] = regexp.QuoteMeta(s)
	}
	return "{" + lbl + "=~" + strconv.Quote(strings.Join(qs, "|")) + "}"
}

func (l *lokiLogs) errFilter() string { return " " + l.cfg().EffectiveErrorFilter() }

// Built-in normalization steps, applied in order after any JSON message
// extraction. They mirror normalizePipes for LogsQL.
var lokiSteps = [][2]string{
	{`^\s+`, ""},
	{`^([^{\[\r\n][^\r\n]*)[\r\n](?s:.*)`, "$1"},
	{`^(\[[A-Za-z][^\r\n]*)[\r\n](?s:.*)`, "$1"},
	{`\s+`, " "},
	{leadingTime, ""},
}

var (
	uuidPattern = `[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`
	ipPattern   = `\b\d{1,3}(?:\.\d{1,3}){3}\b`
	hexPattern  = `\b0x[0-9a-fA-F]+\b`
	numPattern  = `\d+(?:\.\d+)?`
)

// stages returns the LogQL stages that put each error line's pattern in
// the spoor_msg label.
func (l *lokiLogs) stages() string {
	c := l.cfg()
	if pf := c.PatternField; pf != "" {
		return " | label_format spoor_msg=" + strconv.Quote("{{ ."+config.LokiLabel(pf)+" }}")
	}
	var b strings.Builder
	msg := "__line__"
	var codes []string
	if len(c.MessageFields) > 0 || len(c.CodeFields) > 0 {
		var params, m []string
		for i, f := range c.MessageFields {
			params = append(params, fmt.Sprintf("spoor_m%d=%s", i, strconv.Quote(f)))
			m = append(m, fmt.Sprintf(".spoor_m%d", i))
		}
		for i, f := range c.CodeFields {
			params = append(params, fmt.Sprintf("spoor_c%d=%s", i, strconv.Quote(f)))
			codes = append(codes, fmt.Sprintf(".spoor_c%d", i))
		}
		// Lines that are not JSON keep their text: drop the parser error
		// instead of filtering on it.
		b.WriteString(" | json " + strings.Join(params, ", ") + " | drop __error__, __error_details__")
		if len(m) > 0 {
			msg = "(or " + strings.Join(m, " ") + " __line__)"
		}
	}
	steps := append([][2]string{}, lokiSteps...)
	for _, r := range c.Normalize {
		steps = append(steps, [2]string{r.Regex, r.Replace})
	}
	steps = append(steps, [2]string{uuidPattern, "<UUID>"}, [2]string{ipPattern, "<IP>"})
	if c.IDNormalization() {
		steps = append(steps, [2]string{idPattern, "<ID>"})
	}
	steps = append(steps, [2]string{hexPattern, "<N>"}, [2]string{numPattern, "<N>"})
	expr := msg
	for _, s := range steps {
		expr = fmt.Sprintf("(regexReplaceAll %s %s %s)", strconv.Quote(s[0]), expr, strconv.Quote(s[1]))
	}
	tmpl := "{{ trunc 300 " + expr + " }}"
	if len(codes) > 0 {
		// The code goes in after normalization: TEMPLATE_404 is identity.
		any := "(or " + strings.Join(codes, " ") + ")"
		tmpl = "{{ if " + any + " }}{{ " + any + " }} {{ end }}" + tmpl
	}
	b.WriteString(" | label_format spoor_msg=" + strconv.Quote(tmpl))
	return b.String()
}

// logqlDuration formats d for a LogQL range, rounding up to a second.
func logqlDuration(d time.Duration) string {
	if d < time.Second {
		d = time.Second
	}
	return config.PromDuration(d.Round(time.Second))
}

func (l *lokiLogs) Services(ctx context.Context, r window.Range) ([]string, error) {
	return l.api.LabelValues(ctx, l.svcLabel(), r.Start, r.End)
}

func (l *lokiLogs) Evidence(svc string) string {
	return l.selector([]string{svc}, false) + l.errFilter()
}

func (l *lokiLogs) Patterns(ctx context.Context, services []string, r window.Range, buckets []window.Range) (patternResult, error) {
	res := patternResult{patterns: servicePatterns{}, counts: map[string]errorCounts{}}
	if len(services) == 0 {
		return res, nil
	}
	var notes []string
	var mu sync.Mutex
	// Group all services in one query; if Loki refuses for too many series,
	// one query per service; a service still over the limit is sampled.
	err := l.patternsFor(ctx, services, r, buckets == nil, res.patterns)
	if backend.IsSeriesLimit(err) {
		var wg sync.WaitGroup
		sem := make(chan struct{}, logConcurrency)
		var firstErr error
		for _, svc := range services {
			svc := svc
			wg.Add(1)
			go func() {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				one := servicePatterns{}
				e := l.patternsFor(ctx, []string{svc}, r, buckets == nil, one)
				if backend.IsSeriesLimit(e) {
					e = l.sampledPatterns(ctx, svc, r, one)
					mu.Lock()
					notes = append(notes, svc)
					mu.Unlock()
				}
				mu.Lock()
				defer mu.Unlock()
				if e != nil && firstErr == nil {
					firstErr = e
				}
				for s, p := range one {
					res.patterns[s] = p
				}
			}()
		}
		wg.Wait()
		err = firstErr
	}
	if err != nil {
		return res, err
	}
	for svc, pats := range res.patterns {
		res.patterns[svc] = topPatterns(pats, patternLimit)
	}
	if buckets == nil {
		l.examples(ctx, services, r, res.patterns)
	}
	counts, err := l.errorCounts(ctx, services, r, buckets)
	if err != nil {
		return res, err
	}
	res.counts = counts
	if len(notes) > 0 {
		sort.Strings(notes)
		res.note = fmt.Sprintf("%s: more distinct error patterns than Loki returns in one query; patterns grouped from the latest %d error lines (counts exact)",
			strings.Join(notes, ", "), lokiSampleLines)
	}
	return res, nil
}

// patternsFor counts error lines per (service, pattern) over r. With onset
// set it uses a range query, so each pattern's first appearance is known.
func (l *lokiLogs) patternsFor(ctx context.Context, services []string, r window.Range, onset bool, out servicePatterns) error {
	lbl := l.svcLabel()
	base := l.selector(services, false) + l.errFilter() + l.stages()
	// Exact counts over the whole range.
	q := fmt.Sprintf("sum by (%s, spoor_msg) (count_over_time(%s [%s]))", lbl, base, logqlDuration(r.Duration()))
	series, err := l.api.Instant(ctx, q, r.End)
	if err != nil {
		return err
	}
	for _, s := range series {
		svc, pat := s.Labels[lbl], s.Labels["spoor_msg"]
		if n := scalarOf(s); n > 0 {
			if out[svc] == nil {
				out[svc] = map[string]patternStat{}
			}
			st := out[svc][pat]
			st.n += n
			out[svc][pat] = st
		}
	}
	if !onset {
		return nil
	}
	// When each pattern first appeared: counts per whole-second step,
	// anchored on the window end; the first step is clamped to the window.
	step := (r.Duration() / lokiOnsetSteps).Truncate(time.Second)
	if step < time.Minute {
		step = time.Minute
	}
	k := int(r.Duration() / step)
	if k < 1 {
		k = 1
	}
	start := r.End.Add(-time.Duration(k-1) * step)
	q = fmt.Sprintf("sum by (%s, spoor_msg) (count_over_time(%s [%s]))", lbl, base, logqlDuration(step))
	series, err = l.api.Range(ctx, q, start, r.End, step)
	if err != nil {
		return nil // onset is a detail; counts are already exact
	}
	for _, s := range series {
		svc, pat := s.Labels[lbl], s.Labels["spoor_msg"]
		st, ok := out[svc][pat]
		if !ok {
			continue
		}
		for _, p := range s.Points {
			if p.V <= 0 {
				continue
			}
			t := p.T.Add(-step)
			if t.Before(r.Start) {
				t = r.Start
			}
			if st.first.IsZero() || t.Before(st.first) {
				st.first = t
			}
		}
		out[svc][pat] = st
	}
	return nil
}

// sampledPatterns groups the latest error lines of one service in Go, for
// a service with more distinct patterns than Loki returns series. The
// patterns still come from the server-side normalization (spoor_msg).
func (l *lokiLogs) sampledPatterns(ctx context.Context, svc string, r window.Range, out servicePatterns) error {
	q := l.selector([]string{svc}, false) + l.errFilter() + l.stages()
	lines, err := l.api.Lines(ctx, q, r.Start, r.End, lokiSampleLines, false)
	if err != nil {
		return err
	}
	pats := map[string]patternStat{}
	for _, e := range lines {
		pat := e.Labels["spoor_msg"]
		st := pats[pat]
		st.n++
		if st.first.IsZero() || e.Time.Before(st.first) {
			st.first = e.Time
		}
		if st.example == "" {
			st.example = e.Line
		}
		pats[pat] = st
	}
	out[svc] = pats
	return nil
}

// examples fills in one raw line per pattern from the latest error lines.
// Rare patterns may have none; findings then have no example.
func (l *lokiLogs) examples(ctx context.Context, services []string, r window.Range, pats servicePatterns) {
	q := l.selector(services, false) + l.errFilter() + l.stages()
	lines, err := l.api.Lines(ctx, q, r.Start, r.End, lokiExampleLines, false)
	if err != nil {
		return
	}
	lbl := l.svcLabel()
	for _, e := range lines {
		svc, pat := e.Labels[lbl], e.Labels["spoor_msg"]
		if st, ok := pats[svc][pat]; ok && st.example == "" {
			st.example = e.Line
			pats[svc][pat] = st
		}
	}
}

// errorCounts returns each service's exact error-line count over r and per
// bucket.
func (l *lokiLogs) errorCounts(ctx context.Context, services []string, r window.Range, buckets []window.Range) (map[string]errorCounts, error) {
	lbl := l.svcLabel()
	sel := l.selector(services, false) + l.errFilter()
	out := map[string]errorCounts{}
	get := func(svc string) errorCounts {
		c, ok := out[svc]
		if !ok {
			c = errorCounts{buckets: make([]float64, len(buckets))}
		}
		return c
	}
	series, err := l.api.Instant(ctx, fmt.Sprintf("sum by (%s) (count_over_time(%s [%s]))", lbl, sel, logqlDuration(r.Duration())), r.End)
	if err != nil {
		return nil, err
	}
	for _, s := range series {
		c := get(s.Labels[lbl])
		c.total = scalarOf(s)
		out[s.Labels[lbl]] = c
	}
	if len(buckets) == 0 {
		return out, nil
	}
	per, err := l.bucketed(ctx, fmt.Sprintf("sum by (%s) (count_over_time(%s [%%s]))", lbl, sel), buckets)
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

// bucketed evaluates a "sum by (service) (count_over_time(... [%s]))"
// template once per bucket: one range query when the buckets are
// contiguous and equal, else one instant query each.
func (l *lokiLogs) bucketed(ctx context.Context, tmpl string, buckets []window.Range) (map[string][]float64, error) {
	lbl := l.svcLabel()
	out := map[string][]float64{}
	add := func(svc string, i int, v float64) {
		if out[svc] == nil {
			out[svc] = make([]float64, len(buckets))
		}
		out[svc][i] += v
	}
	step := buckets[0].Duration()
	regular := true
	for i := 1; i < len(buckets); i++ {
		regular = regular && buckets[i].Start.Equal(buckets[i-1].End) && buckets[i].Duration() == step
	}
	q := fmt.Sprintf(tmpl, logqlDuration(step))
	if regular {
		series, err := l.api.Range(ctx, q, buckets[0].End, buckets[len(buckets)-1].End, step)
		if err != nil {
			return nil, err
		}
		for _, s := range series {
			for _, p := range s.Points {
				i := int(math.Round(float64(p.T.Sub(buckets[0].End)) / float64(step)))
				if i >= 0 && i < len(buckets) {
					add(s.Labels[lbl], i, p.V)
				}
			}
		}
		return out, nil
	}
	for i, b := range buckets {
		series, err := l.api.Instant(ctx, fmt.Sprintf(tmpl, logqlDuration(b.Duration())), b.End)
		if err != nil {
			return nil, err
		}
		for _, s := range series {
			add(s.Labels[lbl], i, scalarOf(s))
		}
	}
	return out, nil
}

func (l *lokiLogs) Totals(ctx context.Context, services []string, all bool, r window.Range, step time.Duration, n int) (map[string][]volume, error) {
	lbl := l.svcLabel()
	sel := l.selector(services, all)
	out := map[string][]volume{}
	put := func(svc string, i int, total, uncl float64) {
		if out[svc] == nil {
			out[svc] = make([]volume, n)
		}
		out[svc][i].total += total
		out[svc][i].unclassified += uncl
	}
	queries := []string{sel}
	if u := l.cfg().UnclassifiedFilter(); u != "" {
		queries = append(queries, sel+" "+u)
	}
	for qi, s := range queries {
		var buckets []window.Range
		if step == 0 {
			buckets = []window.Range{r}
		} else {
			for i := 0; i < n; i++ {
				buckets = append(buckets, window.Range{Start: r.Start.Add(time.Duration(i) * step), End: r.Start.Add(time.Duration(i+1) * step)})
			}
		}
		per, err := l.bucketed(ctx, fmt.Sprintf("sum by (%s) (count_over_time(%s [%%s]))", lbl, s), buckets)
		if err != nil {
			return nil, err
		}
		for svc, vals := range per {
			for i, v := range vals {
				if qi == 0 {
					put(svc, i, v, 0)
				} else {
					put(svc, i, 0, v)
				}
			}
		}
	}
	return out, nil
}

func (l *lokiLogs) CountFamilies(ctx context.Context, services []string, r window.Range, checks []familyCheck) ([]float64, error) {
	out := make([]float64, len(checks))
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	sem := make(chan struct{}, logConcurrency)
	for i, c := range checks {
		i, c := i, c
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			q := l.selector([]string{c.svc}, false) + l.errFilter()
			for _, w := range c.words {
				q += " |~ " + strconv.Quote("(?i)"+regexp.QuoteMeta(w))
			}
			series, err := l.api.Instant(ctx, fmt.Sprintf("sum(count_over_time(%s [%s]))", q, logqlDuration(r.Duration())), r.End)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			if len(series) > 0 {
				out[i] = scalarOf(series[0])
			}
		}()
	}
	wg.Wait()
	return out, firstErr
}

// CountLines is not available on Loki: every query needs a stream selector,
// so lines without a service label cannot be counted.
func (l *lokiLogs) CountLines(context.Context, window.Range) (float64, error) { return 0, nil }

func scalarOf(s backend.Series) float64 {
	if len(s.Points) == 0 {
		return 0
	}
	return s.Points[len(s.Points)-1].V
}

// topPatterns keeps the n most frequent patterns, ties broken by pattern.
func topPatterns(p map[string]patternStat, n int) map[string]patternStat {
	if len(p) <= n {
		return p
	}
	keys := make([]string, 0, len(p))
	for k := range p {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if p[keys[i]].n != p[keys[j]].n {
			return p[keys[i]].n > p[keys[j]].n
		}
		return keys[i] < keys[j]
	})
	out := make(map[string]patternStat, n)
	for _, k := range keys[:n] {
		out[k] = p[k]
	}
	return out
}
