// Package config loads Spoor's configuration from a YAML file and the
// environment. Every query Spoor sends is driven by templates here, so the
// defaults (OpenTelemetry naming) can be swapped for any setup.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"text/template"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the full Spoor configuration.
type Config struct {
	Metrics MetricsConfig `yaml:"metrics"`
	Logs    LogsConfig    `yaml:"logs"`
	Traces  TracesConfig  `yaml:"traces"`
	Scoring ScoringConfig `yaml:"scoring"`
	// Timeout bounds every backend request.
	Timeout time.Duration `yaml:"timeout"`
	// MaxServices caps how many services an all-services scan visits.
	MaxServices int `yaml:"max_services"`
	// IgnoreServices are left out of all-services scans and root-cause
	// attribution: load generators, collectors, browser clients. They can
	// still be queried by name.
	IgnoreServices []string `yaml:"ignore_services"`
	// Redact adds rules applied to every text field Spoor returns, after the
	// built-in rules (emails, tokens, card-like numbers, IDs).
	Redact []Rule `yaml:"redact"`
	// Grafana, when set, makes evidence links open Grafana Explore instead
	// of VMUI.
	Grafana GrafanaConfig `yaml:"grafana"`
	// NewRelic holds the account and key used by any signal of type newrelic.
	NewRelic NewRelicConfig `yaml:"newrelic"`
}

// Backend types, set per signal with `type:`.
const (
	TypeVictoriaMetrics = "victoriametrics"
	TypePrometheus      = "prometheus" // also Mimir, Thanos, Cortex
	TypeVictoriaLogs    = "victorialogs"
	TypeLoki            = "loki"
	TypeVictoriaTraces  = "victoriatraces"
	TypeTempo           = "tempo"
	TypeNewRelic        = "newrelic"
)

var validTypes = map[string][]string{
	"metrics": {TypeVictoriaMetrics, TypePrometheus, TypeNewRelic},
	"logs":    {TypeVictoriaLogs, TypeLoki, TypeNewRelic},
	"traces":  {TypeVictoriaTraces, TypeTempo, TypeNewRelic},
}

// Types names the backend type of each signal.
type Types struct{ Metrics, Logs, Traces string }

// NewRelicConfig is the New Relic account Spoor reads through NerdGraph.
type NewRelicConfig struct {
	AccountID int64 `yaml:"account_id"` // or SPOOR_NEWRELIC_ACCOUNT_ID / NEW_RELIC_ACCOUNT_ID
	// APIKey is a User API key (NRAK-...). Prefer the environment:
	// SPOOR_NEWRELIC_API_KEY or NEW_RELIC_API_KEY.
	APIKey string `yaml:"api_key"`
	Region string `yaml:"region"` // us (default) or eu
	// URL overrides the NerdGraph endpoint (tests, proxies).
	URL string `yaml:"url"`
}

// Endpoint returns the NerdGraph URL for the configured region.
func (n NewRelicConfig) Endpoint() string {
	switch {
	case n.URL != "":
		return n.URL
	case strings.EqualFold(n.Region, "eu"):
		return "https://api.eu.newrelic.com/graphql"
	}
	return "https://api.newrelic.com/graphql"
}

// Rule is one regular-expression replacement (RE2 syntax).
type Rule struct {
	Regex   string `yaml:"regex"`
	Replace string `yaml:"replace"`
}

// GrafanaConfig describes the Grafana instance used for evidence links.
type GrafanaConfig struct {
	// URL is the Grafana base URL people open in a browser.
	URL     string            `yaml:"url"`
	Metrics GrafanaDatasource `yaml:"metrics_datasource"`
	Logs    GrafanaDatasource `yaml:"logs_datasource"`
	Traces  GrafanaDatasource `yaml:"traces_datasource"`
}

// GrafanaDatasource identifies one Grafana datasource.
type GrafanaDatasource struct {
	UID  string `yaml:"uid"`
	Type string `yaml:"type"` // e.g. prometheus, victoriametrics-logs-datasource
}

