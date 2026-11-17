package docker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/ao/hivemind/internal/runtime"
)

// The header is base64url of the JSON object the Engine API names, with
// exactly the fields username, password and serveraddress.
func TestEncodeRegistryAuth(t *testing.T) {
	sawURLAlphabet := false
	for n := 1; n <= 6; n++ {
		a := runtime.RegistryAuth{Username: "octocat", Password: "p" + strings.Repeat("~", n) + `"\é`, ServerAddress: "ghcr.io"}
		enc, err := EncodeRegistryAuth(a)
		if err != nil {
			t.Fatal(err)
		}
		if strings.ContainsAny(enc, "+/") {
			t.Fatalf("%q has characters of the standard base64 alphabet, not base64url", enc)
		}
		sawURLAlphabet = sawURLAlphabet || strings.ContainsAny(enc, "-_")
		raw, err := base64.URLEncoding.DecodeString(enc)
		if err != nil {
			t.Fatalf("%q is not padded base64url: %v", enc, err)
		}
		var got map[string]string
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		want := map[string]string{"username": a.Username, "password": a.Password, "serveraddress": "ghcr.io"}
		if len(got) != len(want) {
			t.Fatalf("fields %v, want %v", got, want)
		}
		for k, v := range want {
			if got[k] != v {
				t.Fatalf("%s = %q, want %q", k, got[k], v)
			}
		}
	}
	if !sawURLAlphabet {
		t.Fatal("no encoding used - or _, so the test did not tell base64url from base64")
	}
}

func TestPullSendsTheCredential(t *testing.T) {
	c, fd := newFake(t, func(w http.ResponseWriter, ex exchange) {
		_, _ = io.WriteString(w, `{"status":"Status: Downloaded newer image"}`+"\n")
	})
	auth := &runtime.RegistryAuth{Username: "octocat", Password: "s3cret-token", ServerAddress: "ghcr.io"}
	requireNoErr(t, c.Pull(context.Background(), "ghcr.io/acme/app:1", auth))
	requireNoErr(t, c.Pull(context.Background(), "nginx", nil))
	reqs := fd.requests()
	if len(reqs) != 2 {
		t.Fatalf("%d requests, want 2", len(reqs))
	}
	want, _ := EncodeRegistryAuth(*auth)
	if got := reqs[0].Header.Values("X-Registry-Auth"); len(got) != 1 || got[0] != want {
		t.Fatalf("X-Registry-Auth with a credential: %q, want %q", got, want)
	}
	if got := reqs[0].Query.Get("fromImage"); got != "ghcr.io/acme/app" {
		t.Fatalf("fromImage %q", got)
	}
	if got := reqs[1].Header.Values("X-Registry-Auth"); len(got) != 0 {
		t.Fatalf("X-Registry-Auth without a credential: %q", got)
	}
}

// A refused pull is ErrUnauthorized with a fixed message; any other failure
// keeps the daemon's message, without the password, the header or the
// account of a token request.
func TestPullRefusedByTheRegistry(t *testing.T) {
	const (
		user = "octocat"
		pass = "s3cret-token/+=x"
	)
	auth := &runtime.RegistryAuth{Username: user, Password: pass, ServerAddress: "reg.example.com"}
	header, _ := EncodeRegistryAuth(*auth)
	tokenURL := `Get "https://auth.example.com/token?account=` + user + `&scope=repository%3Aacme%2Fapp%3Apull&service=reg": `
	tests := []struct {
		name     string
		status   int
		body     string
		auth     bool
		notFound bool
		wantText string
	}{
		{"no credential, before the stream", 500, `{"message":"Head \"https://reg.example.com/v2/acme/app/manifests/1\": no basic auth credentials"}`, true, false, "no credential"},
		{"wrong credential", 500, `{"message":"Head \"https://reg.example.com/v2/acme/app/manifests/1\": unauthorized: authentication required"}`, true, false, "unauthorized"},
		{"401", 401, `{"message":"something"}`, true, false, "unauthorized"},
		{"403", 403, `{"message":"something"}`, true, false, "forbidden"},
		{"token request names the account", 500, `{"message":"` + strings.ReplaceAll(tokenURL, `"`, `\"`) + `unauthorized: incorrect username or password"}`, true, false, "incorrect username or password"},
		{"Docker Hub, private or missing, in the stream", 200, `{"error":"pull access denied for acme/app, repository does not exist or may require 'docker login': denied: requested access to the resource is denied"}` + "\n", true, true, "no such image, or no access"},
		{"denied", 500, `{"message":"Head \"https://ghcr.io/v2/acme/app/manifests/1\": denied"}`, true, true, "no such image, or no access"},
		// The containerd image store's words, from a Docker 29 daemon
		// (docker:dind) against registry:2 with htpasswd.
		{"containerd store, no credential", 500, `{"message":"failed to resolve reference \"reg.example.com/acme/app:1\": pull access denied, repository does not exist or may require authorization: authorization failed: no basic auth credentials"}`, true, false, "no credential"},
		{"containerd store, wrong credential", 500, `{"message":"unknown: failed to resolve reference \"reg.example.com/acme/app:1\": unexpected status from HEAD request to https://reg.example.com/v2/acme/app/manifests/1: 401 Unauthorized"}`, true, false, "unauthorized"},
		{"containerd store, Docker Hub", 500, `{"message":"failed to resolve reference \"docker.io/acme/app:1\": pull access denied, repository does not exist or may require authorization: server message: insufficient_scope: authorization failed"}`, true, true, "no such image, or no access"},
		{"ECR token expired", 200, `{"status":"Pulling"}` + "\n" + `{"error":"denied: Your authorization token has expired. Reauthenticate and try again."}` + "\n", true, false, "expired"},
		{"local permission denied", 500, `{"message":"open /var/lib/docker/tmp/x: permission denied"}`, false, false, "permission denied"},
		{"manifest unknown", 404, `{"message":"manifest for reg.example.com/acme/app:1 not found: manifest unknown"}`, false, true, "manifest unknown"},
		{"other error echoes secrets", 200, `{"error":"` + strings.ReplaceAll(tokenURL, `"`, `\"`) + `x509: certificate signed by unknown authority ` + pass + ` ` + header + `"}` + "\n", false, false, "x509"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newFake(t, func(w http.ResponseWriter, ex exchange) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			})
			err := c.Pull(context.Background(), "reg.example.com/acme/app:1", auth)
			if err == nil {
				t.Fatal("Pull succeeded")
			}
			if tc.auth {
				requireIs(t, err, runtime.ErrUnauthorized)
			} else {
				requireNotIs(t, err, runtime.ErrUnauthorized)
			}
			if tc.notFound {
				requireIs(t, err, runtime.ErrNotFound)
			} else {
				requireNotIs(t, err, runtime.ErrNotFound)
			}
			msg := err.Error()
			if !strings.Contains(msg, tc.wantText) {
				t.Errorf("error %q does not contain %q", msg, tc.wantText)
			}
			for _, secret := range []string{pass, header, "account=" + user} {
				if strings.Contains(msg, secret) {
					t.Errorf("error %q contains %q", msg, secret)
				}
			}
			if tc.auth && strings.Contains(msg, user) {
				t.Errorf("refusal %q names the user", msg)
			}
		})
	}
}

