package compose

import (
	"errors"
	"fmt"
	"strings"
)

// interpolate substitutes variables in s, as the Compose specification's
// interpolation section describes
// (https://github.com/compose-spec/compose-spec/blob/main/12-interpolation.md):
// $VAR, ${VAR}, ${VAR:-default}, ${VAR-default}, ${VAR:?message},
// ${VAR?message}, and $$ for a literal $. A default may itself contain
// variables. A $ followed by anything else is left as it is.
//
// One rule is stricter than Compose: a variable that is not set, with no
// default, is an error. Compose substitutes an empty string and warns.
//
// Errors name variables and never hold their values.
func interpolate(s string, lookup func(string) (string, bool)) (string, error) {
	return interpolateDepth(s, lookup, 0)
}

// maxNesting caps how deep defaults may nest, as in ${A:-${B:-${C}}}.
const maxNesting = 16

func interpolateDepth(s string, lookup func(string) (string, bool), depth int) (string, error) {
	if depth > maxNesting {
		return "", fmt.Errorf("variables nest more than %d deep", maxNesting)
	}
	if !strings.Contains(s, "$") {
		return s, nil
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] != '$' || i+1 == len(s) {
			b.WriteByte(s[i])
			i++
			continue
		}
		switch next := s[i+1]; {
		case next == '$':
			b.WriteByte('$')
			i += 2
		case next == '{':
			end, err := closingBrace(s, i+2)
			if err != nil {
				return "", err
			}
			v, err := braced(s[i+2:end], lookup, depth)
			if err != nil {
				return "", err
			}
			b.WriteString(v)
			i = end + 1
		case isNameStart(next):
			j := i + 2
			for j < len(s) && isNameChar(s[j]) {
				j++
			}
			v, err := lookupSet(s[i+1:j], lookup)
			if err != nil {
				return "", err
			}
			b.WriteString(v)
			i = j
		default:
			b.WriteByte('$')
			i++
		}
	}
	return b.String(), nil
}

// closingBrace returns the index of the } that closes the ${ before start,
// skipping nested ${...} and $$.
func closingBrace(s string, start int) (int, error) {
	depth := 1
	for j := start; j < len(s); j++ {
		switch {
		case s[j] == '$' && j+1 < len(s) && s[j+1] == '$':
			j++
		case s[j] == '$' && j+1 < len(s) && s[j+1] == '{':
			depth++
			j++
		case s[j] == '}':
			depth--
			if depth == 0 {
				return j, nil
			}
		}
	}
	return 0, errors.New("a ${ has no closing }")
}

// braced evaluates the text between ${ and }.
func braced(body string, lookup func(string) (string, bool), depth int) (string, error) {
	n := 0
	for n < len(body) && (isNameChar(body[n]) && (n > 0 || isNameStart(body[n]))) {
		n++
	}
	if n == 0 {
		return "", errors.New("a ${...} must start with a variable name: letters, digits and underscores, not starting with a digit")
	}
	name, rest := body[:n], body[n:]
	if rest == "" {
		return lookupSet(name, lookup)
	}
	colon := rest[0] == ':'
	if colon {
		rest = rest[1:]
	}
	if rest == "" || (rest[0] != '-' && rest[0] != '?') {
		return "", fmt.Errorf("${%s...}: the forms are ${%[1]s}, ${%[1]s:-default}, ${%[1]s-default}, ${%[1]s:?message} and ${%[1]s?message}", name)
	}
	op, arg := rest[0], rest[1:]
	v, set := lookup(name)
	missing := !set || (colon && v == "")
	if !missing {
		return v, nil
	}
	if op == '-' {
		return interpolateDepth(arg, lookup, depth+1)
	}
	if arg != "" {
		return "", fmt.Errorf("variable %s: %s", name, arg)
	}
	if !set {
		return "", fmt.Errorf("variable %s is not set", name)
	}
	return "", fmt.Errorf("variable %s is empty", name)
}

func lookupSet(name string, lookup func(string) (string, bool)) (string, error) {
	if v, ok := lookup(name); ok {
		return v, nil
	}
	return "", fmt.Errorf("variable %s is not set: set it in the environment or in .env, or give a default with ${%[1]s:-default}", name)
}

func isNameStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isNameChar(c byte) bool { return isNameStart(c) || (c >= '0' && c <= '9') }

func validVarName(s string) bool {
	if s == "" || !isNameStart(s[0]) {
		return false
	}
	for i := 1; i < len(s); i++ {
		if !isNameChar(s[i]) {
			return false
		}
	}
	return true
}
