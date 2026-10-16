// Command spoorgen writes synthetic microservice telemetry with an injected
// fault to VictoriaMetrics, VictoriaLogs and VictoriaTraces, and prints the
// scenario's ground truth. It stands in for the OpenTelemetry Demo when that
// is too heavy to run.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/asinha07/spoor/internal/gen"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "spoorgen:", err)
		os.Exit(1)
	}
}

func run() error {
	scenario := flag.String("scenario", "all", "scenario name, or 'all' (each placed in its own time slot)")
	window := flag.Duration("window", 30*time.Minute, "length of the fault window")
	history := flag.Int("history", 6, "baseline windows of healthy data before the fault")
	end := flag.String("end", "", "end of the (first) fault window, RFC 3339 (default: one minute ago)")
	slot := flag.Duration("slot", 5*time.Hour, "time between scenarios when generating all")
	metrics := flag.String("metrics-url", env("SPOOR_METRICS_URL", "http://localhost:8428"), "VictoriaMetrics URL")
	logs := flag.String("logs-url", env("SPOOR_LOGS_URL", "http://localhost:9428"), "VictoriaLogs URL")
	traces := flag.String("traces-url", env("SPOOR_TRACES_URL", "http://localhost:10428"), "VictoriaTraces URL")
	out := flag.String("truth-dir", "", "write <scenario>.truth.json files here")
	seed := flag.Int64("seed", 42, "random seed")
	list := flag.Bool("list", false, "list scenarios and exit")
	flag.Parse()

	if *list {
		for _, s := range gen.Scenarios {
			fmt.Printf("%-26s %s\n", s.Name, s.Description)
		}
		return nil
	}
	faultEnd := time.Now().Add(-time.Minute).UTC().Truncate(time.Minute)
	if *end != "" {
		t, err := time.Parse(time.RFC3339, *end)
		if err != nil {
			return fmt.Errorf("--end: %w", err)
		}
		faultEnd = t
	}
	var scenarios []gen.Scenario
	if *scenario == "all" {
		scenarios = gen.Scenarios
	} else {
		s, err := gen.ScenarioByName(*scenario)
		if err != nil {
			return err
		}
		scenarios = []gen.Scenario{s}
	}
	sink := gen.HTTPSink{MetricsURL: *metrics, LogsURL: *logs, TracesURL: *traces}
	ctx := context.Background()
	var truths []gen.GroundTruth
	for i, s := range scenarios {
		fe := faultEnd.Add(-time.Duration(i) * *slot)
		plan := gen.Plan{
			Scenario:   s,
			FaultEnd:   fe,
			FaultStart: fe.Add(-*window),
			// One extra window of lead-in so the oldest baseline window has full data.
			DataStart: fe.Add(-*window * time.Duration(*history+2)),
			Seed:      *seed + int64(i),
		}
		started := time.Now()
		st, err := gen.New(plan).Run(ctx, sink)
		if err != nil {
			return fmt.Errorf("%s: %w", s.Name, err)
		}
		fmt.Fprintf(os.Stderr, "%-26s %7d requests, %6d spans, %6d log lines, %7d samples in %s\n",
			s.Name, st.Requests, st.Spans, st.LogLines, st.Samples, time.Since(started).Round(time.Millisecond))
		truths = append(truths, plan.Truth())
		if *out != "" {
			if err := os.MkdirAll(*out, 0o755); err != nil {
				return err
			}
			b, _ := json.MarshalIndent(plan.Truth(), "", "  ")
			if err := os.WriteFile(filepath.Join(*out, s.Name+".truth.json"), append(b, '\n'), 0o644); err != nil {
				return err
			}
		}
	}
	sink.Flush(ctx)
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(truths)
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