// Backend is the shared connection block of each backend.
type Backend struct {
	// Type selects the backend: see the Type* constants. Empty means the
	// VictoriaMetrics stack.
	Type        string `yaml:"type"`
	URL         string `yaml:"url"`
	BearerToken string `yaml:"bearer_token"`
	// Username and Password enable HTTP Basic auth (e.g. Grafana's
	// datasource proxy). Ignored when BearerToken is set.
	Username string `yaml:"username"`
	Password string `yaml:"password"`
	// UIURL is the base used for evidence links; defaults to URL.
	UIURL string `yaml:"ui_url"`
	// Tenant is sent as X-Scope-OrgID to multi-tenant Loki and Tempo.
	Tenant string `yaml:"tenant"`
}

// MetricsConfig describes how to reach VictoriaMetrics and which queries to run.
type MetricsConfig struct {
	Backend `yaml:",inline"`
	// ServiceLabel is the label holding the service name.
	ServiceLabel string `yaml:"service_label"`
	// RED queries. Templates receive {{.Service}} (already quoted-safe) and {{.Window}}.
	RequestRate string `yaml:"request_rate"`
	ErrorRate   string `yaml:"error_rate"`
	LatencyP95  string `yaml:"latency_p95"`
	// Resources are extra per-service gauges or rates worth watching.
	Resources []ResourceQuery `yaml:"resources"`
}

// ResourceQuery is one named extra metric.
type ResourceQuery struct {
	Name  string `yaml:"name"`
	Query string `yaml:"query"`
	Unit  string `yaml:"unit"`
}

// LogsConfig describes VictoriaLogs access and field names.
type LogsConfig struct {
	Backend `yaml:",inline"`
	// ServiceField is the log field holding the service name.
	ServiceField string `yaml:"service_field"`
	// SeverityFields are the fields that may hold a log level. Spoor builds
	// the error filter from them (unless ErrorFilter is set) and reports how
	// many lines carry none of them, so blind spots are visible.
	SeverityFields []string `yaml:"severity_fields"`
	// ErrorFilter, when set, replaces the filter built from SeverityFields.
	// Use it for logs whose level is only in the message text.
	ErrorFilter string `yaml:"error_filter"`
	// NormalizeIDs replaces tokens that mix letters and digits (order, cart,
	// product IDs) with <ID> before grouping error lines into patterns.
	NormalizeIDs *bool `yaml:"normalize_ids"`
	// Normalize adds replacements applied to messages before grouping.
	Normalize []Rule `yaml:"normalize"`
	// PatternField groups error lines by this field (an error code or
	// exception class) instead of by message text.
	PatternField string `yaml:"pattern_field"`
	// MessageFields: when a message is JSON, the first of these keys found
	// is used as the error text (nested keys use dots: error.message).
	// CodeFields likewise, prefixed to the text. Empty lists disable it.
	MessageFields []string `yaml:"message_fields"`
	CodeFields    []string `yaml:"code_fields"`
}

// ErrorLevels are the level values treated as errors when the filter is
// built from SeverityFields.
const ErrorLevels = `(?i)^(err|error|fatal|crit|critical|alert|emerg|emergency|panic|severe)$`

// errorLevelWords are the level values treated as errors, for backends
// whose filters are not regular expressions.
var errorLevelWords = []string{"err", "error", "fatal", "crit", "critical", "alert", "emerg", "emergency", "panic", "severe"}

// EffectiveErrorFilter returns the filter selecting error lines, in the
// logs backend's query language: LogsQL, a LogQL pipeline, or an NRQL
// condition.
func (l LogsConfig) EffectiveErrorFilter() string {
	if l.ErrorFilter != "" {
		return l.ErrorFilter
	}
	if len(l.SeverityFields) == 0 {
		return ""
	}
	switch l.Type {
	case TypeLoki:
		parts := make([]string, 0, len(l.SeverityFields))
		for _, f := range l.SeverityFields {
			parts = append(parts, LokiLabel(f)+`=~"(?i)(`+strings.Join(errorLevelWords, "|")+`)"`)
		}
		return "| " + strings.Join(parts, " or ")
	case TypeNewRelic:
		words := make([]string, len(errorLevelWords))
		for i, w := range errorLevelWords {
			words[i] = "'" + w + "'"
		}
		parts := make([]string, 0, len(l.SeverityFields))
		for _, f := range l.SeverityFields {
			parts = append(parts, "lower("+NRQLField(f)+") IN ("+strings.Join(words, ", ")+")")
		}
		return "(" + strings.Join(parts, " OR ") + ")"
	}
	parts := make([]string, 0, len(l.SeverityFields))
	for _, f := range l.SeverityFields {
		parts = append(parts, quoteField(f)+":~"+strconv.Quote(ErrorLevels))
	}
	if len(parts) == 1 {
		return parts[0]
	}
	return "(" + strings.Join(parts, " OR ") + ")"
}

