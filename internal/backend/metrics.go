package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strconv"
	"time"
)

// Metrics is a MetricsQL client for VictoriaMetrics (or any Prometheus-compatible API).
type Metrics struct{ http httpClient }

// NewMetrics returns a client for base (e.g. http://localhost:8428).
func NewMetrics(base, token string, timeout time.Duration) *Metrics {
	h := newHTTP(base, token, timeout)
	h.name = "metrics"
	return &Metrics{http: h}
}

// Point is one sample of a range query.
type Point struct {
	T time.Time
	V float64
}

// Series is one labelled time series.
type Series struct {
	Labels map[string]string
	Points []Point
}

type promResp struct {
	Status string `json:"status"`
	Error  string `json:"error"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Value  []any             `json:"value"`
			Values [][]any           `json:"values"`
		} `json:"result"`
	} `json:"data"`
}

// Instant evaluates q at time at and returns one value per series.
func (m *Metrics) Instant(ctx context.Context, q string, at time.Time) ([]Series, error) {
	form := url.Values{"query": {q}, "time": {strconv.FormatInt(at.Unix(), 10)}, "nocache": {"1"}}
	body, err := m.http.post(ctx, "/api/v1/query", form)
	if err != nil {
		return nil, err
	}
	var r promResp
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("decode query response: %w", err)
	}
	if r.Status != "success" {
		return nil, fmt.Errorf("query failed: %s", r.Error)
	}
	out := make([]Series, 0, len(r.Data.Result))
	for _, res := range r.Data.Result {
		p, ok := parsePoint(res.Value)
		if !ok {
			continue
		}
		out = append(out, Series{Labels: res.Metric, Points: []Point{p}})
	}
	return out, nil
}

// Range evaluates q over [start, end] at the given step.
func (m *Metrics) Range(ctx context.Context, q string, start, end time.Time, step time.Duration) ([]Series, error) {
	if step < time.Second {
		step = time.Second
	}
	form := url.Values{
		"query":   {q},
		"start":   {strconv.FormatInt(start.Unix(), 10)},
		"end":     {strconv.FormatInt(end.Unix(), 10)},
		"step":    {strconv.FormatInt(int64(step/time.Second), 10) + "s"},
		"nocache": {"1"},
	}
	body, err := m.http.post(ctx, "/api/v1/query_range", form)
	if err != nil {
		return nil, err
	}
	var r promResp
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("decode query_range response: %w", err)
	}
	if r.Status != "success" {
		return nil, fmt.Errorf("query_range failed: %s", r.Error)
	}
	out := make([]Series, 0, len(r.Data.Result))
	for _, res := range r.Data.Result {
		s := Series{Labels: res.Metric}
		for _, v := range res.Values {
			if p, ok := parsePoint(v); ok {
				s.Points = append(s.Points, p)
			}
		}
		out = append(out, s)
	}
	return out, nil
}

// LabelValues lists values of label seen in [start, end].
func (m *Metrics) LabelValues(ctx context.Context, label string, start, end time.Time) ([]string, error) {
	form := url.Values{"start": {strconv.FormatInt(start.Unix(), 10)}, "end": {strconv.FormatInt(end.Unix(), 10)}}
	body, err := m.http.post(ctx, "/api/v1/label/"+url.PathEscape(label)+"/values", form)
	if err != nil {
		return nil, err
	}
	var r struct {
		Status string   `json:"status"`
		Data   []string `json:"data"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("decode label values: %w", err)
	}
	sort.Strings(r.Data)
	return r.Data, nil
}

// parsePoint reads a Prometheus [ts, "value"] pair, dropping NaN and Inf.
func parsePoint(v []any) (Point, bool) {
	if len(v) != 2 {
		return Point{}, false
	}
	ts, ok := v[0].(float64)
	if !ok {
		return Point{}, false
	}
	str, ok := v[1].(string)
	if !ok {
		return Point{}, false
	}
	f, err := strconv.ParseFloat(str, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return Point{}, false
	}
	sec, frac := math.Modf(ts)
	return Point{T: time.Unix(int64(sec), int64(frac*1e9)).UTC(), V: f}, true
}

// Scalar returns the single value of an instant result, or false when empty.
func Scalar(s []Series) (float64, bool) {
	if len(s) == 0 || len(s[0].Points) == 0 {
		return 0, false
	}
	return s[0].Points[0].V, true
}
