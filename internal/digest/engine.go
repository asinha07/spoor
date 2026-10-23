package digest

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/asinha07/spoor/internal/backend"
	"github.com/asinha07/spoor/internal/config"
	"github.com/asinha07/spoor/internal/links"
	"github.com/asinha07/spoor/internal/window"
)

// Engine runs the digest tools against the configured backends.
type Engine struct {
	cfg     config.Config
	metrics MetricsSource
	logs    LogStore
	traces  TraceStore
	// rawLogs and rawTraces are the LogsQL clients behind VictoriaLogs and
	// VictoriaTraces stores (nil for other backends); doctor uses them.
	rawLogs, rawTraces LogsSource
	now                func() time.Time
	redact             *redactor
}

// New builds an engine on VictoriaMetrics-compatible metrics and LogsQL logs
// and traces (VictoriaLogs, VictoriaTraces). Any source may be nil when that
// backend is not configured.
func New(cfg config.Config, metrics MetricsSource, logs, traces LogsSource) *Engine {
	e := &Engine{cfg: cfg, metrics: metrics, rawLogs: logs, rawTraces: traces, now: time.Now, redact: newRedactor(cfg.Redact)}
	if logs != nil {
		e.logs = &victoriaLogs{e: e, src: logs}
	}
	if traces != nil {
		e.traces = &victoriaTraces{e: e, src: traces}
	}
	return e
}

// NewWithStores builds an engine on any backends. Any store may be nil.
func NewWithStores(cfg config.Config, metrics MetricsSource, logs LogStore, traces TraceStore) *Engine {
	return &Engine{cfg: cfg, metrics: metrics, logs: logs, traces: traces, now: time.Now, redact: newRedactor(cfg.Redact)}
}

// NewFromConfig builds an engine with real HTTP clients.
func NewFromConfig(cfg config.Config) *Engine {
	var m MetricsSource
	var l, t LogsSource
	e := New(cfg, nil, nil, nil)
	nr := newRelicClient(cfg)
	switch cfg.Metrics.Type {
	case config.TypeNewRelic:
		m = newNewRelicMetrics(nr, cfg.Metrics.RequestRate)
	default:
		if cfg.Metrics.URL != "" {
			m = backend.NewMetrics(cfg.Metrics.URL, cfg.Metrics.BearerToken, cfg.Timeout).WithBasicAuth(cfg.Metrics.Username, cfg.Metrics.Password)
		}
	}
	e.metrics = m
	switch cfg.Logs.Type {
	case config.TypeLoki:
		if cfg.Logs.URL != "" {
			e.logs = NewLokiStore(e, backend.NewLoki(cfg.Logs.URL, cfg.Logs.BearerToken, cfg.Timeout).
				WithBasicAuth(cfg.Logs.Username, cfg.Logs.Password).WithTenant(cfg.Logs.Tenant))
		}
	case config.TypeNewRelic:
		e.logs = NewNewRelicLogs(e, nr)
	default:
		if cfg.Logs.URL != "" {
			l = backend.NewLogsQL(cfg.Logs.URL, cfg.Logs.BearerToken, cfg.Timeout).WithBasicAuth(cfg.Logs.Username, cfg.Logs.Password).WithName("logs")
			e.rawLogs, e.logs = l, &victoriaLogs{e: e, src: l}
		}
	}
	switch cfg.Traces.Type {
	case config.TypeTempo:
		if cfg.Traces.URL != "" {
			e.traces = NewTempoStore(e, backend.NewTempo(cfg.Traces.URL, cfg.Traces.BearerToken, cfg.Timeout).
				WithBasicAuth(cfg.Traces.Username, cfg.Traces.Password).WithTenant(cfg.Traces.Tenant))
		}
	case config.TypeNewRelic:
		e.traces = NewNewRelicTraces(e, nr)
	default:
		if cfg.Traces.URL != "" {
			t = backend.NewLogsQL(cfg.Traces.URL, cfg.Traces.BearerToken, cfg.Timeout).WithBasicAuth(cfg.Traces.Username, cfg.Traces.Password).WithName("traces")
			e.rawTraces, e.traces = t, &victoriaTraces{e: e, src: t}
		}
	}
	return e
}

