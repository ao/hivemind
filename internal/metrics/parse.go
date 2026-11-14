package metrics

import (
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

var (
	metricNameRE = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)
	labelNameRE  = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
)

// Key returns the key Parse uses for one series: the name, then the labels
// sorted by name, with values escaped as in the format. Key("up") is "up";
// Key("x", "b", "2", "a", "1") is `x{a="1",b="2"}`.
func Key(name string, kv ...string) string {
	ls := L(kv...)
	slices.SortFunc(ls, func(a, b Label) int { return strings.Compare(a.Name, b.Name) })
	if len(ls) == 0 {
		return name
	}
	parts := make([]string, len(ls))
	for i, l := range ls {
		parts[i] = l.Name + `="` + EscapeLabelValue(l.Value) + `"`
	}
	return name + "{" + strings.Join(parts, ",") + "}"
}

// Parse reads an exposition in the text format, version 0.0.4, and returns
// every sample by Key. It is strict where the format allows choice, so it
// also checks what Writer produces:
//   - every sample belongs to a family declared by a TYPE line before it,
//     and each family has exactly one HELP and one TYPE line;
//   - a counter or gauge sample has the family's name, and a histogram's
//     samples are name_bucket (with an le label), name_sum and name_count;
//   - names, label names, escapes and values are well-formed;
//   - no series appears twice, and no label name twice in one series;
//   - the input ends with a line feed.
func Parse(r io.Reader) (map[string]float64, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	text := string(data)
	if text != "" && !strings.HasSuffix(text, "\n") {
		return nil, errors.New("exposition does not end with a line feed")
	}
	type family struct {
		typ          string
		help, sample bool
	}
	families := map[string]*family{}
	out := map[string]float64{}
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	if text == "" {
		lines = nil
	}
	for i, line := range lines {
		n := i + 1
		fail := func(format string, args ...any) error {
			return fmt.Errorf("line %d %q: %s", n, line, fmt.Sprintf(format, args...))
		}
		if line == "" {
			return nil, fail("empty line")
		}
		if strings.HasPrefix(line, "#") {
			fields := strings.SplitN(line, " ", 4)
			if len(fields) < 2 || fields[0] != "#" {
				return nil, fail("comment must start with \"# \"")
			}
			switch fields[1] {
			case "HELP":
				if len(fields) < 3 || !metricNameRE.MatchString(fields[2]) {
					return nil, fail("HELP needs a metric name")
				}
				doc := ""
				if len(fields) == 4 {
					doc = fields[3]
				}
				if err := checkHelpEscapes(doc); err != nil {
					return nil, fail("%v", err)
				}
				f := families[fields[2]]
				if f == nil {
					f = &family{}
					families[fields[2]] = f
				}
				if f.help {
					return nil, fail("second HELP for %s", fields[2])
				}
				if f.sample {
					return nil, fail("HELP after samples of %s", fields[2])
				}
				f.help = true
			case "TYPE":
				if len(fields) != 4 || !metricNameRE.MatchString(fields[2]) {
					return nil, fail("TYPE needs a metric name and a type")
				}
				switch fields[3] {
				case "counter", "gauge", "histogram", "summary", "untyped":
				default:
					return nil, fail("unknown type %q", fields[3])
				}
				f := families[fields[2]]
				if f == nil {
					f = &family{}
					families[fields[2]] = f
				}
				if f.typ != "" {
					return nil, fail("second TYPE for %s", fields[2])
				}
				if f.sample {
					return nil, fail("TYPE after samples of %s", fields[2])
				}
				f.typ = fields[3]
			}
			continue
		}

		name, labels, value, err := parseSample(line)
		if err != nil {
			return nil, fail("%v", err)
		}
		fam, suffix := name, ""
		f := families[name]
		if f == nil {
			for _, s := range []string{"_bucket", "_sum", "_count"} {
				if base, ok := strings.CutSuffix(name, s); ok && families[base] != nil {
					fam, suffix, f = base, s, families[base]
					break
				}
			}
		}
		if f == nil || f.typ == "" {
			return nil, fail("sample of %s has no TYPE line before it", name)
		}
		if !f.help {
			return nil, fail("family %s has no HELP line", fam)
		}
		switch f.typ {
		case "histogram":
			if suffix == "" {
				return nil, fail("histogram %s sample needs _bucket, _sum or _count", fam)
			}
			_, hasLE := labelValue(labels, "le")
			if (suffix == "_bucket") != hasLE {
				return nil, fail("le label belongs on _bucket samples only")
			}
		case "summary":
			if suffix == "_bucket" {
				return nil, fail("summary %s has no _bucket samples", fam)
			}
		default:
			if suffix != "" {
				return nil, fail("%s %s sample has suffix %s", f.typ, fam, suffix)
			}
		}
		if f.typ == "counter" && value < 0 {
			return nil, fail("negative counter")
		}
		f.sample = true
		kv := make([]string, 0, 2*len(labels))
		for _, l := range labels {
			kv = append(kv, l.Name, l.Value)
		}
		k := Key(name, kv...)
		if _, dup := out[k]; dup {
			return nil, fail("series %s appears twice", k)
		}
		out[k] = value
	}
	for name, f := range families {
		if f.typ == "" {
			return nil, fmt.Errorf("family %s has HELP but no TYPE", name)
		}
	}
	return out, nil
}

