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
	"strconv"
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
}

// Backend is the shared connection block of each backend.
type Backend struct {
	URL         string `yaml:"url"`
	BearerToken string `yaml:"bearer_token"`
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
	// ErrorFilter is a LogsQL filter selecting error-level lines.
	ErrorFilter string `yaml:"error_filter"`
}

// TracesConfig describes VictoriaTraces access and field names.
type TracesConfig struct {
	Backend       `yaml:",inline"`
	ServiceField  string `yaml:"service_field"`
	NameField     string `yaml:"name_field"`
	DurationField string `yaml:"duration_field"` // nanoseconds
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
			Backend:      Backend{URL: "http://localhost:9428"},
			ServiceField: "service.name",
			ErrorFilter:  `severity_text:~"(?i)^(error|fatal|critical)$"`,
		},
		Traces: TracesConfig{
			Backend:       Backend{URL: "http://localhost:10428"},
			ServiceField:  "resource_attr:service.name",
			NameField:     "name",
			DurationField: "duration",
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