// sampler is implemented by stores whose answers come from a sample; the
// note is reported in coverage.
type sampler interface{ SampleNote() string }

// ignored reports whether svc is configured to be left out of scans.
func (e *Engine) ignored(svc string) bool {
	for _, s := range e.cfg.IgnoreServices {
		if s == svc {
			return true
		}
	}
	return false
}

// SetClock overrides the clock (tests only).
func (e *Engine) SetClock(now func() time.Time) { e.now = now }

func (e *Engine) uiBase(b config.Backend) string {
	if b.UIURL != "" {
		return b.UIURL
	}
	return b.URL
}

// Evidence links open Grafana Explore when a Grafana datasource is
// configured for the signal, and the backend's own VMUI otherwise.
func (e *Engine) metricsLink(q string, start, end time.Time) string {
	if g := e.cfg.Grafana; g.URL != "" && g.Metrics.UID != "" {
		return links.Grafana(g.URL, g.Metrics.UID, g.Metrics.Type, q, start, end)
	}
	switch e.cfg.Metrics.Type {
	case config.TypePrometheus:
		return links.Prometheus(e.uiBase(e.cfg.Metrics.Backend), q, start, end)
	case config.TypeNewRelic:
		return links.NewRelic(e.cfg.NewRelic.AccountID, e.cfg.NewRelic.Region, q, start, end)
	}
	return links.Metrics(e.uiBase(e.cfg.Metrics.Backend), q, start, end)
}

func (e *Engine) logsLink(q string, start, end time.Time) string {
	if g := e.cfg.Grafana; g.URL != "" && g.Logs.UID != "" {
		return links.Grafana(g.URL, g.Logs.UID, g.Logs.Type, q, start, end)
	}
	switch e.cfg.Logs.Type {
	case config.TypeLoki:
		return "" // Loki has no UI of its own; set grafana.logs_datasource for links
	case config.TypeNewRelic:
		return links.NewRelic(e.cfg.NewRelic.AccountID, e.cfg.NewRelic.Region, q, start, end)
	}
	return links.LogsQL(e.uiBase(e.cfg.Logs.Backend), q, start, end)
}

func (e *Engine) tracesLink(q string, start, end time.Time) string {
	if g := e.cfg.Grafana; g.URL != "" && g.Traces.UID != "" {
		return links.Grafana(g.URL, g.Traces.UID, g.Traces.Type, q, start, end)
	}
	switch e.cfg.Traces.Type {
	case config.TypeTempo:
		return "" // Tempo has no UI of its own; set grafana.traces_datasource for links
	case config.TypeNewRelic:
		return links.NewRelic(e.cfg.NewRelic.AccountID, e.cfg.NewRelic.Region, q, start, end)
	}
	return links.LogsQL(e.uiBase(e.cfg.Traces.Backend), q, start, end)
}

// scorerSet selects which signals a tool consults.
type scorerSet struct{ metrics, logs, traces bool }

// WhatChanged ranks changes across all signals for one service, or for every
// known service when req.Service is empty.
func (e *Engine) WhatChanged(ctx context.Context, req Request) (Envelope, error) {
	return e.run(ctx, req, scorerSet{true, true, true})
}

// NewErrors reports error log patterns that are new or spiking.
func (e *Engine) NewErrors(ctx context.Context, req Request) (Envelope, error) {
	return e.run(ctx, req, scorerSet{logs: true})
}

// TraceDiff reports span names that got slower or started failing.
func (e *Engine) TraceDiff(ctx context.Context, req Request) (Envelope, error) {
	return e.run(ctx, req, scorerSet{traces: true})
}

