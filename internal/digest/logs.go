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
	"github.com/asinha07/spoor/internal/window"
)

const (
	patternLimit   = 200 // patterns kept per service and range
	maxPatternLen  = 140
	maxExampleLen  = 200
	spikeMinRatio  = 4.0
	logSpikeMinZ   = 3.0
	newPatternBase = 0.5
	spikeMaxP      = 1e-3 // family-wise false-alarm rate for spike findings per call
	maxSteady      = 3    // steady services listed per response
	maxVerify      = 30   // new-family candidates checked against the full baseline per call
	logConcurrency = 4    // log queries in flight per call
	newMinLines    = 10   // a new error needs this many lines...
	newMinShare    = 0.05 // ...or this share of the service's error lines
	familyWords    = 8    // words that identify an error family
	// Error volume: a service's error lines per window against the baseline.
	volumeMinRatio = 1.3
	volumeRelFloor = 0.15 // relative noise floor: z=3 needs about +45%
	volumeAbsFloor = 5.0  // lines
	shareAbsFloor  = 0.0005
	// steadyMaxRatio and steadyMinP decide "steady" when no volume data is
	// available and Spoor falls back to summing patterns.
	steadyMaxRatio = 2.0
	steadyMinP     = 0.01
	// idPattern matches tokens that mix letters and digits, such as order,
	// cart, product or session IDs (ord_7f3k2m9q, OLJCESPC7Z, ord_99a1). Pure
	// numbers, UUIDs and IPs are left to collapse_nums.
	idPattern = `\b(?:[A-Za-z_]+[0-9]+[A-Za-z0-9_]+|[0-9]+[A-Za-z_]+[0-9][A-Za-z0-9_]*)\b`
	// leadingTime strips a timestamp that starts a plain-text line.
	leadingTime = `^\[?\d{4}-\d{2}-\d{2}[T ][0-9:.,]+(Z|[+-]\d{2}:?\d{2})?\]?\s*`
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

// logScope selects the log lines of the given services. With all set, it
// selects every line, so lines without a service field can be counted.
func (e *Engine) logScope(services []string, all bool) string {
	f := e.cfg.Logs.ServiceField
	switch {
	case all:
		return "*"
	case len(services) == 1:
		return backend.Eq(f, services[0])
	}
	qs := make([]string, len(services))
	for i, s := range services {
		qs[i] = strconv.Quote(s)
	}
	return backend.Quote(f) + ":in(" + strings.Join(qs, ",") + ")"
}

// logFilter is the evidence filter for one service's error lines.
func (e *Engine) logFilter(svc string) string {
	return backend.Eq(e.cfg.Logs.ServiceField, svc) + " " + e.cfg.Logs.EffectiveErrorFilter()
}

// normalizePipes turn an error message into its pattern. Raw lines are never
// changed; this runs at query time.
//
//  1. JSON messages: the first configured message field found (and code
//     field, if any) becomes the text, so pretty-printed JSON errors keep
//     their identity instead of collapsing to "{".
//  2. Plain text: first line only, so stack traces do not split one error
//     into many. JSON (starting with { or a JSON array) is never cut.
//  3. Whitespace collapsed, a leading timestamp dropped, user rules,
//     letter-digit IDs, then numbers, UUIDs and IPs via collapse_nums.
func (e *Engine) normalizePipes() string {
	var b strings.Builder
	l := e.cfg.Logs
	if fields := append(append([]string{}, l.MessageFields...), l.CodeFields...); len(fields) > 0 {
		q := make([]string, len(fields))
		for i, f := range fields {
			q[i] = backend.Quote(f)
		}
		fmt.Fprintf(&b, ` | unpack_json from _msg fields (%s) result_prefix "spoor_j_"`, strings.Join(q, ", "))
		// format pipes run in order and later ones win, so apply the
		// highest-priority field last.
		for i := len(l.MessageFields) - 1; i >= 0; i-- {
			f := "spoor_j_" + l.MessageFields[i]
			fmt.Fprintf(&b, ` | format if (%s:*) "<%s>" as spoor_m`, backend.Quote(f), f)
		}
		for i := len(l.CodeFields) - 1; i >= 0; i-- {
			f := "spoor_j_" + l.CodeFields[i]
			fmt.Fprintf(&b, ` | format if (%s:*) "<%s> " as spoor_c`, backend.Quote(f), f)
		}
		b.WriteString(` | format if (spoor_m:*) "<spoor_m>" as _msg`)
	}
	b.WriteString(` | replace_regexp ("^\\s+", "")`)
	b.WriteString(` | replace_regexp ("^([^{\\[\\r\\n][^\\r\\n]*)[\\r\\n](?s:.*)", "$1")`)
	b.WriteString(` | replace_regexp ("^(\\[[A-Za-z][^\\r\\n]*)[\\r\\n](?s:.*)", "$1")`)
	b.WriteString(` | replace_regexp ("\\s+", " ")`)
	fmt.Fprintf(&b, ` | replace_regexp (%s, "")`, strconv.Quote(leadingTime))
	for _, r := range e.cfg.Logs.Normalize {
		fmt.Fprintf(&b, " | replace_regexp (%s, %s)", strconv.Quote(r.Regex), strconv.Quote(r.Replace))
	}
	if e.cfg.Logs.IDNormalization() {
		fmt.Fprintf(&b, ` | replace_regexp (%s, "<ID>")`, strconv.Quote(idPattern))
	}
	b.WriteString(" | collapse_nums prettify")
	if len(l.CodeFields) > 0 {
		// The code goes in after normalization: codes like TEMPLATE_404 are
		// the error's identity, not IDs to collapse.
		b.WriteString(` | format if (spoor_c:*) "<spoor_c><_msg>" as _msg`)
	}
	return b.String()
}

// patternQuery groups error lines into patterns per service, keeping the
// top patternLimit of each service. It also returns, on every row, the
// service's exact error-line total (spoor_etotal) and, for each bucket, the
// service's error lines in it (spoor_e0, spoor_e1, ...), computed before the
// limit. That way the error filter, the expensive part with text rules, runs
// once per range: no separate error-count query is needed.
func (e *Engine) patternQuery(scope string, buckets []window.Range) string {
	q := scope + " " + e.cfg.Logs.EffectiveErrorFilter() +
		" | copy " + backend.Quote(e.cfg.Logs.ServiceField) + " as spoor_svc | copy _msg as raw"
	if pf := e.cfg.Logs.PatternField; pf != "" {
		q += " | copy " + backend.Quote(pf) + " as _msg"
	} else {
		q += e.normalizePipes()
	}
	q += " | stats by (spoor_svc, _msg) count() n"
	totals := []string{"sum(n) spoor_etotal"}
	for i, b := range buckets {
		q += fmt.Sprintf(", count() if (_time:[%s, %s)) b%d", b.Start.UTC().Format(time.RFC3339Nano), b.End.UTC().Format(time.RFC3339Nano), i)
		totals = append(totals, fmt.Sprintf("sum(b%d) spoor_e%d", i, i))
	}
	// The pattern breaks ties at the limit, so the same patterns are kept on
	// every run.
	return q + ", min(_time) first, any(raw) example | total_stats by (spoor_svc) " + strings.Join(totals, ", ") +
		fmt.Sprintf(" | sort by (n desc, _msg) partition by (spoor_svc) limit %d", patternLimit)
}

type patternStat struct {
	n       float64
	first   time.Time
	example string
}

// servicePatterns maps service -> pattern -> stats.
type servicePatterns map[string]map[string]patternStat

// errorCounts holds a service's exact error-line count over a range, and per
// bucket when buckets were requested.
type errorCounts struct {
	total   float64
	buckets []float64
}

func (e *Engine) patternsIn(ctx context.Context, src LogsSource, scope string, r window.Range, buckets []window.Range) (servicePatterns, map[string]errorCounts, error) {
	rows, err := src.Query(ctx, e.patternQuery(scope, buckets), r.Start, r.End, 0)
	if err != nil {
		return nil, nil, err
	}
	out := servicePatterns{}
	counts := map[string]errorCounts{}
	for _, row := range rows {
		svc := row["spoor_svc"]
		if out[svc] == nil {
			out[svc] = map[string]patternStat{}
			c := errorCounts{total: row.Float("spoor_etotal"), buckets: make([]float64, len(buckets))}
			for i := range buckets {
				c.buckets[i] = row.Float(fmt.Sprintf("spoor_e%d", i))
			}
			counts[svc] = c
		}
		pat := row["_msg"]
		if pat == "" && e.cfg.Logs.PatternField != "" {
			pat = "(no " + e.cfg.Logs.PatternField + ")"
		}
		out[svc][pat] = patternStat{n: row.Float("n"), first: row.Time("first"), example: row["example"]}
	}
	return out, counts, nil
}

// volume holds log line counts for one service.
type volume struct {
	total, errors, unclassified float64
}

// totalsQuery counts all lines per service and, when severity fields are
// configured, lines with none of them, in buckets of step aligned to start
// (or over the whole range when step is 0). It has no error filter, so it is
// cheap even when the error filter matches message text.
func (e *Engine) totalsQuery(scope string, step time.Duration, start time.Time) string {
	by := "spoor_svc"
	if step > 0 {
		ms := step.Milliseconds()
		// VictoriaLogs shifts bucket boundaries back by the offset, so a
		// boundary at start (start mod step past the epoch grid) needs an
		// offset of step minus that.
		by = fmt.Sprintf("_time:%dms offset %dms, spoor_svc", ms, (ms-((start.UnixMilli()%ms)+ms)%ms)%ms)
	}
	q := scope + " | copy " + backend.Quote(e.cfg.Logs.ServiceField) + " as spoor_svc | stats by (" + by + ") count() total"
	if u := e.cfg.Logs.UnclassifiedFilter(); u != "" {
		q += ", count() if (" + u + ") unclassified"
	}
	return q
}

// totals runs totalsQuery over r split into n buckets of step (n=1, step=0:
// the whole range) and returns per-service counts per bucket.
func (e *Engine) totals(ctx context.Context, src LogsSource, scope string, r window.Range, step time.Duration, n int) (map[string][]volume, error) {
	rows, err := src.Query(ctx, e.totalsQuery(scope, step, r.Start), r.Start, r.End, 0)
	if err != nil {
		return nil, err
	}
	out := map[string][]volume{}
	for _, row := range rows {
		i := 0
		if step > 0 {
			i = int(math.Floor(float64(row.Time("_time").Sub(r.Start)) / float64(step)))
		}
		if i < 0 || i >= n {
			continue
		}
		svc := row["spoor_svc"]
		if out[svc] == nil {
			out[svc] = make([]volume, n)
		}
		out[svc][i].total += row.Float("total")
		out[svc][i].unclassified += row.Float("unclassified")
	}
	return out, nil
}

// lineTotals fetches all-line counts for the window and each baseline
// sub-window: one bucketed query when the baseline is contiguous with the
// window (baseline=previous), two otherwise. Error counts come from the
// pattern queries.
func (e *Engine) lineTotals(ctx context.Context, services []string, all bool, spec window.Spec, run func(func() error)) (wait func() (map[string]volume, map[string][]volume, error)) {
	subs := spec.SubWindows
	k := len(subs)
	step := subs[0].End.Sub(subs[0].Start)
	var full, base, win map[string][]volume
	var errFull, errBase, errWin error
	contiguous := subs[k-1].End.Equal(spec.Window.Start) && spec.Window.Duration() == step
	if contiguous {
		run(func() error {
			full, errFull = e.logs.Totals(ctx, services, all, window.Range{Start: subs[0].Start, End: spec.Window.End}, step, k+1)
			return nil
		})
	} else {
		run(func() error {
			base, errBase = e.logs.Totals(ctx, services, all, window.Range{Start: subs[0].Start, End: subs[k-1].End}, step, k)
			return nil
		})
		run(func() error { win, errWin = e.logs.Totals(ctx, services, all, spec.Window, 0, 1); return nil })
	}
	return func() (map[string]volume, map[string][]volume, error) {
		for _, err := range []error{errFull, errBase, errWin} {
			if err != nil {
				return nil, nil, err
			}
		}
		w, b := map[string]volume{}, map[string][]volume{}
		if contiguous {
			for svc, vs := range full {
				w[svc] = vs[k]
				b[svc] = vs[:k]
			}
			return w, b, nil
		}
		for svc, vs := range win {
			w[svc] = vs[0]
		}
		return w, base, nil
	}
}

// family is the coarse identity of an error: its first line with every
// variable part removed (numbers, IDs, quoted values, key=value values),
// lowercased, first familyWords words. Two messages that differ only in an
// ID, a quoted name or a stack-trace detail share a family.
var (
	familyQuoted = regexp.MustCompile(`"[^"]*"|'[^']*'|` + "`[^`]*`")
	familyKV     = regexp.MustCompile(`=\S+`)
	familyPlace  = regexp.MustCompile(`<[A-Za-z_]+>`)
	familyWord   = regexp.MustCompile(`[A-Za-z][A-Za-z0-9_.-]*`)
	// familyBracket finds bracketed log-prefix segments: [ERROR],
	// [http-nio-8080-exec-1], [correlationId:...], [Foo.java:88].
	familyBracket = regexp.MustCompile(`\[[^\[\]]*\]`)
	familyLineNo  = regexp.MustCompile(`:(<N>|\d+)$`)
	levelWords    = map[string]bool{"error": true, "err": true, "warn": true, "warning": true, "info": true, "debug": true, "trace": true, "fatal": true, "critical": true, "crit": true, "severe": true, "panic": true}
)

// familyPrefix removes log-prefix noise from bracketed segments: segments
// that are a level word or hold a variable value (thread names, correlation
// IDs) are dropped; a class name keeps its name but loses its line number.
func familyPrefix(p string) string {
	return familyBracket.ReplaceAllStringFunc(p, func(seg string) string {
		inner := strings.TrimSpace(seg[1 : len(seg)-1])
		inner = familyLineNo.ReplaceAllString(inner, "")
		if levelWords[strings.ToLower(inner)] || strings.ContainsAny(inner, "0123456789<:=") || inner == "" {
			return " "
		}
		return " " + inner + " "
	})
}

func family(pattern string) string {
	if i := strings.IndexAny(pattern, "\r\n"); i >= 0 {
		pattern = pattern[:i]
	}
	p := familyPrefix(pattern)
	p = familyQuoted.ReplaceAllString(p, " ")
	p = familyKV.ReplaceAllString(p, "=")
	p = familyPlace.ReplaceAllString(p, " ")
	words := make([]string, 0, familyWords)
	for i, w := range familyWord.FindAllString(p, -1) {
		if strings.ContainsAny(w, "0123456789") || (i == 0 && levelWords[strings.ToLower(w)]) {
			continue
		}
		words = append(words, strings.ToLower(strings.Trim(w, ".-_")))
		if len(words) == familyWords {
			break
		}
	}
	if len(words) == 0 {
		return strings.ToLower(strings.TrimSpace(pattern))
	}
	return strings.Join(words, " ")
}

// familyStat aggregates the patterns of one family in one service.
type familyStat struct {
	n        float64
	first    time.Time
	top      string // most frequent pattern, used in titles
	topN     float64
	example  string
	variants int
}

func families(pats map[string]patternStat) map[string]*familyStat {
	out := map[string]*familyStat{}
	keys := make([]string, 0, len(pats))
	for p := range pats {
		keys = append(keys, p)
	}
	sort.Strings(keys)
	for _, p := range keys {
		s := pats[p]
		k := family(p)
		f := out[k]
		if f == nil {
			f = &familyStat{first: s.first}
			out[k] = f
		}
		f.n += s.n
		f.variants++
		if !s.first.IsZero() && (f.first.IsZero() || s.first.Before(f.first)) {
			f.first = s.first
		}
		if s.n > f.topN {
			f.top, f.topN, f.example = p, s.n, s.example
		}
	}
	return out
}

// verifyWords picks distinctive words of a family for a word filter: the
// longest ones, at most four.
func verifyWords(fam string) []string {
	ws := strings.Fields(fam)
	sort.SliceStable(ws, func(i, j int) bool { return len(ws[i]) > len(ws[j]) })
	out := []string{}
	for _, w := range ws {
		if len(w) >= 4 && len(out) < 4 {
			out = append(out, w)
		}
	}
	return out
}

// logResult is everything the log scorer reports for one call.
type logResult struct {
	findings []Finding
	steady   []SteadyError
	notes    map[string]string // extra coverage entries
	state    string            // replaces coverage["logs"] when not "ok"
	summary  string            // sentence appended to the summary
}

// scoreLogs scores error logs for all services at once, in a handful of
// queries whatever the number of services:
//
//  1. error patterns in the window (top patterns per service),
//  2. error patterns in the baseline,
//  3. line counts in the window (all, error, unclassified),
//  4. line counts per baseline sub-window,
//  5. only when a service's baseline patterns hit the cap: a direct count
//     for each "new" error family, so a family that ranked below the cap in
//     the baseline is not called new.
//
// Patterns are compared at family level, so an error whose exact text varies
// by ID is one error, and a new variant of an existing error is not "new".
func (e *Engine) scoreLogs(ctx context.Context, services []string, all bool, spec window.Spec) (logResult, error) {
	res := logResult{notes: map[string]string{}}
	if len(services) == 0 {
		if all {
			e.noServiceField(ctx, &res, spec.Window)
		}
		return res, nil
	}
	inScope := map[string]bool{}
	for _, s := range services {
		inScope[s] = true
	}
	hist := window.Range{Start: spec.SubWindows[0].Start, End: spec.Baseline.End}
	k := float64(len(spec.SubWindows))
	minCount := float64(e.cfg.Scoring.MinPatternCount)

	// All queries run in parallel (four at a time): wall time is that of the
	// slowest one, not the sum.
	var wg sync.WaitGroup
	sem := make(chan struct{}, logConcurrency)
	run := func(f func() error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			_ = f()
		}()
	}
	var curRes, baseRes patternResult
	var curErr, baseErr error
	run(func() error { curRes, curErr = e.logs.Patterns(ctx, services, spec.Window, nil); return nil })
	run(func() error { baseRes, baseErr = e.logs.Patterns(ctx, services, hist, spec.SubWindows); return nil })
	totalsDone := e.lineTotals(ctx, services, all, spec, run)
	wg.Wait()
	if curErr != nil {
		return res, curErr
	}
	if baseErr != nil {
		return res, baseErr
	}
	cur, curErrs := curRes.patterns, curRes.counts
	base, baseErrs := baseRes.patterns, baseRes.counts
	if n := firstNonEmpty(curRes.note, baseRes.note); n != "" {
		res.notes["logs_sample"] = n
	}
	// Line totals are context: if they fail, patterns still answer.
	winVol, baseVol, totalsErr := totalsDone()
	if totalsErr != nil {
		res.notes["logs_volume"] = "line totals unavailable, so error share and blind spots are not checked: " + truncate(totalsErr.Error(), 120)
		winVol, baseVol = map[string]volume{}, map[string][]volume{}
	}
	// Exact error counts from the pattern queries.
	k0 := len(spec.SubWindows)
	for _, svc := range services {
		w := winVol[svc]
		w.errors = curErrs[svc].total
		winVol[svc] = w
		if baseVol[svc] == nil {
			baseVol[svc] = make([]volume, k0)
		}
		for i, n := range baseErrs[svc].buckets {
			baseVol[svc][i].errors = n
		}
	}
	svcErrors := func(svc string) float64 { return winVol[svc].errors }

	type candidate struct {
		svc, fam string
		f        *familyStat
		words    []string
	}
	var verify []candidate
	var capped []string
	verified, skipped := 0, 0
	svcs := make([]string, 0, len(cur))
	for s := range cur {
		if inScope[s] {
			svcs = append(svcs, s)
		}
	}
	sort.Strings(svcs)

	hasFinding := map[string]bool{}
	for _, svc := range svcs {
		curF, baseF := families(cur[svc]), families(base[svc])
		baseCapped := len(base[svc]) >= patternLimit
		if len(cur[svc]) >= patternLimit || baseCapped {
			capped = append(capped, svc)
		}
		keys := make([]string, 0, len(curF))
		for fam := range curF {
			keys = append(keys, fam)
		}
		sort.Strings(keys)
		for _, fam := range keys {
			f := curF[fam]
			if f.n < minCount {
				continue
			}
			b := baseF[fam]
			switch {
			case b != nil && b.n > 0:
				if fd, ok := e.spikeFinding(svc, f, b.n/k, len(keys), spec); ok {
					res.findings = append(res.findings, fd)
					hasFinding[svc] = true
				}
			case baseCapped:
				// The family may exist in the baseline below the cap.
				verify = append(verify, candidate{svc, fam, f, verifyWords(fam)})
			default:
				if fd, ok := e.newFinding(svc, f, svcErrors(svc), spec); ok {
					res.findings = append(res.findings, fd)
					hasFinding[svc] = true
				}
			}
		}
	}

	if len(verify) > 0 {
		sort.SliceStable(verify, func(i, j int) bool { return verify[i].f.n > verify[j].f.n })
		if len(verify) > maxVerify {
			skipped = len(verify) - maxVerify
			verify = verify[:maxVerify]
		}
		verified = len(verify)
		checks := make([]familyCheck, len(verify))
		for i, c := range verify {
			checks[i] = familyCheck{svc: c.svc, words: c.words}
		}
		counts, err := e.logs.CountFamilies(ctx, services, hist, checks)
		if err != nil {
			return res, err
		}
		for i, c := range verify {
			seen := counts[i]
			var fd Finding
			var ok bool
			if seen > 0 && len(c.words) > 0 {
				fd, ok = e.spikeFinding(c.svc, c.f, seen/k, len(verify), spec)
			} else {
				fd, ok = e.newFinding(c.svc, c.f, svcErrors(c.svc), spec)
			}
			if ok {
				res.findings = append(res.findings, fd)
				hasFinding[c.svc] = true
			}
		}
	}
	if len(capped) > 0 {
		note := fmt.Sprintf("more than %d distinct error patterns in %s; ", patternLimit, strings.Join(capped, ", "))
		switch {
		case verified == 0:
			note += "no possible new error needed checking against the full baseline"
		case skipped > 0:
			note += fmt.Sprintf("%d possible new errors checked against the full baseline, %d more not checked", verified, skipped)
		default:
			note += fmt.Sprintf("%d possible new errors checked against the full baseline", verified)
		}
		res.notes["logs_patterns"] = note
	}

	// Error volume and steady errors, per service.
	var steady []SteadyError
	for _, svc := range services {
		if hasFinding[svc] {
			continue
		}
		fd, st, ok := e.volumeVerdict(svc, winVol[svc], baseVol[svc], cur[svc], spec)
		if ok {
			res.findings = append(res.findings, fd)
		} else if st != nil {
			steady = append(steady, *st)
		}
	}
	res.steady = steady

	if totalsErr == nil {
		e.logCoverage(&res, services, all, winVol)
	}
	return res, nil
}

