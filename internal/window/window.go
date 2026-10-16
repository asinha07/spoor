// Package window turns the since/until/baseline tool inputs into concrete
// time ranges.
package window

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var dayPrefix = regexp.MustCompile(`^(\d+)d(.*)$`)

// Range is a half-open time interval [Start, End).
type Range struct {
	Start time.Time
	End   time.Time
}

// Duration returns End-Start.
func (r Range) Duration() time.Duration { return r.End.Sub(r.Start) }

// Shift moves the range by d.
func (r Range) Shift(d time.Duration) Range { return Range{r.Start.Add(d), r.End.Add(d)} }

// Spec is the parsed tool input.
type Spec struct {
	Window   Range
	Baseline Range
	// SubWindows are the baseline slices (each Window.Duration() long) used to
	// estimate normal variation, oldest first. The last one equals Baseline.
	SubWindows []Range
	// Mode is the baseline mode that produced Baseline: previous, 1d or 7d.
	Mode string
}

// Defaults applied when inputs are empty.
const (
	DefaultSince = 30 * time.Minute
	MinWindow    = time.Minute
	MaxWindow    = 7 * 24 * time.Hour
)

// Parse resolves since/until/baseline relative to now.
//
// since and until accept RFC 3339 timestamps or a duration ago ("30m", "2h").
// baseline is "previous" (default), "1d" or "7d". subWindows (>=2) controls how
// many window-length slices of history are used to measure normal variation.
func Parse(now time.Time, since, until, baseline string, subWindows int) (Spec, error) {
	end := now
	if until != "" {
		t, err := parseTime(now, until)
		if err != nil {
			return Spec{}, fmt.Errorf("until: %w", err)
		}
		end = t
	}
	start := end.Add(-DefaultSince)
	if since != "" {
		t, err := parseTime(now, since)
		if err != nil {
			return Spec{}, fmt.Errorf("since: %w", err)
		}
		start = t
	}
	w := Range{start.UTC().Truncate(time.Second), end.UTC().Truncate(time.Second)}
	d := w.Duration()
	if d <= 0 {
		return Spec{}, fmt.Errorf("since (%s) must be before until (%s)", w.Start.Format(time.RFC3339), w.End.Format(time.RFC3339))
	}
	if d < MinWindow {
		return Spec{}, fmt.Errorf("window must be at least %s (got %s)", MinWindow, d)
	}
	if d > MaxWindow {
		return Spec{}, fmt.Errorf("window must be at most %s (got %s)", MaxWindow, d)
	}
	if subWindows < 2 {
		subWindows = 2
	}
	mode := strings.ToLower(strings.TrimSpace(baseline))
	var anchor Range
	switch mode {
	case "", "previous":
		mode = "previous"
		anchor = w.Shift(-d)
	case "1d", "7d":
		shift := 24 * time.Hour
		if mode == "7d" {
			shift *= 7
		}
		if d > shift {
			return Spec{}, fmt.Errorf("a %s window cannot use the %s baseline: it would overlap the window it is compared with", d, mode)
		}
		anchor = w.Shift(-shift)
	default:
		return Spec{}, fmt.Errorf("baseline must be previous, 1d or 7d (got %q)", baseline)
	}
	subs := make([]Range, subWindows)
	for i := 0; i < subWindows; i++ {
		subs[i] = anchor.Shift(-time.Duration(subWindows-1-i) * d)
	}
	return Spec{Window: w, Baseline: anchor, SubWindows: subs, Mode: mode}, nil
}

func parseTime(now time.Time, s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if s == "now" {
		return now, nil
	}
	ago := strings.TrimPrefix(strings.TrimSuffix(s, " ago"), "-")
	d, err := parseDuration(ago)
	if err != nil {
		return time.Time{}, fmt.Errorf("want RFC 3339 time or a duration like 30m/2h/1d, got %q", s)
	}
	return now.Add(-d), nil
}

// parseDuration extends time.ParseDuration with a leading day unit: "2d",
// "1d12h".
func parseDuration(s string) (time.Duration, error) {
	var days time.Duration
	if m := dayPrefix.FindStringSubmatch(s); m != nil {
		n, _ := strconv.Atoi(m[1])
		days = time.Duration(n) * 24 * time.Hour
		s = m[2]
		if s == "" {
			if days <= 0 {
				return 0, fmt.Errorf("duration must be positive")
			}
			return days, nil
		}
	}
	d, err := time.ParseDuration(s)
	d += days
	if err == nil && d <= 0 {
		return 0, fmt.Errorf("duration must be positive")
	}
	return d, err
}
