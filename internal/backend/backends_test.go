package backend

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNewRelicSendsReadOnlyNRQL(t *testing.T) {
	var got map[string]any
	var key string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key = r.Header.Get("API-Key")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		_, _ = io.WriteString(w, `{"data":{"actor":{"account":{"nrql":{"results":[{"facet":"pay","p95":{"95":0.42}}]}}}}}`)
	}))
	defer srv.Close()
	nr := NewNewRelic(srv.URL+"/graphql", "NRAK-TEST", 42, time.Second)
	rows, err := nr.NRQL(context.Background(), "SELECT percentile(duration, 95) AS p95 FROM Transaction FACET appName")
	if err != nil {
		t.Fatal(err)
	}
	if key != "NRAK-TEST" {
		t.Errorf("API-Key header = %q", key)
	}
	q, _ := got["query"].(string)
	if !strings.HasPrefix(q, "query(") || strings.Contains(strings.ToLower(q), "mutation") || !strings.Contains(q, "nrql(query: $nrql, timeout: 60)") {
		t.Errorf("NerdGraph document must be a read-only nrql query: %s", q)
	}
	vars, _ := got["variables"].(map[string]any)
	if vars["id"] != float64(42) || !strings.Contains(vars["nrql"].(string), "FACET appName") {
		t.Errorf("variables = %v", vars)
	}
	if v, ok := NRQLNumber(rows[0]["p95"]); !ok || v != 0.42 {
		t.Errorf("percentile value = %v", rows[0]["p95"])
	}
}

func TestNewRelicErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"errors":[{"message":"Invalid API key"}]}`)
	}))
	defer srv.Close()
	_, err := NewNewRelic(srv.URL, "bad", 1, time.Second).NRQL(context.Background(), "SELECT 1 FROM Log")
	if err == nil || !strings.Contains(err.Error(), "backend unavailable") {
		t.Errorf("an auth failure must read as unavailable: %v", err)
	}
	if _, err := NewNewRelic(srv.URL, "", 1, time.Second).NRQL(context.Background(), "SELECT 1 FROM Log"); err == nil || !strings.Contains(err.Error(), "NEW_RELIC_API_KEY") {
		t.Errorf("a missing key must say how to set it: %v", err)
	}
}

func TestLokiParsesVectorMatrixStreams(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("Loki must be read with GET, got %s", r.Method)
		}
		switch {
		case r.URL.Path == "/loki/api/v1/query":
			_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"service_name":"pay"},"value":[1700000000,"12"]}]}}`)
		case r.URL.Query().Get("limit") != "":
			_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"streams","result":[{"stream":{"spoor_msg":"boom"},"values":[["1700000001000000000","boom 2"],["1700000000000000000","boom 1"]]}]}}`)
		default:
			_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"service_name":"pay"},"values":[[1700000000,"1"],[1700000060,"2"]]}]}}`)
		}
	}))
	defer srv.Close()
	l := NewLoki(srv.URL, "", time.Second)
	ctx := context.Background()
	v, err := l.Instant(ctx, `sum(count_over_time({a="b"}[5m]))`, time.Unix(1700000000, 0))
	if err != nil || v[0].Points[0].V != 12 {
		t.Fatalf("vector: %+v %v", v, err)
	}
	m, err := l.Range(ctx, `q`, time.Unix(1700000000, 0), time.Unix(1700000060, 0), time.Minute)
	if err != nil || len(m[0].Points) != 2 {
		t.Fatalf("matrix: %+v %v", m, err)
	}
	lines, err := l.Lines(ctx, `{a="b"}`, time.Unix(1700000000, 0), time.Unix(1700000060, 0), 10, true)
	if err != nil || len(lines) != 2 || lines[0].Line != "boom 1" {
		t.Fatalf("streams (forward = oldest first): %+v %v", lines, err)
	}
	if !IsSeriesLimit(errString("query returned 400: maximum of series (500) reached for a single query")) {
		t.Error("series limit not recognized")
	}
}

type errString string

func (e errString) Error() string { return string(e) }

func TestTempoParsesSearchAndTrace(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/search":
			_, _ = io.WriteString(w, `{"traces":[{"traceID":"abc","startTimeUnixNano":"5","spanSets":[{"spans":[{"spanID":"s1","name":"POST /charge","startTimeUnixNano":"6","durationNanos":"35000000","attributes":[{"key":"status","value":{"stringValue":"error"}}]}]}]}]}`)
		case strings.HasPrefix(r.URL.Path, "/api/v2/traces/"):
			_, _ = io.WriteString(w, `{"trace":{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"pay"}}]},"scopeSpans":[{"spans":[{"spanId":"s1","parentSpanId":"r","name":"POST /charge","startTimeUnixNano":"10","endTimeUnixNano":"40","status":{"code":"STATUS_CODE_ERROR"}}]}]}]}}`)
		default:
			_, _ = io.WriteString(w, `{"tagValues":[{"type":"string","value":"pay"}]}`)
		}
	}))
	defer srv.Close()
	tp := NewTempo(srv.URL, "", time.Second)
	ctx := context.Background()
	tr, err := tp.Search(ctx, `{ }`, time.Unix(0, 0), time.Unix(100, 0), 10, 5)
	if err != nil || len(tr) != 1 || !tr[0].Spans[0].Error || tr[0].Spans[0].Duration != 35e6 || tr[0].Spans[0].Name != "POST /charge" {
		t.Fatalf("search: %+v %v", tr, err)
	}
	sp, err := tp.Trace(ctx, "abc", time.Unix(0, 0), time.Unix(100, 0))
	if err != nil || len(sp) != 1 || sp[0].Service != "pay" || sp[0].Duration != 30 || !sp[0].Error || sp[0].ParentID != "r" {
		t.Fatalf("trace: %+v %v", sp, err)
	}
	vals, err := tp.TagValues(ctx, "resource.service.name", time.Unix(0, 0), time.Unix(100, 0))
	if err != nil || len(vals) != 1 || vals[0] != "pay" {
		t.Fatalf("tag values: %v %v", vals, err)
	}
}

func TestLabelValuesUsesGet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed) // as Prometheus does
			return
		}
		_, _ = io.WriteString(w, `{"status":"success","data":["b","a"]}`)
	}))
	defer srv.Close()
	vals, err := NewMetrics(srv.URL, "", time.Second).LabelValues(context.Background(), "service_name", time.Unix(0, 0), time.Unix(100, 0))
	if err != nil || len(vals) != 2 || vals[0] != "a" {
		t.Fatalf("label values: %v %v", vals, err)
	}
}