func (e *Engine) newFinding(svc string, f *familyStat, svcErrors float64, spec window.Spec) (Finding, bool) {
	// A handful of lines of a new error in a service that logs hundreds of
	// errors per window is noise ("password reset link expired" four times): it must be
	// either newMinLines lines or newMinShare of the service's errors.
	if f.n < newMinLines && f.n < newMinShare*svcErrors {
		return Finding{}, false
	}
	// A new error family is noteworthy on its own; volume raises it further.
	score := newPatternBase + 0.4*(1-1/(1+f.n/20))
	if score < e.cfg.Scoring.ReportThreshold {
		return Finding{}, false
	}
	fd := e.logFinding(svc, KindNewLogPattern, score, f, spec)
	fd.Before = "0"
	fd.Title = fmt.Sprintf("New error log pattern: %q%s", truncate(f.top, maxPatternLen), variantNote(f))
	return fd, true
}

func (e *Engine) spikeFinding(svc string, f *familyStat, expected float64, checked int, spec window.Spec) (Finding, bool) {
	ratio := (f.n + 1) / (expected + 1)
	z := (f.n - expected) / math.Sqrt(expected+1)
	if ratio < spikeMinRatio || z < logSpikeMinZ {
		return Finding{}, false
	}
	// Many families are checked per window, so a chance blip in one of them
	// (a background error seen 7 times instead of once) must not count.
	// Require the Poisson tail probability, corrected for the number of
	// families checked, to be small.
	if poissonSF(f.n, math.Max(expected, 1))*float64(max(checked, 1)) > spikeMaxP {
		return Finding{}, false
	}
	score := zScore(z)
	if score < e.cfg.Scoring.ReportThreshold {
		return Finding{}, false
	}
	fd := e.logFinding(svc, KindSpikingLogPattern, score, f, spec)
	fd.Before = fmt.Sprintf("%s per window", trimFloat(expected))
	fd.Title = fmt.Sprintf("Error log pattern spiking: %q%s", truncate(f.top, maxPatternLen), variantNote(f))
	return fd, true
}

