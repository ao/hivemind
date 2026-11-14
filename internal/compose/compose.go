// Package compose converts a Docker Compose file into hivemind app specs.
//
// It reads a strict subset of the Compose specification
// (https://github.com/compose-spec/compose-spec/blob/main/spec.md): every key
// it reads maps onto spec.App exactly, and every other key is refused with its
// path and a reason. docs/compose.md lists both.
//
// The package does no I/O. The caller passes the file, the .env file beside
// it, and a lookup into its environment.
package compose

import (
	"bytes"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/ao/hivemind/internal/spec"
)

// Limits on what one file may contain.
const (
	// MaxFileBytes caps a compose file and its .env file. It is the same as
	// the limit on `hivemind deploy -f` and on an API request body.
	MaxFileBytes = 1 << 20
	// MaxServices is the most services one file may define.
	MaxServices = 100
	// maxAliasValues caps the values that YAML alias expansion may add to
	// the document, so that a small file cannot expand into a huge one.
	maxAliasValues = 100_000
)

// Options are the inputs to Convert other than the file itself.
type Options struct {
	// Prefix, when set, names each app Prefix-<service>. Without it the app
	// name is the service name.
	Prefix string
	// Lookup returns a variable from the caller's environment. A nil Lookup
	// finds nothing.
	Lookup func(name string) (string, bool)
	// DotEnv is the content of the .env file beside the compose file, or nil
	// when there is none. Lookup takes precedence over it.
	DotEnv []byte
}

// App is one converted service.
type App struct {
	// Service is the service's name in the file.
	Service string
	// Spec is the app spec, normalised and valid.
	Spec spec.App
}

// Note is something the caller should tell the user, such as where a port
// with no address is published.
type Note struct {
	Path string
	Text string
}

func (n Note) String() string { return n.Path + ": " + n.Text }

// Result is a converted file.
type Result struct {
	// Apps are sorted by service name.
	Apps []App
	// Notes are sorted by path.
	Notes []Note
}

// Problem is one thing wrong with the file, at a path such as
// services.web.ports[0].
type Problem struct {
	Path string
	Msg  string
}

func (p Problem) String() string {
	if p.Path == "" {
		return p.Msg
	}
	return p.Path + ": " + p.Msg
}

// Error is every problem found in a file, sorted by path. Its text never
// contains a value from the file's environment section or from a variable,
// because those may be secrets.
type Error struct {
	Problems []Problem
}

func (e *Error) Error() string {
	if len(e.Problems) == 1 {
		return e.Problems[0].String()
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d problems:", len(e.Problems))
	for _, p := range e.Problems {
		b.WriteString("\n  " + p.String())
	}
	return b.String()
}

// Convert parses a compose file and returns one app per service. Either
// every service converts and validates, or the error lists every problem.
func Convert(data []byte, opts Options) (*Result, error) {
	root, err := parse(data)
	if err != nil {
		return nil, err
	}
	env := map[string]string{}
	if opts.DotEnv != nil {
		if env, err = parseDotEnv(opts.DotEnv, opts.Lookup); err != nil {
			return nil, err
		}
	}
	c := &converter{prefix: opts.Prefix, lookup: func(k string) (string, bool) {
		if opts.Lookup != nil {
			if v, ok := opts.Lookup(k); ok {
				return v, true
			}
		}
		v, ok := env[k]
		return v, ok
	}, volumeUser: map[string]string{}}
	apps := c.file(root)
	if len(c.problems) > 0 {
		return nil, newError(c.problems)
	}
	sortByPath(c.notes, func(n Note) string { return n.Path })
	return &Result{Apps: apps, Notes: c.notes}, nil
}

// AppNames returns the app name for each service in the file, sorted by
// service name. It reads only the service names, so it needs none of the
// file's variables, and it ignores the keys Convert would refuse. delete
// uses it.
func AppNames(data []byte, prefix string) ([]string, error) {
	root, err := parse(data)
	if err != nil {
		return nil, err
	}
	c := &converter{prefix: prefix}
	services := c.services(root)
	if len(c.problems) > 0 {
		return nil, newError(c.problems)
	}
	var names []string
	for _, s := range services {
		if name, ok := c.appName(s.name); ok {
			names = append(names, name)
		}
	}
	if len(c.problems) > 0 {
		return nil, newError(c.problems)
	}
	return names, nil
}

// Marshal writes apps as YAML, one document per app, in the form that
// `hivemind deploy -f` reads.
func Marshal(apps []App) ([]byte, error) {
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	for _, a := range apps {
		if err := enc.Encode(a.Spec); err != nil {
			return nil, err
		}
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func newError(ps []Problem) *Error {
	sortByPath(ps, func(p Problem) string { return p.Path })
	return &Error{Problems: ps}
}

func fail(path, format string, args ...any) error {
	return &Error{Problems: []Problem{{Path: path, Msg: fmt.Sprintf(format, args...)}}}
}

// sortByPath sorts by path, comparing runs of digits as numbers so that
// ports[2] comes before ports[10]. Entries with equal paths keep their order.
func sortByPath[T any](s []T, key func(T) string) {
	sort.SliceStable(s, func(i, j int) bool { return naturalLess(key(s[i]), key(s[j])) })
}

func naturalLess(a, b string) bool {
	for a != "" && b != "" {
		da, db := digits(a), digits(b)
		if da > 0 && db > 0 {
			na, nb := strings.TrimLeft(a[:da], "0"), strings.TrimLeft(b[:db], "0")
			if len(na) != len(nb) {
				return len(na) < len(nb)
			}
			if na != nb {
				return na < nb
			}
			a, b = a[da:], b[db:]
			continue
		}
		if a[0] != b[0] {
			return a[0] < b[0]
		}
		a, b = a[1:], b[1:]
	}
	return len(a) < len(b)
}

func digits(s string) int {
	n := 0
	for n < len(s) && s[n] >= '0' && s[n] <= '9' {
		n++
	}
	return n
}
