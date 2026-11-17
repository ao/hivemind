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
	"path/filepath"
	"regexp"
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
	// finds nothing. It is never asked for a variable that starts with
	// HIVEMIND_: a file that refers to one is refused.
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
	// Name is the file's top-level name, the Compose project name, or ""
	// when the file has none.
	Name string
}

// projectRE is a Compose project name: lowercase letters, digits, dashes
// and underscores, starting with a letter or a digit.
var projectRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// maxProjectLen bounds a project name, so that the origin it makes fits the
// store's limit.
const maxProjectLen = 64

// ProjectName returns the project name of a file without a top-level name:
// the name of its directory, lowercased, without the characters a project
// name cannot have, as Docker Compose makes it. It is "" when nothing is left.
func ProjectName(dir string) string {
	base := strings.ToLower(filepath.Base(filepath.Clean(dir)))
	var b strings.Builder
	for _, r := range base {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	s := strings.TrimLeft(b.String(), "-_")
	if len(s) > maxProjectLen {
		s = s[:maxProjectLen]
	}
	return s
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
	ip, err := variables(opts)
	if err != nil {
		return nil, err
	}
	c := &converter{prefix: opts.Prefix, ip: ip, volumeUser: map[string]string{}}
	apps := c.file(root)
	name := c.projectName(root)
	if len(c.problems) > 0 {
		return nil, newError(c.problems)
	}
	sortByPath(c.notes, func(n Note) string { return n.Path })
	return &Result{Apps: apps, Notes: c.notes, Name: name}, nil
}

// variables returns the interpolator for a file: the caller's environment
// first, without hivemind's own variables, which interpolate refuses before
// it asks, then the .env. The .env and the file share one budget
// (MaxInterpolatedBytes).
func variables(opts Options) (interpolator, error) {
	look := func(string) (string, bool) { return "", false }
	if opts.Lookup != nil {
		look = func(k string) (string, bool) {
			if isOwnVariable(k) {
				return "", false
			}
			return opts.Lookup(k)
		}
	}
	ip := newInterpolator(look)
	env := map[string]string{}
	if opts.DotEnv != nil {
		var err error
		if env, err = parseDotEnv(opts.DotEnv, ip); err != nil {
			return interpolator{}, err
		}
	}
	return ip.with(func(k string) (string, bool) {
		if v, ok := look(k); ok {
			return v, true
		}
		v, ok := env[k]
		return v, ok
	}), nil
}

// Names is what compose delete reads from a file.
type Names struct {
	// Apps are the app names, sorted by service name.
	Apps []string
	// Name is the file's top-level name, the Compose project name, or ""
	// when the file has none.
	Name string
}

// AppNames returns the app name for each service in the file, sorted by
// service name, and the file's top-level name. It reads only the service
// names and the top-level name, and ignores the keys Convert would refuse.
// It needs the file's variables, and reads the .env, only when the
// top-level name uses one. delete uses it.
func AppNames(data []byte, opts Options) (*Names, error) {
	root, err := parse(data)
	if err != nil {
		return nil, err
	}
	c := &converter{prefix: opts.Prefix, ip: newInterpolator(nil)}
	services := c.services(root)
	if len(c.problems) > 0 {
		return nil, newError(c.problems)
	}
	out := &Names{}
	for _, s := range services {
		if name, ok := c.appName(s.name); ok {
			out.Apps = append(out.Apps, name)
		}
	}
	if n := root.vals["name"]; n != nil && n.kind == scalarKind && strings.Contains(n.text, "$") {
		if c.ip, err = variables(opts); err != nil {
			return nil, err
		}
	}
	out.Name = c.projectName(root)
	if len(c.problems) > 0 {
		return nil, newError(c.problems)
	}
	return out, nil
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
