package config

import (
	"testing"
)

// FuzzLoadFile feeds arbitrary bytes in as the config file. It must never
// panic. Load must either return an error, or a config that passes Validate.
func FuzzLoadFile(f *testing.F) {
	f.Add([]byte(""))
	f.Add([]byte("log-level: debug\nreconcile-interval: 3s\n"))
	f.Add([]byte("api-addr: 0.0.0.0:4483\napi-tls-cert: /c\napi-tls-key: /k\n"))
	f.Add([]byte("api-adr: 127.0.0.1:4483\n"))
	f.Add([]byte("cluster-port: [1, 2]\n"))
	f.Add([]byte("token: \"tok\"\nadvertise: \"::\"\n"))
	f.Add([]byte("a: &x\n  - *x\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		opts := Options{
			Lookup:   func(string) (string, bool) { return "", false },
			ReadFile: func(string) ([]byte, error) { return data, nil },
			Getuid:   func() int { return 1000 },
		}
		cfg, err := Load(nil, opts)
		if err != nil {
			return
		}
		if verr := cfg.Validate(); verr != nil {
			t.Fatalf("Load returned a config that does not validate: %v", verr)
		}
	})
}
