package digest

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/asinha07/spoor/internal/backend"
	"github.com/asinha07/spoor/internal/config"
	"github.com/asinha07/spoor/internal/window"
)

// DoctorCheck is one finding of `spoor doctor`.
type DoctorCheck struct {
	Signal  string `json:"signal"`
	Status  string `json:"status"` // ok, warn, fail, info
	Message string `json:"message"`
}

// DoctorReport says whether Spoor can see what it needs in the backends, and
// suggests configuration where it cannot.
type DoctorReport struct {
	Window    string        `json:"window"`
	Checks    []DoctorCheck `json:"checks"`
	Suggested string        `json:"suggested_config,omitempty"`
}

var (
	serviceFieldHints  = []string{"service.name", "service_name", "service", "app", "application", "app.kubernetes.io/name", "kubernetes.container_name", "kubernetes.labels.app", "k8s.container.name", "container", "container_name", "job"}
	severityFieldHints = []string{"severity_text", "level", "log.level", "severity", "levelname", "loglevel", "log_level", "lvl", "log.severity", "severity_number"}
	errorLevelRe       = regexp.MustCompile(config.ErrorLevels)
	// textLevel finds a level near the start of an unstructured line, after
	// up to three leading tokens (timestamp, thread, logger): the common
	// "2026-01-01T10:00:00Z [ERROR] ..." and "2026-01-01 10:00:00 ERROR ..." shapes.
	textLevel = `^(\S+\s+){1,3}\[?(ERROR|FATAL|CRITICAL|SEVERE|PANIC)\]?[\s:]`
)

// Doctor inspects the configured backends over the window and reports blind
// spots: missing service or severity fields, an error filter that matches
// nothing, error messages fragmented by IDs, metric queries that return
// nothing. It only reads.
func (e *Engine) Doctor(ctx context.Context, since, until string) (DoctorReport, error) {
	spec, err := window.Parse(e.now(), since, until, "", 2)
	if err != nil {
		return DoctorReport{}, err
	}
	r := spec.Window
	rep := DoctorReport{Window: rangeString(r)}
	add := func(sig, status, msg string, args ...any) {
		rep.Checks = append(rep.Checks, DoctorCheck{Signal: sig, Status: status, Message: fmt.Sprintf(msg, args...)})
	}
	var suggest []string

	// Metrics.
	if e.metrics == nil {
		add(SignalMetrics, "info", "not configured")
	} else if names, err := e.metrics.LabelValues(ctx, e.cfg.Metrics.ServiceLabel, r.Start, r.End); err != nil {
		add(SignalMetrics, "fail", "cannot list services: %s", truncate(err.Error(), 200))
	} else if len(names) == 0 {
		add(SignalMetrics, "warn", "no values for label %q: set metrics.service_label to the label holding service names", e.cfg.Metrics.ServiceLabel)
	} else {
		sort.Strings(names)
		answered := 0
		sample := names
		if len(sample) > 3 {
			sample = sample[:3]
		}
		for _, svc := range sample {
			q, _ := config.Render(e.cfg.Metrics.RequestRate, svc, r.End.Sub(r.Start))
			if s, err := e.metrics.Instant(ctx, q, r.End); err == nil && len(s) > 0 {
				answered++
			}
		}
		if answered == 0 {
			add(SignalMetrics, "warn", "%d services found, but metrics.request_rate returned nothing for %s: check the metric names in your config", len(names), strings.Join(sample, ", "))
		} else {
			add(SignalMetrics, "ok", "%d services; request_rate answers for %d of %d sampled", len(names), answered, len(sample))
		}
	}

	// Logs.
	switch d, ok := e.logs.(storeDoctor); {
	case e.logs == nil:
		add(SignalLogs, "info", "not configured")
	case e.rawLogs != nil:
		if err := e.doctorLogs(ctx, r, add, &suggest); err != nil {
			add(SignalLogs, "fail", "%s", truncate(err.Error(), 200))
		}
	case ok:
		d.doctor(ctx, e, r, func(status, msg string, args ...any) { add(SignalLogs, status, msg, args...) })
	default:
		e.doctorServices(ctx, SignalLogs, e.logs.Services, r, add)
	}

	// Traces.
	switch d, ok := e.traces.(storeDoctor); {
	case e.traces == nil:
		add(SignalTraces, "info", "not configured")
	case e.rawTraces != nil:
		if err := e.doctorTraces(ctx, r, add); err != nil {
			add(SignalTraces, "fail", "%s", truncate(err.Error(), 200))
		}
	case ok:
		d.doctor(ctx, e, r, func(status, msg string, args ...any) { add(SignalTraces, status, msg, args...) })
	default:
		e.doctorServices(ctx, SignalTraces, e.traces.Services, r, add)
	}

	if len(suggest) > 0 {
		rep.Suggested = "logs:\n" + strings.Join(suggest, "\n") + "\n"
	}
	return rep, nil
}