func variantNote(f *familyStat) string {
	if f.variants > 1 {
		return fmt.Sprintf(" (+%d variants)", f.variants-1)
	}
	return ""
}

func (e *Engine) logFinding(svc, kind string, score float64, f *familyStat, spec window.Spec) Finding {
	hist := window.Range{Start: spec.SubWindows[0].Start, End: spec.Baseline.End}
	fd := Finding{
		Service:      svc,
		Signal:       SignalLogs,
		Kind:         kind,
		Score:        round2(score),
		After:        fmt.Sprintf("%.0f in window", f.n),
		Example:      Redact(f.example, maxExampleLen),
		EvidenceLink: e.logsLink(e.logs.Evidence(svc), hist.Start, spec.Window.End),
	}
	fd.onset = f.first
	fd.Onset = fmtTime(f.first)
	return fd
}

// volumeVerdict compares a service's error lines with its baseline
// sub-windows. A clear rise is a finding; otherwise the service is steady
// and the change is reported with it. When total line counts are known, the
// error share of all lines must rise too, so more traffic alone is not
// reported as more errors.
func (e *Engine) volumeVerdict(svc string, now volume, hist []volume, pats map[string]patternStat, spec window.Spec) (Finding, *SteadyError, bool) {
	minCount := float64(e.cfg.Scoring.MinPatternCount)
	if now.errors < minCount || len(hist) == 0 {
		return Finding{}, nil, false
	}
	errs := make([]float64, len(hist))
	totals := make([]float64, len(hist))
	var shares []float64
	for i, v := range hist {
		errs[i], totals[i] = v.errors, v.total
		if v.total > 0 {
			shares = append(shares, v.errors/v.total)
		}
	}
	zErr, usual := robustZ(now.errors, errs, volumeRelFloor, volumeAbsFloor)
	ratio := (now.errors + 1) / (usual + 1)
	z := zErr
	shareKnown := now.total > 0 && len(shares) >= 2
	var zShare, shareNow, shareUsual float64
	if shareKnown {
		shareNow = now.errors / now.total
		zShare, shareUsual = robustZ(shareNow, shares, volumeRelFloor, shareAbsFloor)
		z = math.Min(zErr, zShare)
	}
	top := ""
	topN := -1.0
	for p, s := range pats {
		if s.n > topN || (s.n == topN && p < top) {
			top, topN = p, s.n
		}
	}
	if usual > 0 && ratio >= volumeMinRatio && z >= 3 && poissonSF(now.errors, math.Max(usual, 1)) < spikeMaxP {
		score := zScore(z)
		if score >= e.cfg.Scoring.ReportThreshold {
			hist := window.Range{Start: spec.SubWindows[0].Start, End: spec.Baseline.End}
			title := fmt.Sprintf("Error log volume up %s: %s → %s lines per window", pctChange(now.errors, usual), fmtCount(usual), fmtCount(now.errors))
			if shareKnown {
				title += fmt.Sprintf(" (share of log lines %.2f%% → %.2f%%)", 100*shareUsual, 100*shareNow)
			}
			if top != "" {
				title += fmt.Sprintf("; most common: %q", truncate(top, maxPatternLen))
			}
			return Finding{
				Service: svc, Signal: SignalLogs, Kind: KindErrorVolumeUp, Score: round2(score),
				Title: title, Before: fmtCount(usual) + " per window", After: fmtCount(now.errors) + " in window",
				EvidenceLink: e.logsLink(e.logs.Evidence(svc), hist.Start, spec.Window.End),
			}, nil, true
		}
	}
	st := &SteadyError{
		Service:     svc,
		ErrorsNow:   math.Round(now.errors),
		ErrorsUsual: math.Round(usual*10) / 10,
		Change:      pctChange(now.errors, usual),
		Example:     Redact(truncate(top, maxPatternLen), maxPatternLen),
	}
	if med := median(totals); now.total > 0 && med > 0 {
		st.LinesChange = pctChange(now.total, med)
	}
	return Finding{}, st, false
}

