//go:build integration

package integration

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/asinha07/spoor/internal/backend"
	"github.com/asinha07/spoor/internal/config"
	"github.com/asinha07/spoor/internal/digest"
	"github.com/asinha07/spoor/internal/gen"
)

// TestMessyLogs checks Spoor on production-shaped logs (see gen.GenerateMessy):
// with a mapped config it must find exactly the two real changes despite IDs,
// stack traces, mixed level fields and text-only levels; with the default
// config it must say it is blind, not "no change"; and doctor must suggest
// the mapping.
func TestMessyLogs(t *testing.T) {
	ctx := context.Background()
	requireHealthy(t)
	// Far from the other scenarios' time slots, so baselines do not overlap.
	end := time.Now().Add(-40 * time.Hour).UTC().Truncate(time.Minute)
	truth, err := gen.GenerateMessy(ctx, gen.HTTPSink{LogsURL: logsURL}, end, 30*time.Minute, 6, 7)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Second)
	req := digest.Request{Since: truth.Since, Until: truth.Until, BudgetTokens: digest.MaxBudget}

	t.Run("mapped", func(t *testing.T) {
		cfg, err := config.Load(filepath.Join(repoRoot(t), "deploy", "test", "messy.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		cfg.Logs.URL = logsURL
		eng := digest.NewFromConfig(cfg)
		before, _ := backend.Stats()
		env, err := eng.WhatChanged(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		after, _ := backend.Stats()
		t.Logf("mapped scan: %d backend requests", after-before)
		// Bucketed line totals must line up with the window: Spoor's total
		// must equal a direct count over exactly the window.
		rows, err := backend.NewLogsQL(logsURL, "", time.Minute).Query(ctx, `container:* | stats count() n`, mustTime(t, truth.Since), mustTime(t, truth.Until), 0)
		if err != nil || len(rows) == 0 {
			t.Fatalf("direct count: %v", err)
		}
		want := fmt.Sprintf(" in %.0f log lines", rows[0].Float("n"))
		if !strings.Contains(env.Coverage["logs_lines"], want) {
			t.Errorf("window line total misaligned: coverage %q, direct count%s", env.Coverage["logs_lines"], want)
		}
		got := map[string]string{}
		for _, f := range env.Findings {
			got[f.Service] = f.Kind
		}
		for svc, kind := range truth.Expect {
			if got[svc] != kind {
				t.Errorf("%s: want %s, got %q", svc, kind, got[svc])
			}
		}
		for _, svc := range truth.Quiet {
			if k, ok := got[svc]; ok {
				t.Errorf("%s should be quiet, got %s", svc, k)
			}
		}
		steady := map[string]bool{}
		for _, s := range env.SteadyErrors {
			steady[s.Service] = true
		}
		if !steady["search"] {
			t.Errorf("search's errors rose with its traffic and should be steady: %+v", env.SteadyErrors)
		}
		if !strings.Contains(env.Coverage["logs_lines"], "no level/log.level field") {
			t.Errorf("coverage should report unclassified lines: %v", env.Coverage)
		}
		for _, f := range env.Findings {
			if strings.Contains(f.Title+f.Example, "ses_") && !strings.Contains(f.Title+f.Example, "<ID>") {
				t.Errorf("an ID survived: %q / %q", f.Title, f.Example)
			}
		}
		if n := after - before; n > 8 {
			t.Errorf("logs-only scan used %d backend requests, want at most 8", n)
		}
		logTop(t, env)
	})

	t.Run("default-config-is-honest", func(t *testing.T) {
		cfg := config.Default()
		cfg.Metrics.URL, cfg.Traces.URL, cfg.Logs.URL = "", "", logsURL
		env, err := digest.NewFromConfig(cfg).WhatChanged(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(env.Summary, "service.name") || !strings.Contains(env.Summary, "not evidence of health") {
			t.Errorf("default config must explain it is blind, got %q", env.Summary)
		}
		if strings.Contains(env.Summary, "No significant change") || strings.HasPrefix(env.Summary, "No data") {
			t.Errorf("a blind scan must lead with the blind spot, not with no-change wording: %q", env.Summary)
		}
	})

	t.Run("doctor", func(t *testing.T) {
		cfg := config.Default()
		cfg.Metrics.URL, cfg.Traces.URL, cfg.Logs.URL = "", "", logsURL
		rep, err := digest.NewFromConfig(cfg).Doctor(ctx, truth.Since, truth.Until)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"service_field: container", "severity_fields: [level, log.level]", "_msg:~"} {
			if !strings.Contains(rep.Suggested, want) {
				t.Errorf("suggested config lacks %q:\n%s", want, rep.Suggested)
			}
		}
		failed := false
		for _, c := range rep.Checks {
			failed = failed || (c.Status == "fail" && strings.Contains(c.Message, "matches 0 lines"))
		}
		if !failed {
			t.Errorf("doctor should fail the error filter that matches nothing: %+v", rep.Checks)
		}
	})
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return ts
}
