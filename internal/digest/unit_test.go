package digest

import (
	"fmt"
	"math"
	"math/rand"
	"strings"
	"testing"
	"testing/quick"
	"time"

	"github.com/asinha07/spoor/internal/config"
	"github.com/asinha07/spoor/internal/window"
)

func TestMedianMAD(t *testing.T) {
	if m := median([]float64{3, 1, 2}); m != 2 {
		t.Errorf("median odd = %v", m)
	}
	if m := median([]float64{4, 1, 2, 3}); m != 2.5 {
		t.Errorf("median even = %v", m)
	}
	if m := median(nil); m != 0 {
		t.Errorf("median empty = %v", m)
	}
	xs := []float64{1, 1, 2, 2, 4, 6, 9}
	if got := mad(xs, median(xs)); got != 1 {
		t.Errorf("mad = %v, want 1", got)
	}
}

func TestRobustZFloors(t *testing.T) {
	// A perfectly flat baseline must not turn a tiny wobble into a huge z.
	z, _ := robustZ(0.011, []float64{0.01, 0.01, 0.01}, 0.5, 0.02)
	if z > 0.1 {
		t.Errorf("flat baseline z = %v, want near 0 thanks to the floors", z)
	}
	// A real jump clears the floors easily.
	z, med := robustZ(0.4, []float64{0.001, 0.002, 0.001, 0.0015}, 0.5, 0.02)
	if med <= 0 || z < 10 {
		t.Errorf("jump z = %v (median %v), want >= 10", z, med)
	}
}

func TestZScore(t *testing.T) {
	if zScore(3) != 0.5 {
		t.Errorf("z=3 should map to the 0.5 threshold, got %v", zScore(3))
	}
	if zScore(-1) != 0 || zScore(0) != 0 {
		t.Error("non-positive z must score 0")
	}
	if !(zScore(100) < 1 && zScore(100) > 0.95) {
		t.Errorf("z=100 -> %v", zScore(100))
	}
}

func TestHumanValue(t *testing.T) {
	for _, c := range []struct {
		v    float64
		unit string
		want string
	}{
		{0.381, "ratio", "38.1%"},
		{0.024, "seconds", "24ms"},
		{2.4, "seconds", "2.40s"},
		{0.00002, "seconds", "20µs"},
		{52.3, "reqps", "52.3/s"},
		{1.5 * (1 << 30), "bytes", "1.50 GiB"},
		{6115, "messages", "6115 messages"},
		{0.72, "cores", "0.72 cores"},
	} {
		if got := humanValue(c.v, c.unit); got != c.want {
			t.Errorf("humanValue(%v,%q) = %q, want %q", c.v, c.unit, got, c.want)
		}
	}
}

func TestRedact(t *testing.T) {
	for in, want := range map[string]string{
		"login failed for alice@example.com":                     "login failed for <EMAIL>",
		"Authorization: Bearer abcdefghijklmnop":                 "Authorization: Bearer <TOKEN>",
		"db password=hunter22 host=x":                            "db password=<REDACTED> host=x",
		`{"api_key": "sk-123456"}`:                               `{"api_key": "<REDACTED>"}`,
		"card 4242 4242 4242 4242 declined":                      "card <CARD> declined",
		"req 9f1c2a7e-1111-2222-3333-444455556666":               "req <UUID>",
		"jwt eyJhbGciOiJIUzI1.eyJzdWIiOiIxMjM0.SflKxwRJSMeKKF2Q": "jwt <JWT>",
		"order 12345 failed":                                     "order 12345 failed",
	} {
		if got := Redact(in, 0); got != want {
			t.Errorf("Redact(%q) = %q, want %q", in, got, want)
		}
	}
	if got := Redact("abcdef", 4); got != "abc…" {
		t.Errorf("truncate = %q", got)
	}
}

func finding(svc, signal, kind string, score float64, onset time.Time) Finding {
	return Finding{Service: svc, Signal: signal, Kind: kind, Score: score, Title: fmt.Sprintf("%s %s %s", svc, signal, kind), onset: onset}
}

