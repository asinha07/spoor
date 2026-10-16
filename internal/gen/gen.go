package gen

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Step is the sampling interval for metrics and the simulation tick.
const Step = 15 * time.Second

// TraceSampleRate is the share of requests whose spans are exported.
const TraceSampleRate = 0.08

// Sink receives generated telemetry batches.
type Sink interface {
	Metrics(ctx context.Context, promText []byte) error
	Logs(ctx context.Context, jsonLines []byte) error
	Spans(ctx context.Context, otlpJSON []byte) error
}

type histKey struct{ service, status string }

type histState struct {
	counts []float64 // cumulative per bucket (len(buckets)+1 for +Inf)
	sum    float64
	total  float64
}

type span struct {
	traceID, spanID, parentID string
	name                      string
	kind                      int // 2 server, 3 client, 5 consumer
	start, end                time.Time
	failed                    bool
	statusMsg                 string
}

// Generator simulates one scenario plan.
type Generator struct {
	plan  Plan
	rng   *rand.Rand
	topo  []entry
	hist  map[histKey]*histState
	cpu   map[string]float64
	spans map[string][]span // by service
	logs  bytes.Buffer
	stats Stats
}

// Stats counts what was generated.
type Stats struct {
	Requests, Spans, LogLines, Samples int
}

// New prepares a generator.
func New(p Plan) *Generator {
	return &Generator{
		plan:  p,
		rng:   rand.New(rand.NewSource(p.Seed)),
		topo:  topology(),
		hist:  map[histKey]*histState{},
		cpu:   map[string]float64{},
		spans: map[string][]span{},
	}
}

// Run simulates the whole plan, flushing to sink every 10 simulated minutes.
func (g *Generator) Run(ctx context.Context, sink Sink) (Stats, error) {
	const flushEvery = 40 // steps
	var metrics bytes.Buffer
	step := 0
	for t := g.plan.DataStart; t.Before(g.plan.FaultEnd); t = t.Add(Step) {
		if err := ctx.Err(); err != nil {
			return g.stats, err
		}
		g.tick(t)
		g.writeMetrics(&metrics, t.Add(Step))
		step++
		if step%flushEvery == 0 {
			if err := g.flush(ctx, sink, &metrics); err != nil {
				return g.stats, err
			}
		}
	}
	return g.stats, g.flush(ctx, sink, &metrics)
}

// progress returns the fault progress at t, or -1 outside the fault window.
func (g *Generator) progress(t time.Time) float64 {
	if t.Before(g.plan.FaultStart) || !t.Before(g.plan.FaultEnd) {
		return -1
	}
	return float64(t.Sub(g.plan.FaultStart)) / float64(g.plan.FaultEnd.Sub(g.plan.FaultStart))
}

func (g *Generator) effect(service string, p float64) (Effect, bool) {
	if p < 0 {
		return Effect{}, false
	}
	e, ok := g.plan.Scenario.Effects[service]
	return e, ok
}

func (g *Generator) tick(t time.Time) {
	// Gentle traffic drift plus per-tick noise, so baselines are realistic.
	drift := 1 + 0.08*math.Sin(2*math.Pi*float64(t.Unix())/(3*3600))
	for _, en := range g.topo {
		lambda := en.rps * Step.Seconds() * drift * (1 + 0.05*g.rng.NormFloat64())
		n := g.poisson(math.Max(lambda, 0))
		for i := 0; i < n; i++ {
			start := t.Add(time.Duration(g.rng.Int63n(int64(Step))))
			sampled := g.rng.Float64() < TraceSampleRate
			traceID := g.hexID(16)
			pid := productIDs[g.rng.Intn(len(productIDs))]
			dur, failed := g.simulate(en.root, start, pid, sampled, traceID, "")
			g.stats.Requests++
			if en.root.op == "POST /api/checkout" && !failed {
				g.consume(start.Add(dur+time.Duration(g.rng.Int63n(int64(200*time.Millisecond)))), sampled)
			}
		}
	}
}