// UnclassifiedFilter selects lines that carry none of the severity fields,
// or "" when no severity fields are configured.
func (l LogsConfig) UnclassifiedFilter() string {
	if len(l.SeverityFields) == 0 {
		return ""
	}
	switch l.Type {
	case TypeLoki:
		// Loki 3 sets detected_level to "unknown" when it finds no level.
		parts := make([]string, 0, len(l.SeverityFields))
		for _, f := range l.SeverityFields {
			parts = append(parts, `(`+LokiLabel(f)+`="" or `+LokiLabel(f)+`="unknown")`)
		}
		return "| " + strings.Join(parts, " and ")
	case TypeNewRelic:
		parts := make([]string, 0, len(l.SeverityFields))
		for _, f := range l.SeverityFields {
			parts = append(parts, NRQLField(f)+" IS NULL")
		}
		return strings.Join(parts, " AND ")
	}
	parts := make([]string, 0, len(l.SeverityFields))
	for _, f := range l.SeverityFields {
		parts = append(parts, "NOT "+quoteField(f)+":*")
	}
	return strings.Join(parts, " ")
}

// LokiLabel turns a field name into a LogQL label name: dots and other
// characters become underscores, as Loki does for OTLP attributes.
func LokiLabel(f string) string {
	b := []byte(f)
	for i, c := range b {
		if !(c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
			b[i] = '_'
		}
	}
	return string(b)
}

// NRQLField quotes an attribute name with backticks when it is not a plain
// identifier (log.level, entity.name are fine; dashes and spaces are not).
func NRQLField(f string) string {
	for _, r := range f {
		if !(r == '_' || r == '.' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			return "`" + strings.ReplaceAll(f, "`", "") + "`"
		}
	}
	return f
}

// NRQLString quotes s as an NRQL string literal.
func NRQLString(s string) string {
	return "'" + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), "'", `\'`) + "'"
}

// IDNormalization reports whether built-in ID normalization is on.
func (l LogsConfig) IDNormalization() bool { return l.NormalizeIDs == nil || *l.NormalizeIDs }

// quoteField quotes a LogsQL field name when it contains characters other
// than letters, digits, '_' and '.'.
func quoteField(f string) string {
	for _, r := range f {
		plain := r == '_' || r == '.' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'
		if !plain {
			return strconv.Quote(f)
		}
	}
	return f
}

// TracesConfig describes VictoriaTraces access and field names.
type TracesConfig struct {
	Backend       `yaml:",inline"`
	ServiceField  string `yaml:"service_field"`
	NameField     string `yaml:"name_field"`
	DurationField string `yaml:"duration_field"` // nanoseconds
	// StartField holds the span start time in Unix nanoseconds. It lets
	// self time subtract overlapping child spans once, not twice.
	StartField string `yaml:"start_field"`
	// ErrorFilter is a LogsQL filter selecting failed spans.
	ErrorFilter string `yaml:"error_filter"`
}

// ScoringConfig holds the tunable thresholds.
type ScoringConfig struct {
	// ReportThreshold: findings scoring below this are dropped (0-1).
	ReportThreshold float64 `yaml:"report_threshold"`
	// BaselineSubWindows: how many window-length slices of history form the baseline.
	BaselineSubWindows int `yaml:"baseline_sub_windows"`
	// MinRequestRate (req/s): services quieter than this skip error/latency scoring.
	MinRequestRate float64 `yaml:"min_request_rate"`
	// MinPatternCount: a log pattern needs this many hits in the window to count.
	MinPatternCount int `yaml:"min_pattern_count"`
	// MinSpans: a span name needs this many spans in the window to be compared.
	MinSpans int `yaml:"min_spans"`
}

// Default returns the configuration used when no file is given: the
// VictoriaMetrics stack, with query names following the OpenTelemetry
// semantic conventions as exported to Prometheus.
func Default() Config { return DefaultFor(Types{}) }

