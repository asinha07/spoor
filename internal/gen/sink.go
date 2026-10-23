package gen

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// HTTPSink writes to VictoriaMetrics, VictoriaLogs and VictoriaTraces.
type HTTPSink struct {
	MetricsURL, LogsURL, TracesURL string
	Client                         *http.Client
}

func (s HTTPSink) client() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	return &http.Client{Timeout: 2 * time.Minute}
}

func (s HTTPSink) post(ctx context.Context, url, contentType string, body []byte, opts ...func(*http.Request)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	for _, o := range opts {
		o(req)
	}
	resp, err := s.client().Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }() // read-only body; nothing to recover
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s: %d %s", url, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}

// Metrics imports Prometheus text with timestamps.
func (s HTTPSink) Metrics(ctx context.Context, b []byte) error {
	return s.post(ctx, strings.TrimRight(s.MetricsURL, "/")+"/api/v1/import/prometheus", "text/plain", b)
}

// Logs imports JSON lines. VictoriaLogs ignores the body without this content type.
func (s HTTPSink) Logs(ctx context.Context, b []byte) error {
	u := strings.TrimRight(s.LogsURL, "/") + "/insert/jsonline?_stream_fields=service.name&_time_field=_time&_msg_field=_msg"
	return s.post(ctx, u, "application/stream+json", b)
}

// Spans imports OTLP/JSON traces.
func (s HTTPSink) Spans(ctx context.Context, b []byte) error {
	return s.post(ctx, strings.TrimRight(s.TracesURL, "/")+"/insert/opentelemetry/v1/traces", "application/json", b)
}

// Flush asks each backend to make recent writes searchable immediately.
func (s HTTPSink) Flush(ctx context.Context) {
	for _, base := range []string{s.MetricsURL, s.LogsURL, s.TracesURL} {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/internal/force_flush", nil)
		if err != nil {
			continue
		}
		if resp, err := s.client().Do(req); err == nil {
			_ = resp.Body.Close() // best-effort flush; the response is not used
		}
	}
}
