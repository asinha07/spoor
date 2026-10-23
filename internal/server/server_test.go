package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/asinha07/spoor/internal/backend"
	"github.com/asinha07/spoor/internal/config"
	"github.com/asinha07/spoor/internal/digest"
	"github.com/asinha07/spoor/internal/gen"
	"github.com/asinha07/spoor/internal/replay"
)

const fixtures = "../digest/testdata/scenarios"

// replayEngine serves the recorded payment-failure scenario.
func replayEngine(t *testing.T) (*digest.Engine, gen.GroundTruth) {
	t.Helper()
	p, err := replay.Load(filepath.Join(fixtures, "payment-failure.fixture.json.gz"))
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(fixtures, "payment-failure.truth.json"))
	if err != nil {
		t.Fatal(err)
	}
	var truth gen.GroundTruth
	if err := json.Unmarshal(b, &truth); err != nil {
		t.Fatal(err)
	}
	e := digest.New(config.Default(), p.Metrics(), p.Logs("logs"), p.Logs("traces"))
	clock := p.Clock()
	e.SetClock(func() time.Time { return clock })
	return e, truth
}

func connect(t *testing.T, s *mcp.Server) *mcp.ClientSession {
	t.Helper()
	ct, st := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := s.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := c.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func callJSON(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any, v any) string {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if res.IsError {
		t.Fatalf("%s returned a tool error: %+v", name, res.Content)
	}
	if len(res.Content) != 1 {
		t.Fatalf("%s: want exactly one content block, got %d", name, len(res.Content))
	}
	if res.StructuredContent != nil {
		t.Errorf("%s: structured content would duplicate the payload", name)
	}
	text := res.Content[0].(*mcp.TextContent).Text
	if err := json.Unmarshal([]byte(text), v); err != nil {
		t.Fatalf("%s: response is not JSON: %v\n%s", name, err, text)
	}
	return text
}

func TestToolsListed(t *testing.T) {
	e, _ := replayEngine(t)
	cs := connect(t, New(e, "test"))
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
		if tool.Description == "" {
			t.Errorf("%s has no description", tool.Name)
		}
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Errorf("%s must be annotated read-only", tool.Name)
		}
		schema, _ := json.Marshal(tool.InputSchema)
		if tool.Name != "list_services" && !strings.Contains(string(schema), "budget_tokens") {
			t.Errorf("%s schema lacks budget_tokens: %s", tool.Name, schema)
		}
	}
	sort.Strings(names)
	if got := strings.Join(names, ","); got != "list_services,new_errors,trace_diff,what_changed" {
		t.Errorf("tools = %s", got)
	}
	init := cs.InitializeResult()
	if init == nil || !strings.Contains(init.Instructions, "what_changed") {
		t.Errorf("server instructions missing")
	}
}

func TestToolCalls(t *testing.T) {
	e, truth := replayEngine(t)
	cs := connect(t, New(e, "test"))

	var list digest.ServiceList
	callJSON(t, cs, "list_services", map[string]any{"since": truth.Since, "until": truth.Until}, &list)
	if len(list.Services) < 5 {
		t.Errorf("list_services found %d services", len(list.Services))
	}

	var env digest.Envelope
	text := callJSON(t, cs, "what_changed", map[string]any{"since": truth.Since, "until": truth.Until}, &env)
	if len(env.Findings) == 0 || env.Findings[0].Service != truth.Service {
		t.Errorf("what_changed top = %+v, want %s", env.Findings, truth.Service)
	}
	if tokens := digest.EstimateTokens([]byte(text)); tokens > digest.DefaultBudget {
		t.Errorf("default response is %d tokens, over the %d budget", tokens, digest.DefaultBudget)
	}

	callJSON(t, cs, "new_errors", map[string]any{"service": truth.Service, "since": truth.Since, "until": truth.Until, "budget_tokens": 4000}, &env)
	if len(env.Findings) == 0 || env.Findings[0].Kind != digest.KindNewLogPattern {
		t.Errorf("new_errors = %+v", env.Findings)
	}
	callJSON(t, cs, "trace_diff", map[string]any{"service": truth.Service, "since": truth.Since, "until": truth.Until, "budget_tokens": 4000}, &env)
	if len(env.Findings) == 0 {
		t.Error("trace_diff found nothing")
	}
}

func TestToolInputErrors(t *testing.T) {
	e, _ := replayEngine(t)
	cs := connect(t, New(e, "test"))
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "what_changed", Arguments: map[string]any{"since": "last tuesday"}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("bad input should be a tool error the agent can read")
	}
	if text := res.Content[0].(*mcp.TextContent).Text; !strings.Contains(text, "since") {
		t.Errorf("error text = %q", text)
	}
	// Wrong types are rejected by schema validation.
	res, err = cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "what_changed", Arguments: map[string]any{"budget_tokens": "lots"}})
	if err == nil && !res.IsError {
		t.Error("string budget_tokens should be rejected")
	}
}

