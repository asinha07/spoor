package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Loki queries Grafana Loki's HTTP API with LogQL.
type Loki struct{ http httpClient }

// NewLoki returns a Loki client.
func NewLoki(base, token string, timeout time.Duration) *Loki {
	c := newHTTP(base, token, timeout)
	c.name = "loki"
	return &Loki{http: c}
}

// WithBasicAuth sets HTTP Basic credentials, used when no bearer token is set.
func (l *Loki) WithBasicAuth(user, pass string) *Loki {
	l.http.user, l.http.pass = user, pass
	return l
}

// WithTenant sets the X-Scope-OrgID header for multi-tenant Loki.
func (l *Loki) WithTenant(tenant string) *Loki {
	if tenant != "" {
		l.http.header = map[string]string{"X-Scope-OrgID": tenant}
	}
	return l
}

// LokiEntry is one log line with its labels.
type LokiEntry struct {
	Labels map[string]string
	Time   time.Time
	Line   string
}

type lokiResp struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string          `json:"resultType"`
		Result     json.RawMessage `json:"result"`
	} `json:"data"`
}

func nsString(t time.Time) string { return strconv.FormatInt(t.UnixNano(), 10) }

// Instant evaluates a LogQL metric query at t.
func (l *Loki) Instant(ctx context.Context, q string, at time.Time) ([]Series, error) {
	body, err := l.http.get(ctx, "/loki/api/v1/query", url.Values{"query": {q}, "time": {nsString(at)}})
	if err != nil {
		return nil, err
	}
	return parseLokiSeries(body)
}

// Range evaluates a LogQL metric query from start to end every step.
func (l *Loki) Range(ctx context.Context, q string, start, end time.Time, step time.Duration) ([]Series, error) {
	params := url.Values{"query": {q}, "start": {nsString(start)}, "end": {nsString(end)}, "step": {strconv.FormatFloat(step.Seconds(), 'f', -1, 64)}}
	body, err := l.http.get(ctx, "/loki/api/v1/query_range", params)
	if err != nil {
		return nil, err
	}
	return parseLokiSeries(body)
}

// Lines runs a LogQL log query and returns up to limit entries; forward
// returns the oldest first.
func (l *Loki) Lines(ctx context.Context, q string, start, end time.Time, limit int, forward bool) ([]LokiEntry, error) {
	dir := "backward"
	if forward {
		dir = "forward"
	}
	params := url.Values{"query": {q}, "start": {nsString(start)}, "end": {nsString(end)}, "limit": {strconv.Itoa(limit)}, "direction": {dir}}
	body, err := l.http.get(ctx, "/loki/api/v1/query_range", params)
	if err != nil {
		return nil, err
	}
	var r lokiResp
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("decode loki response: %w", err)
	}
	if r.Data.ResultType != "streams" {
		return nil, fmt.Errorf("loki: want streams, got %q", r.Data.ResultType)
	}
	var streams []struct {
		Stream map[string]string `json:"stream"`
		Values [][]any           `json:"values"`
	}
	if err := json.Unmarshal(r.Data.Result, &streams); err != nil {
		return nil, fmt.Errorf("decode loki streams: %w", err)
	}
	var out []LokiEntry
	for _, s := range streams {
		for _, v := range s.Values {
			if len(v) < 2 {
				continue
			}
			ts, _ := v[0].(string)
			line, _ := v[1].(string)
			ns, err := strconv.ParseInt(ts, 10, 64)
			if err != nil {
				continue
			}
			out = append(out, LokiEntry{Labels: s.Stream, Time: time.Unix(0, ns).UTC(), Line: line})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if forward {
			return out[i].Time.Before(out[j].Time)
		}
		return out[i].Time.After(out[j].Time)
	})
	return out, nil
}

// LabelValues lists the values of a label seen in [start, end].
func (l *Loki) LabelValues(ctx context.Context, label string, start, end time.Time) ([]string, error) {
	body, err := l.http.get(ctx, "/loki/api/v1/label/"+url.PathEscape(label)+"/values", url.Values{"start": {nsString(start)}, "end": {nsString(end)}})
	if err != nil {
		return nil, err
	}
	var r struct {
		Data []string `json:"data"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("decode label values: %w", err)
	}
	sort.Strings(r.Data)
	return r.Data, nil
}

// IsSeriesLimit reports whether err is Loki refusing a query that returns
// too many series.
func IsSeriesLimit(err error) bool {
	return err != nil && strings.Contains(err.Error(), "maximum of series")
}

func parseLokiSeries(body []byte) ([]Series, error) {
	var r lokiResp
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("decode loki response: %w", err)
	}
	switch r.Data.ResultType {
	case "vector":
		var vec []struct {
			Metric map[string]string `json:"metric"`
			Value  []any             `json:"value"`
		}
		if err := json.Unmarshal(r.Data.Result, &vec); err != nil {
			return nil, err
		}
		out := make([]Series, 0, len(vec))
		for _, v := range vec {
			if p, ok := parsePoint(v.Value); ok {
				out = append(out, Series{Labels: v.Metric, Points: []Point{p}})
			}
		}
		return out, nil
	case "matrix":
		var mat []struct {
			Metric map[string]string `json:"metric"`
			Values [][]any           `json:"values"`
		}
		if err := json.Unmarshal(r.Data.Result, &mat); err != nil {
			return nil, err
		}
		out := make([]Series, 0, len(mat))
		for _, m := range mat {
			s := Series{Labels: m.Metric}
			for _, v := range m.Values {
				if p, ok := parsePoint(v); ok {
					s.Points = append(s.Points, p)
				}
			}
			out = append(out, s)
		}
		return out, nil
	}
	return nil, fmt.Errorf("loki: unexpected result type %q", r.Data.ResultType)
}
