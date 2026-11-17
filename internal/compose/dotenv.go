package compose

import (
	"errors"
	"fmt"
	"strings"
)

// parseDotEnv reads a .env file in the env_file format of the Compose
// specification (https://github.com/compose-spec/compose-spec/blob/main/spec.md#env_file-format):
//
//   - blank lines and lines starting with # are skipped;
//   - each other line is VAR=VAL, optionally after "export ";
//   - VAR alone, with no =, leaves VAR unset;
//   - 'single quotes' are literal, except \' for a quote;
//   - "double quotes" take the escapes \n, \r, \t, \\ and \";
//   - unquoted values end at a # that follows a space or a tab;
//   - unquoted and double-quoted values are interpolated, from lookup first
//     and then from the lines above.
//
// A line that sets one of hivemind's own variables (HIVEMIND_*) is refused,
// as a reference to one is.
//
// Errors give line numbers, never values.
func parseDotEnv(data []byte, ip interpolator) (map[string]string, error) {
	if len(data) > MaxFileBytes {
		return nil, fail(".env", "the file is larger than %d bytes", MaxFileBytes)
	}
	env := map[string]string{}
	outer := ip.lookup
	ip = ip.with(func(k string) (string, bool) {
		if v, ok := outer(k); ok {
			return v, true
		}
		v, ok := env[k]
		return v, ok
	})
	var problems []Problem
	tooMuch := false
	for i, line := range strings.Split(string(data), "\n") {
		path := fmt.Sprintf(".env line %d", i+1)
		t := strings.TrimLeft(strings.TrimSuffix(line, "\r"), " \t")
		if t == "" || t[0] == '#' {
			continue
		}
		if rest, ok := strings.CutPrefix(t, "export"); ok && rest != "" && (rest[0] == ' ' || rest[0] == '\t') {
			t = strings.TrimLeft(rest, " \t")
		}
		key, val, hasEq := strings.Cut(t, "=")
		key = strings.TrimRight(key, " \t")
		if !validVarName(key) {
			problems = append(problems, Problem{path, "want VAR=VALUE, where VAR is letters, digits and underscores, not starting with a digit"})
			continue
		}
		if isOwnVariable(key) {
			problems = append(problems, Problem{path, fmt.Sprintf("%s: .env cannot set the variables that start with %s: the compose file cannot use them, and the hivemind CLI reads its settings from its own environment, not from .env", key, ownPrefix)})
			continue
		}
		if !hasEq {
			continue
		}
		v, err := dotEnvValue(strings.TrimLeft(val, " \t"), ip)
		if err != nil {
			// The budget is spent once; every line after it would say so.
			if !errors.Is(err, errTooMuch) || !tooMuch {
				problems = append(problems, Problem{path, err.Error()})
			}
			tooMuch = tooMuch || errors.Is(err, errTooMuch)
			continue
		}
		env[key] = v
	}
	if len(problems) > 0 {
		return nil, newError(problems)
	}
	return env, nil
}

func dotEnvValue(v string, ip interpolator) (string, error) {
	if v == "" {
		return "", nil
	}
	switch v[0] {
	case '\'':
		var b strings.Builder
		for j := 1; j < len(v); j++ {
			switch {
			case v[j] == '\\' && j+1 < len(v) && v[j+1] == '\'':
				b.WriteByte('\'')
				j++
			case v[j] == '\'':
				if !onlyComment(v[j+1:]) {
					return "", errors.New("text after the closing quote")
				}
				return b.String(), nil
			default:
				b.WriteByte(v[j])
			}
		}
		return "", errors.New("the single quote is not closed")
	case '"':
		var b strings.Builder
		for j := 1; j < len(v); j++ {
			switch {
			case v[j] == '\\' && j+1 < len(v):
				j++
				switch v[j] {
				case 'n':
					b.WriteByte('\n')
				case 'r':
					b.WriteByte('\r')
				case 't':
					b.WriteByte('\t')
				case '\\', '"':
					b.WriteByte(v[j])
				default:
					b.WriteByte('\\')
					b.WriteByte(v[j])
				}
			case v[j] == '"':
				if !onlyComment(v[j+1:]) {
					return "", errors.New("text after the closing quote")
				}
				return ip.interpolate(b.String())
			default:
				b.WriteByte(v[j])
			}
		}
		return "", errors.New("the double quote is not closed")
	}
	for j := 1; j < len(v); j++ {
		if v[j] == '#' && (v[j-1] == ' ' || v[j-1] == '\t') {
			v = v[:j]
			break
		}
	}
	return ip.interpolate(strings.TrimRight(v, " \t"))
}

func onlyComment(s string) bool {
	s = strings.TrimLeft(s, " \t")
	return s == "" || s[0] == '#'
}