func TestStreamableHTTP(t *testing.T) {
	e, truth := replayEngine(t)
	hs := httptest.NewServer(HTTPHandler(New(e, "test"), HTTPOptions{}))
	defer hs.Close()

	resp, err := http.Get(hs.URL + "/healthz")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("healthz: %v %v", err, resp)
	}
	_ = resp.Body.Close()

	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := c.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: hs.URL + "/mcp"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Close() }()
	var env digest.Envelope
	callJSON(t, cs, "what_changed", map[string]any{"since": truth.Since, "until": truth.Until}, &env)
	if len(env.Findings) == 0 {
		t.Error("no findings over HTTP")
	}
}

// TestReadOnly runs every tool against fake backends and checks Spoor only
// ever calls query endpoints.
func TestReadOnly(t *testing.T) {
	var mu sync.Mutex
	paths := map[string]bool{}
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths[r.Method+" "+r.URL.Path] = true
		mu.Unlock()
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/v1/label/"):
			_, _ = io.WriteString(w, `{"status":"success","data":["svc"]}`)
		case strings.HasPrefix(r.URL.Path, "/api/v1/"):
			_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
		default:
			_, _ = io.WriteString(w, "")
		}
	}))
	defer fake.Close()
	cfg := config.Default()
	cfg.Metrics.URL, cfg.Logs.URL, cfg.Traces.URL = fake.URL, fake.URL, fake.URL
	cs := connect(t, New(digest.NewFromConfig(cfg), "test"))
	for _, tool := range []string{"what_changed", "new_errors", "trace_diff", "list_services"} {
		var v any
		callJSON(t, cs, tool, map[string]any{}, &v)
	}
	if len(paths) == 0 {
		t.Fatal("no backend calls observed")
	}
	for p := range paths {
		method, path, _ := strings.Cut(p, " ")
		if method != http.MethodPost && method != http.MethodGet {
			t.Errorf("unexpected method %s", p)
		}
		ok := false
		for _, allowed := range backend.AllowedPaths {
			ok = ok || strings.HasPrefix(path, allowed)
		}
		if !ok {
			t.Errorf("call to %s is not a read-only query endpoint", path)
		}
	}
}

func TestHTTPAuth(t *testing.T) {
	e, truth := replayEngine(t)
	hs := httptest.NewServer(HTTPHandler(New(e, "test"), HTTPOptions{AuthToken: "s3cret"}))
	defer hs.Close()

	init := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`
	post := func(auth string) int {
		req, _ := http.NewRequest(http.MethodPost, hs.URL+"/mcp", strings.NewReader(init))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if code := post(""); code != http.StatusUnauthorized {
		t.Errorf("no token: status %d, want 401", code)
	}
	if code := post("Bearer wrong"); code != http.StatusUnauthorized {
		t.Errorf("wrong token: status %d, want 401", code)
	}
	if code := post("Bearer s3cret"); code != http.StatusOK {
		t.Errorf("right token: status %d, want 200", code)
	}
	for _, path := range []string{"/healthz", "/metrics"} {
		resp, err := http.Get(hs.URL + path)
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Errorf("%s must stay open without a token: %v %v", path, err, resp)
		}
		_ = resp.Body.Close()
	}

	// An authenticated MCP client works end to end.
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	hc := &http.Client{Transport: headerTransport{"Authorization", "Bearer s3cret"}}
	cs, err := c.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: hs.URL + "/mcp", HTTPClient: hc}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Close() }()
	var env digest.Envelope
	callJSON(t, cs, "what_changed", map[string]any{"since": truth.Since, "until": truth.Until}, &env)
}

type headerTransport struct{ key, value string }

func (h headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set(h.key, h.value)
	return http.DefaultTransport.RoundTrip(r)
}

func TestMetricsAndLogs(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(prev)

	e, truth := replayEngine(t)
	hs := httptest.NewServer(HTTPHandler(New(e, "test"), HTTPOptions{}))
	defer hs.Close()
	cs := connect(t, New(e, "test"))
	var env digest.Envelope
	callJSON(t, cs, "what_changed", map[string]any{"since": truth.Since, "until": truth.Until}, &env)
	if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "what_changed", Arguments: map[string]any{"since": "bogus"}}); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get(hs.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	for _, want := range []string{
		`spoor_tool_calls_total{tool="what_changed",status="ok"}`,
		`spoor_tool_calls_total{tool="what_changed",status="error"}`,
		`spoor_tool_duration_seconds_bucket{tool="what_changed"`,
		`spoor_findings_bucket{tool="what_changed"`,
		`process_resident_memory_bytes`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("/metrics lacks %s", want)
		}
	}
	var ok, failed bool
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		if rec["tool"] == "what_changed" && rec["msg"] == "tool call" && rec["findings"] != nil {
			ok = true
		}
		if rec["tool"] == "what_changed" && rec["msg"] == "tool call failed" && strings.Contains(fmt.Sprint(rec["error"]), "since") {
			failed = true
		}
	}
	if !ok || !failed {
		t.Errorf("want one success and one failure log record, got:\n%s", logs.String())
	}
}
