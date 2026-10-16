// Package server exposes the digest engine as MCP tools.
package server

import (
	"context"
	"crypto/subtle"
	"log/slog"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/asinha07/spoor/internal/digest"
	"github.com/asinha07/spoor/internal/telemetry"
)

// Instructions are sent to MCP clients at initialization.
const Instructions = `Spoor answers "what changed?" for services monitored by VictoriaMetrics, VictoriaLogs and VictoriaTraces.
Start with what_changed (omit service to scan everything); it ranks changes across metrics, logs and traces against a baseline and returns a short summary plus findings with evidence links.
Use new_errors or trace_diff to dig into one signal, and list_services to find service names.
Every tool takes since/until (RFC 3339 or a duration ago like 30m), baseline and budget_tokens (response size cap).
Use the default baseline (previous: the windows just before) unless you know the backends hold data from a day or a week earlier; if coverage says the baseline has no data, retry with baseline=previous.
steady_errors lists services whose errors are not significantly above their usual level (change shows by how much, log_lines_change how traffic moved): they existed before the window and are unlikely to explain a new problem, so do not report them as the cause unless nothing else changed and the user's symptoms match them. A large percentage on small counts is noise.
Error log findings group messages that differ only by IDs or values into one error; "new" means that error did not occur in the baseline at all.
Findings are evidence, not conclusions. Read coverage: any signal state other than "ok" (an error, no data, an error filter that matched nothing, lines without a service or severity field) means Spoor may be blind there, and an empty result is then not evidence of health.`

// WindowInput is shared by the digest tools.
type WindowInput struct {
	Service      string `json:"service,omitempty" jsonschema:"Service name. Omit to scan every service seen in the window."`
	Since        string `json:"since,omitempty" jsonschema:"Window start: RFC 3339 time or a duration ago such as 30m or 2h. Default 30m."`
	Until        string `json:"until,omitempty" jsonschema:"Window end: RFC 3339 time or a duration ago. Default now."`
	Baseline     string `json:"baseline,omitempty" jsonschema:"What to compare against: previous (the windows just before; default), 1d or 7d (same window that many days earlier)."`
	BudgetTokens int    `json:"budget_tokens,omitempty" jsonschema:"Upper bound on the response size in tokens, 200 to 4000. Default 800."`
}

// TraceDiffInput adds an optional operation filter.
type TraceDiffInput struct {
	Service      string `json:"service,omitempty" jsonschema:"Service name. Omit to scan every service seen in the window."`
	Operation    string `json:"operation,omitempty" jsonschema:"Limit to one span name, e.g. 'POST /checkout'."`
	Since        string `json:"since,omitempty" jsonschema:"Window start: RFC 3339 time or a duration ago such as 30m or 2h. Default 30m."`
	Until        string `json:"until,omitempty" jsonschema:"Window end: RFC 3339 time or a duration ago. Default now."`
	Baseline     string `json:"baseline,omitempty" jsonschema:"What to compare against: previous (default), 1d or 7d."`
	BudgetTokens int    `json:"budget_tokens,omitempty" jsonschema:"Upper bound on the response size in tokens, 200 to 4000. Default 800."`
}

// ListInput is the list_services input.
type ListInput struct {
	Since string `json:"since,omitempty" jsonschema:"Window start: RFC 3339 time or a duration ago. Default 30m."`
	Until string `json:"until,omitempty" jsonschema:"Window end. Default now."`
}

