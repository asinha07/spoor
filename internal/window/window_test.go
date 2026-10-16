package window

import (
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

func TestParseDefaults(t *testing.T) {
	s, err := Parse(now, "", "", "", 6)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := s.Window.Duration(), 30*time.Minute; got != want {
		t.Errorf("window = %s, want %s", got, want)
	}
	if !s.Window.End.Equal(now) {
		t.Errorf("end = %s, want now", s.Window.End)
	}
	if s.Mode != "previous" {
		t.Errorf("mode = %q", s.Mode)
	}
	if !s.Baseline.End.Equal(s.Window.Start) {
		t.Errorf("previous baseline should end where the window starts: %v vs %v", s.Baseline.End, s.Window.Start)
	}
	if len(s.SubWindows) != 6 {
		t.Fatalf("sub-windows = %d, want 6", len(s.SubWindows))
	}
	for i := 1; i < len(s.SubWindows); i++ {
		if !s.SubWindows[i].Start.Equal(s.SubWindows[i-1].End) {
			t.Errorf("sub-window %d not contiguous", i)
		}
		if s.SubWindows[i].Duration() != s.Window.Duration() {
			t.Errorf("sub-window %d has length %s", i, s.SubWindows[i].Duration())
		}
	}
	if last := s.SubWindows[len(s.SubWindows)-1]; last != s.Baseline {
		t.Errorf("last sub-window %v != baseline %v", last, s.Baseline)
	}
}

func TestParseForms(t *testing.T) {
	cases := []struct {
		since, until, baseline string
		wantStart, wantEnd     time.Time
		wantBaselineEnd        time.Time
	}{
		{"2h", "", "", now.Add(-2 * time.Hour), now, now.Add(-2 * time.Hour)},
		{"1d", "", "7d", now.Add(-24 * time.Hour), now, now.Add(-7 * 24 * time.Hour)},
		{"2026-10-16T10:00:00Z", "2026-10-16T10:30:00Z", "1d",
			time.Date(2026, 10, 16, 10, 0, 0, 0, time.UTC), time.Date(2026, 10, 16, 10, 30, 0, 0, time.UTC),
			time.Date(2026, 10, 15, 10, 30, 0, 0, time.UTC)},
		{"45m ago", "15m", "previous", now.Add(-45 * time.Minute), now.Add(-15 * time.Minute), now.Add(-45 * time.Minute)},
	}
	for _, c := range cases {
		s, err := Parse(now, c.since, c.until, c.baseline, 3)
		if err != nil {
			t.Errorf("Parse(%q,%q,%q): %v", c.since, c.until, c.baseline, err)
			continue
		}
		if !s.Window.Start.Equal(c.wantStart) || !s.Window.End.Equal(c.wantEnd) {
			t.Errorf("Parse(%q,%q): window %v, want %v–%v", c.since, c.until, s.Window, c.wantStart, c.wantEnd)
		}
		if !s.Baseline.End.Equal(c.wantBaselineEnd) {
			t.Errorf("Parse(%q,%q,%q): baseline end %v, want %v", c.since, c.until, c.baseline, s.Baseline.End, c.wantBaselineEnd)
		}
	}
}

func TestParseErrors(t *testing.T) {
	cases := []struct{ since, until, baseline, want string }{
		{"yesterday", "", "", "since"},
		{"", "soon", "", "until"},
		{"30s", "", "", "at least"},
		{"30d", "", "", "at most"},
		{"10m", "20m", "", "must be before"}, // start after end
		{"2d", "", "1d", "overlap"},
		{"1d12h", "", "", ""},
		{"", "", "1w", "baseline"},
		{"-5m", "", "", ""}, // a leading minus is accepted as "ago"
	}
	for _, c := range cases {
		_, err := Parse(now, c.since, c.until, c.baseline, 6)
		if c.want == "" {
			if err != nil {
				t.Errorf("Parse(%q): unexpected error %v", c.since, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("Parse(%q,%q,%q) error = %v, want mention of %q", c.since, c.until, c.baseline, err, c.want)
		}
	}
}

func TestSubWindowsMinimum(t *testing.T) {
	s, err := Parse(now, "", "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.SubWindows) != 2 {
		t.Errorf("sub-windows = %d, want the minimum of 2", len(s.SubWindows))
	}
}
