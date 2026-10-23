//go:build integration && grafana

// Grafana-stack integration test: the same incidents as TestScenarios, loaded
// into real Prometheus, Loki and Tempo and read back through those backends.
//
//	make it-grafana   (starts deploy/test/grafana-compose.yaml first)
package integration

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/asinha07/spoor/internal/backend"
	"github.com/asinha07/spoor/internal/config"
	"github.com/asinha07/spoor/internal/digest"
	"github.com/asinha07/spoor/internal/gen"
)

var (
	promURL      = env("SPOOR_IT_PROMETHEUS_URL", "http://localhost:9090")
	lokiURL      = env("SPOOR_IT_LOKI_URL", "http://localhost:3100")
	tempoURL     = env("SPOOR_IT_TEMPO_URL", "http://localhost:3200")
	tempoOTLPURL = env("SPOOR_IT_TEMPO_OTLP_URL", "http://localhost:4318")
)

func grafanaConfig() config.Config {
	cfg := config.DefaultFor(config.Types{Metrics: config.TypePrometheus, Logs: config.TypeLoki, Traces: config.TypeTempo})
	cfg.Metrics.URL, cfg.Logs.URL, cfg.Traces.URL = promURL, lokiURL, tempoURL
	// spoorgen writes the level as severity_text, like OpenTelemetry logs.
	cfg.Logs.SeverityFields = []string{"severity_text"}
	return cfg
}

func TestGrafanaStack(t *testing.T) {
	ctx := context.Background()
	for _, u := range []string{promURL + "/-/ready", lokiURL + "/ready", tempoURL + "/ready"} {
		resp, err := http.Get(u)
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("%s not ready (start the stack with `make it-grafana-up`): %v", u, err)
		}
		resp.Body.Close()
	}
	if vals, _ := backend.NewMetrics(promURL, "", 10*time.Second).LabelValues(ctx, "service_name", time.Now().Add(-48*time.Hour), time.Now()); len(vals) > 0 {
		t.Fatalf("Prometheus already holds data for %v; reset with `make it-grafana-up`", vals)
	}

	sink := gen.GrafanaSink{PrometheusURL: promURL, LokiURL: lokiURL, TempoOTLPURL: tempoOTLPURL, ServiceFields: []string{"service.name"}}
	end := time.Now().Add(-2 * time.Minute).UTC().Truncate(time.Minute)
	var loaded []gen.Plan
	i := 0
	for _, s := range gen.Scenarios {
		if s.Name != "payment-failure" && s.Name != "control" {
			continue
		}
		fe := end.Add(-time.Duration(i) * slot)
		loaded = append(loaded, gen.Plan{Scenario: s, FaultEnd: fe, FaultStart: fe.Add(-30 * time.Minute), DataStart: fe.Add(-30 * time.Minute * 8), Seed: 42 + int64(i)})
		i++
	}
	// Oldest first: Loki rejects entries far behind the newest one in a stream.
	for j := len(loaded) - 1; j >= 0; j-- {
		if _, err := gen.New(loaded[j]).Run(ctx, sink); err != nil {
			t.Fatalf("generate %s: %v", loaded[j].Scenario.Name, err)
		}
	}
	waitForTempo(t, ctx, loaded)

	for _, p := range loaded {
		p := p
		truth := p.Truth()
		t.Run(p.Scenario.Name, func(t *testing.T) {
			eng := digest.NewFromConfig(grafanaConfig())
			clock := p.FaultEnd.Add(time.Minute)
			eng.SetClock(func() time.Time { return clock })
			env, err := eng.WhatChanged(ctx, digest.Request{Since: truth.Since, Until: truth.Until, BudgetTokens: 4000})
			if err != nil {
				t.Fatal(err)
			}
			logTop(t, env)
			t.Logf("coverage: %v", env.Coverage)
			for _, sig := range []string{digest.SignalMetrics, digest.SignalLogs, digest.SignalTraces} {
				if cov := env.Coverage[sig]; cov != "ok" {
					t.Errorf("coverage %s = %q, want ok", sig, cov)
				}
			}
			if !strings.Contains(env.Coverage["traces_sample"], "sample") {
				t.Errorf("Tempo statistics are sampled; coverage should say so: %v", env.Coverage)
			}
			if truth.Service == "" {
				if len(env.Findings) != 0 {
					t.Errorf("control: want no findings, got %d (top: %s)", len(env.Findings), env.Findings[0].Title)
				}
				return
			}
			if rank := rankOf(env.Findings, truth.Service); rank == 0 || rank > 3 {
				t.Errorf("ground-truth service %s ranked %d, want top 3", truth.Service, rank)
			}
			kinds := map[string]bool{}
			for _, f := range env.Findings {
				if f.Service == truth.Service {
					kinds[f.Kind] = true
				}
			}
			for _, k := range []string{digest.KindErrorOrigin, digest.KindNewLogPattern, digest.KindErrorRateUp} {
				if !kinds[k] {
					t.Errorf("want a %s finding for %s (Tempo attribution, Loki patterns, Prometheus metrics); got %v", k, truth.Service, kinds)
				}
			}
			for _, f := range env.Findings {
				if strings.Contains(f.Title, `Span ""`) {
					t.Errorf("span finding without a span name: %s", f.Title)
				}
			}
			echo := false
			for _, f := range env.Findings {
				echo = echo || (f.Service != truth.Service && strings.Contains(f.Title, "originates in "+truth.Service))
			}
			if !echo {
				t.Errorf("want at least one caller labelled as an echo of %s", truth.Service)
			}

			ls, err := eng.ListServices(ctx, truth.Since, truth.Until)
			if err != nil || len(ls.Services) < 3 {
				t.Errorf("list_services: %v, %d services", err, len(ls.Services))
			}
			rep, err := eng.Doctor(ctx, truth.Since, truth.Until)
			if err != nil {
				t.Fatal(err)
			}
			for _, c := range rep.Checks {
				t.Logf("doctor %s %s: %s", c.Signal, c.Status, c.Message)
				if c.Status == "fail" {
					t.Errorf("doctor fail on %s: %s", c.Signal, c.Message)
				}
			}
		})
	}
}

// waitForTempo waits until Tempo serves time-ranged searches for the loaded
// scenarios: test spans are written hours in the past and become searchable
// once their blocks are flushed.
func waitForTempo(t *testing.T, ctx context.Context, plans []gen.Plan) {
	tempo := backend.NewTempo(tempoURL, "", 10*time.Second)
	deadline := time.Now().Add(4 * time.Minute)
	for {
		ok := true
		for _, p := range plans {
			tr, err := tempo.Search(ctx, "{ }", p.DataStart, p.FaultEnd, 1, 1)
			ok = ok && err == nil && len(tr) > 0
		}
		if ok {
			// Let the last blocks land too.
			time.Sleep(30 * time.Second)
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("Tempo did not serve the test traces within 4 minutes")
		}
		time.Sleep(10 * time.Second)
	}
}
