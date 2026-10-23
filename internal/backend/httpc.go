// Package backend holds thin, read-only HTTP clients for VictoriaMetrics
// (MetricsQL) and for VictoriaLogs/VictoriaTraces (LogsQL).
//
// Spoor never writes to a backend: every request goes to a query endpoint.
// AllowedPaths lists them so tests can assert the read-only guarantee.
package backend

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/asinha07/spoor/internal/telemetry"
)

// AllowedPaths are the only endpoints Spoor ever calls.
var AllowedPaths = []string{
	"/api/v1/query",
	"/api/v1/query_range",
	"/api/v1/label/",
	"/select/logsql/query",
	"/loki/api/v1/query",
	"/loki/api/v1/query_range",
	"/loki/api/v1/label/",
	"/api/search",
	"/api/v2/search/tag/",
	"/api/v2/traces/",
	"/graphql", // New Relic NerdGraph: NRQL queries only, never mutations
}

// ErrUnavailable wraps transport-level failures so callers can mark a
// signal's coverage as missing rather than failing the whole tool call.
var ErrUnavailable = errors.New("backend unavailable")

type httpClient struct {
	name  string // backend label for Spoor's own metrics
	base  string
	token string
	user  string // HTTP Basic auth, used when token is empty
	pass  string
	// header holds extra request headers (e.g. New Relic's API-Key).
	header map[string]string
	hc     *http.Client
}

var (
	requestCount atomic.Int64
	requestNanos atomic.Int64
)

// Stats returns how many backend requests this process has sent and the
// total time spent waiting for them.
func Stats() (requests int64, total time.Duration) {
	return requestCount.Load(), time.Duration(requestNanos.Load())
}

func newHTTP(base, token string, timeout time.Duration) httpClient {
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	return httpClient{name: "unnamed", base: strings.TrimRight(base, "/"), token: token, hc: &http.Client{Timeout: timeout}}
}

// post sends a form-encoded POST and returns the body.
func (c httpClient) post(ctx context.Context, path string, form url.Values) ([]byte, error) {
	return c.do(ctx, http.MethodPost, path, form.Get("query"), strings.NewReader(form.Encode()), "application/x-www-form-urlencoded")
}

// get sends a GET with query parameters and returns the body.
func (c httpClient) get(ctx context.Context, path string, params url.Values) ([]byte, error) {
	if len(params) > 0 {
		path += "?" + params.Encode()
	}
	return c.do(ctx, http.MethodGet, path, params.Get("query")+params.Get("q"), nil, "")
}

// postJSON sends a JSON body and returns the response body.
func (c httpClient) postJSON(ctx context.Context, path string, body []byte, logQuery string) ([]byte, error) {
	return c.do(ctx, http.MethodPost, path, logQuery, bytes.NewReader(body), "application/json")
}

func (c httpClient) do(ctx context.Context, method, path, logQuery string, body io.Reader, ctype string) ([]byte, error) {
	if c.base == "" {
		return nil, fmt.Errorf("%w: no URL configured", ErrUnavailable)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return nil, err
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	req.Header.Set("User-Agent", "spoor")
	for k, v := range c.header {
		req.Header.Set(k, v)
	}
	switch {
	case c.token != "":
		req.Header.Set("Authorization", "Bearer "+c.token)
	case c.user != "" || c.pass != "":
		req.SetBasicAuth(c.user, c.pass)
	}
	started := time.Now()
	requestCount.Add(1)
	defer func() { requestNanos.Add(int64(time.Since(started))) }()
	resp, err := c.hc.Do(req)
	if err != nil {
		telemetry.BackendRequest(c.name, "error", time.Since(started))
		slog.Debug("backend request failed", slog.String("backend", c.name), slog.String("path", path),
			slog.String("query", logQuery), slog.Duration("duration", time.Since(started)), slog.String("error", err.Error()))
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }() // read-only body; nothing to recover
	defer func() {
		d := time.Since(started)
		telemetry.BackendRequest(c.name, statusLabel(resp.StatusCode), d)
		slog.Debug("backend request", slog.String("backend", c.name), slog.String("path", path),
			slog.String("query", logQuery), slog.Int("status", resp.StatusCode), slog.Duration("duration", d))
	}()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("%w: reading %s: %v", ErrUnavailable, path, err)
	}
	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(out))
		if len(msg) > 300 {
			msg = msg[:300] + "…"
		}
		where := path
		if where == "" {
			where = c.name
		}
		if resp.StatusCode >= 500 || resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return nil, fmt.Errorf("%w: %s returned %d: %s", ErrUnavailable, where, resp.StatusCode, msg)
		}
		return nil, fmt.Errorf("%s returned %d: %s", where, resp.StatusCode, msg)
	}
	return out, nil
}

func statusLabel(code int) string {
	if code == http.StatusOK {
		return "ok"
	}
	return strconv.Itoa(code)
}
