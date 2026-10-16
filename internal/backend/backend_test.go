package backend

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func server(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return s
}

func TestMetricsInstantDropsNaN(t *testing.T) {
	var gotForm url.Values
	var gotAuth string
	s := server(t, func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotForm, gotAuth = r.PostForm, r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"vector","result":[
			{"metric":{"a":"1"},"value":[1700000000,"0.5"]},
			{"metric":{"a":"2"},"value":[1700000000,"NaN"]},
			{"metric":{"a":"3"},"value":[1700000000,"+Inf"]}]}}`)
	})
	m := NewMetrics(s.URL, "tok", time.Second)
	res, err := m.Instant(context.Background(), "up", time.Unix(1700000000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].Labels["a"] != "1" || res[0].Points[0].V != 0.5 {
		t.Errorf("result = %+v, want only the finite sample", res)
	}
	if gotForm.Get("query") != "up" || gotForm.Get("time") != "1700000000" {
		t.Errorf("form = %v", gotForm)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("auth header = %q", gotAuth)
	}
	if v, ok := Scalar(res); !ok || v != 0.5 {
		t.Errorf("Scalar = %v %v", v, ok)
	}
}

func TestMetricsRange(t *testing.T) {
	s := server(t, func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.URL.Path != "/api/v1/query_range" || r.PostForm.Get("step") != "60s" {
			t.Errorf("path %s step %s", r.URL.Path, r.PostForm.Get("step"))
		}
		_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{},"values":[[1,"1"],[61,"2"]]}]}}`)
	})
	res, err := NewMetrics(s.URL, "", time.Second).Range(context.Background(), "x", time.Unix(1, 0), time.Unix(61, 0), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || len(res[0].Points) != 2 || res[0].Points[1].V != 2 {
		t.Errorf("range = %+v", res)
	}
}

func TestErrorsClassified(t *testing.T) {
	cases := []struct {
		status      int
		unavailable bool
	}{{500, true}, {401, true}, {400, false}, {422, false}}
	for _, c := range cases {
		s := server(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(c.status)
			_, _ = io.WriteString(w, "boom")
		})
		_, err := NewMetrics(s.URL, "", time.Second).Instant(context.Background(), "x", time.Now())
		if err == nil {
			t.Fatalf("status %d: want error", c.status)
		}
		if got := errors.Is(err, ErrUnavailable); got != c.unavailable {
			t.Errorf("status %d: unavailable = %v, want %v (%v)", c.status, got, c.unavailable, err)
		}
	}
	_, err := NewMetrics("", "", time.Second).Instant(context.Background(), "x", time.Now())
	if !errors.Is(err, ErrUnavailable) {
		t.Errorf("empty URL: %v", err)
	}
	_, err = NewMetrics("http://127.0.0.1:1", "", time.Second).Instant(context.Background(), "x", time.Now())
	if !errors.Is(err, ErrUnavailable) {
		t.Errorf("refused connection: %v", err)
	}
}

func TestLogsQLQuery(t *testing.T) {
	var form url.Values
	s := server(t, func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		form = r.PostForm
		_, _ = io.WriteString(w, "{\"_msg\":\"a <N>\",\"n\":\"3\",\"first\":\"2026-01-01T00:00:00Z\"}\n\n{\"_msg\":\"b\",\"n\":7,\"x\":null}\n")
	})
	l := NewLogsQL(s.URL, "", time.Second)
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rows, err := l.Query(context.Background(), "*", start, start.Add(time.Hour), 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Float("n") != 3 || rows[1].Float("n") != 7 || rows[1]["x"] != "" {
		t.Errorf("rows = %+v", rows)
	}
	if !rows[0].Time("first").Equal(start) {
		t.Errorf("Time = %v", rows[0].Time("first"))
	}
	if form.Get("limit") != "5" || form.Get("start") != "2026-01-01T00:00:00Z" || !strings.HasPrefix(form.Get("end"), "2026-01-01T00:59:59.999") {
		t.Errorf("form = %v (end should be exclusive)", form)
	}
}

func TestQuoting(t *testing.T) {
	if got := Eq("resource_attr:service.name", `a"b`); got != `"resource_attr:service.name":="a\"b"` {
		t.Errorf("Eq = %s", got)
	}
}

// Grafana's datasource proxy: a base URL with a path prefix and Basic auth.
func TestBasicAuthAndPathPrefix(t *testing.T) {
	var gotPath, gotUser, gotPass string
	var basic bool
	s := server(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotUser, gotPass, basic = r.BasicAuth()
		_, _ = io.WriteString(w, `{"n":"1"}`+"\n")
	})
	l := NewLogsQL(s.URL+"/api/datasources/proxy/uid/vl", "", time.Second).WithBasicAuth("u", "p")
	rows, err := l.Query(context.Background(), "*", time.Unix(0, 0), time.Unix(60, 0), 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
	if gotPath != "/api/datasources/proxy/uid/vl/select/logsql/query" {
		t.Errorf("path = %s", gotPath)
	}
	if !basic || gotUser != "u" || gotPass != "p" {
		t.Errorf("basic auth = %v %q %q", basic, gotUser, gotPass)
	}
	before, _ := Stats()
	_, _ = l.Query(context.Background(), "*", time.Unix(0, 0), time.Unix(60, 0), 0)
	if after, _ := Stats(); after != before+1 {
		t.Errorf("Stats requests %d -> %d, want +1", before, after)
	}
}

func TestBearerWinsOverBasic(t *testing.T) {
	var auth string
	s := server(t, func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, "")
	})
	_, _ = NewLogsQL(s.URL, "tok", time.Second).WithBasicAuth("u", "p").Query(context.Background(), "*", time.Unix(0, 0), time.Unix(60, 0), 0)
	if auth != "Bearer tok" {
		t.Errorf("Authorization = %q, want bearer", auth)
	}
}
