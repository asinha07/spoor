package digest

import (
	"fmt"
	"math"
	"sort"
	"time"
)

// median returns the median of xs (0 for empty input).
func median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// mad returns the median absolute deviation of xs around m.
func mad(xs []float64, m float64) float64 {
	d := make([]float64, len(xs))
	for i, x := range xs {
		d[i] = math.Abs(x - m)
	}
	return median(d)
}

// robustZ measures how far cur sits from the baseline history, in units of
// the history's own spread. The spread is floored by relFloor×|median| and
// absFloor so that a flat or tiny baseline does not turn noise into signal.
func robustZ(cur float64, history []float64, relFloor, absFloor float64) (z, med float64) {
	med = median(history)
	scale := math.Max(1.4826*mad(history, med), math.Max(relFloor*math.Abs(med), absFloor))
	if scale == 0 {
		scale = 1e-12
	}
	return (cur - med) / scale, med
}

// zScore maps a non-negative z to 0..1. z=3 maps to 0.5, the default report threshold.
func zScore(z float64) float64 {
	if z <= 0 {
		return 0
	}
	return z / (z + 3)
}

// round2 keeps scores short in JSON.
func round2(x float64) float64 { return math.Round(x*100) / 100 }

// humanValue formats a metric value with its unit.
func humanValue(v float64, unit string) string {
	switch unit {
	case "ratio":
		return fmt.Sprintf("%.1f%%", v*100)
	case "seconds":
		d := time.Duration(v * float64(time.Second))
		switch {
		case d >= time.Second:
			return fmt.Sprintf("%.2fs", d.Seconds())
		case d >= time.Millisecond:
			return fmt.Sprintf("%dms", d.Milliseconds())
		default:
			return fmt.Sprintf("%dµs", d.Microseconds())
		}
	case "reqps":
		return trimFloat(v) + "/s"
	case "bytes":
		return humanBytes(v)
	case "":
		return trimFloat(v)
	default:
		return trimFloat(v) + " " + unit
	}
}

func trimFloat(v float64) string {
	a := math.Abs(v)
	switch {
	case a >= 100:
		return fmt.Sprintf("%.0f", v)
	case a >= 10:
		return fmt.Sprintf("%.1f", v)
	case a >= 0.01 || a == 0:
		return fmt.Sprintf("%.2f", v)
	default:
		return fmt.Sprintf("%.2g", v)
	}
}

func humanBytes(v float64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	i := 0
	for math.Abs(v) >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	return trimFloat(v) + " " + units[i]
}

// humanNanos formats a nanosecond duration.
func humanNanos(ns float64) string { return humanValue(ns/1e9, "seconds") }

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
