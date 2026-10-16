// Package replay records backend responses during integration runs and plays
// them back in unit tests, so scenario tests run offline and deterministically.
//
// A Recorder wraps real sources and captures every call; Save writes the
// fixture (gzipped JSON). A Player answers the same calls from a fixture and
// fails loudly on any call it has not seen, which flags query drift.
package replay

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/asinha07/spoor/internal/backend"
)

// MetricsSource matches digest.MetricsSource.
type MetricsSource interface {
	Instant(ctx context.Context, q string, at time.Time) ([]backend.Series, error)
	Range(ctx context.Context, q string, start, end time.Time, step time.Duration) ([]backend.Series, error)
	LabelValues(ctx context.Context, label string, start, end time.Time) ([]string, error)
}

// LogsSource matches digest.LogsSource.
type LogsSource interface {
	Query(ctx context.Context, q string, start, end time.Time, limit int) ([]backend.Row, error)
}

// Fixture is everything one test case needs.
type Fixture struct {
	Clock   time.Time                   `json:"clock"`
	Series  map[string][]backend.Series `json:"series"`
	Labels  map[string][]string         `json:"labels"`
	Rows    map[string][]backend.Row    `json:"rows"`
	Errors  map[string]string           `json:"errors,omitempty"`
	Comment string                      `json:"comment,omitempty"`
}

func newFixture(clock time.Time) *Fixture {
	return &Fixture{Clock: clock, Series: map[string][]backend.Series{}, Labels: map[string][]string{}, Rows: map[string][]backend.Row{}, Errors: map[string]string{}}
}

func ts(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }

func instantKey(q string, at time.Time) string { return "instant|" + ts(at) + "|" + q }
func rangeKey(q string, s, e time.Time, step time.Duration) string {
	return "range|" + ts(s) + "|" + ts(e) + "|" + step.String() + "|" + q
}
func labelKey(l string, s, e time.Time) string { return "labels|" + ts(s) + "|" + ts(e) + "|" + l }
func rowsKey(src, q string, s, e time.Time, limit int) string {
	return src + "|" + ts(s) + "|" + ts(e) + "|" + strconv.Itoa(limit) + "|" + q
}

// Recorder captures calls made through its wrapped sources.
type Recorder struct {
	mu sync.Mutex
	f  *Fixture
}

// NewRecorder starts a fixture whose clock is the engine's "now".
func NewRecorder(clock time.Time) *Recorder { return &Recorder{f: newFixture(clock)} }

func (r *Recorder) put(fn func(f *Fixture)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fn(r.f)
}

// Metrics wraps a metrics source.
func (r *Recorder) Metrics(src MetricsSource) MetricsSource { return &recMetrics{r, src} }

// Logs wraps a LogsQL source; name distinguishes logs from traces.
func (r *Recorder) Logs(name string, src LogsSource) LogsSource { return &recLogs{r, name, src} }

// Save writes the fixture as gzipped JSON.
func (r *Recorder) Save(path, comment string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.f.Comment = comment
	return save(path, r.f)
}

type recMetrics struct {
	r   *Recorder
	src MetricsSource
}

func (m *recMetrics) Instant(ctx context.Context, q string, at time.Time) ([]backend.Series, error) {
	s, err := m.src.Instant(ctx, q, at)
	m.r.put(func(f *Fixture) { record(f, instantKey(q, at), s, err, f.Series) })
	return s, err
}

func (m *recMetrics) Range(ctx context.Context, q string, start, end time.Time, step time.Duration) ([]backend.Series, error) {
	s, err := m.src.Range(ctx, q, start, end, step)
	m.r.put(func(f *Fixture) { record(f, rangeKey(q, start, end, step), s, err, f.Series) })
	return s, err
}

func (m *recMetrics) LabelValues(ctx context.Context, label string, start, end time.Time) ([]string, error) {
	v, err := m.src.LabelValues(ctx, label, start, end)
	m.r.put(func(f *Fixture) { record(f, labelKey(label, start, end), v, err, f.Labels) })
	return v, err
}

