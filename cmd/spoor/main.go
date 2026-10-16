// Command spoor is an MCP server (and CLI) that tells an AI agent what changed
// in a service across metrics, logs and traces.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/asinha07/spoor/internal/backend"
	"github.com/asinha07/spoor/internal/config"
	"github.com/asinha07/spoor/internal/digest"
	"github.com/asinha07/spoor/internal/server"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `spoor: tells an AI agent what changed, across metrics, logs and traces.

Usage:
  spoor serve [--http :8080] [--auth-token T]       run the MCP server (stdio by default)
              [--config spoor.yaml] [--log-format text|json] [--log-level info]
  spoor what-changed  [flags]                        rank changes across all signals
  spoor new-errors    [flags]                        new or spiking error log patterns
  spoor trace-diff    [flags] [--operation NAME]     slower or failing spans
  spoor list-services [--since 30m] [--until now]    services and the signals they have
  spoor doctor        [--since 15m] [--json]         check what Spoor can see; suggest config
  spoor version

Common flags: --service NAME (omit for all), --since 30m, --until now,
--baseline previous|1d|7d, --budget 800, --config FILE, --pretty,
--stats (print backend request count and time to stderr).
Backends default to localhost; override with SPOOR_METRICS_URL, SPOOR_LOGS_URL, SPOOR_TRACES_URL.
`

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "spoor:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		_, err := fmt.Fprint(stdout, usage)
		return err
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "version", "--version", "-v":
		_, err := fmt.Fprintln(stdout, version)
		return err
	case "help", "--help", "-h":
		_, err := fmt.Fprint(stdout, usage)
		return err
	case "serve":
		return serve(rest)
	case "what-changed", "new-errors", "trace-diff", "list-services", "doctor":
		return query(cmd, rest, stdout)
	}
	return fmt.Errorf("unknown command %q (run 'spoor help')", cmd)
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	httpAddr := fs.String("http", "", "serve streamable HTTP on this address instead of stdio (e.g. :8080)")
	cfgPath := fs.String("config", os.Getenv("SPOOR_CONFIG"), "config file (YAML)")
	authToken := fs.String("auth-token", os.Getenv("SPOOR_HTTP_TOKEN"), "require this bearer token on MCP requests over HTTP (env SPOOR_HTTP_TOKEN)")
	logFormat := fs.String("log-format", envOr("SPOOR_LOG_FORMAT", "text"), "log format: text or json")
	logLevel := fs.String("log-level", envOr("SPOOR_LOG_LEVEL", "info"), "log level: debug, info, warn or error")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := setupLogging(*logFormat, *logLevel); err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	srv := server.New(digest.NewFromConfig(cfg), version)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *httpAddr == "" {
		// stdout belongs to the protocol; logs go to stderr.
		return srv.Run(ctx, &mcp.StdioTransport{})
	}
	if *authToken == "" {
		slog.Warn("serving MCP over HTTP without authentication; set --auth-token or SPOOR_HTTP_TOKEN, or keep it on a private network")
	}
	hs := &http.Server{Addr: *httpAddr, Handler: server.HTTPHandler(srv, server.HTTPOptions{AuthToken: *authToken}), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = hs.Shutdown(sctx)
	}()
	slog.Info("serving MCP over HTTP", slog.String("version", version), slog.String("addr", *httpAddr),
		slog.Bool("auth", *authToken != ""), slog.String("metrics", "/metrics"), slog.String("health", "/healthz"))
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func query(cmd string, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	cfgPath := fs.String("config", os.Getenv("SPOOR_CONFIG"), "config file (YAML)")
	service := fs.String("service", "", "service name (omit to scan all services)")
	operation := fs.String("operation", "", "span name (trace-diff only)")
	since := fs.String("since", "", "window start: RFC 3339 or duration ago (default 30m)")
	until := fs.String("until", "", "window end: RFC 3339 or duration ago (default now)")
	baseline := fs.String("baseline", "", "previous (default), 1d or 7d")
	budget := fs.Int("budget", 0, "response size cap in tokens (default 800)")
	pretty := fs.Bool("pretty", false, "indent JSON output")
	stats := fs.Bool("stats", false, "print backend request count and time to stderr")
	asJSON := fs.Bool("json", false, "doctor: print JSON instead of text")
	if err := fs.Parse(args); err != nil {
		return err
	}
	started := time.Now()
	if *stats {
		defer func() {
			n, d := backend.Stats()
			fmt.Fprintf(os.Stderr, "spoor: %d backend requests, %.2fs waiting on backends, %.2fs total\n", n, d.Seconds(), time.Since(started).Seconds())
		}()
	}
	if cmd == "doctor" && *since == "" {
		*since = "15m"
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	eng := digest.NewFromConfig(cfg)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	req := digest.Request{Service: *service, Operation: *operation, Since: *since, Until: *until, Baseline: *baseline, BudgetTokens: *budget}

	var out any
	switch cmd {
	case "what-changed":
		out, err = eng.WhatChanged(ctx, req)
	case "new-errors":
		out, err = eng.NewErrors(ctx, req)
	case "trace-diff":
		out, err = eng.TraceDiff(ctx, req)
	case "list-services":
		out, err = eng.ListServices(ctx, *since, *until)
	case "doctor":
		var rep digest.DoctorReport
		rep, err = eng.Doctor(ctx, *since, *until)
		if err == nil && !*asJSON {
			return printDoctor(stdout, rep)
		}
		out = rep
	}
	if err != nil {
		return err
	}
	enc := json.NewEncoder(stdout)
	enc.SetEscapeHTML(false)
	if *pretty {
		enc.SetIndent("", "  ")
	}
	return enc.Encode(out)
}

func printDoctor(w io.Writer, rep digest.DoctorReport) error {
	marks := map[string]string{"ok": "ok  ", "warn": "WARN", "fail": "FAIL", "info": "    "}
	var b strings.Builder
	fmt.Fprintf(&b, "spoor doctor, window %s\n", rep.Window)
	sig := ""
	for _, c := range rep.Checks {
		if c.Signal != sig {
			sig = c.Signal
			fmt.Fprintf(&b, "\n%s\n", sig)
		}
		fmt.Fprintf(&b, "  %s %s\n", marks[c.Status], c.Message)
	}
	if rep.Suggested != "" {
		fmt.Fprintf(&b, "\nSuggested config (review before use):\n\n%s", rep.Suggested)
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// setupLogging sends structured logs to stderr (stdout carries the MCP
// protocol in stdio mode).
func setupLogging(format, level string) error {
	var lv slog.Level
	if err := lv.UnmarshalText([]byte(level)); err != nil {
		return fmt.Errorf("--log-level: %w", err)
	}
	opts := &slog.HandlerOptions{Level: lv}
	switch format {
	case "text":
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, opts)))
	case "json":
		slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, opts)))
	default:
		return fmt.Errorf("--log-format must be text or json")
	}
	return nil
}