// DefaultFor returns the defaults for the given backend types: URLs, field
// names and queries that fit each backend out of the box.
func DefaultFor(t Types) Config {
	c := victoriaDefaults()
	switch t.Metrics {
	case TypePrometheus:
		c.Metrics.Type = TypePrometheus
		c.Metrics.URL = "http://localhost:9090"
	case TypeNewRelic:
		c.Metrics = MetricsConfig{
			Backend:      Backend{Type: TypeNewRelic},
			ServiceLabel: "appName",
			RequestRate:  `SELECT rate(count(*), 1 second) FROM Transaction WHERE appName = {{.Service}}`,
			ErrorRate:    `SELECT filter(count(*), WHERE error IS true) / count(*) FROM Transaction WHERE appName = {{.Service}}`,
			LatencyP95:   `SELECT percentile(duration, 95) FROM Transaction WHERE appName = {{.Service}}`,
		}
	}
	switch t.Logs {
	case TypeLoki:
		c.Logs.Type = TypeLoki
		c.Logs.URL = "http://localhost:3100"
		c.Logs.ServiceField = "service_name"
		c.Logs.SeverityFields = []string{"detected_level", "level"}
	case TypeNewRelic:
		c.Logs.Backend = Backend{Type: TypeNewRelic}
		c.Logs.ServiceField = "entity.name"
		c.Logs.SeverityFields = []string{"level", "log.level"}
	}
	switch t.Traces {
	case TypeTempo:
		c.Traces = TracesConfig{
			Backend:      Backend{Type: TypeTempo, URL: "http://localhost:3200"},
			ServiceField: "resource.service.name",
			NameField:    "name",
			ErrorFilter:  "status = error",
		}
	case TypeNewRelic:
		c.Traces = TracesConfig{
			Backend:       Backend{Type: TypeNewRelic},
			ServiceField:  "entity.name",
			NameField:     "name",
			DurationField: "duration.ms",
			ErrorFilter:   "error IS true",
		}
	}
	return c
}

func victoriaDefaults() Config {
	return Config{
		Timeout:     20 * time.Second,
		MaxServices: 25,
		Metrics: MetricsConfig{
			Backend:      Backend{URL: "http://localhost:8428"},
			ServiceLabel: "service_name",
			RequestRate:  `sum(rate(http_server_request_duration_seconds_count{service_name={{.Service}}}[{{.Window}}]))`,
			ErrorRate: `(sum(rate(http_server_request_duration_seconds_count{service_name={{.Service}},http_response_status_code=~"5.."}[{{.Window}}])) or vector(0))` +
				` / sum(rate(http_server_request_duration_seconds_count{service_name={{.Service}}}[{{.Window}}]))`,
			LatencyP95: `histogram_quantile(0.95, sum(rate(http_server_request_duration_seconds_bucket{service_name={{.Service}}}[{{.Window}}])) by (le))`,
			Resources: []ResourceQuery{
				{Name: "cpu", Unit: "cores", Query: `sum(rate(process_cpu_seconds_total{service_name={{.Service}}}[{{.Window}}]))`},
				{Name: "memory", Unit: "bytes", Query: `max(max_over_time(process_resident_memory_bytes{service_name={{.Service}}}[{{.Window}}]))`},
				{Name: "consumer_lag", Unit: "messages", Query: `max(max_over_time(kafka_consumer_lag{service_name={{.Service}}}[{{.Window}}]))`},
			},
		},
		Logs: LogsConfig{
			Backend:        Backend{URL: "http://localhost:9428"},
			ServiceField:   "service.name",
			SeverityFields: []string{"severity_text"},
			MessageFields:  []string{"message", "msg", "error.message", "error_message", "errorMessage", "exception.message", "error", "err", "exception"},
			CodeFields:     []string{"error_code", "errorCode", "error.code", "code", "exception.type", "error.type"},
		},
		Traces: TracesConfig{
			Backend:       Backend{URL: "http://localhost:10428"},
			ServiceField:  "resource_attr:service.name",
			NameField:     "name",
			DurationField: "duration",
			StartField:    "start_time_unix_nano",
			ErrorFilter:   `status_code:=2`,
		},
		Scoring: ScoringConfig{
			ReportThreshold:    0.5,
			BaselineSubWindows: 6,
			MinRequestRate:     0.05,
			MinPatternCount:    3,
			MinSpans:           5,
		},
	}
}