func labelValue(ls []Label, name string) (string, bool) {
	for _, l := range ls {
		if l.Name == name {
			return l.Value, true
		}
	}
	return "", false
}

func checkHelpEscapes(s string) error {
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' {
			continue
		}
		if i+1 >= len(s) || (s[i+1] != '\\' && s[i+1] != 'n') {
			return errors.New(`HELP text has a bad escape: only \\ and \n are allowed`)
		}
		i++
	}
	return nil
}

// parseSample parses name{labels} value [timestamp].
func parseSample(line string) (string, []Label, float64, error) {
	i := strings.IndexAny(line, "{ ")
	if i < 0 {
		return "", nil, 0, errors.New("no value")
	}
	name := line[:i]
	if !metricNameRE.MatchString(name) {
		return "", nil, 0, fmt.Errorf("bad metric name %q", name)
	}
	rest := line[i:]
	var labels []Label
	if rest[0] == '{' {
		var err error
		labels, rest, err = parseLabels(rest[1:])
		if err != nil {
			return "", nil, 0, err
		}
	}
	if !strings.HasPrefix(rest, " ") {
		return "", nil, 0, errors.New("no space before the value")
	}
	fields := strings.Split(rest[1:], " ")
	if len(fields) < 1 || len(fields) > 2 || fields[0] == "" {
		return "", nil, 0, errors.New("want a value and an optional timestamp, separated by one space")
	}
	v, err := parseValue(fields[0])
	if err != nil {
		return "", nil, 0, err
	}
	if len(fields) == 2 {
		if _, err := strconv.ParseInt(fields[1], 10, 64); err != nil {
			return "", nil, 0, fmt.Errorf("bad timestamp %q", fields[1])
		}
	}
	return name, labels, v, nil
}

func parseValue(s string) (float64, error) {
	switch s {
	case "+Inf":
		return math.Inf(1), nil
	case "-Inf":
		return math.Inf(-1), nil
	case "NaN":
		return math.NaN(), nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || strings.ContainsAny(s, "IiNn") {
		return 0, fmt.Errorf("bad value %q", s)
	}
	return v, nil
}

// parseLabels parses `a="x",b="y"}` and returns what follows the brace.
func parseLabels(s string) ([]Label, string, error) {
	var out []Label
	seen := map[string]bool{}
	for {
		if strings.HasPrefix(s, "}") {
			return out, s[1:], nil
		}
		eq := strings.IndexByte(s, '=')
		if eq < 0 {
			return nil, "", errors.New("label without =")
		}
		name := s[:eq]
		if !labelNameRE.MatchString(name) {
			return nil, "", fmt.Errorf("bad label name %q", name)
		}
		if seen[name] {
			return nil, "", fmt.Errorf("label %s appears twice", name)
		}
		seen[name] = true
		s = s[eq+1:]
		if !strings.HasPrefix(s, `"`) {
			return nil, "", fmt.Errorf("label %s value is not quoted", name)
		}
		s = s[1:]
		var val strings.Builder
		closed := false
		for len(s) > 0 && !closed {
			c := s[0]
			switch c {
			case '\\':
				if len(s) < 2 {
					return nil, "", errors.New("label value ends in a backslash")
				}
				switch s[1] {
				case '\\':
					val.WriteByte('\\')
				case '"':
					val.WriteByte('"')
				case 'n':
					val.WriteByte('\n')
				default:
					return nil, "", fmt.Errorf("bad escape \\%c in label %s", s[1], name)
				}
				s = s[2:]
			case '"':
				closed = true
				s = s[1:]
			case '\n':
				return nil, "", errors.New("raw line feed in a label value")
			default:
				val.WriteByte(c)
				s = s[1:]
			}
		}
		if !closed {
			return nil, "", fmt.Errorf("label %s value is not closed", name)
		}
		out = append(out, Label{name, val.String()})
		switch {
		case strings.HasPrefix(s, ","):
			s = s[1:]
		case strings.HasPrefix(s, "}"):
		default:
			return nil, "", errors.New("labels must be separated by commas")
		}
	}
}
