// Package links builds evidence URLs that open the exact query behind a
// finding in the backend's built-in UI (VMUI), so a human can verify it.
package links

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strings"
	"time"
)

// Metrics returns a VMUI link for a MetricsQL query over [start, end].
func Metrics(base, query string, start, end time.Time) string {
	if base == "" {
		return ""
	}
	v := url.Values{}
	v.Set("g0.expr", query)
	v.Set("g0.range_input", durationInput(end.Sub(start)))
	v.Set("g0.end_input", end.UTC().Format("2006-01-02T15:04:05"))
	v.Set("g0.relative_time", "none")
	return strings.TrimRight(base, "/") + "/vmui/#/?" + v.Encode()
}

// LogsQL returns a VMUI link for a LogsQL query over [start, end] on
// VictoriaLogs or VictoriaTraces.
func LogsQL(base, query string, start, end time.Time) string {
	if base == "" {
		return ""
	}
	v := url.Values{}
	v.Set("query", query)
	v.Set("g0.range_input", durationInput(end.Sub(start)))
	v.Set("g0.end_input", end.UTC().Format("2006-01-02T15:04:05"))
	v.Set("g0.relative_time", "none")
	return strings.TrimRight(base, "/") + "/select/vmui/#/?" + v.Encode()
}

// Grafana returns a Grafana Explore link that runs expr against one
// datasource over [start, end].
func Grafana(grafanaURL, uid, dsType, expr string, start, end time.Time) string {
	if grafanaURL == "" || uid == "" {
		return ""
	}
	q := map[string]any{"refId": "A", "expr": expr, "datasource": map[string]string{"uid": uid, "type": dsType}}
	if dsType == "tempo" {
		// Tempo's datasource takes a TraceQL query, not expr.
		q = map[string]any{"refId": "A", "query": expr, "queryType": "traceql", "datasource": map[string]string{"uid": uid, "type": dsType}}
	}
	if dsType == "" {
		q["datasource"] = map[string]string{"uid": uid}
	}
	pane := map[string]any{
		"datasource": uid,
		"queries":    []any{q},
		"range": map[string]string{
			"from": itoa(start.UnixMilli()),
			"to":   itoa(end.UnixMilli()),
		},
	}
	b, _ := json.Marshal(map[string]any{"spoor": pane})
	v := url.Values{}
	v.Set("schemaVersion", "1")
	v.Set("panes", string(b))
	return strings.TrimRight(grafanaURL, "/") + "/explore?" + v.Encode()
}

func durationInput(d time.Duration) string {
	s := int64(d / time.Second)
	switch {
	case s%86400 == 0 && s > 0:
		return itoa(s/86400) + "d"
	case s%3600 == 0 && s > 0:
		return itoa(s/3600) + "h"
	case s%60 == 0 && s > 0:
		return itoa(s/60) + "m"
	default:
		return itoa(s) + "s"
	}
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// Prometheus returns a link to the Prometheus expression browser.
func Prometheus(base, query string, start, end time.Time) string {
	if base == "" {
		return ""
	}
	v := url.Values{}
	v.Set("g0.expr", query)
	v.Set("g0.tab", "0")
	v.Set("g0.range_input", durationInput(end.Sub(start)))
	v.Set("g0.end_input", end.UTC().Format("2006-01-02 15:04:05"))
	return strings.TrimRight(base, "/") + "/graph?" + v.Encode()
}

// NewRelic returns a New Relic query builder link that runs an NRQL query
// over [start, end] in one account.
func NewRelic(accountID int64, region, nrql string, start, end time.Time) string {
	if accountID == 0 || nrql == "" {
		return ""
	}
	host := "https://one.newrelic.com"
	if strings.EqualFold(region, "eu") {
		host = "https://one.eu.newrelic.com"
	}
	q := nrql + " SINCE " + itoa(start.UnixMilli()) + " UNTIL " + itoa(end.UnixMilli())
	pane, _ := json.Marshal(map[string]any{"nerdletId": "data-exploration.query-builder", "initialActiveInterface": "nrqlEditor",
		"initialNrqlValue": q, "initialAccountId": accountID})
	v := url.Values{}
	v.Set("pane", base64URL(pane))
	return host + "/launcher/data-exploration.query-builder?" + v.Encode()
}

func base64URL(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
