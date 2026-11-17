package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/pflag"
	"gopkg.in/yaml.v3"
)

// fileConfig is the YAML file. Each key is a pointer so that an absent key is
// told apart from an empty one. Numbers and durations are read as strings and
// parsed by the same code that parses flags and environment variables.
type fileConfig struct {
	DataDir            *string `yaml:"data-dir"`
	APIAddr            *string `yaml:"api-addr"`
	Advertise          *string `yaml:"advertise"`
	ClusterAddr        *string `yaml:"cluster-addr"`
	ClusterPort        *string `yaml:"cluster-port"`
	APITLSCert         *string `yaml:"api-tls-cert"`
	APITLSKey          *string `yaml:"api-tls-key"`
	DockerHost         *string `yaml:"docker-host"`
	LogLevel           *string `yaml:"log-level"`
	LogFormat          *string `yaml:"log-format"`
	MetricsAddr        *string `yaml:"metrics-addr"`
	ReconcileInterval  *string `yaml:"reconcile-interval"`
	MinReady           *string `yaml:"min-ready"`
	RouteAddr          *string `yaml:"route-addr"`
	RouteHTTPPort      *string `yaml:"route-http-port"`
	RouteHTTPSPort     *string `yaml:"route-https-port"`
	RouteMaxConnsPerIP *string `yaml:"route-max-conns-per-ip"`
	ACMEEmail          *string `yaml:"acme-email"`
	ACMECA             *string `yaml:"acme-ca"`
	Server             *string `yaml:"server"`
	Token              *string `yaml:"token"`
}

// decodeFile decodes data into fc. Unknown keys are errors. The message names
// the key and the line. An empty file is valid.
// maxFileSize bounds the config file. Real files are a few hundred bytes; the
// cap stops a pathological file (deep nesting, alias chains) from stalling startup.
const maxFileSize = 64 << 10

func decodeFile(data []byte, name string, fc *fileConfig) error {
	if len(data) > maxFileSize {
		return fmt.Errorf("%s: %d bytes is larger than the %d-byte limit for a config file", name, len(data), maxFileSize)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(fc); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("%s: %w", name, err)
	}
	// The file is decoded into the token field only so that this error can name
	// it: a config file is easy to copy, share or commit, so it is no place for
	// a credential.
	if fc.Token != nil {
		return fmt.Errorf("%s: token: the API token does not belong in the config file; set HIVEMIND_TOKEN or --token, or let the client read the token file that init wrote", name)
	}
	return nil
}

// Setting is one setting as the YAML file names it: a key and its value as
// text.
type Setting struct {
	Key   string
	Value string
}

// pathKeys are the settings whose value is a file, written as an absolute
// path so the file means the same from any working directory.
var pathKeys = map[string]bool{"api-tls-cert": true, "api-tls-key": true}

// GivenFlags returns the node settings set with a flag on fs, in the order
// Bind registers them, except config and data-dir, which say where the file
// is. File paths are made absolute. These are what init writes for serve.
func GivenFlags(fs *pflag.FlagSet) ([]Setting, error) {
	var out []Setting
	for _, s := range settings(Options{}.withDefaults()) {
		if s.client || s.name == "config" || s.name == "data-dir" {
			continue
		}
		f := fs.Lookup(s.name)
		if f == nil || !f.Changed {
			continue
		}
		v := f.Value.String()
		if pathKeys[s.name] && v != "" {
			abs, err := filepath.Abs(v)
			if err != nil {
				return nil, fmt.Errorf("--%s %s: %w", s.name, v, err)
			}
			v = abs
		}
		out = append(out, Setting{Key: s.name, Value: v})
	}
	return out, nil
}

// WriteFile creates the YAML config file path, with mode 0600, holding
// settings, each value a quoted string, after a comment line, header. It
// never replaces a file: one that exists is an error that wraps
// fs.ErrExist. Load reads the file back as it reads one written by hand.
func WriteFile(path, header string, settings []Setting) error {
	doc := &yaml.Node{Kind: yaml.MappingNode}
	for _, s := range settings {
		doc.Content = append(doc.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: s.Key},
			&yaml.Node{Kind: yaml.ScalarNode, Value: s.Value, Style: yaml.DoubleQuotedStyle})
	}
	b, err := yaml.Marshal(doc)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append([]byte("# "+header+"\n"), b...)); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return err
	}
	return nil
}
