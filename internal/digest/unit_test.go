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
	pq := e.patternQuery(e.logScope([]string{"pay"}, false), []window.Range{{Start: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), End: time.Date(2026, 1, 1, 0, 30, 0, 0, time.UTC)}})
	for _, want := range []string{
		`"service.name":="pay" severity_text:~`,
		`| copy _msg as raw | unpack_json from _msg fields ("message", "msg"`,
		`| format if ("spoor_j_message":*) "<spoor_j_message>" as spoor_m | format if`, // highest priority applied last
		`| format if (spoor_m:*) "<spoor_m>" as _msg`,
		`| replace_regexp ("^([^{\\[\\r\\n][^\\r\\n]*)[\\r\\n](?s:.*)", "$1")`,
		`| collapse_nums prettify | format if (spoor_c:*) "<spoor_c><_msg>" as _msg`,
		`, count() if (_time:[2026-01-01T00:00:00Z, 2026-01-01T00:30:00Z)) b0, min(_time) first, any(raw) example | total_stats by (spoor_svc) sum(n) spoor_etotal, sum(b0) spoor_e0 | sort by (n desc) partition by (spoor_svc) limit 200`,
	} {
		if !strings.Contains(pq, want) {
			t.Errorf("patternQuery lacks %s\n%s", want, pq)
		}
	}
	if !strings.Contains(pq, `"spoor_j_msg":*) "<spoor_j_msg>" as spoor_m | format if ("spoor_j_message":*)`) {
		t.Errorf("message must win over msg (applied last): %s", pq)
	}
	if got, want := e.logScope([]string{"a", "b"}, false), `"service.name":in("a","b")`; got != want {
		t.Errorf("logScope = %s, want %s", got, want)
	}
	start := time.Date(2026, 1, 1, 0, 7, 0, 0, time.UTC)
	if got, want := e.totalsQuery("*", 30*time.Minute, start), `* | copy "service.name" as spoor_svc | stats by (_time:1800000ms offset 1380000ms, spoor_svc) count() total, count() if (NOT severity_text:*) unclassified`; got != want {
		t.Errorf("totalsQuery:\n got %s\nwant %s", got, want)
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

func TestRedactorEnvelope(t *testing.T) {
	r := newRedactor([]config.Rule{{Regex: `sess-[a-z]+`, Replace: "<SESSION>"}})
	env := Envelope{
		Summary: "1 change in svc2-api during 2026-10-15T17:52:00Z/2026-10-15T18:22:00Z",
		Findings: []Finding{{
			Service: "svc2-api",
			Title:   `New error log pattern: "cart ord_7f3k2m9q for sess-abcdef failed after 1500ms at 10.0.0.12"`,
			Example: "order OLJCESPC7Z failed for a@b.com",
		}},
		SteadyErrors: []SteadyError{{Example: "user u_9f8e7d6c timed out"}},
		Coverage:     map[string]string{"logs": "ok"},
	}
	r.envelope(&env, []string{"svc2-api"})
	f := env.Findings[0]
	for _, bad := range []string{"ord_7f3k2m9q", "sess-abcdef", "10.0.0.12", "OLJCESPC7Z", "a@b.com"} {
		if strings.Contains(f.Title+f.Example, bad) {
			t.Errorf("%q survived redaction: %q / %q", bad, f.Title, f.Example)
		}
	}
	if !strings.Contains(f.Title, "1500ms") || !strings.Contains(env.Summary, "svc2-api") || !strings.Contains(env.Summary, "2026-10-15T17:52:00Z") {
		t.Errorf("redaction too aggressive: %q / %q", f.Title, env.Summary)
	}
	if strings.Contains(env.SteadyErrors[0].Example, "u_9f8e7d6c") {
		t.Errorf("steady example not redacted: %q", env.SteadyErrors[0].Example)
	}
}

func TestFamily(t *testing.T) {
	same := [][2]string{
		{`Null address book for session <ID> order <N>`, `Null address book for session <N> order <ID>`},
		{`No matching template found for "home_v2"`, `No matching template found for "category_x"`},
		{"failed to load product <ID>: database query returned error\n\tat com.x.Y(Y.java:12)", "failed to load product <N>: database query returned error"},
		{`request failed id=abc status=<N>`, `request failed id=xyz status=<N>`},
	}
	for _, p := range same {
		if family(p[0]) != family(p[1]) {
			t.Errorf("family(%q)=%q != family(%q)=%q", p[0], family(p[0]), p[1], family(p[1]))
		}
	}
	if family("failed to load product <N>: timeout") == family("failed to load product <N>: database query returned error") {
		t.Error("different failure reasons must be different families")
	}
}

func TestChildTimeUnionsParallelChildren(t *testing.T) {
	p := spanRow{start: 1000, dur: 100}
	kids := []spanRow{{start: 1010, dur: 50}, {start: 1020, dur: 50}, {start: 1090, dur: 30}}
	// [1010,1070] ∪ [1090,1100 clipped] = 60 + 10
	if got := childTime(p, kids); got != 70 {
		t.Errorf("childTime = %v, want 70", got)
	}
	// Without start times, durations are summed.
	if got := childTime(spanRow{dur: 100}, []spanRow{{dur: 50}, {dur: 50}}); got != 100 {
		t.Errorf("childTime without starts = %v, want 100", got)
	}
}

func TestFamilyJavaPrefix(t *testing.T) {
	a := `[ERROR] [http-nio-<N>-exec-<N>] [correlationId:<ID>-<ID>-<N>] [ProfileController.java:<N>] profile fetch failed for account <ID>`
	b := `[ERROR] [http-nio-<N>-exec-<N>] [correlationId:<ID>-<N>] [ProfileController.java:<N>] profile fetch failed for account <N>`
	c := `[ERROR] [http-nio-<N>-exec-<N>] [correlationId:<ID>] [ProfileController.java:<N>] address book missing`
	if family(a) != family(b) {
		t.Errorf("same error, different threads/IDs: %q vs %q", family(a), family(b))
	}
	if family(a) == family(c) {
		t.Errorf("different errors from one class merged: %q", family(a))
	}
	if !strings.Contains(family(a), "profilecontroller.java") || strings.Contains(family(a), "http") || strings.HasPrefix(family(a), "error") {
		t.Errorf("family = %q: want the class kept and the thread and level dropped", family(a))
	}
	if family("ERROR payment declined") != family("payment declined") {
		t.Error("a leading level word is not part of the error")
	}
}

// Bucket offsets: VictoriaLogs places boundaries at k*step - offset, so the
// offset must be step minus the start's position on the epoch grid.
func TestTotalsBucketOffset(t *testing.T) {
	e := New(config.Default(), nil, nil, nil)
	for start, want := range map[time.Time]string{
		time.Date(2026, 10, 15, 12, 20, 0, 0, time.UTC): "offset 600000ms",  // :20 on a 30m grid
		time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC):  "offset 0ms",       // already aligned
		time.Date(2026, 10, 15, 12, 7, 0, 0, time.UTC):  "offset 1380000ms", // :07
	} {
		if q := e.totalsQuery("*", 30*time.Minute, start); !strings.Contains(q, want) {
			t.Errorf("start %s: %s, want %s", start.Format("15:04"), q, want)
		}
	}
}
