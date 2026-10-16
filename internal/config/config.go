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
	URL         string `yaml:"url"`
	BearerToken string `yaml:"bearer_token"`
	// Username and Password enable HTTP Basic auth (e.g. Grafana's
	// datasource proxy). Ignored when BearerToken is set.
	Username string `yaml:"username"`
	Password string `yaml:"password"`
	// UIURL is the base used for evidence links; defaults to URL.
	UIURL string `yaml:"ui_url"`
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

// EffectiveErrorFilter returns the LogsQL filter selecting error lines.
func (l LogsConfig) EffectiveErrorFilter() string {
	if l.ErrorFilter != "" {
		return l.ErrorFilter
	}
	if len(l.SeverityFields) == 0 {
		return ""
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
	parts := make([]string, 0, len(l.SeverityFields))
	for _, f := range l.SeverityFields {
		parts = append(parts, "NOT "+quoteField(f)+":*")
	}
	return strings.Join(parts, " ")
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

// Default returns the configuration used when no file is given. Query names
// follow the OpenTelemetry semantic conventions as exported to Prometheus.
func Default() Config {
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
	cfg := Default()
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return cfg, fmt.Errorf("read config: %w", err)
		}
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
	if v := os.Getenv("SPOOR_REPORT_THRESHOLD"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return fmt.Errorf("SPOOR_REPORT_THRESHOLD: want a number between 0 and 1, got %q", v)
		}
		cfg.Scoring.ReportThreshold = f
	}
	return nil
}

// Validate checks templates parse and thresholds are sane.
func (c Config) Validate() error {
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
	if c.Logs.URL != "" && c.Logs.EffectiveErrorFilter() == "" {
		return fmt.Errorf("logs: set severity_fields or error_filter, or Spoor cannot find error lines")
	}
	return nil
}

// Render fills a query template with a service name and a window. The service
// value is emitted as a quoted MetricsQL string literal, so callers write
// {service_name={{.Service}}} without adding quotes themselves.
func Render(tmpl, service string, window time.Duration) (string, error) {
	t, err := template.New("q").Option("missingkey=error").Parse(tmpl)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	err = t.Execute(&buf, struct {
		Service string
		Window  string
	}{Service: strconv.Quote(service), Window: PromDuration(window)})
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
