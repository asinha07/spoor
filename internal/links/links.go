// Package links builds evidence URLs that open the exact query behind a
// finding in the backend's built-in UI (VMUI), so a human can verify it.
package links

import (
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