// simulate walks one call tree and records metrics, logs and (if sampled) spans.
func (g *Generator) simulate(c *call, start time.Time, productID string, sampled bool, traceID, parentID string) (time.Duration, bool) {
	p := g.progress(start)
	eff, faulty := g.effect(c.service, p)
	mult := 1.0
	if faulty && eff.LatencyMult != nil {
		mult = eff.LatencyMult(p)
	}
	own := g.lognormal(float64(c.median)*mult, c.sigma)
	spanID := g.hexID(8)
	cursor := start.Add(own / 2)
	failed := false
	var failedChild *call
	for _, ch := range c.children {
		clientID := g.hexID(8)
		d, f := g.simulate(ch, cursor.Add(500*time.Microsecond), productID, sampled, traceID, clientID)
		d += time.Millisecond // network
		if sampled {
			g.addSpan(c.service, span{traceID: traceID, spanID: clientID, parentID: spanID, name: ch.client, kind: 3, start: cursor, end: cursor.Add(d), failed: f})
		}
		cursor = cursor.Add(d)
		if f {
			failed = true
			failedChild = ch
		}
	}
	total := cursor.Add(own / 2).Sub(start)

	errProb := c.errProb
	if faulty && eff.ErrProb != nil {
		errProb = eff.ErrProb(p, productID)
	}
	ownFail := g.rng.Float64() < errProb
	if ownFail {
		failed = true
		pattern := c.errorLog
		if faulty && eff.ErrorLog != "" {
			pattern = eff.ErrorLog
		}
		g.log(start.Add(total), c.service, "ERROR", g.fill(pattern, productID))
	} else if failedChild != nil {
		g.log(start.Add(total), c.service, "ERROR", fmt.Sprintf("call to %s failed with status 500", failedChild.client))
	} else if g.rng.Intn(25) == 0 {
		g.log(start.Add(total), c.service, "INFO", fmt.Sprintf("%s completed in %dms", c.op, total.Milliseconds()))
	}

	status := "200"
	if failed {
		status = "500"
	}
	g.observe(c.service, status, total.Seconds())
	if sampled {
		s := span{traceID: traceID, spanID: spanID, parentID: parentID, name: c.op, kind: 2, start: start, end: start.Add(total), failed: failed}
		if failed {
			s.statusMsg = "request failed"
		}
		g.addSpan(c.service, s)
	}
	return total, failed
}

// consume models the accounting service processing an order from Kafka.
func (g *Generator) consume(at time.Time, sampled bool) {
	p := g.progress(at)
	eff, faulty := g.effect("accounting", p)
	mult := 1.0
	if faulty && eff.LatencyMult != nil {
		mult = eff.LatencyMult(p)
	}
	d := g.lognormal(float64(15*time.Millisecond)*mult, 0.3)
	if g.rng.Intn(25) == 0 {
		g.log(at.Add(d), "accounting", "INFO", fmt.Sprintf("order processed in %dms", d.Milliseconds()))
	}
	if sampled {
		g.addSpan("accounting", span{traceID: g.hexID(16), spanID: g.hexID(8), name: "orders process", kind: 5, start: at, end: at.Add(d)})
	}
}

func (g *Generator) fill(pattern, productID string) string {
	s := strings.ReplaceAll(pattern, "{id}", productID)
	return strings.ReplaceAll(s, "{n}", strconv.Itoa(1000+g.rng.Intn(90000)))
}

func (g *Generator) observe(service, status string, secs float64) {
	k := histKey{service, status}
	h := g.hist[k]
	if h == nil {
		h = &histState{counts: make([]float64, len(buckets)+1)}
		g.hist[k] = h
	}
	for i, b := range buckets {
		if secs <= b {
			h.counts[i]++
		}
	}
	h.counts[len(buckets)]++
	h.sum += secs
	h.total++
}

func (g *Generator) log(t time.Time, service, severity, msg string) {
	line, _ := json.Marshal(map[string]string{
		"_time": t.UTC().Format(time.RFC3339Nano), "_msg": msg,
		"service.name": service, "severity_text": severity,
	})
	g.logs.Write(line)
	g.logs.WriteByte('\n')
	g.stats.LogLines++
}

func (g *Generator) addSpan(service string, s span) {
	g.spans[service] = append(g.spans[service], s)
	g.stats.Spans++
}

