package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"time"
)

// Tempo queries Grafana Tempo's HTTP API (search with TraceQL, trace by ID,
// tag values).
type Tempo struct{ http httpClient }

// NewTempo returns a Tempo client.
func NewTempo(base, token string, timeout time.Duration) *Tempo {
	c := newHTTP(base, token, timeout)
	c.name = "tempo"
	return &Tempo{http: c}
}

// WithBasicAuth sets HTTP Basic credentials, used when no bearer token is set.
func (t *Tempo) WithBasicAuth(user, pass string) *Tempo {
	t.http.user, t.http.pass = user, pass
	return t
}

// WithTenant sets the X-Scope-OrgID header for multi-tenant Tempo.
func (t *Tempo) WithTenant(tenant string) *Tempo {
	if tenant != "" {
		t.http.header = map[string]string{"X-Scope-OrgID": tenant}
	}
	return t
}

// TempoSpan is one span from a search result or a full trace.
type TempoSpan struct {
	TraceID, SpanID, ParentID string
	Service, Name             string
	Start                     int64 // Unix ns
	Duration                  int64 // ns
	Error                     bool
}

// TempoTrace is one search hit with its matched spans.
type TempoTrace struct {
	TraceID string
	Start   int64 // Unix ns
	Spans   []TempoSpan
}

func unixSec(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }

// Search runs a TraceQL search over [start, end] and returns up to limit
// traces with up to spss matched spans each.
func (t *Tempo) Search(ctx context.Context, q string, start, end time.Time, limit, spss int) ([]TempoTrace, error) {
	params := url.Values{"q": {q}, "start": {unixSec(start)}, "end": {strconv.FormatInt(end.Unix()+1, 10)},
		"limit": {strconv.Itoa(limit)}, "spss": {strconv.Itoa(spss)}}
	body, err := t.http.get(ctx, "/api/search", params)
	if err != nil {
		return nil, err
	}
	var r struct {
		Traces []struct {
			TraceID           string `json:"traceID"`
			StartTimeUnixNano string `json:"startTimeUnixNano"`
			SpanSets          []struct {
				Spans []struct {
					SpanID            string `json:"spanID"`
					Name              string `json:"name"`
					StartTimeUnixNano string `json:"startTimeUnixNano"`
					DurationNanos     string `json:"durationNanos"`
					Attributes        []struct {
						Key   string `json:"key"`
						Value struct {
							StringValue string `json:"stringValue"`
						} `json:"value"`
					} `json:"attributes"`
				} `json:"spans"`
			} `json:"spanSets"`
		} `json:"traces"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("decode tempo search: %w", err)
	}
	out := make([]TempoTrace, 0, len(r.Traces))
	for _, tr := range r.Traces {
		tt := TempoTrace{TraceID: tr.TraceID, Start: atoi64(tr.StartTimeUnixNano)}
		seen := map[string]bool{}
		for _, set := range tr.SpanSets {
			for _, s := range set.Spans {
				if seen[s.SpanID] {
					continue
				}
				seen[s.SpanID] = true
				sp := TempoSpan{TraceID: tr.TraceID, SpanID: s.SpanID, Name: s.Name, Start: atoi64(s.StartTimeUnixNano), Duration: atoi64(s.DurationNanos)}
				for _, a := range s.Attributes {
					switch a.Key {
					case "status":
						sp.Error = a.Value.StringValue == "error"
					case "service.name":
						sp.Service = a.Value.StringValue
					}
				}
				tt.Spans = append(tt.Spans, sp)
			}
		}
		out = append(out, tt)
	}
	return out, nil
}

// Trace fetches every span of one trace.
func (t *Tempo) Trace(ctx context.Context, traceID string, start, end time.Time) ([]TempoSpan, error) {
	params := url.Values{"start": {unixSec(start)}, "end": {strconv.FormatInt(end.Unix()+1, 10)}}
	body, err := t.http.get(ctx, "/api/v2/traces/"+url.PathEscape(traceID), params)
	if err != nil {
		return nil, err
	}
	var r struct {
		Trace otlpTrace `json:"trace"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("decode tempo trace: %w", err)
	}
	return r.Trace.spans(traceID), nil
}

// TagValues lists the values of a tag (e.g. resource.service.name) seen in
// [start, end].
func (t *Tempo) TagValues(ctx context.Context, tag string, start, end time.Time) ([]string, error) {
	params := url.Values{"start": {unixSec(start)}, "end": {strconv.FormatInt(end.Unix()+1, 10)}}
	body, err := t.http.get(ctx, "/api/v2/search/tag/"+url.PathEscape(tag)+"/values", params)
	if err != nil {
		return nil, err
	}
	var r struct {
		TagValues []struct {
			Value string `json:"value"`
		} `json:"tagValues"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("decode tempo tag values: %w", err)
	}
	out := make([]string, 0, len(r.TagValues))
	for _, v := range r.TagValues {
		out = append(out, v.Value)
	}
	sort.Strings(out)
	return out, nil
}

// otlpTrace is the OTLP/JSON shape Tempo returns for a trace.
type otlpTrace struct {
	ResourceSpans []struct {
		Resource struct {
			Attributes []otlpAttr `json:"attributes"`
		} `json:"resource"`
		ScopeSpans []struct {
			Spans []struct {
				SpanID            string `json:"spanId"`
				ParentSpanID      string `json:"parentSpanId"`
				Name              string `json:"name"`
				StartTimeUnixNano string `json:"startTimeUnixNano"`
				EndTimeUnixNano   string `json:"endTimeUnixNano"`
				Status            struct {
					Code any `json:"code"`
				} `json:"status"`
			} `json:"spans"`
		} `json:"scopeSpans"`
	} `json:"resourceSpans"`
}

type otlpAttr struct {
	Key   string `json:"key"`
	Value struct {
		StringValue string `json:"stringValue"`
	} `json:"value"`
}

func (o otlpTrace) spans(traceID string) []TempoSpan {
	var out []TempoSpan
	for _, rs := range o.ResourceSpans {
		svc := ""
		for _, a := range rs.Resource.Attributes {
			if a.Key == "service.name" {
				svc = a.Value.StringValue
			}
		}
		for _, ss := range rs.ScopeSpans {
			for _, s := range ss.Spans {
				start, end := atoi64(s.StartTimeUnixNano), atoi64(s.EndTimeUnixNano)
				out = append(out, TempoSpan{
					TraceID: traceID, SpanID: s.SpanID, ParentID: s.ParentSpanID, Service: svc, Name: s.Name,
					Start: start, Duration: end - start, Error: statusIsError(s.Status.Code),
				})
			}
		}
	}
	return out
}

// statusIsError reads an OTLP status code given as a number (2) or an enum
// name (STATUS_CODE_ERROR).
func statusIsError(c any) bool {
	switch v := c.(type) {
	case float64:
		return v == 2
	case string:
		return v == "STATUS_CODE_ERROR" || v == "2"
	}
	return false
}

func atoi64(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}
