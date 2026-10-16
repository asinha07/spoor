package digest_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/asinha07/spoor/internal/cases"
	"github.com/asinha07/spoor/internal/config"
	"github.com/asinha07/spoor/internal/digest"
	"github.com/asinha07/spoor/internal/gen"
	"github.com/asinha07/spoor/internal/replay"
)

var update = flag.Bool("update", false, "rewrite golden files from the current output")

const scenarioDir = "testdata/scenarios"

// loadScenario builds an engine that replays one recorded scenario.
func loadScenario(t *testing.T, name string) (*digest.Engine, gen.GroundTruth) {
	t.Helper()
	var truth gen.GroundTruth
	b, err := os.ReadFile(filepath.Join(scenarioDir, name+".truth.json"))
	if err != nil {
		t.Fatalf("read truth: %v", err)
	}
	if err := json.Unmarshal(b, &truth); err != nil {
		t.Fatal(err)
	}
	p, err := replay.Load(filepath.Join(scenarioDir, name+".fixture.json.gz"))
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	e := digest.New(config.Default(), p.Metrics(), p.Logs("logs"), p.Logs("traces"))
	clock := p.Clock()
	e.SetClock(func() (t0 time.Time) { return clock })
	return e, truth
}

func scenarioNames(t *testing.T) []string {
	t.Helper()
	matches, _ := filepath.Glob(filepath.Join(scenarioDir, "*.truth.json"))
	if len(matches) == 0 {
		t.Fatal("no recorded scenarios; run `make record`")
	}
	var names []string
	for _, m := range matches {
		names = append(names, strings.TrimSuffix(filepath.Base(m), ".truth.json"))
	}
	return names
}

// TestGolden replays every recorded scenario through every tool and compares
// the output byte for byte with the golden files. Any change in scoring,
// ranking, wording or queries shows up here; refresh deliberately with -update.
func TestGolden(t *testing.T) {
	for _, name := range scenarioNames(t) {
		t.Run(name, func(t *testing.T) {
			e, truth := loadScenario(t, name)
			for _, c := range cases.For(truth) {
				out, err := cases.Run(context.Background(), e, c)
				var nr replay.ErrNotRecorded
				if errors.As(err, &nr) {
					t.Fatalf("%s: query drifted from the recording (%v); re-record with `make record` if intended", c.Name, err)
				}
				if err != nil {
					t.Fatalf("%s: %v", c.Name, err)
				}
				got, _ := json.MarshalIndent(out, "", "  ")
				got = append(got, '\n')
				path := filepath.Join(scenarioDir, name, c.Name+".golden.json")
				if *update {
					if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, got, 0o644); err != nil {
						t.Fatal(err)
					}
					continue
				}
				want, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("%s: missing golden file (run `go test ./internal/digest -run TestGolden -update`): %v", c.Name, err)
				}
				if !bytes.Equal(got, want) {
					t.Errorf("%s: output differs from %s\n--- got ---\n%s", c.Name, path, got)
				}
			}
		})
	}
}

// TestScenarioGroundTruth checks the property the benchmark depends on: the
// faulty service ranks first, and the control scenario stays quiet.
func TestScenarioGroundTruth(t *testing.T) {
	for _, name := range scenarioNames(t) {
		t.Run(name, func(t *testing.T) {
			e, truth := loadScenario(t, name)
			env, err := e.WhatChanged(context.Background(), digest.Request{Since: truth.Since, Until: truth.Until, BudgetTokens: digest.MaxBudget})
			if err != nil {
				t.Fatal(err)
			}
			if truth.Service == "" {
				if len(env.Findings) != 0 {
					t.Fatalf("control: want no findings, got %d; first: %s", len(env.Findings), env.Findings[0].Title)
				}
				// Routine background errors must be named as routine, so an agent
				// does not blame them for the reported problem.
				if len(env.SteadyErrors) == 0 || !strings.Contains(env.Summary, "not a new problem") {
					t.Errorf("control: want steady errors in the summary, got %q", env.Summary)
				}
				return
			}
			for _, st := range env.SteadyErrors {
				if st.Service == truth.Service {
					t.Errorf("the faulty service %s is listed as steady", truth.Service)
				}
			}
			if len(env.Findings) == 0 {
				t.Fatalf("no findings; want %s first", truth.Service)
			}
			top := env.Findings[0]
			if top.Service != truth.Service {
				t.Errorf("top finding is %s (%s), want %s", top.Service, top.Title, truth.Service)
			}
			kinds := map[string]bool{}
			for _, f := range env.Findings {
				if f.Service == truth.Service {
					kinds[f.Kind] = true
				}
			}
			matched := false
			for _, k := range truth.Kinds {
				matched = matched || kinds[k]
			}
			if !matched {
				t.Errorf("no finding of an expected kind %v for %s; got %v", truth.Kinds, truth.Service, kinds)
			}
			if !strings.Contains(env.Summary, truth.Service) {
				t.Errorf("summary does not name %s: %s", truth.Service, env.Summary)
			}
		})
	}
}

// TestDownstreamPointer: asking about a caller during a downstream failure
// must point at the real cause.
func TestDownstreamPointer(t *testing.T) {
	e, truth := loadScenario(t, "payment-failure")
	// The recorded cases include what_changed for the faulty service only, so
	// check the all-services answer marks caller findings as echoes.
	env, err := e.WhatChanged(context.Background(), digest.Request{Since: truth.Since, Until: truth.Until, BudgetTokens: digest.MaxBudget})
	if err != nil {
		t.Fatal(err)
	}
	echoes := 0
	for _, f := range env.Findings {
		if f.Service != truth.Service && strings.Contains(f.Title, "originates in "+truth.Service) {
			echoes++
		}
	}
	if echoes == 0 {
		t.Errorf("no caller findings were attributed to %s", truth.Service)
	}
}