// Load reads path (optional; "" means defaults only) and applies environment overrides.
func Load(path string) (Config, error) {
	var b []byte
	if path != "" {
		var err error
		if b, err = os.ReadFile(path); err != nil {
			return Default(), fmt.Errorf("read config: %w", err)
		}
	}
	// Each signal's type decides its defaults, so read the types first (from
	// the file, then the environment) and start from those defaults.
	var peek struct {
		Metrics struct{ Type string } `yaml:"metrics"`
		Logs    struct{ Type string } `yaml:"logs"`
		Traces  struct{ Type string } `yaml:"traces"`
	}
	_ = yaml.Unmarshal(b, &peek)
	types := Types{Metrics: peek.Metrics.Type, Logs: peek.Logs.Type, Traces: peek.Traces.Type}
	for dst, key := range map[*string]string{&types.Metrics: "SPOOR_METRICS_TYPE", &types.Logs: "SPOOR_LOGS_TYPE", &types.Traces: "SPOOR_TRACES_TYPE"} {
		if v := os.Getenv(key); v != "" {
			*dst = v
		}
	}
	cfg := DefaultFor(types)
	if path != "" {
		// Strict: an unknown key is almost always a typo that would otherwise
		// fall back to a default without anyone noticing.
		dec := yaml.NewDecoder(bytes.NewReader(b))
		dec.KnownFields(true)
		if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
			return cfg, fmt.Errorf("parse config %s: %w", path, err)
		}
	}
	if err := applyEnv(&cfg); err != nil {
		return cfg, err
	}
	return cfg, cfg.Validate()
}

func applyEnv(cfg *Config) error {
	set := func(dst *string, key string) {
		if v, ok := os.LookupEnv(key); ok && v != "" {
			*dst = v
		}
	}
	set(&cfg.Metrics.URL, "SPOOR_METRICS_URL")
	set(&cfg.Metrics.BearerToken, "SPOOR_METRICS_TOKEN")
	set(&cfg.Logs.URL, "SPOOR_LOGS_URL")
	set(&cfg.Logs.BearerToken, "SPOOR_LOGS_TOKEN")
	set(&cfg.Traces.URL, "SPOOR_TRACES_URL")
	set(&cfg.Traces.BearerToken, "SPOOR_TRACES_TOKEN")
	set(&cfg.Metrics.Username, "SPOOR_METRICS_USER")
	set(&cfg.Metrics.Password, "SPOOR_METRICS_PASSWORD")
	set(&cfg.Logs.Username, "SPOOR_LOGS_USER")
	set(&cfg.Logs.Password, "SPOOR_LOGS_PASSWORD")
	set(&cfg.Traces.Username, "SPOOR_TRACES_USER")
	set(&cfg.Traces.Password, "SPOOR_TRACES_PASSWORD")
	set(&cfg.Metrics.Type, "SPOOR_METRICS_TYPE")
	set(&cfg.Logs.Type, "SPOOR_LOGS_TYPE")
	set(&cfg.Traces.Type, "SPOOR_TRACES_TYPE")
	set(&cfg.NewRelic.APIKey, "NEW_RELIC_API_KEY")
	set(&cfg.NewRelic.APIKey, "SPOOR_NEWRELIC_API_KEY")
	set(&cfg.NewRelic.Region, "SPOOR_NEWRELIC_REGION")
	for _, key := range []string{"NEW_RELIC_ACCOUNT_ID", "SPOOR_NEWRELIC_ACCOUNT_ID"} {
		if v := os.Getenv(key); v != "" {
			id, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return fmt.Errorf("%s: want a numeric account ID, got %q", key, v)
			}
			cfg.NewRelic.AccountID = id
		}
	}
	if v := os.Getenv("SPOOR_REPORT_THRESHOLD"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return fmt.Errorf("SPOOR_REPORT_THRESHOLD: want a number between 0 and 1, got %q", v)
		}
		cfg.Scoring.ReportThreshold = f
	}
	return nil
}

// Types returns each signal's backend type, with empty meaning the
// VictoriaMetrics stack.
func (c Config) Types() Types {
	t := Types{Metrics: c.Metrics.Type, Logs: c.Logs.Type, Traces: c.Traces.Type}
	if t.Metrics == "" {
		t.Metrics = TypeVictoriaMetrics
	}
	if t.Logs == "" {
		t.Logs = TypeVictoriaLogs
	}
	if t.Traces == "" {
		t.Traces = TypeVictoriaTraces
	}
	return t
}

