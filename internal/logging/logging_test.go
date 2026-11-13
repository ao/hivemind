package logging

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/ao/hivemind/internal/config"
)

const (
	adminToken = "hm_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	joinToken  = "hivemind-join-v1.0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef.BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
	sessionID  = "s3ss10n-1d-CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC"
	csrf       = "csrf-DDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDD"
	envValue   = "env-value-EEEEEEEEEEEEEEEE"
	pemKey     = "-----BEGIN EC PRIVATE KEY-----\nMHcCAQEEIFFFFFFFFFFFFFFFFFFFFF\n-----END EC PRIVATE KEY-----"
)

var secrets = []string{adminToken, joinToken, sessionID, csrf, envValue, "MHcCAQEEIFFFFFFFFFFFFFFFFFFFFF"}

type spec struct {
	Name  string
	Token string
}

// logEverything logs each secret in each way a caller might pass it.
func logEverything(log *slog.Logger) {
	log.Debug("token in message " + adminToken)
	log.Debug("fields",
		"token", adminToken,
		"admin-token", "plain words",
		"join_token", joinToken,
		"session", sessionID,
		"X-CSRF-Token", csrf,
		"Authorization", "Bearer "+adminToken,
		"cookie", "hivemind_session="+sessionID,
		"env", map[string]string{"DB_PASSWORD": envValue},
		"api_tls_key", pemKey,
		"secret", config.Secret(adminToken),
		"cfg", config.Config{Token: config.Secret(adminToken)},
	)
	log.Info("embedded",
		"err", errors.New("dial failed with token "+adminToken),
		"note", "join with "+joinToken+" please",
		"header", "Bearer "+adminToken,
		"pem", "key file:\n"+pemKey,
		"raw", []byte(adminToken),
		"spec", spec{Name: "web", Token: adminToken},
		"wrapped", fmt.Errorf("outer: %w", errors.New(joinToken)),
	)
	log.With("token", adminToken, "csrf_token", csrf).Warn("with attrs")
	log.WithGroup("session").Info("grouped", "id", sessionID)
	log.WithGroup("req").Info("nested", slog.Group("headers", "authorization", "Bearer "+adminToken),
		slog.Group("env", "A", envValue))
	log.Error("value", "v", slog.AnyValue(config.Secret(csrf)))
}

func TestRedactsEverySecret(t *testing.T) {
	for _, format := range []string{"text", "json"} {
		t.Run(format, func(t *testing.T) {
			var b bytes.Buffer
			log, err := New(&b, "debug", format)
			if err != nil {
				t.Fatal(err)
			}
			logEverything(log)
			out := b.String()
			if strings.Count(out, "\n") != 7 {
				t.Fatalf("want 7 lines at debug level, got:\n%s", out)
			}
			for _, s := range secrets {
				if strings.Contains(out, s) {
					t.Errorf("output holds %q:\n%s", s, out)
				}
			}
			if !strings.Contains(out, Mask) || !strings.Contains(out, "web") {
				t.Errorf("output lost its shape:\n%s", out)
			}
			if format == "json" {
				for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
					if !json.Valid([]byte(line)) {
						t.Errorf("not JSON: %s", line)
					}
				}
			}
		})
	}
}

func TestLevels(t *testing.T) {
	var b bytes.Buffer
	log, err := New(&b, "warn", "text")
	if err != nil {
		t.Fatal(err)
	}
	log.Info("hidden")
	log.Warn("shown")
	if strings.Contains(b.String(), "hidden") || !strings.Contains(b.String(), "shown") {
		t.Fatalf("level warn wrote:\n%s", b.String())
	}
	if _, err := New(&b, "loud", "text"); err == nil {
		t.Error("bad level accepted")
	}
	if _, err := New(&b, "info", "xml"); err == nil {
		t.Error("bad format accepted")
	}
}

func TestPlainValuesPassThrough(t *testing.T) {
	var b bytes.Buffer
	log, _ := New(&b, "info", "json")
	log.Info("http request", "method", "GET", "path", "/v1/apps/web", "status", 200, "app", "web", "replica", 1)
	var m map[string]any
	if err := json.Unmarshal(b.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if m["path"] != "/v1/apps/web" || m["status"] != float64(200) || m["app"] != "web" {
		t.Fatalf("record changed: %v", m)
	}
}

func TestSecretKey(t *testing.T) {
	for _, k := range []string{"token", "Admin-Token", "join_token", "X-CSRF-Token", "csrf", "session_id",
		"Authorization", "Cookie", "password", "env", "Env", "api_tls_key", "private_key", "key", "client_secret"} {
		if !SecretKey(k) {
			t.Errorf("%q is not treated as a secret", k)
		}
	}
	for _, k := range []string{"app", "path", "method", "status", "node", "replica", "image", "err", "container", "tls", "duration"} {
		if SecretKey(k) {
			t.Errorf("%q is treated as a secret", k)
		}
	}
}
