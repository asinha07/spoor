// Package digest turns raw telemetry into a short, ranked list of what changed.
//
// It contains one scorer per signal (metrics, logs, traces), a ranker that
// merges and correlates their findings, and a budgeter that cuts the result to
// the caller's token budget. Everything is deterministic: the same backend
// data always produces the same output.
package digest

import (
	"context"
	"time"

	"github.com/asinha07/spoor/internal/backend"
)

// Signal names.
const (
	SignalMetrics = "metrics"
	SignalLogs    = "logs"
	SignalTraces  = "traces"
)

// Finding kinds.
const (
	KindErrorRateUp       = "error_rate_up"
	KindLatencyUp         = "latency_up"
	KindThroughputDown    = "throughput_down"
	KindThroughputUp      = "throughput_up"
	KindResourceUp        = "resource_up"
	KindNewLogPattern     = "new_log_pattern"
	KindSpikingLogPattern = "spiking_log_pattern"
	KindSpanErrorsUp      = "span_errors_up"
	KindSpanSlower        = "span_slower"
)

// Finding is one ranked observation. Fields are short strings so the JSON
// stays compact for agents.
type Finding struct {
	Service      string  `json:"service"`
	Signal       string  `json:"signal"`
	Kind         string  `json:"kind"`
	Score        float64 `json:"score"`
	Title        string  `json:"title"`
	Before       string  `json:"before,omitempty"`
	After        string  `json:"after,omitempty"`
	Onset        string  `json:"onset,omitempty"`
	Example      string  `json:"example,omitempty"`
	TraceID      string  `json:"trace_id,omitempty"`
	EvidenceLink string  `json:"evidence_link,omitempty"`

	onset time.Time
}

// Envelope is the common response of every digest tool.
type Envelope struct {
	Summary  string    `json:"summary"`
	Window   string    `json:"window"`
	Baseline string    `json:"baseline"`
	Findings []Finding `json:"findings"`
	// SteadyErrors lists services whose error logs run at their usual rate:
	// they predate the window and are unlikely to explain a new problem.
	SteadyErrors []SteadyError     `json:"steady_errors,omitempty"`
	Truncated    int               `json:"truncated"`
	Coverage     map[string]string `json:"coverage"`
}

// SteadyError describes a service's error logs running at their baseline rate.
type SteadyError struct {
	Service     string  `json:"service"`
	ErrorsNow   float64 `json:"errors_now"`   // error lines in the window
	ErrorsUsual float64 `json:"errors_usual"` // average per baseline window
	Example     string  `json:"example"`      // the most frequent pattern
}

// Request carries the common tool inputs.
type Request struct {
	Service      string
	Operation    string // trace_diff only
	Since        string
	Until        string
	Baseline     string
	BudgetTokens int
}

// MetricsSource is the subset of the VictoriaMetrics client Spoor uses.
type MetricsSource interface {
	Instant(ctx context.Context, q string, at time.Time) ([]backend.Series, error)
	Range(ctx context.Context, q string, start, end time.Time, step time.Duration) ([]backend.Series, error)
	LabelValues(ctx context.Context, label string, start, end time.Time) ([]string, error)
}

// LogsSource is the subset of the LogsQL client Spoor uses (logs and traces).
type LogsSource interface {
	Query(ctx context.Context, q string, start, end time.Time, limit int) ([]backend.Row, error)
}

// Budget limits.
const (
	DefaultBudget = 800
	MinBudget     = 200
	MaxBudget     = 4000
)

// ClampBudget applies the default and limits.
func ClampBudget(b int) int {
	switch {
	case b == 0:
		return DefaultBudget
	case b < MinBudget:
		return MinBudget
	case b > MaxBudget:
		return MaxBudget
	}
	return b
}