type recLogs struct {
	r    *Recorder
	name string
	src  LogsSource
}

func (l *recLogs) Query(ctx context.Context, q string, start, end time.Time, limit int) ([]backend.Row, error) {
	rows, err := l.src.Query(ctx, q, start, end, limit)
	l.r.put(func(f *Fixture) { record(f, rowsKey(l.name, q, start, end, limit), rows, err, f.Rows) })
	return rows, err
}

func record[T any](f *Fixture, key string, v T, err error, into map[string]T) {
	if err != nil {
		f.Errors[key] = err.Error()
		return
	}
	into[key] = v
}

// Player answers calls from a fixture.
type Player struct{ f *Fixture }

// Load reads a fixture written by Recorder.Save.
func Load(path string) (*Player, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = fh.Close() }() // opened read-only
	zr, err := gzip.NewReader(fh)
	if err != nil {
		return nil, err
	}
	defer func() { _ = zr.Close() }() // read side; decode errors are reported above
	f := newFixture(time.Time{})
	if err := json.NewDecoder(zr).Decode(f); err != nil {
		return nil, fmt.Errorf("decode fixture %s: %w", path, err)
	}
	return &Player{f: f}, nil
}

// Clock returns the recorded engine time.
func (p *Player) Clock() time.Time { return p.f.Clock }

// Metrics returns a replaying metrics source.
func (p *Player) Metrics() MetricsSource { return playMetrics{p.f} }

// Logs returns a replaying LogsQL source for name ("logs" or "traces").
func (p *Player) Logs(name string) LogsSource { return playLogs{p.f, name} }

// ErrNotRecorded is returned for calls the fixture never saw.
type ErrNotRecorded struct{ Key string }

func (e ErrNotRecorded) Error() string { return "replay: call not recorded: " + e.Key }

func lookup[T any](f *Fixture, key string, from map[string]T) (T, error) {
	var zero T
	if msg, ok := f.Errors[key]; ok {
		return zero, fmt.Errorf("%s", msg)
	}
	v, ok := from[key]
	if !ok {
		return zero, ErrNotRecorded{key}
	}
	return v, nil
}

type playMetrics struct{ f *Fixture }

func (p playMetrics) Instant(_ context.Context, q string, at time.Time) ([]backend.Series, error) {
	return lookup(p.f, instantKey(q, at), p.f.Series)
}

func (p playMetrics) Range(_ context.Context, q string, start, end time.Time, step time.Duration) ([]backend.Series, error) {
	return lookup(p.f, rangeKey(q, start, end, step), p.f.Series)
}

func (p playMetrics) LabelValues(_ context.Context, label string, start, end time.Time) ([]string, error) {
	return lookup(p.f, labelKey(label, start, end), p.f.Labels)
}

type playLogs struct {
	f    *Fixture
	name string
}

func (p playLogs) Query(_ context.Context, q string, start, end time.Time, limit int) ([]backend.Row, error) {
	return lookup(p.f, rowsKey(p.name, q, start, end, limit), p.f.Rows)
}

func save(path string, f *Fixture) error {
	fh, err := os.Create(path)
	if err != nil {
		return err
	}
	zw, _ := gzip.NewWriterLevel(fh, gzip.BestCompression)
	enc := json.NewEncoder(zw)
	if err := enc.Encode(sortedFixture(f)); err != nil {
		_ = fh.Close() // already failing with err
		return err
	}
	if err := zw.Close(); err != nil {
		_ = fh.Close() // already failing with err
		return err
	}
	return fh.Close()
}

// sortedFixture is f with deterministic key order (encoding/json already
// sorts map keys; this keeps row slices stable too).
func sortedFixture(f *Fixture) *Fixture {
	for _, v := range f.Labels {
		sort.Strings(v)
	}
	return f
}
