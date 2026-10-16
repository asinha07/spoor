package backend

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// LogsQL is a client for the LogsQL query API. VictoriaLogs serves logs with
// it and VictoriaTraces serves spans with it, so one client covers both.
type LogsQL struct{ http httpClient }

// NewLogsQL returns a client for base (e.g. http://localhost:9428).
func NewLogsQL(base, token string, timeout time.Duration) *LogsQL {
	h := newHTTP(base, token, timeout)
	h.name = "logsql"
	return &LogsQL{http: h}
}

// WithBasicAuth sends HTTP Basic credentials when no bearer token is set.
func (l *LogsQL) WithBasicAuth(user, pass string) *LogsQL {
	l.http.user, l.http.pass = user, pass
	return l
}

// WithName labels this client's requests in Spoor's own metrics ("logs", "traces").
func (l *LogsQL) WithName(name string) *LogsQL {
	l.http.name = name
	return l
}

// Row is one result line: field name to string value.
type Row map[string]string

// Query runs a LogsQL query over [start, end) and returns up to limit rows
// (0 means the server default).
func (l *LogsQL) Query(ctx context.Context, q string, start, end time.Time, limit int) ([]Row, error) {
	form := url.Values{
		"query": {q},
		"start": {start.UTC().Format(time.RFC3339Nano)},
		"end":   {end.UTC().Add(-time.Nanosecond).Format(time.RFC3339Nano)},
	}
	if limit > 0 {
		form.Set("limit", strconv.Itoa(limit))
	}
	body, err := l.http.post(ctx, "/select/logsql/query", form)
	if err != nil {
		return nil, err
	}
	return parseJSONLines(body)
}

func parseJSONLines(body []byte) ([]Row, error) {
	var rows []Row
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 0, 64*1024), 16<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var raw map[string]any
		if err := json.Unmarshal(line, &raw); err != nil {
			return nil, fmt.Errorf("decode LogsQL row: %w", err)
		}
		row := make(Row, len(raw))
		for k, v := range raw {
			switch t := v.(type) {
			case string:
				row[k] = t
			case nil:
				row[k] = ""
			default:
				row[k] = fmt.Sprint(t)
			}
		}
		rows = append(rows, row)
	}
	return rows, sc.Err()
}

// Quote returns s as a LogsQL quoted string (safe for field names and values).
func Quote(s string) string { return strconv.Quote(s) }

// Eq returns an exact-match LogsQL filter: "field":="value".
func Eq(field, value string) string { return Quote(field) + ":=" + Quote(value) }

// Float reads a numeric field, returning 0 when absent or malformed.
func (r Row) Float(field string) float64 {
	f, _ := strconv.ParseFloat(strings.TrimSpace(r[field]), 64)
	return f
}

// Time reads an RFC 3339 field, returning zero time when absent.
func (r Row) Time(field string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, r[field])
	return t
}
