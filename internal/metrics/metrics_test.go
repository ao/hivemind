package metrics

import (
	"bytes"
	"math"
	"strings"
	"sync"
	"testing"
)

func TestEscaping(t *testing.T) {
	if got := EscapeLabelValue("a\\b\"c\nd"); got != `a\\b\"c\nd` {
		t.Errorf("label value: %q", got)
	}
	if got := EscapeHelp("a\\b\"c\nd"); got != `a\\b"c\nd` {
		t.Errorf("help: %q", got)
	}
}

func TestFormatValue(t *testing.T) {
	cases := map[float64]string{0: "0", 3: "3", -2: "-2", 0.25: "0.25", 1e20: "1e+20",
		math.Inf(1): "+Inf", math.Inf(-1): "-Inf", math.NaN(): "NaN"}
	for v, want := range cases {
		if got := FormatValue(v); got != want {
			t.Errorf("%v -> %q, want %q", v, got, want)
		}
	}
}

// TestWriterRoundTrip writes every kind of family, with label values that
// need escaping, and reads it back with Parse.
func TestWriterRoundTrip(t *testing.T) {
	var b bytes.Buffer
	w := NewWriter(&b)
	w.Family("t_gauge", "A gauge.\nWith a second line and a \\ backslash.", TypeGauge)
	w.Sample("t_gauge", nil, 7)
	odd := "quote \" backslash \\ newline \n end"
	w.Family("t_labelled", "Labels.", TypeGauge)
	w.Sample("t_labelled", L("app", odd, "replica", "0"), 1.5)
	w.Sample("t_labelled", L("app", "plain", "replica", "1"), 2)

	c := NewCounterVec("method", "code")
	c.Inc("GET", "200")
	c.Inc("GET", "200")
	c.Add(3, "POST", "201")
	w.Family("t_requests_total", "Requests.", TypeCounter)
	c.Write(w, "t_requests_total")

	h := NewHistogram(0.1, 1, 10)
	for _, v := range []float64{0.05, 0.1, 0.5, 5, 50} {
		h.Observe(v)
	}
	w.Family("t_seconds", "Durations.", TypeHistogram)
	h.Write(w, "t_seconds")
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}

	text := b.String()
	got, err := Parse(strings.NewReader(text))
	if err != nil {
		t.Fatalf("%v\n%s", err, text)
	}
	want := map[string]float64{
		Key("t_gauge"): 7,
		Key("t_labelled", "app", odd, "replica", "0"):            1.5,
		Key("t_labelled", "app", "plain", "replica", "1"):        2,
		Key("t_requests_total", "method", "GET", "code", "200"):  2,
		Key("t_requests_total", "method", "POST", "code", "201"): 3,
		Key("t_seconds_bucket", "le", "0.1"):                     2, // le is inclusive
		Key("t_seconds_bucket", "le", "1"):                       3,
		Key("t_seconds_bucket", "le", "10"):                      4,
		Key("t_seconds_bucket", "le", "+Inf"):                    5,
		Key("t_seconds_sum"):                                     55.65,
		Key("t_seconds_count"):                                   5,
	}
	if len(got) != len(want) {
		t.Errorf("parsed %d series, want %d: %v", len(got), len(want), got)
	}
	for k, v := range want {
		if g, ok := got[k]; !ok || math.Abs(g-v) > 1e-9 {
			t.Errorf("%s = %v (present %v), want %v", k, g, ok, v)
		}
	}
	// The raw text keeps the escapes on one line.
	if !strings.Contains(text, `app="quote \" backslash \\ newline \n end"`) {
		t.Errorf("escaped label not found in:\n%s", text)
	}
	if !strings.Contains(text, `# HELP t_gauge A gauge.\nWith a second line and a \\ backslash.`+"\n") {
		t.Errorf("escaped HELP not found in:\n%s", text)
	}
}

func TestParseRejects(t *testing.T) {
	bad := map[string]string{
		"no TYPE":           "# HELP x X.\nx 1\n",
		"no HELP":           "# TYPE x gauge\nx 1\n",
		"no final newline":  "# HELP x X.\n# TYPE x gauge\nx 1",
		"bad name":          "# HELP x X.\n# TYPE x gauge\n1x 1\n",
		"bad escape":        "# HELP x X.\n# TYPE x gauge\nx{a=\"\\t\"} 1\n",
		"unquoted":          "# HELP x X.\n# TYPE x gauge\nx{a=b} 1\n",
		"unclosed":          "# HELP x X.\n# TYPE x gauge\nx{a=\"b} 1\n",
		"duplicate series":  "# HELP x X.\n# TYPE x gauge\nx{a=\"b\"} 1\nx{a=\"b\"} 2\n",
		"duplicate label":   "# HELP x X.\n# TYPE x gauge\nx{a=\"b\",a=\"c\"} 1\n",
		"bad value":         "# HELP x X.\n# TYPE x gauge\nx one\n",
		"inf spelled wrong": "# HELP x X.\n# TYPE x gauge\nx inf\n",
		"two TYPEs":         "# HELP x X.\n# TYPE x gauge\n# TYPE x gauge\nx 1\n",
		"unknown type":      "# HELP x X.\n# TYPE x meter\nx 1\n",
		"bucket without le": "# HELP x X.\n# TYPE x histogram\nx_bucket 1\n",
		"histogram bare":    "# HELP x X.\n# TYPE x histogram\nx 1\n",
		"negative counter":  "# HELP x X.\n# TYPE x counter\nx -1\n",
		"suffix on gauge":   "# HELP x X.\n# TYPE x gauge\nx_count 1\n",
		"empty line":        "# HELP x X.\n\n# TYPE x gauge\nx 1\n",
		"bad help escape":   "# HELP x X \\t.\n# TYPE x gauge\nx 1\n",
		"two spaces":        "# HELP x X.\n# TYPE x gauge\nx  1\n",
	}
	for name, text := range bad {
		if _, err := Parse(strings.NewReader(text)); err == nil {
			t.Errorf("%s: accepted %q", name, text)
		}
	}
	if _, err := Parse(strings.NewReader("# HELP x X.\n# TYPE x gauge\nx{a=\"b\"} 1 1700000000000\n")); err != nil {
		t.Errorf("timestamped sample: %v", err)
	}
}

func TestCounterVecConcurrent(t *testing.T) {
	c := NewCounterVec("a")
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 1000 {
				c.Inc("x")
			}
		}()
	}
	wg.Wait()
	if got := c.Get("x"); got != 8000 {
		t.Fatalf("count %v, want 8000", got)
	}
}