func fieldHits(rows []backend.Row) (map[string]float64, []string) {
	hits := map[string]float64{}
	names := []string{}
	for _, row := range rows {
		hits[row["name"]] = row.Float("hits")
		names = append(names, row["name"])
	}
	sort.Strings(names)
	return hits, names
}

func candidates(names []string, hints []string, contains ...string) []string {
	seen := map[string]bool{}
	var out []string
	have := map[string]bool{}
	for _, n := range names {
		have[n] = true
	}
	for _, h := range hints {
		if have[h] && !seen[h] {
			out = append(out, h)
			seen[h] = true
		}
	}
	for _, n := range names {
		l := strings.ToLower(n)
		for _, c := range contains {
			if strings.Contains(l, c) && !seen[n] && !strings.HasPrefix(n, "_") {
				out = append(out, n)
				seen[n] = true
			}
		}
	}
	return out
}

func (e *Engine) doctorLogs(ctx context.Context, r window.Range, add func(string, string, string, ...any), suggest *[]string) error {
	src := e.rawLogs
	rows, err := src.Query(ctx, "* | stats count() n", r.Start, r.End, 0)
	if err != nil {
		return err
	}
	total := 0.0
	if len(rows) > 0 {
		total = rows[0].Float("n")
	}
	if total == 0 {
		add(SignalLogs, "warn", "no log lines in this window: widen --since, or check logs.url")
		return nil
	}
	rows, err = src.Query(ctx, "* | field_names", r.Start, r.End, 0)
	if err != nil {
		return err
	}
	hits, names := fieldHits(rows)
	add(SignalLogs, "info", "%s log lines, %d fields", fmtCount(total), len(names))

	// Service field.
	cfgSvc := e.cfg.Logs.ServiceField
	best, bestShare := "", 0.0
	for _, f := range candidates(names, serviceFieldHints, "service", "container", "app") {
		share := hits[f] / total
		rows, err := src.Query(ctx, "* | stats count_uniq("+backend.Quote(f)+") u", r.Start, r.End, 0)
		if err != nil || len(rows) == 0 {
			continue
		}
		u := rows[0].Float("u")
		if u < 2 && share < 1 || u > 2000 {
			continue
		}
		mark := ""
		if f == cfgSvc {
			mark = " (configured)"
		}
		add(SignalLogs, "info", "service field candidate %s: on %.0f%% of lines, %.0f distinct values%s", f, 100*share, u, mark)
		if share > bestShare+0.05 || (share >= bestShare-0.05 && f == cfgSvc) {
			best, bestShare = f, share
		}
	}
	cfgShare := hits[cfgSvc] / total
	switch {
	case cfgShare >= 0.95:
		add(SignalLogs, "ok", "service field %s is on %.0f%% of lines", cfgSvc, 100*cfgShare)
	case best != "" && best != cfgSvc && bestShare > cfgShare:
		add(SignalLogs, "warn", "service field %s is on only %.0f%% of lines; %s is on %.0f%%", cfgSvc, 100*cfgShare, best, 100*bestShare)
		*suggest = append(*suggest, "  service_field: "+best)
	default:
		add(SignalLogs, "warn", "service field %s is on only %.0f%% of lines: lines without it are invisible to Spoor", cfgSvc, 100*cfgShare)
	}
	svcField := cfgSvc
	if best != "" && bestShare > cfgShare {
		svcField = best
	}

	// Severity fields.
	var sevFields []string
	for _, f := range candidates(names, severityFieldHints, "level", "severity") {
		share := hits[f] / total
		rows, err := src.Query(ctx, "* | stats by ("+backend.Quote(f)+") count() n | sort by (n desc, "+backend.Quote(f)+") | limit 8", r.Start, r.End, 0)
		if err != nil {
			continue
		}
		vals := []string{}
		hasError, allNumeric := false, true
		for _, row := range rows {
			v := row[f]
			if v == "" {
				continue
			}
			vals = append(vals, v)
			hasError = hasError || errorLevelRe.MatchString(v)
			if _, err := strconv.ParseFloat(v, 64); err != nil {
				allNumeric = false
			}
		}
		if len(vals) == 0 {
			continue
		}
		if allNumeric {
			// log4j level_value, syslog severity numbers: not level names.
			add(SignalLogs, "info", "severity field candidate %s holds numbers (%s), not level names: not used", f, strings.Join(vals, ", "))
			continue
		}
		add(SignalLogs, "info", "severity field candidate %s: on %.1f%% of lines, values %s", f, 100*share, strings.Join(vals, ", "))
		if share >= 0.001 && (hasError || len(vals) >= 2) && !strings.Contains(strings.ToLower(f), "number") {
			sevFields = append(sevFields, f)
		}
	}
	cfgFields := e.cfg.Logs.SeverityFields
	if len(sevFields) == 0 {
		add(SignalLogs, "warn", "no severity field found: errors can only be found by message text (logs.error_filter)")
	} else if strings.Join(sevFields, ",") != strings.Join(cfgFields, ",") {
		*suggest = append(*suggest, "  severity_fields: ["+strings.Join(sevFields, ", ")+"]")
	}
	fields := sevFields
	if len(fields) == 0 {
		fields = cfgFields
	}

	// Lines with no severity field, and level words in their text.
	tmp := config.LogsConfig{SeverityFields: fields}
	if uncl := tmp.UnclassifiedFilter(); uncl != "" {
		q := "* | stats count() if (" + uncl + ") u, count() if (" + uncl + " _msg:~" + strconv.Quote(textLevel) + ") t"
		rows, err := src.Query(ctx, q, r.Start, r.End, 0)
		if err == nil && len(rows) > 0 {
			u, t := rows[0].Float("u"), rows[0].Float("t")
			status := "ok"
			if u/total >= 0.3 {
				status = "warn"
			}
			add(SignalLogs, status, "%.0f%% of lines (%s) have no %s field", 100*u/total, fmtCount(u), strings.Join(fields, "/"))
			if t > 0 {
				add(SignalLogs, "warn", "%s of those lines carry a level in their text (e.g. \"... [ERROR] ...\"); add a text rule to error_filter to include them", fmtCount(t))
				built := tmp.EffectiveErrorFilter()
				*suggest = append(*suggest, "  # Structured levels, plus lines whose level is only in the text:",
					"  error_filter: '("+built+" OR ("+uncl+" _msg:~"+strconv.Quote(textLevel)+"))'")
			}
		}
	}

	// Error lines matched by the current config.
	if ef := e.cfg.Logs.EffectiveErrorFilter(); ef != "" {
		rows, err := src.Query(ctx, "* | stats count() if ("+ef+") n", r.Start, r.End, 0)
		if err == nil && len(rows) > 0 {
			n := rows[0].Float("n")
			if n == 0 {
				add(SignalLogs, "fail", "the configured error filter matches 0 lines: Spoor sees no log errors (and would report no change)")
			} else {
				add(SignalLogs, "ok", "the configured error filter matches %s lines (%.2f%%)", fmtCount(n), 100*n/total)
			}
		}
	}

	// Fragmentation: how many patterns per error family after normalization.
	probe := *e
	probe.cfg.Logs.ServiceField = svcField
	if len(sevFields) > 0 && e.cfg.Logs.ErrorFilter == "" {
		probe.cfg.Logs.SeverityFields = sevFields
	}
	if probe.cfg.Logs.EffectiveErrorFilter() != "" {
		pats, _, err := probe.patternsIn(ctx, e.rawLogs, "*", r, nil)
		if err == nil {
			nPats, nFam := 0, 0
			type frag struct {
				svc, top string
				variants int
			}
			var worst []frag
			for svc, ps := range pats {
				nPats += len(ps)
				fams := families(ps)
				nFam += len(fams)
				for _, f := range fams {
					if f.variants >= 10 {
						worst = append(worst, frag{svc, f.top, f.variants})
					}
				}
			}
			sort.Slice(worst, func(i, j int) bool { return worst[i].variants > worst[j].variants })
			add(SignalLogs, "info", "%d error patterns in %d errors (families) after normalization", nPats, nFam)
			for i, w := range worst {
				if i == 3 {
					break
				}
				add(SignalLogs, "info", "%s: %d variants grouped as one error, e.g. %q. If they are different errors, set logs.pattern_field or add a logs.normalize rule; if they are the same, nothing to do (a rule only makes titles cleaner)",
					w.svc, w.variants, e.redact.text(truncate(w.top, 100), map[string]bool{w.svc: true}))
			}
			// JSON errors with none of the message fields keep their whole
			// body as the pattern: still distinct, but titles are noisy.
			jsonPats := 0
			for _, ps := range pats {
				for p := range ps {
					if strings.HasPrefix(p, "{") || strings.HasPrefix(p, "[{") {
						jsonPats++
					}
				}
			}
			if jsonPats > 0 {
				add(SignalLogs, "warn", "%d error patterns are JSON with none of logs.message_fields (%s): add the key that holds the error text to logs.message_fields",
					jsonPats, strings.Join(probe.cfg.Logs.MessageFields, ", "))
			}
		}
	}
	return nil
}