// writeMetrics appends one sample of every series at ts.
func (g *Generator) writeMetrics(buf *bytes.Buffer, ts time.Time) {
	ms := ts.UnixMilli()
	keys := make([]histKey, 0, len(g.hist))
	for k := range g.hist {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].service != keys[j].service {
			return keys[i].service < keys[j].service
		}
		return keys[i].status < keys[j].status
	})
	for _, k := range keys {
		h := g.hist[k]
		lbl := fmt.Sprintf(`service_name=%q,http_response_status_code=%q`, k.service, k.status)
		for i, b := range buckets {
			fmt.Fprintf(buf, "http_server_request_duration_seconds_bucket{%s,le=%q} %g %d\n", lbl, strconv.FormatFloat(b, 'g', -1, 64), h.counts[i], ms)
		}
		fmt.Fprintf(buf, "http_server_request_duration_seconds_bucket{%s,le=\"+Inf\"} %g %d\n", lbl, h.counts[len(buckets)], ms)
		fmt.Fprintf(buf, "http_server_request_duration_seconds_count{%s} %g %d\n", lbl, h.total, ms)
		fmt.Fprintf(buf, "http_server_request_duration_seconds_sum{%s} %g %d\n", lbl, h.sum, ms)
		g.stats.Samples += len(buckets) + 3
	}
	p := g.progress(ts.Add(-Step))
	svcs := make([]string, 0, len(resources))
	for s := range resources {
		svcs = append(svcs, s)
	}
	sort.Strings(svcs)
	for _, s := range svcs {
		r := resources[s]
		eff, faulty := g.effect(s, p)
		cpuMult, memMult := 1.0, 1.0
		if faulty && eff.CPU != nil {
			cpuMult = eff.CPU(p)
		}
		if faulty && eff.Memory != nil {
			memMult = eff.Memory(p)
		}
		g.cpu[s] += r.cpu * cpuMult * Step.Seconds() * (1 + 0.1*g.rng.NormFloat64())
		mem := r.mem * memMult * (1 + 0.02*g.rng.NormFloat64())
		fmt.Fprintf(buf, "process_cpu_seconds_total{service_name=%q} %g %d\n", s, g.cpu[s], ms)
		fmt.Fprintf(buf, "process_resident_memory_bytes{service_name=%q} %.0f %d\n", s, mem, ms)
		g.stats.Samples += 2
	}
	lag := 10 + 5*g.rng.Float64()
	if eff, faulty := g.effect("accounting", p); faulty && eff.Lag != nil {
		lag = eff.Lag(p) * (1 + 0.05*g.rng.NormFloat64())
	}
	fmt.Fprintf(buf, "kafka_consumer_lag{service_name=\"accounting\",topic=\"orders\"} %.0f %d\n", lag, ms)
	g.stats.Samples++
}

func (g *Generator) flush(ctx context.Context, sink Sink, metrics *bytes.Buffer) error {
	if metrics.Len() > 0 {
		if err := sink.Metrics(ctx, metrics.Bytes()); err != nil {
			return fmt.Errorf("write metrics: %w", err)
		}
		metrics.Reset()
	}
	if g.logs.Len() > 0 {
		if err := sink.Logs(ctx, g.logs.Bytes()); err != nil {
			return fmt.Errorf("write logs: %w", err)
		}
		g.logs.Reset()
	}
	if len(g.spans) > 0 {
		doc := otlpDoc(g.spans)
		b, err := json.Marshal(doc)
		if err != nil {
			return err
		}
		if err := sink.Spans(ctx, b); err != nil {
			return fmt.Errorf("write spans: %w", err)
		}
		g.spans = map[string][]span{}
	}
	return nil
}

func (g *Generator) poisson(lambda float64) int {
	if lambda > 30 {
		return int(math.Max(0, math.Round(lambda+math.Sqrt(lambda)*g.rng.NormFloat64())))
	}
	l, k, p := math.Exp(-lambda), 0, 1.0
	for {
		p *= g.rng.Float64()
		if p <= l {
			return k
		}
		k++
	}
}

func (g *Generator) lognormal(median, sigma float64) time.Duration {
	return time.Duration(median * math.Exp(sigma*g.rng.NormFloat64()))
}

func (g *Generator) hexID(n int) string {
	b := make([]byte, n)
	g.rng.Read(b)
	return hex.EncodeToString(b)
}

// otlpDoc renders spans as an OTLP/JSON ExportTraceServiceRequest.
func otlpDoc(bySvc map[string][]span) map[string]any {
	svcs := make([]string, 0, len(bySvc))
	for s := range bySvc {
		svcs = append(svcs, s)
	}
	sort.Strings(svcs)
	var rs []any
	for _, svc := range svcs {
		var ss []any
		for _, s := range bySvc[svc] {
			code := 1
			if s.failed {
				code = 2
			}
			m := map[string]any{
				"traceId": s.traceID, "spanId": s.spanID, "name": s.name, "kind": s.kind,
				"startTimeUnixNano": strconv.FormatInt(s.start.UnixNano(), 10),
				"endTimeUnixNano":   strconv.FormatInt(s.end.UnixNano(), 10),
				"status":            map[string]any{"code": code, "message": s.statusMsg},
			}
			if s.parentID != "" {
				m["parentSpanId"] = s.parentID
			}
			ss = append(ss, m)
		}
		rs = append(rs, map[string]any{
			"resource":   map[string]any{"attributes": []any{map[string]any{"key": "service.name", "value": map[string]any{"stringValue": svc}}}},
			"scopeSpans": []any{map[string]any{"scope": map[string]any{"name": "spoorgen"}, "spans": ss}},
		})
	}
	return map[string]any{"resourceSpans": rs}
}
