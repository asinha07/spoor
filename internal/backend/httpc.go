// Package backend holds thin, read-only HTTP clients for VictoriaMetrics
// (MetricsQL) and for VictoriaLogs/VictoriaTraces (LogsQL).
//
// Spoor never writes to a backend: every request goes to a query endpoint.
// AllowedPaths lists them so tests can assert the read-only guarantee.
package backend

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/asinha07/spoor/internal/telemetry"
)

// AllowedPaths are the only endpoints Spoor ever calls.
var AllowedPaths = []string{
	"/api/v1/query",
	"/api/v1/query_range",
	"/api/v1/label/",
	"/select/logsql/query",
}

// ErrUnavailable wraps transport-level failures so callers can mark a
// signal's coverage as missing rather than failing the whole tool call.
var ErrUnavailable = errors.New("backend unavailable")

type httpClient struct {
	name  string // backend label for Spoor's own metrics
	base  string
	token string
	hc    *http.Client
}

func newHTTP(base, token string, timeout time.Duration) httpClient {
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	return httpClient{name: "unnamed", base: strings.TrimRight(base, "/"), token: token, hc: &http.Client{Timeout: timeout}}
}

// post sends a form-encoded POST and returns the body.
func (c httpClient) post(ctx context.Context, path string, form url.Values) ([]byte, error) {
	if c.base == "" {
		return nil, fmt.Errorf("%w: no URL configured", ErrUnavailable)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "spoor")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	started := time.Now()
	resp, err := c.hc.Do(req)
	if err != nil {
		telemetry.BackendRequest(c.name, "error", time.Since(started))
		slog.Debug("backend request failed", slog.String("backend", c.name), slog.String("path", path),
			slog.String("query", form.Get("query")), slog.Duration("duration", time.Since(started)), slog.String("error", err.Error()))
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	defer func() {
		d := time.Since(started)
		telemetry.BackendRequest(c.name, statusLabel(resp.StatusCode), d)
		slog.Debug("backend request", slog.String("backend", c.name), slog.String("path", path),
			slog.String("query", form.Get("query")), slog.Int("status", resp.StatusCode), slog.Duration("duration", d))
	}()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("%w: reading %s: %v", ErrUnavailable, path, err)
	}
	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(body))
		if len(msg) > 300 {
			msg = msg[:300] + "…"
		}
		if resp.StatusCode >= 500 || resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return nil, fmt.Errorf("%w: %s returned %d: %s", ErrUnavailable, path, resp.StatusCode, msg)
		}
		return nil, fmt.Errorf("%s returned %d: %s", path, resp.StatusCode, msg)
	}
	return body, nil
}

func statusLabel(code int) string {
	if code == http.StatusOK {
		return "ok"
	}
	return strconv.Itoa(code)
}
