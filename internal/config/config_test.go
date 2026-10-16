package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRenderQuotesService(t *testing.T) {
	q, err := Render(`up{service_name={{.Service}}}[{{.Window}}]`, `pay"ment`, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if want := `up{service_name="pay\"ment"}[30m]`; q != want {
		t.Errorf("Render = %s, want %s", q, want)
	}
}

func TestRenderRejectsUnknownField(t *testing.T) {
	if _, err := Render(`{{.Nope}}`, "x", time.Minute); err == nil {
		t.Error("want error for unknown template field")
	}
}

func TestPromDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		2 * time.Hour:           "2h",
		30 * time.Minute:        "30m",
		90 * time.Second:        "90s",
		0:                       "1s",
		1500 * time.Millisecond: "1s",
	} {
		if got := PromDuration(d); got != want {
			t.Errorf("PromDuration(%s) = %s, want %s", d, got, want)
		}
	}
}

func TestDefaultsValidate(t *testing.T) {
	if err := Default().Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestLoadFileAndEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "spoor.yaml")
	yaml := `
metrics:
  url: http://vm.internal:8428
  ui_url: https://grafana.example.com
  service_label: job
logs:
  service_field: service
scoring:
  report_threshold: 0.6
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SPOOR_LOGS_URL", "http://vl.internal:9428")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Metrics.URL != "http://vm.internal:8428" || cfg.Metrics.UIURL != "https://grafana.example.com" || cfg.Metrics.ServiceLabel != "job" {
		t.Errorf("metrics config not loaded: %+v", cfg.Metrics.Backend)
	}
	if cfg.Logs.URL != "http://vl.internal:9428" {
		t.Errorf("env override not applied: %s", cfg.Logs.URL)
	}
	if cfg.Logs.ServiceField != "service" || cfg.Scoring.ReportThreshold != 0.6 {
		t.Errorf("fields not loaded: %+v %+v", cfg.Logs, cfg.Scoring)
	}
	// Unset keys keep their defaults.
	if cfg.Traces.URL != "http://localhost:10428" || cfg.Metrics.RequestRate == "" {
		t.Errorf("defaults lost: traces=%s", cfg.Traces.URL)
	}
}

func TestValidateErrors(t *testing.T) {
	bad := []func(*Config){
		func(c *Config) { c.Metrics.ErrorRate = "{{" },
		func(c *Config) { c.Metrics.Resources = append(c.Metrics.Resources, ResourceQuery{Query: "x"}) },
		func(c *Config) { c.Scoring.ReportThreshold = 1.5 },
		func(c *Config) { c.Scoring.BaselineSubWindows = 1 },
	}
	for i, mut := range bad {
		c := Default()
		mut(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("case %d: want validation error", i)
		}
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err == nil || !strings.Contains(err.Error(), "read config") {
		t.Errorf("err = %v", err)
	}
}

func TestLoadRejectsUnknownKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spoor.yaml")
	if err := os.WriteFile(path, []byte("metrics:\n  servce_label: job\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "servce_label") {
		t.Errorf("want an error naming the unknown key, got %v", err)
	}
	empty := filepath.Join(t.TempDir(), "empty.yaml")
	_ = os.WriteFile(empty, nil, 0o644)
	if _, err := Load(empty); err != nil {
		t.Errorf("an empty file should load defaults, got %v", err)
	}
}

// Every config shipped in the repo must load strictly.
func TestShippedConfigsLoad(t *testing.T) {
	for _, p := range []string{"../../examples/spoor.yaml", "../../deploy/demo/spoor.yaml", "../../deploy/docker/spoor.yaml"} {
		if _, err := Load(p); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
}

func TestLoadRejectsBadThresholdEnv(t *testing.T) {
	t.Setenv("SPOOR_REPORT_THRESHOLD", "abc")
	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "SPOOR_REPORT_THRESHOLD") {
		t.Fatalf("want SPOOR_REPORT_THRESHOLD error, got %v", err)
	}
}
