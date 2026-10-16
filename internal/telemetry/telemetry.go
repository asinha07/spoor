// Package telemetry exposes Spoor's own metrics in Prometheus text format, so
// an operator can monitor Spoor with the same stack it reads from.
package telemetry

import (
	"fmt"
	"io"
	"time"

	"github.com/VictoriaMetrics/metrics"
)

var set = metrics.NewSet()

// ToolCall records one MCP tool call. status is "ok" or "error".
func ToolCall(tool, status string, d time.Duration) {
	set.GetOrCreateCounter(fmt.Sprintf(`spoor_tool_calls_total{tool=%q,status=%q}`, tool, status)).Inc()
	set.GetOrCreateHistogram(fmt.Sprintf(`spoor_tool_duration_seconds{tool=%q}`, tool)).Update(d.Seconds())
}

// BackendRequest records one request to a backend. status is "ok", "error"
// (transport failure) or the HTTP status code.
func BackendRequest(backend, status string, d time.Duration) {
	set.GetOrCreateCounter(fmt.Sprintf(`spoor_backend_requests_total{backend=%q,status=%q}`, backend, status)).Inc()
	set.GetOrCreateHistogram(fmt.Sprintf(`spoor_backend_request_duration_seconds{backend=%q}`, backend)).Update(d.Seconds())
}

// Findings records how many findings a tool call returned.
func Findings(tool string, n int) {
	set.GetOrCreateHistogram(fmt.Sprintf(`spoor_findings{tool=%q}`, tool)).Update(float64(n))
}

// WritePrometheus writes all Spoor metrics plus Go runtime and process metrics.
func WritePrometheus(w io.Writer) {
	set.WritePrometheus(w)
	metrics.WriteProcessMetrics(w)
}
