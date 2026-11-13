package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"gopkg.in/yaml.v3"
)

// fileConfig is the YAML file. Each key is a pointer so that an absent key is
// told apart from an empty one. Numbers and durations are read as strings and
// parsed by the same code that parses flags and environment variables.
type fileConfig struct {
	DataDir           *string `yaml:"data-dir"`
	APIAddr           *string `yaml:"api-addr"`
	Advertise         *string `yaml:"advertise"`
	ClusterPort       *string `yaml:"cluster-port"`
	APITLSCert        *string `yaml:"api-tls-cert"`
	APITLSKey         *string `yaml:"api-tls-key"`
	DockerHost        *string `yaml:"docker-host"`
	LogLevel          *string `yaml:"log-level"`
	LogFormat         *string `yaml:"log-format"`
	MetricsAddr       *string `yaml:"metrics-addr"`
	ReconcileInterval *string `yaml:"reconcile-interval"`
	MinReady          *string `yaml:"min-ready"`
	Server            *string `yaml:"server"`
	Token             *string `yaml:"token"`
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
	return nil
}