func (e *Engine) run(ctx context.Context, req Request, set scorerSet) (Envelope, error) {
	spec, err := window.Parse(e.now(), req.Since, req.Until, req.Baseline, e.cfg.Scoring.BaselineSubWindows)
	if err != nil {
		return Envelope{}, err
	}
	// Look across baseline and window so a service that went silent still shows up.
	list := e.listIn(ctx, window.Range{Start: spec.SubWindows[0].Start, End: spec.Window.End})
	known := map[string][]string{}
	for _, s := range list.Services {
		known[s.Name] = s.Signals
	}
	services := []string{strings.TrimSpace(req.Service)}
	scope := services[0]
	scanNote := ""
	// Logs are scored in a few batched queries whatever the number of
	// services, so they cover every service; metrics and traces are queried
	// per service and are capped at max_services.
	logServices := services
	if scope == "" {
		services = services[:0]
		for _, s := range list.Services {
			if !e.ignored(s.Name) {
				services = append(services, s.Name)
			}
		}
		logServices = append([]string{}, services...)
		perService := (set.metrics && e.metrics != nil) || (set.traces && e.traces != nil)
		if perService && len(services) > e.cfg.MaxServices {
			scanNote = fmt.Sprintf("metrics and traces scanned for the first %d of %d services (alphabetical; logs cover all); pass service to check others", e.cfg.MaxServices, len(services))
			services = services[:e.cfg.MaxServices]
		}
		scope = fmt.Sprintf("%d services", len(logServices))
	}

	cov := newCoverage(set, e)
	var (
		mu       sync.Mutex
		findings []Finding
		steady   []SteadyError
		wg       sync.WaitGroup
		sem      = make(chan struct{}, 4)
		logRes   logResult
	)
	// Logs are scored for all services at once: a few batched queries
	// instead of two per service.
	if set.logs && e.logs != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := e.scoreLogs(ctx, logServices, req.Service == "", spec)
			mu.Lock()
			defer mu.Unlock()
			cov.record(SignalLogs, err)
			findings = append(findings, r.findings...)
			steady = append(steady, r.steady...)
			logRes = r
		}()
	}
	for _, svc := range services {
		svc := svc
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			type job struct {
				signal string
				on     bool
				src    bool
				fn     func() ([]Finding, error)
			}
			jobs := []job{
				{SignalMetrics, set.metrics, e.metrics != nil, func() ([]Finding, error) { return e.scoreMetrics(ctx, svc, spec) }},
				{SignalTraces, set.traces, e.traces != nil, func() ([]Finding, error) { return e.scoreTraces(ctx, svc, req.Operation, spec) }},
			}
			for _, j := range jobs {
				if !j.on || !j.src {
					continue
				}
				fs, err := j.fn()
				mu.Lock()
				cov.record(j.signal, err)
				findings = append(findings, fs...)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return Envelope{}, err
	}

	// A change can only be measured against a baseline that has data. Without
	// this check an empty baseline (a 1d baseline before data existed, short
	// retention) makes every recurring error look new.
	baseRange := window.Range{Start: spec.SubWindows[0].Start, End: spec.Baseline.End}
	findings, baseNotes := checkBaseline(findings, e.listIn(ctx, baseRange), cov.wanted)

	// Use trace structure to separate causes from their echoes in callers.
	// Skipped for a single-operation trace_diff, whose scope is narrower.
	var attrErr error
	if (set.traces || set.metrics) && e.traces != nil && len(findings) > 0 && req.Operation == "" {
		var a *attribution
		if a, attrErr = e.attribute(ctx, spec); attrErr == nil {
			findings = e.applyAttribution(a, services, findings, spec)
		}
	}

	ranked := rank(findings, e.cfg.Scoring.ReportThreshold)
	env := Envelope{
		Window:   rangeString(spec.Window),
		Baseline: fmt.Sprintf("%s (%s, %d windows)", rangeString(window.Range{Start: spec.SubWindows[0].Start, End: spec.Baseline.End}), spec.Mode, len(spec.SubWindows)),
		Findings: ranked,
		Coverage: cov.result(len(services)),
	}
	noData := markNoData(env.Coverage, list.Coverage, services, known)
	for sig, note := range baseNotes {
		if env.Coverage[sig] == "ok" {
			env.Coverage[sig] = note
		}
	}
	if noData == "" && len(ranked) == 0 && len(baseNotes) > 0 {
		sigs := make([]string, 0, len(baseNotes))
		for sig := range baseNotes {
			sigs = append(sigs, sig)
		}
		sort.Strings(sigs)
		noData = fmt.Sprintf("No baseline data for %s in %s, so Spoor cannot measure what changed there. Use baseline=previous, or a period the backends retain.",
			strings.Join(sigs, ", "), rangeString(baseRange))
	}
	if attrErr != nil {
		env.Coverage["attribution"] = "error: " + truncate(attrErr.Error(), 160)
	}
	if scanNote != "" {
		env.Coverage["scope"] = scanNote
	}
	if s, ok := e.traces.(sampler); ok && env.Coverage[SignalTraces] == "ok" {
		env.Coverage["traces_sample"] = s.SampleNote()
	}
	if _, ok := env.Coverage[SignalLogs]; ok {
		if logRes.state != "" && !strings.HasPrefix(env.Coverage[SignalLogs], "error") {
			env.Coverage[SignalLogs] = logRes.state
		}
		for k, v := range logRes.notes {
			env.Coverage[k] = v
		}
	}
	env.SteadyErrors = topSteady(withoutFindings(steady, ranked), baseNotes[SignalLogs] != "")
	// Redact before summarizing, so the summary is built from clean titles;
	// the summary itself is redacted again with everything else.
	allow := append(append([]string{}, services...), keys(known)...)
	e.redact.envelope(&env, allow)
	env.Summary = summarize(scope, env.Findings, spec) + steadySentence(env.SteadyErrors, len(ranked) == 0)
	if len(ranked) == 0 && noData != "" {
		env.Summary = noData
	}
	switch {
	case logRes.state != "" && len(ranked) == 0:
		// Blind, not quiet: say so first, and do not open with "no change".
		env.Summary = strings.TrimSpace(logRes.summary)
		if noData == "" && (env.Coverage[SignalMetrics] == "ok" || env.Coverage[SignalTraces] == "ok") {
			env.Summary += " Metrics and traces show no significant change."
		}
	default:
		env.Summary += logRes.summary
	}
	e.redact.envelope(&env, allow)
	return Fit(env, ClampBudget(req.BudgetTokens)), nil
}