// UsesNewRelic reports whether any signal reads from New Relic.
func (c Config) UsesNewRelic() bool {
	return c.Metrics.Type == TypeNewRelic || c.Logs.Type == TypeNewRelic || c.Traces.Type == TypeNewRelic
}

// Validate checks templates parse and thresholds are sane.
func (c Config) Validate() error {
	for sig, typ := range map[string]string{"metrics": c.Metrics.Type, "logs": c.Logs.Type, "traces": c.Traces.Type} {
		if typ == "" {
			continue
		}
		ok := false
		for _, v := range validTypes[sig] {
			ok = ok || v == typ
		}
		if !ok {
			return fmt.Errorf("%s.type %q: want one of %s", sig, typ, strings.Join(validTypes[sig], ", "))
		}
	}
	if c.UsesNewRelic() && c.NewRelic.AccountID == 0 {
		return fmt.Errorf("newrelic.account_id is required when a signal has type newrelic (or set NEW_RELIC_ACCOUNT_ID)")
	}
	for name, q := range map[string]string{
		"metrics.request_rate": c.Metrics.RequestRate,
		"metrics.error_rate":   c.Metrics.ErrorRate,
		"metrics.latency_p95":  c.Metrics.LatencyP95,
	} {
		if _, err := Render(q, "x", time.Minute); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	for _, r := range c.Metrics.Resources {
		if r.Name == "" {
			return fmt.Errorf("metrics.resources: entry with empty name")
		}
		if _, err := Render(r.Query, "x", time.Minute); err != nil {
			return fmt.Errorf("metrics.resources[%s]: %w", r.Name, err)
		}
	}
	if c.Scoring.ReportThreshold < 0 || c.Scoring.ReportThreshold > 1 {
		return fmt.Errorf("scoring.report_threshold must be within 0..1")
	}
	if c.Scoring.BaselineSubWindows < 2 {
		return fmt.Errorf("scoring.baseline_sub_windows must be at least 2")
	}
	for name, rules := range map[string][]Rule{"logs.normalize": c.Logs.Normalize, "redact": c.Redact} {
		for i, r := range rules {
			if r.Regex == "" {
				return fmt.Errorf("%s[%d]: empty regex", name, i)
			}
			if _, err := regexp.Compile(r.Regex); err != nil {
				return fmt.Errorf("%s[%d]: %w", name, i, err)
			}
		}
	}
	if (c.Logs.URL != "" || c.Logs.Type == TypeNewRelic) && c.Logs.EffectiveErrorFilter() == "" {
		return fmt.Errorf("logs: set severity_fields or error_filter, or Spoor cannot find error lines")
	}
	return nil
}

// Render fills a metrics query template in the configured backend's
// language: the service is quoted as a PromQL string, or an NRQL one.
func (m MetricsConfig) Render(tmpl, service string, window time.Duration) (string, error) {
	if m.Type == TypeNewRelic {
		return render(tmpl, NRQLString(service), window)
	}
	return Render(tmpl, service, window)
}

// Render fills a query template with a service name and a window. The service
// value is emitted as a quoted MetricsQL string literal, so callers write
// {service_name={{.Service}}} without adding quotes themselves.
func Render(tmpl, service string, window time.Duration) (string, error) {
	return render(tmpl, strconv.Quote(service), window)
}

func render(tmpl, quoted string, window time.Duration) (string, error) {
	t, err := template.New("q").Option("missingkey=error").Parse(tmpl)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	err = t.Execute(&buf, struct {
		Service string
		Window  string
	}{Service: quoted, Window: PromDuration(window)})
	return buf.String(), err
}

// PromDuration formats d as a MetricsQL duration (e.g. 30m, 90s).
func PromDuration(d time.Duration) string {
	s := int64(d / time.Second)
	switch {
	case s <= 0:
		return "1s"
	case s%3600 == 0:
		return fmt.Sprintf("%dh", s/3600)
	case s%60 == 0:
		return fmt.Sprintf("%dm", s/60)
	default:
		return fmt.Sprintf("%ds", s)
	}
}