func TestRank(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	fs := []Finding{
		finding("b", SignalLogs, KindNewLogPattern, 0.6, t0.Add(time.Minute)),
		finding("a", SignalMetrics, KindErrorRateUp, 0.7, t0),
		finding("a", SignalMetrics, KindErrorRateUp, 0.7, t0),        // duplicate
		finding("c", SignalMetrics, KindLatencyUp, 0.4, time.Time{}), // below threshold
		finding("b", SignalMetrics, KindErrorRateUp, 0.6, t0),
	}
	got := rank(fs, 0.5)
	if len(got) != 3 {
		t.Fatalf("rank kept %d findings, want 3: %+v", len(got), got)
	}
	// b's two signals start within the correlation window, so both get +0.05.
	if got[0].Service != "a" && !(got[0].Service == "b" && got[0].Score == 0.65) {
		t.Errorf("unexpected order: %+v", got)
	}
	for _, f := range got {
		if f.Service == "b" && f.Score != 0.65 {
			t.Errorf("corroborated finding %q score = %v, want 0.65", f.Title, f.Score)
		}
		if f.Service == "a" && f.Score != 0.7 {
			t.Errorf("uncorroborated finding score changed: %v", f.Score)
		}
	}
	// Deterministic order: equal scores sort by service then title.
	again := rank(append([]Finding(nil), fs...), 0.5)
	for i := range got {
		if got[i].Title != again[i].Title {
			t.Fatal("rank is not deterministic")
		}
	}
	if rank(nil, 0.5) == nil {
		t.Error("rank must return an empty slice, not nil, so JSON shows []")
	}
}

func TestFitRespectsBudget(t *testing.T) {
	prop := func(seed int64, n uint8, budget uint16) bool {
		r := rand.New(rand.NewSource(seed))
		b := ClampBudget(int(budget)%5000 + 1)
		env := randomEnvelope(r, int(n)%60)
		out := Fit(env, b)
		raw, _ := MarshalCompact(out)
		if EstimateTokens(raw) > b {
			t.Logf("budget %d exceeded: %d tokens", b, EstimateTokens(raw))
			return false
		}
		if len(out.Findings)+out.Truncated != len(env.Findings) {
			t.Logf("truncated count wrong: kept %d + dropped %d != %d", len(out.Findings), out.Truncated, len(env.Findings))
			return false
		}
		for i := range out.Findings { // order and identity preserved
			if out.Findings[i].Title != env.Findings[i].Title {
				return false
			}
		}
		return true
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: 300}); err != nil {
		t.Error(err)
	}
}

func TestFitKeepsSmallEnvelopeIntact(t *testing.T) {
	env := randomEnvelope(rand.New(rand.NewSource(1)), 2)
	out := Fit(env, MaxBudget)
	if out.Truncated != 0 || len(out.Findings) != 2 || out.Findings[0].EvidenceLink == "" {
		t.Errorf("small envelope was trimmed: %+v", out)
	}
}

func randomEnvelope(r *rand.Rand, n int) Envelope {
	env := Envelope{Summary: strings.Repeat("summary ", 20+r.Intn(40)), Window: "w", Baseline: "b", Coverage: map[string]string{"metrics": "ok", "logs": "error: " + strings.Repeat("<x>", r.Intn(60))}}
	for i := 0; i < n; i++ {
		env.Findings = append(env.Findings, Finding{
			Service: fmt.Sprintf("svc-%d", i), Signal: SignalLogs, Kind: KindNewLogPattern, Score: 0.9,
			Title:        fmt.Sprintf("finding %d %s", i, strings.Repeat("x", r.Intn(120))),
			Example:      strings.Repeat("e", r.Intn(200)),
			EvidenceLink: "http://localhost:9428/select/vmui/#/?query=" + strings.Repeat("q", r.Intn(300)),
		})
	}
	return env
}