// markNoData replaces "ok" with a no-data note where a signal had nothing to
// look at, so "no change" is never confused with "no data". It returns a
// summary to use when nothing was found and the reason is missing data.
func markNoData(cov, listCov map[string]string, services []string, known map[string][]string) string {
	has := func(svc, sig string) bool {
		for _, s := range known[svc] {
			if s == sig {
				return true
			}
		}
		return false
	}
	for sig, state := range cov {
		if state != "ok" {
			continue
		}
		if lc := listCov[sig]; strings.HasPrefix(lc, "error") {
			cov[sig] = lc
			continue
		}
		any := false
		for _, svc := range services {
			any = any || has(svc, sig)
		}
		switch {
		case any:
		case len(services) == 1:
			cov[sig] = fmt.Sprintf("no data for service %q", services[0])
		default:
			cov[sig] = "no data in window"
		}
	}
	healthy := false
	for _, state := range cov {
		healthy = healthy || state == "ok" || strings.HasPrefix(state, "partial")
	}
	if healthy {
		return ""
	}
	switch {
	case len(services) == 0:
		return "No data: no backend returned any service for this window, so Spoor cannot say whether anything changed. Check coverage for errors."
	case len(services) == 1 && len(known[services[0]]) == 0:
		return fmt.Sprintf("No data for service %q in any backend for this window. Check the name with list_services.", services[0])
	}
	return "No data for the requested signals in this window, so Spoor cannot say whether anything changed. Check coverage."
}

func rangeString(r window.Range) string {
	return r.Start.UTC().Format(time.RFC3339) + "/" + r.End.UTC().Format(time.RFC3339)
}

// partialError marks a scorer that answered with some of its queries
// failing: its findings are kept and coverage says "partial".
type partialError struct {
	svc string
	err error
}

func (p partialError) Error() string { return p.svc + ": " + p.err.Error() }

// coverage tracks which signals answered.
type coverage struct {
	wanted  map[string]string // signal -> "not configured" or ""
	ok      map[string]int
	failed  map[string]int
	partial map[string]int
	last    map[string]string
}