// New returns an MCP server with Spoor's four tools.
func New(engine *digest.Engine, version string) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "spoor", Title: "Spoor", Version: version}, &mcp.ServerOptions{Instructions: Instructions})

	mcp.AddTool(s, &mcp.Tool{
		Name:  "what_changed",
		Title: "What changed",
		Description: "Ranked list of what changed in a service (or all services) across metrics, logs and traces, compared with a baseline. " +
			"Returns a one-paragraph summary, findings with before/after values, onset times and evidence links, and per-signal coverage. Start here.",
		Annotations: readOnly(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in WindowInput) (*mcp.CallToolResult, any, error) {
		return observe("what_changed", toRequest(in), func() (any, error) { return engine.WhatChanged(ctx, toRequest(in)) })
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:  "new_errors",
		Title: "New errors",
		Description: "Error logs compared with a baseline: errors that are new or spiking (messages that differ only by IDs count as one error), services whose error volume rose beyond their traffic, " +
			"each with counts, first-seen time and one redacted example line, plus services whose errors are steady.",
		Annotations: readOnly(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in WindowInput) (*mcp.CallToolResult, any, error) {
		return observe("new_errors", toRequest(in), func() (any, error) { return engine.NewErrors(ctx, toRequest(in)) })
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:  "trace_diff",
		Title: "Trace diff",
		Description: "Span names that got slower or started failing compared with a baseline: p95 and error rate before/after, " +
			"each span's share of the added latency, and an example trace ID.",
		Annotations: readOnly(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in TraceDiffInput) (*mcp.CallToolResult, any, error) {
		req := digest.Request{
			Service: in.Service, Operation: in.Operation, Since: in.Since, Until: in.Until,
			Baseline: in.Baseline, BudgetTokens: in.BudgetTokens,
		}
		return observe("trace_diff", req, func() (any, error) { return engine.TraceDiff(ctx, req) })
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_services",
		Title:       "List services",
		Description: "Services seen in the window and which signals (metrics, logs, traces) have data for each.",
		Annotations: readOnly(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in ListInput) (*mcp.CallToolResult, any, error) {
		return observe("list_services", digest.Request{Since: in.Since, Until: in.Until}, func() (any, error) {
			return engine.ListServices(ctx, in.Since, in.Until)
		})
	})
	return s
}

// HTTPOptions configure the HTTP transport.
type HTTPOptions struct {
	// AuthToken, when set, is required as "Authorization: Bearer <token>" on
	// MCP requests. /healthz and /metrics stay open for probes and scrapers.
	AuthToken string
}

// HTTPHandler serves the MCP server over streamable HTTP, plus /healthz and
// /metrics (Spoor's own metrics in Prometheus text format).
func HTTPHandler(s *mcp.Server, opts HTTPOptions) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		telemetry.WritePrometheus(w)
	})
	var mcpHandler http.Handler = mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, nil)
	if opts.AuthToken != "" {
		mcpHandler = requireToken(opts.AuthToken, mcpHandler)
	}
	mux.Handle("/", mcpHandler)
	return mux
}

// requireToken rejects requests without the expected bearer token, comparing
// in constant time.
func requireToken(token string, next http.Handler) http.Handler {
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("Authorization"))
		if subtle.ConstantTimeCompare(got, want) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="spoor"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// observe runs one tool call, then logs it and records Spoor's own metrics.
func observe(tool string, req digest.Request, call func() (any, error)) (*mcp.CallToolResult, any, error) {
	started := time.Now()
	out, err := call()
	d := time.Since(started)
	attrs := []any{
		slog.String("tool", tool), slog.Duration("duration", d),
		slog.String("service", req.Service), slog.String("since", req.Since), slog.String("until", req.Until),
	}
	if req.Baseline != "" {
		attrs = append(attrs, slog.String("baseline", req.Baseline))
	}
	if err != nil {
		telemetry.ToolCall(tool, "error", d)
		slog.Warn("tool call failed", append(attrs, slog.String("error", err.Error()))...)
		return reply(nil, err)
	}
	telemetry.ToolCall(tool, "ok", d)
	if env, ok := out.(digest.Envelope); ok {
		telemetry.Findings(tool, len(env.Findings))
		attrs = append(attrs, slog.Int("findings", len(env.Findings)), slog.Int("truncated", env.Truncated))
		for sig, state := range env.Coverage {
			if state != "ok" {
				attrs = append(attrs, slog.String("coverage_"+sig, state))
			}
		}
	}
	slog.Info("tool call", attrs...)
	return reply(out, nil)
}

func readOnly() *mcp.ToolAnnotations {
	f := false
	return &mcp.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: &f, IdempotentHint: true, OpenWorldHint: &f}
}

func toRequest(in WindowInput) digest.Request {
	return digest.Request{Service: in.Service, Since: in.Since, Until: in.Until, Baseline: in.Baseline, BudgetTokens: in.BudgetTokens}
}

// reply returns v as one compact JSON text block. Spoor deliberately sends no
// structured duplicate: the budget applies to exactly what the agent reads.
func reply(v any, err error) (*mcp.CallToolResult, any, error) {
	if err != nil {
		return nil, nil, err
	}
	b, err := digest.MarshalCompact(v)
	if err != nil {
		return nil, nil, err
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}, nil, nil
}