func TestClampBudget(t *testing.T) {
	for in, want := range map[int]int{0: DefaultBudget, 50: MinBudget, 900: 900, 99999: MaxBudget} {
		if got := ClampBudget(in); got != want {
			t.Errorf("ClampBudget(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestErrorOrigins(t *testing.T) {
	a := &attribution{failedTraces: map[string]int{}, originBy: map[string]map[string]int{}, originOps: map[string]map[string]int{}}
	// Two traces: frontend -> checkout -> payment, failure starts in payment.
	// One trace: frontend fails on its own.
	var rows []spanRow
	for _, tr := range []string{"t1", "t2"} {
		rows = append(rows,
			spanRow{traceID: tr, spanID: "f", svc: "frontend", name: "POST /api/checkout"},
			spanRow{traceID: tr, spanID: "fc", parentID: "f", svc: "frontend", name: "checkout.PlaceOrder"},
			spanRow{traceID: tr, spanID: "c", parentID: "fc", svc: "checkout", name: "POST /checkout"},
			spanRow{traceID: tr, spanID: "cc", parentID: "c", svc: "checkout", name: "payment.Charge"},
			spanRow{traceID: tr, spanID: "p", parentID: "cc", svc: "payment", name: "POST /charge"},
		)
	}
	rows = append(rows, spanRow{traceID: "t3", spanID: "x", svc: "frontend", name: "GET /"})
	a.errorOrigins(rows, nil)
	if o, share, n := a.topOrigin("checkout"); o != "payment" || share != 1 || n != 2 {
		t.Errorf("checkout origin = %s %.2f %d, want payment 1.00 2", o, share, n)
	}
	if o, share, n := a.topOrigin("frontend"); o != "payment" || n != 3 || math.Abs(share-2.0/3) > 1e-9 {
		t.Errorf("frontend origin = %s %.2f %d, want payment 0.67 3", o, share, n)
	}
	if op := a.topOp(a.originOps["payment"]); op != "POST /charge" {
		t.Errorf("origin op = %q", op)
	}
}

// TestErrorOriginsAcrossGap: a caller whose own span is not marked failed
// must not make the failed span above it look like a second origin.
func TestErrorOriginsAcrossGap(t *testing.T) {
	a := &attribution{failedTraces: map[string]int{}, originBy: map[string]map[string]int{}, originOps: map[string]map[string]int{}}
	all := []spanRow{
		{traceID: "t", spanID: "proxy", svc: "frontend-proxy", name: "router frontend egress"},
		{traceID: "t", spanID: "fe", parentID: "proxy", svc: "frontend", name: "POST /api/checkout"}, // not marked failed
		{traceID: "t", spanID: "co", parentID: "fe", svc: "checkout", name: "PlaceOrder"},
		{traceID: "t", spanID: "pay", parentID: "co", svc: "payment", name: "charge"},
	}
	failed := []spanRow{all[0], all[2], all[3]}
	a.errorOrigins(failed, all)
	for _, svc := range []string{"frontend-proxy", "checkout", "payment"} {
		if o, share, _ := a.topOrigin(svc); o != "payment" || share != 1 {
			t.Errorf("origin for %s = %s (%.2f), want payment", svc, o, share)
		}
	}
	// Without the full span set the gap splits the trace into two origins.
	b := &attribution{failedTraces: map[string]int{}, originBy: map[string]map[string]int{}, originOps: map[string]map[string]int{}}
	b.errorOrigins(failed, nil)
	if len(b.originOps) != 2 {
		t.Errorf("expected the gap to show two origins without ancestry, got %v", b.originOps)
	}
}

func TestSelfTimes(t *testing.T) {
	rows := []spanRow{
		{traceID: "t", spanID: "a", svc: "frontend", name: "GET /", dur: 100},
		{traceID: "t", spanID: "b", parentID: "a", svc: "frontend", name: "ad.GetAds", dur: 80},
		{traceID: "t", spanID: "c", parentID: "b", svc: "ad", name: "GET /ads", dur: 75},
	}
	st := selfTimes(rows)
	for k, want := range map[opKey]float64{
		{"frontend", "GET /"}:     20,
		{"frontend", "ad.GetAds"}: 5,
		{"ad", "GET /ads"}:        75,
	} {
		if got := st[k].mean(); got != want {
			t.Errorf("self(%v) = %v, want %v", k, got, want)
		}
	}
}

func TestApplyAttributionNeedsAChange(t *testing.T) {
	// Failed traces with no error finding to explain must not create an
	// origin finding: healthy systems always have a few failures.
	e := New(config.Default(), nil, nil, nil)
	a := &attribution{failedTraces: map[string]int{"payment": 10}, originBy: map[string]map[string]int{"payment": {"payment": 10}},
		originOps: map[string]map[string]int{"payment": {"POST /charge": 10}}}
	spec, _ := window.Parse(time.Now(), "", "", "", 2)
	if got := e.applyAttribution(a, []string{"payment"}, nil, spec); len(got) != 0 {
		t.Errorf("origin finding emitted without an error change: %+v", got)
	}
	fs := []Finding{{Service: "payment", Kind: KindErrorRateUp, Score: 0.9, Title: "Error rate up"}}
	got := e.applyAttribution(a, []string{"payment"}, fs, spec)
	if len(got) != 2 || got[1].Kind != KindErrorOrigin {
		t.Errorf("want an error_origin finding, got %+v", got)
	}
}

func TestQueryBuilders(t *testing.T) {
	e := New(config.Default(), nil, nil, nil)
	if got, want := e.errorPatternQuery("pay"), `"service.name":="pay" severity_text:~"(?i)^(error|fatal|critical)$" | copy _msg as raw | collapse_nums prettify | stats by (_msg) count() n, min(_time) first, any(raw) example | sort by (n desc) | limit 200`; got != want {
		t.Errorf("errorPatternQuery:\n got %s\nwant %s", got, want)
	}
	if got, want := e.spanStatsQuery(e.spanFilter("pay", "")), `span_id:* "resource_attr:service.name":="pay" | stats by ("name") count() n, count() if (status_code:=2) errs, quantile(0.95, "duration") p95, sum("duration") total | sort by (n desc) | limit 200`; got != want {
		t.Errorf("spanStatsQuery:\n got %s\nwant %s", got, want)
	}
	if got := e.spanFilter("pay", "POST /charge"); got != `span_id:* "resource_attr:service.name":="pay" "name":="POST /charge"` {
		t.Errorf("spanFilter with op = %s", got)
	}
	q, _ := config.Render(config.Default().Metrics.ErrorRate, "pay", 30*time.Minute)
	if !strings.Contains(q, `or vector(0)`) || !strings.Contains(q, `service_name="pay"`) || !strings.Contains(q, `[30m]`) {
		t.Errorf("error-rate query = %s", q)
	}
}

func TestSummaryNoFindings(t *testing.T) {
	spec, _ := window.Parse(time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC), "", "", "", 2)
	got := summarize("checkout", nil, spec)
	if !strings.HasPrefix(got, "No significant change in checkout") {
		t.Errorf("summary = %q", got)
	}
}

func TestPoissonSF(t *testing.T) {
	cases := []struct{ k, lambda, want, tol float64 }{
		{0, 1, 1, 0},
		{1, 1, 1 - math.Exp(-1), 1e-12},
		{7, 1, 8.324e-5, 1e-7},
		{3, 5, 0.875348, 1e-5},
	}
	for _, c := range cases {
		if got := poissonSF(c.k, c.lambda); math.Abs(got-c.want) > c.tol {
			t.Errorf("poissonSF(%v,%v) = %v, want %v", c.k, c.lambda, got, c.want)
		}
	}
	if got := poissonSF(1555, 4.83); got > 1e-100 {
		t.Errorf("a real spike should be astronomically unlikely, got %v", got)
	}
}