func newCoverage(set scorerSet, e *Engine) *coverage {
	c := &coverage{wanted: map[string]string{}, ok: map[string]int{}, failed: map[string]int{}, partial: map[string]int{}, last: map[string]string{}}
	add := func(on bool, sig string, configured bool) {
		if !on {
			return
		}
		if configured {
			c.wanted[sig] = ""
		} else {
			c.wanted[sig] = "not configured"
		}
	}
	add(set.metrics, SignalMetrics, e.metrics != nil)
	add(set.logs, SignalLogs, e.logs != nil)
	add(set.traces, SignalTraces, e.traces != nil)
	return c
}

func (c *coverage) record(sig string, err error) {
	if err == nil {
		c.ok[sig]++
		return
	}
	if p, ok := err.(partialError); ok {
		c.ok[sig]++
		c.partial[sig]++
		c.last[sig] = truncate(p.Error(), 160)
		return
	}
	c.failed[sig]++
	// Backend errors already read "backend unavailable: …"; keep them as is.
	c.last[sig] = truncate(err.Error(), 160)
}

func (c *coverage) result(services int) map[string]string {
	out := map[string]string{}
	for sig, note := range c.wanted {
		switch {
		case note != "":
			out[sig] = note
		case c.failed[sig] == 0 && c.partial[sig] > 0:
			out[sig] = fmt.Sprintf("partial (some queries failed for %d of %d services): %s", c.partial[sig], services, c.last[sig])
		case c.failed[sig] == 0:
			out[sig] = "ok"
		case c.ok[sig] == 0:
			out[sig] = "error: " + c.last[sig]
		default:
			out[sig] = fmt.Sprintf("partial (%d/%d services failed): %s", c.failed[sig], services, c.last[sig])
		}
	}
	return out
}

// correlationWindow: findings from different signals whose onsets fall this
// close together are treated as corroborating each other.
const correlationWindow = 2 * time.Minute

// rank drops weak findings, boosts cross-signal corroboration, removes
// duplicates and sorts deterministically.
func rank(fs []Finding, threshold float64) []Finding {
	kept := fs[:0:0]
	seen := map[string]bool{}
	for _, f := range fs {
		key := f.Service + "\x00" + f.Kind + "\x00" + f.Title
		if f.Score < threshold || seen[key] {
			continue
		}
		seen[key] = true
		kept = append(kept, f)
	}
	for i := range kept {
		if kept[i].onset.IsZero() {
			continue
		}
		for j := range kept {
			if i == j || kept[j].onset.IsZero() || kept[j].Signal == kept[i].Signal || kept[j].Service != kept[i].Service {
				continue
			}
			d := kept[i].onset.Sub(kept[j].onset)
			if d < 0 {
				d = -d
			}
			if d <= correlationWindow {
				kept[i].Score = round2(minF(0.99, kept[i].Score+0.05))
				break
			}
		}
	}
	sort.SliceStable(kept, func(a, b int) bool {
		if kept[a].Score != kept[b].Score {
			return kept[a].Score > kept[b].Score
		}
		if kept[a].Service != kept[b].Service {
			return kept[a].Service < kept[b].Service
		}
		return kept[a].Title < kept[b].Title
	})
	if kept == nil {
		kept = []Finding{}
	}
	return kept
}

