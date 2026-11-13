// Package metrics writes the Prometheus text exposition format, version
// 0.0.4, and holds the few metric types hivemind needs: counters keyed by
// label values and a histogram with fixed buckets. It has no dependency on
// the Prometheus client library.
//
// The format is described at
// https://prometheus.io/docs/instrumenting/exposition_formats/.
package metrics

import (
	"bufio"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// ContentType is the Content-Type of a scrape response.
const ContentType = "text/plain; version=0.0.4; charset=utf-8"

// Metric types, as written on a TYPE line.
const (
	TypeCounter   = "counter"
	TypeGauge     = "gauge"
	TypeHistogram = "histogram"
)

// Label is one name and value. Names must match [a-zA-Z_][a-zA-Z0-9_]*;
// values may hold any UTF-8 text and are escaped when written.
type Label struct {
	Name, Value string
}

// L builds labels from name, value pairs. It panics on an odd count, which
// is a programming error.
func L(kv ...string) []Label {
	if len(kv)%2 != 0 {
		panic("metrics: odd number of label arguments")
	}
	out := make([]Label, 0, len(kv)/2)
	for i := 0; i < len(kv); i += 2 {
		out = append(out, Label{kv[i], kv[i+1]})
	}
	return out
}

// Writer writes one exposition. Call Family once per metric name, then
// Sample for each of its series. Errors are sticky: Err reports the first.
type Writer struct {
	w   *bufio.Writer
	err error
}

// NewWriter returns a Writer on w. Call Flush at the end.
func NewWriter(w io.Writer) *Writer { return &Writer{w: bufio.NewWriter(w)} }

// Family writes the HELP and TYPE lines for a metric.
func (w *Writer) Family(name, help, typ string) {
	w.str("# HELP " + name + " " + EscapeHelp(help) + "\n")
	w.str("# TYPE " + name + " " + typ + "\n")
}

// Sample writes one series line. name may carry a suffix such as _bucket.
func (w *Writer) Sample(name string, labels []Label, v float64) {
	var b strings.Builder
	b.WriteString(name)
	if len(labels) > 0 {
		b.WriteByte('{')
		for i, l := range labels {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(l.Name)
			b.WriteString(`="`)
			b.WriteString(EscapeLabelValue(l.Value))
			b.WriteByte('"')
		}
		b.WriteByte('}')
	}
	b.WriteByte(' ')
	b.WriteString(FormatValue(v))
	b.WriteByte('\n')
	w.str(b.String())
}

func (w *Writer) str(s string) {
	if w.err == nil {
		_, w.err = w.w.WriteString(s)
	}
}

// Flush writes buffered output and returns the first error.
func (w *Writer) Flush() error {
	if w.err == nil {
		w.err = w.w.Flush()
	}
	return w.err
}

// EscapeHelp escapes a HELP docstring: backslash and line feed.
func EscapeHelp(s string) string {
	return strings.NewReplacer(`\`, `\\`, "\n", `\n`).Replace(s)
}

// EscapeLabelValue escapes a label value: backslash, double quote and line
// feed.
func EscapeLabelValue(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}

// FormatValue writes a sample value. Whole numbers are written without an
// exponent, so counters read as integers.
func FormatValue(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case math.IsNaN(v):
		return "NaN"
	case v == math.Trunc(v) && math.Abs(v) < 1e15:
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// CounterVec counts events by a fixed list of label names. It is safe for
// concurrent use.
type CounterVec struct {
	names []string
	mu    sync.Mutex
	m     map[string]*series
}

type series struct {
	values []string
	n      float64
}

// NewCounterVec returns a counter with the given label names.
func NewCounterVec(names ...string) *CounterVec {
	return &CounterVec{names: names, m: map[string]*series{}}
}

// Inc adds one to the series with these label values, in the order of the
// names. A wrong number of values panics.
func (c *CounterVec) Inc(values ...string) { c.Add(1, values...) }

// Add adds v to the series with these label values.
func (c *CounterVec) Add(v float64, values ...string) {
	if len(values) != len(c.names) {
		panic("metrics: wrong number of label values")
	}
	k := strings.Join(values, "\xff")
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.m[k]
	if s == nil {
		s = &series{values: slices.Clone(values)}
		c.m[k] = s
	}
	s.n += v
}

// Get returns the value of one series, 0 if it has none.
func (c *CounterVec) Get(values ...string) float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s := c.m[strings.Join(values, "\xff")]; s != nil {
		return s.n
	}
	return 0
}

// Write writes the counter's series, sorted by label values.
func (c *CounterVec) Write(w *Writer, name string) {
	c.mu.Lock()
	ss := make([]series, 0, len(c.m))
	for _, s := range c.m {
		ss = append(ss, *s)
	}
	c.mu.Unlock()
	slices.SortFunc(ss, func(a, b series) int { return slices.Compare(a.values, b.values) })
	for _, s := range ss {
		ls := make([]Label, len(c.names))
		for i, n := range c.names {
			ls[i] = Label{n, s.values[i]}
		}
		w.Sample(name, ls, s.n)
	}
}

// Histogram is one unlabelled histogram with fixed buckets. It is safe for
// concurrent use.
type Histogram struct {
	bounds []float64
	mu     sync.Mutex
	counts []uint64 // per bucket, not cumulative; the last is +Inf
	sum    float64
	n      uint64
}

// NewHistogram returns a histogram with the given upper bounds, which must
// be sorted and finite. A +Inf bucket is added.
func NewHistogram(bounds ...float64) *Histogram {
	if !slices.IsSorted(bounds) {
		panic("metrics: histogram bounds are not sorted")
	}
	return &Histogram{bounds: slices.Clone(bounds), counts: make([]uint64, len(bounds)+1)}
}

// Observe records one value.
func (h *Histogram) Observe(v float64) {
	i, _ := slices.BinarySearch(h.bounds, v) // first bound >= v; le is inclusive
	h.mu.Lock()
	defer h.mu.Unlock()
	h.counts[i]++
	h.sum += v
	h.n++
}

// Count returns how many values were observed.
func (h *Histogram) Count() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.n
}

// Write writes the _bucket, _sum and _count series.
func (h *Histogram) Write(w *Writer, name string) {
	h.mu.Lock()
	counts := slices.Clone(h.counts)
	sum, n := h.sum, h.n
	h.mu.Unlock()
	var cum uint64
	for i, b := range h.bounds {
		cum += counts[i]
		w.Sample(name+"_bucket", L("le", FormatValue(b)), float64(cum))
	}
	w.Sample(name+"_bucket", L("le", "+Inf"), float64(n))
	w.Sample(name+"_sum", nil, sum)
	w.Sample(name+"_count", nil, float64(n))
}