func (e *Engine) doctorTraces(ctx context.Context, r window.Range, add func(string, string, string, ...any)) error {
	t := e.cfg.Traces
	rows, err := e.rawTraces.Query(ctx, realSpans+" | field_names", r.Start, r.End, 0)
	if err != nil {
		return err
	}
	hits, _ := fieldHits(rows)
	spans := hits["span_id"]
	if spans == 0 {
		add(SignalTraces, "warn", "no spans in this window")
		return nil
	}
	missing := []string{}
	for _, f := range []string{t.ServiceField, t.NameField, t.DurationField, "trace_id", "parent_span_id"} {
		if hits[f] == 0 {
			missing = append(missing, f)
		}
	}
	if len(missing) > 0 {
		add(SignalTraces, "warn", "%s spans, but fields missing: %s", fmtCount(spans), strings.Join(missing, ", "))
	} else {
		add(SignalTraces, "ok", "%s spans with service, name, duration and parent links", fmtCount(spans))
	}
	if t.StartField != "" && hits[t.StartField] == 0 {
		add(SignalTraces, "info", "no %s field: self time sums child durations, so parallel calls are counted twice", t.StartField)
	}
	return nil
}

// storeDoctor is implemented by stores with their own doctor checks.
type storeDoctor interface {
	doctor(ctx context.Context, e *Engine, r window.Range, add func(status, msg string, args ...any))
}

// doctorServices is the minimal check for stores without their own: can
// Spoor list services at all.
func (e *Engine) doctorServices(ctx context.Context, sig string, list func(context.Context, window.Range) ([]string, error), r window.Range, add func(string, string, string, ...any)) {
	names, err := list(ctx, r)
	switch {
	case err != nil:
		add(sig, "fail", "cannot list services: %s", truncate(err.Error(), 200))
	case len(names) == 0:
		add(sig, "warn", "no services found in the window: check the service field or label")
	default:
		add(sig, "ok", "%d services", len(names))
	}
}