func minF(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

// summarize writes the one-paragraph answer an agent reads first.
func summarize(scope string, fs []Finding, spec window.Spec) string {
	if len(fs) == 0 {
		return fmt.Sprintf("No significant change in %s during %s compared with the %s baseline.",
			scope, rangeString(spec.Window), spec.Mode)
	}
	svcs := map[string]int{}
	var earliest time.Time
	for _, f := range fs {
		svcs[f.Service]++
		if !f.onset.IsZero() && (earliest.IsZero() || f.onset.Before(earliest)) {
			earliest = f.onset
		}
	}
	top := fs[0]
	var b strings.Builder
	fmt.Fprintf(&b, "%d change(s) in %s. Strongest: %s — %s.", len(fs), plural(len(svcs), "service"), top.Service, top.Title)
	n := 1
	for _, f := range fs[1:] {
		if n == 3 {
			break
		}
		if f.Signal == top.Signal && f.Service == top.Service && f.Kind == top.Kind {
			continue
		}
		fmt.Fprintf(&b, " Also: %s — %s.", f.Service, f.Title)
		n++
	}
	if !earliest.IsZero() {
		fmt.Fprintf(&b, " Earliest onset %s.", earliest.UTC().Format(time.RFC3339))
	}
	return truncate(b.String(), 600)
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// noBaselineNote marks a signal whose baseline period holds no data at all.
const noBaselineNote = "no data in baseline period; changes cannot be measured (try baseline=previous)"

// checkBaseline drops findings for signals whose baseline period has no data
// in any service, and marks findings for services that are new since the
// baseline. It returns a coverage note per affected signal.
func checkBaseline(fs []Finding, base ServiceList, wanted map[string]string) ([]Finding, map[string]string) {
	has := map[string]map[string]bool{}
	for _, svc := range base.Services {
		for _, sig := range svc.Signals {
			if has[sig] == nil {
				has[sig] = map[string]bool{}
			}
			has[sig][svc.Name] = true
		}
	}
	notes := map[string]string{}
	for sig, configured := range wanted {
		if configured != "" { // not configured
			continue
		}
		switch state := base.Coverage[sig]; {
		case strings.HasPrefix(state, "error"):
			notes[sig] = "baseline check failed: " + strings.TrimPrefix(state, "error: ")
		case len(has[sig]) == 0:
			notes[sig] = noBaselineNote
		}
	}
	out := fs[:0:0]
	for _, f := range fs {
		if notes[f.Signal] == noBaselineNote {
			continue
		}
		if has[f.Signal] != nil && !has[f.Signal][f.Service] && !strings.Contains(f.Title, "no baseline data") {
			f.Title += " (no baseline data for this service)"
		}
		out = append(out, f)
	}
	return out, notes
}

// topSteady orders steady services by error volume and keeps the first few.
// With no usable logs baseline nothing can be called steady.
func topSteady(st []SteadyError, noBaseline bool) []SteadyError {
	if noBaseline || len(st) == 0 {
		return nil
	}
	sort.Slice(st, func(i, j int) bool {
		if st[i].ErrorsNow != st[j].ErrorsNow {
			return st[i].ErrorsNow > st[j].ErrorsNow
		}
		return st[i].Service < st[j].Service
	})
	if len(st) > maxSteady {
		st = st[:maxSteady]
	}
	return st
}

// steadySentence tells the agent, in the summary it reads first, which errors
// are routine. With no findings it is explicit that they do not explain a new
// problem; with findings it is a short pointer.
func steadySentence(st []SteadyError, noFindings bool) string {
	if len(st) == 0 {
		return ""
	}
	parts := make([]string, 0, len(st))
	for _, s := range st {
		change := s.Change
		if s.LinesChange != "" {
			change += ", all log lines " + s.LinesChange
		}
		parts = append(parts, fmt.Sprintf("%s (%.0f error lines now vs %s usual, %s; e.g. %q)",
			s.Service, s.ErrorsNow, fmtCount(s.ErrorsUsual), change, truncate(s.Example, 80)))
	}
	if noFindings {
		return " Errors present before this window and not significantly up, so not a new problem: " + strings.Join(parts, "; ") + "."
	}
	names := make([]string, 0, len(st))
	for _, s := range st {
		names = append(names, s.Service)
	}
	return " Errors not significantly up (not new) in: " + strings.Join(names, ", ") + "."
}

// fmtCount prints a count without trailing zeros: 5, 9.7.
func fmtCount(v float64) string {
	if v == math.Trunc(v) {
		return fmt.Sprintf("%.0f", v)
	}
	return fmt.Sprintf("%.1f", v)
}

// withoutFindings drops steady entries for services that have any finding, on
// any signal: calling such a service's errors "not a new problem" could steer
// an agent away from its real change (a memory leak with steady error logs).
func withoutFindings(st []SteadyError, fs []Finding) []SteadyError {
	flagged := map[string]bool{}
	for _, f := range fs {
		flagged[f.Service] = true
	}
	out := st[:0:0]
	for _, s := range st {
		if !flagged[s.Service] {
			out = append(out, s)
		}
	}
	return out
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