// Without a credential there is nothing to hide: a refusal keeps the
// daemon's words, and is still ErrUnauthorized.
func TestAnonymousRefusalKeepsTheDaemonsMessage(t *testing.T) {
	c, _ := newFake(t, func(w http.ResponseWriter, ex exchange) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"message":"Head \"https://reg.example.com/v2/acme/app/manifests/1\": no basic auth credentials"}`)
	})
	err := c.Pull(context.Background(), "reg.example.com/acme/app:1", nil)
	requireIs(t, err, runtime.ErrUnauthorized)
	requireNotIs(t, err, runtime.ErrNotFound)
	if !strings.Contains(err.Error(), "no basic auth credentials") {
		t.Fatalf("error %q lost the daemon's message", err)
	}
}

// redact takes out every form of the password a message may hold, for a
// password of characters that each encoding writes differently.
func TestRedactEveryForm(t *testing.T) {
	const (
		user = "octocat"
		// Lower-case letters only, so that strings.ToLower of an escape
		// changes nothing but its hex digits. The tildes put a + or a / in
		// the base64 of user:pass, and its 28 bytes need padding, so the
		// four kinds of base64 differ.
		pass = `p" \<>&+/% üw~~~~~~`
	)
	auth := &runtime.RegistryAuth{Username: user, Password: pass, ServerAddress: "reg.example.com"}
	header, _ := EncodeRegistryAuth(*auth)
	quoted := func(format string) string { s := fmt.Sprintf(format, pass); return s[1 : len(s)-1] }
	inJSON := func(html bool) string {
		var b strings.Builder
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(html)
		_ = enc.Encode(pass)
		s := strings.TrimSpace(b.String())
		return s[1 : len(s)-1]
	}
	basic := []byte(user + ":" + pass)
	forms := map[string]string{
		"as it is":             pass,
		"query escape":         url.QueryEscape(pass),
		"query escape, lower":  strings.ToLower(url.QueryEscape(pass)),
		"path escape":          url.PathEscape(pass),
		"path escape, lower":   strings.ToLower(url.PathEscape(pass)),
		"JSON":                 inJSON(true),
		"JSON without HTML":    inJSON(false),
		"%q":                   quoted("%q"),
		"%+q":                  quoted("%+q"),
		"basic, base64":        base64.StdEncoding.EncodeToString(basic),
		"basic, unpadded":      base64.RawStdEncoding.EncodeToString(basic),
		"basic, URL-safe":      base64.URLEncoding.EncodeToString(basic),
		"basic, URL-safe, raw": base64.RawURLEncoding.EncodeToString(basic),
		"header":               header,
	}
	distinct := map[string]bool{}
	for name, form := range forms {
		distinct[form] = true
		msg := redact("pull failed: <"+form+"> at the end", auth, header)
		if strings.Contains(msg, form) || msg != "pull failed: <[redacted]> at the end" {
			t.Errorf("%s (%q): redacted to %q", name, form, msg)
		}
	}
	// JSON without the HTML escapes and %q write this password alike;
	// every other pair differs.
	if len(distinct) != len(forms)-1 {
		t.Fatalf("%d distinct forms of %d: the password does not tell the encodings apart", len(distinct), len(forms))
	}
	// Every form in one message, one after another.
	var all []string
	for _, form := range forms {
		all = append(all, form)
	}
	msg := redact(strings.Join(all, " | "), auth, header)
	if got := strings.Count(msg, "[redacted]"); got != len(all) {
		t.Errorf("%d of %d forms redacted: %q", got, len(all), msg)
	}
	for name, form := range forms {
		if strings.Contains(msg, form) {
			t.Errorf("%s (%q) is left in %q", name, form, msg)
		}
	}
}
