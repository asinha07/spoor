package gen

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// GrafanaSink writes the same scenarios to Prometheus (remote write), Loki
// (push API) and Tempo (OTLP/HTTP), for the Grafana-stack integration tests.
type GrafanaSink struct {
	PrometheusURL, LokiURL, TempoOTLPURL string
	// ServiceFields are the log fields that become Loki stream labels
	// (dots become underscores: service.name -> service_name).
	ServiceFields []string
	Client        *http.Client
}

func (s GrafanaSink) http() HTTPSink { return HTTPSink{Client: s.Client} }

// Metrics converts Prometheus text with millisecond timestamps into a
// remote-write request.
func (s GrafanaSink) Metrics(ctx context.Context, promText []byte) error {
	series := map[string]*rwSeries{}
	var order []string
	for _, line := range strings.Split(string(promText), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		labels, rest, err := parseSeries(line)
		if err != nil {
			return err
		}
		f := strings.Fields(rest)
		if len(f) != 2 {
			return fmt.Errorf("metrics line without value and timestamp: %q", line)
		}
		v, err := strconv.ParseFloat(f[0], 64)
		if err != nil {
			return err
		}
		ts, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil {
			return err
		}
		key := labelKey(labels)
		sr := series[key]
		if sr == nil {
			sr = &rwSeries{labels: labels}
			series[key] = sr
			order = append(order, key)
		}
		sr.samples = append(sr.samples, rwSample{v, ts})
	}
	var req []byte
	for _, k := range order {
		req = appendBytes(req, 1, series[k].encode())
	}
	return s.http().post(ctx, strings.TrimRight(s.PrometheusURL, "/")+"/api/v1/write", "application/x-protobuf", snappyLiteral(req), func(r *http.Request) {
		r.Header.Set("Content-Encoding", "snappy")
		r.Header.Set("X-Prometheus-Remote-Write-Version", "0.1.0")
	})
}

// Logs pushes JSON lines to Loki: the service fields become stream labels,
// _msg the line, and every other field structured metadata.
func (s GrafanaSink) Logs(ctx context.Context, jsonLines []byte) error {
	type stream struct {
		Stream map[string]string `json:"stream"`
		Values [][]any           `json:"values"`
	}
	streams := map[string]*stream{}
	var order []string
	for _, line := range bytes.Split(jsonLines, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var doc map[string]any
		if err := json.Unmarshal(line, &doc); err != nil {
			return err
		}
		t, err := time.Parse(time.RFC3339Nano, fmt.Sprint(doc["_time"]))
		if err != nil {
			return err
		}
		labels := map[string]string{"source": "spoorgen"}
		meta := map[string]string{}
		for k, v := range doc {
			if k == "_time" || k == "_msg" {
				continue
			}
			name := lokiName(k)
			isService := false
			for _, f := range s.ServiceFields {
				isService = isService || f == k
			}
			if isService {
				labels[name] = fmt.Sprint(v)
			} else {
				meta[name] = fmt.Sprint(v)
			}
		}
		key := labelKey(labels)
		st := streams[key]
		if st == nil {
			st = &stream{Stream: labels}
			streams[key] = st
			order = append(order, key)
		}
		st.Values = append(st.Values, []any{strconv.FormatInt(t.UnixNano(), 10), fmt.Sprint(doc["_msg"]), meta})
	}
	body := struct {
		Streams []*stream `json:"streams"`
	}{}
	for _, k := range order {
		body.Streams = append(body.Streams, streams[k])
	}
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	return s.http().post(ctx, strings.TrimRight(s.LokiURL, "/")+"/loki/api/v1/push", "application/json", b)
}

// Spans sends OTLP/JSON traces to Tempo's OTLP/HTTP receiver.
func (s GrafanaSink) Spans(ctx context.Context, otlpJSON []byte) error {
	return s.http().post(ctx, strings.TrimRight(s.TempoOTLPURL, "/")+"/v1/traces", "application/json", otlpJSON)
}

func lokiName(f string) string {
	b := []byte(f)
	for i, c := range b {
		if !(c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
			b[i] = '_'
		}
	}
	return string(b)
}

func labelKey(m map[string]string) string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	var b strings.Builder
	for _, k := range ks {
		b.WriteString(k + "\x00" + m[k] + "\x00")
	}
	return b.String()
}

// parseSeries reads `name{k="v",...}` and returns the labels (with
// __name__) and the rest of the line.
func parseSeries(line string) (map[string]string, string, error) {
	labels := map[string]string{}
	i := strings.IndexAny(line, "{ ")
	if i < 0 {
		return nil, "", fmt.Errorf("bad metrics line %q", line)
	}
	labels["__name__"] = line[:i]
	if line[i] == ' ' {
		return labels, line[i:], nil
	}
	j := i + 1
	for j < len(line) && line[j] != '}' {
		eq := strings.IndexByte(line[j:], '=')
		if eq < 0 {
			return nil, "", fmt.Errorf("bad labels in %q", line)
		}
		name := strings.TrimSpace(strings.TrimPrefix(line[j:j+eq], ","))
		k := j + eq + 1
		if k >= len(line) || line[k] != '"' {
			return nil, "", fmt.Errorf("bad label value in %q", line)
		}
		end := k + 1
		for end < len(line) && !(line[end] == '"' && line[end-1] != '\\') {
			end++
		}
		val, err := strconv.Unquote(line[k : end+1])
		if err != nil {
			return nil, "", err
		}
		labels[name] = val
		j = end + 1
		if j < len(line) && line[j] == ',' {
			j++
		}
	}
	return labels, line[j+1:], nil
}

type rwSample struct {
	v  float64
	ts int64
}

type rwSeries struct {
	labels  map[string]string
	samples []rwSample
}

// encode writes a prometheus.TimeSeries protobuf: labels (sorted), samples.
func (s *rwSeries) encode() []byte {
	names := make([]string, 0, len(s.labels))
	for k := range s.labels {
		names = append(names, k)
	}
	sort.Strings(names)
	var out []byte
	for _, n := range names {
		var l []byte
		l = appendBytes(l, 1, []byte(n))
		l = appendBytes(l, 2, []byte(s.labels[n]))
		out = appendBytes(out, 1, l)
	}
	sort.Slice(s.samples, func(i, j int) bool { return s.samples[i].ts < s.samples[j].ts })
	for _, sm := range s.samples {
		var b []byte
		b = append(b, 1<<3|1) // field 1, fixed64
		b = binary.LittleEndian.AppendUint64(b, math.Float64bits(sm.v))
		b = append(b, 2<<3|0) // field 2, varint
		b = binary.AppendUvarint(b, uint64(sm.ts))
		out = appendBytes(out, 2, b)
	}
	return out
}

func appendBytes(b []byte, field int, v []byte) []byte {
	b = binary.AppendUvarint(b, uint64(field<<3|2))
	b = binary.AppendUvarint(b, uint64(len(v)))
	return append(b, v...)
}

// snappyLiteral encodes src as a valid snappy block made only of literals:
// no compression, but any snappy decoder reads it.
func snappyLiteral(src []byte) []byte {
	out := binary.AppendUvarint(nil, uint64(len(src)))
	for len(src) > 0 {
		n := len(src)
		if n > 65536 {
			n = 65536
		}
		l := n - 1
		switch {
		case l < 60:
			out = append(out, byte(l<<2))
		case l < 1<<8:
			out = append(out, 60<<2, byte(l))
		default:
			out = append(out, 61<<2, byte(l), byte(l>>8))
		}
		out = append(out, src[:n]...)
		src = src[n:]
	}
	return out
}