func pctChange(now, usual float64) string {
	if usual <= 0 {
		return "new"
	}
	p := math.Round(100*(now-usual)/usual) + 0 // + 0 turns -0 into 0
	if p >= 0 {
		return fmt.Sprintf("+%.0f%%", p)
	}
	return fmt.Sprintf("%.0f%%", p)
}

// steadyFromPatterns is the fallback when line counts are unavailable: it
// sums the (capped) patterns instead.
func steadyFromPatterns(svc string, cur, base map[string]patternStat, k, minCount float64) *SteadyError {
	if k <= 0 {
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
	if (now+1)/(usual+1) > steadyMaxRatio && poissonSF(now, math.Max(usual, 1)) < steadyMinP {
		return nil
	}
	return &SteadyError{
		Service:     svc,
		ErrorsNow:   math.Round(now),
		ErrorsUsual: math.Round(usual*10) / 10,
		Change:      pctChange(now, usual),
		Example:     Redact(truncate(top, maxPatternLen), maxPatternLen),
	}
}

// logCoverage turns window line counts into coverage notes, so a filter that
// matches nothing, lines without a severity field, or lines without a
// service field are visible instead of looking like "no change".
func (e *Engine) logCoverage(res *logResult, services []string, all bool, win map[string]volume) {
	var total, errs, uncl float64
	for _, s := range services {
		v := win[s]
		total += v.total
		errs += v.errors
		uncl += v.unclassified
	}
	unattributed := 0.0
	if all {
		unattributed = win[""].total
	}
	if total == 0 {
		return
	}
	parts := []string{fmt.Sprintf("%s error lines in %s log lines", fmtCount(errs), fmtCount(total))}
	if f := e.cfg.Logs.SeverityFields; len(f) > 0 && uncl > 0 {
		parts = append(parts, fmt.Sprintf("%.0f%% of lines have no %s field", 100*uncl/total, strings.Join(f, "/")))
	}
	if unattributed > 0 {
		parts = append(parts, fmt.Sprintf("%s lines (%.0f%%) have no %s field and belong to no service",
			fmtCount(unattributed), 100*unattributed/(total+unattributed), e.cfg.Logs.ServiceField))
	}
	res.notes["logs_lines"] = strings.Join(parts, "; ")

	if errs == 0 {
		res.state = fmt.Sprintf("no error lines: the error filter matched none of %s log lines; check logs.severity_fields or logs.error_filter (run spoor doctor)", fmtCount(total))
		res.summary = fmt.Sprintf(" Logs: the error filter matched none of %s log lines, so Spoor cannot see log errors; this is not evidence of health (run spoor doctor).", fmtCount(total))
		return
	}
	if len(e.cfg.Logs.SeverityFields) > 0 && e.cfg.Logs.ErrorFilter == "" && uncl/total >= 0.3 {
		res.summary = fmt.Sprintf(" Caution: %.0f%% of log lines have no %s field, so errors among them are not checked (run spoor doctor).",
			100*uncl/total, strings.Join(e.cfg.Logs.SeverityFields, "/"))
	}
	if unattributed/(total+unattributed) >= 0.3 {
		res.summary += fmt.Sprintf(" Caution: %.0f%% of log lines have no %s field and belong to no service.",
			100*unattributed/(total+unattributed), e.cfg.Logs.ServiceField)
	}
}

// noServiceField explains an empty scan: lines exist, but none carries the
// service field, so Spoor would otherwise just say "no data".
func (e *Engine) noServiceField(ctx context.Context, res *logResult, r window.Range) {
	n, err := e.logs.CountLines(ctx, r)
	if err != nil {
		return
	}
	if n > 0 {
		f := e.cfg.Logs.ServiceField
		res.state = fmt.Sprintf("%s log lines in window, but none has a %s field; set logs.service_field (run spoor doctor)", fmtCount(n), f)
		res.summary = fmt.Sprintf(" Logs: %s lines arrived, but none has the %s field, so Spoor cannot attribute them to services; this is not evidence of health (run spoor doctor).", fmtCount(n), f)
	}
}

// Row aliases backend.Row for local use.
type Row = backend.Row

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}
