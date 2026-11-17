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
// A variable whose name starts with HIVEMIND_, in any case, is refused
// (ownVariable), default or not.
//
// Errors name variables and never hold their values.
//
// This form has a budget of its own; Convert shares one budget between a
// file and its .env (interpolator).
func interpolate(s string, lookup func(string) (string, bool)) (string, error) {
	return newInterpolator(lookup).interpolate(s)
}

// maxNesting caps how deep defaults may nest, as in ${A:-${B:-${C}}}.
const maxNesting = 16

// MaxInterpolatedBytes caps the text that interpolation makes for one file
// and its .env together. A value is interpolated each time it is read, so a
// value with a $ in it is made again at each alias of it, and a .env line
// can hold the one above it twice: without a cap a 1 MiB file could make
// many gigabytes. A value without a $ is used as it is, and costs nothing.
const MaxInterpolatedBytes = 16 << 20

// errTooMuch is the error once the budget is spent.
var errTooMuch = fmt.Errorf("variables make more than %d MiB of text in this file and its .env; "+
	"a value with a $ in it is made again at each alias of it", MaxInterpolatedBytes>>20)

// interpolator substitutes variables from lookup, and counts the bytes it
// makes against left, which several interpolators may share.
type interpolator struct {
	lookup func(string) (string, bool)
	left   *int
}

func newInterpolator(lookup func(string) (string, bool)) interpolator {
	if lookup == nil {
		lookup = func(string) (string, bool) { return "", false }
	}
	left := MaxInterpolatedBytes
	return interpolator{lookup, &left}
}

// with returns an interpolator with another lookup and the same budget.
func (ip interpolator) with(lookup func(string) (string, bool)) interpolator {
	return interpolator{lookup, ip.left}
}

func (ip interpolator) interpolate(s string) (string, error) { return ip.depth(s, 0) }

func (ip interpolator) depth(s string, depth int) (string, error) {
	if depth > maxNesting {
		return "", fmt.Errorf("variables nest more than %d deep", maxNesting)
	}
	if !strings.Contains(s, "$") {
		return s, nil
	}
	var b strings.Builder
	// add writes v, unless that would spend more than is left.
	add := func(v string) error {
		if b.Len()+len(v) > *ip.left {
			return errTooMuch
		}
		b.WriteString(v)
		return nil
	}
	for i := 0; i < len(s); {
		var v string
		switch {
		case s[i] != '$' || i+1 == len(s):
			j := i + 1
			for j < len(s) && s[j] != '$' {
				j++
			}
			v, i = s[i:j], j
		case s[i+1] == '$':
			v, i = "$", i+2
		case s[i+1] == '{':
			end, err := closingBrace(s, i+2)
			if err != nil {
				return "", err
			}
			if v, err = ip.braced(s[i+2:end], depth); err != nil {
				return "", err
			}
			i = end + 1
		case isNameStart(s[i+1]):
			j := i + 2
			for j < len(s) && isNameChar(s[j]) {
				j++
			}
			if err := ownVariable(s[i+1 : j]); err != nil {
				return "", err
			}
			var err error
			if v, err = lookupSet(s[i+1:j], ip.lookup); err != nil {
				return "", err
			}
			i = j
		default:
			v, i = "$", i+1
		}
		if err := add(v); err != nil {
			return "", err
		}
	}
	*ip.left -= b.Len()
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
func (ip interpolator) braced(body string, depth int) (string, error) {
	n := 0
	for n < len(body) && (isNameChar(body[n]) && (n > 0 || isNameStart(body[n]))) {
		n++
	}
	if n == 0 {
		return "", errors.New("a ${...} must start with a variable name: letters, digits and underscores, not starting with a digit")
	}
	name, rest := body[:n], body[n:]
	if err := ownVariable(name); err != nil {
		return "", err
	}
	if rest == "" {
		return lookupSet(name, ip.lookup)
	}
	colon := rest[0] == ':'
	if colon {
		rest = rest[1:]
	}
	if rest == "" || (rest[0] != '-' && rest[0] != '?') {
		return "", fmt.Errorf("${%s...}: the forms are ${%[1]s}, ${%[1]s:-default}, ${%[1]s-default}, ${%[1]s:?message} and ${%[1]s?message}", name)
	}
	op, arg := rest[0], rest[1:]
	v, set := ip.lookup(name)
	missing := !set || (colon && v == "")
	if !missing {
		return v, nil
	}
	if op == '-' {
		return ip.depth(arg, depth+1)
	}
	if arg != "" {
		return "", fmt.Errorf("variable %s: %s", name, arg)
	}
	if !set {
		return "", fmt.Errorf("variable %s is not set", name)
	}
	return "", fmt.Errorf("variable %s is empty", name)
}

// ownPrefix starts the names of the variables the hivemind CLI reads, such
// as HIVEMIND_TOKEN, the admin token, and HIVEMIND_SERVER.
const ownPrefix = "HIVEMIND_"

// isOwnVariable reports whether name is one of hivemind's own variables:
// it starts with ownPrefix, in any case, since Windows reads variable names
// without regard to case.
func isOwnVariable(name string) bool {
	return len(name) >= len(ownPrefix) && strings.EqualFold(name[:len(ownPrefix)], ownPrefix)
}

// ownVariable refuses a reference to one of hivemind's own variables. A
// compose file, which may come from anyone, must not be able to copy the
// admin token into a container's environment, where its author's image
// can read it, or into what convert prints.
func ownVariable(name string) error {
	if isOwnVariable(name) {
		return fmt.Errorf("variable %s: a compose file cannot use the variables that start with %s, which hold the hivemind CLI's own settings, the admin token among them", name, ownPrefix)
	}
	return nil
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
