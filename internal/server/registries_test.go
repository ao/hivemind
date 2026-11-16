package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ao/hivemind/internal/spec"
	"github.com/ao/hivemind/internal/store"
)

const apiPassword = "api-registry-password-3e8b"

func loginBody(user, pass string) string {
	b, _ := json.Marshal(RegistryLogin{Username: user, Password: pass})
	return string(b)
}

// checkNoPassword fails if the password is in a reply or in the log.
func checkNoPassword(t *testing.T, h *harness, rec *httptest.ResponseRecorder) {
	t.Helper()
	if strings.Contains(rec.Body.String(), apiPassword) {
		t.Fatalf("reply contains the password: %s", rec.Body)
	}
	if strings.Contains(h.logs.String(), apiPassword) {
		t.Fatalf("log contains the password:\n%s", h.logs)
	}
}

func TestRegistryRoutes(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.store.PutApp(ctx, spec.App{Name: "priv", Image: "ghcr.io/acme/app:1"}); err != nil {
		t.Fatal(err)
	}

	rec := h.do(t, http.MethodPut, "/v1/registries/GHCR.io", loginBody("octocat", apiPassword))
	wantStatus(t, rec, http.StatusOK)
	checkNoPassword(t, h, rec)
	var v RegistryView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil || v.Host != "ghcr.io" || v.Username != "octocat" || v.UpdatedAt.IsZero() {
		t.Fatalf("PUT reply: %s, %v", rec.Body, err)
	}
	if strings.Contains(rec.Body.String(), "password") {
		t.Fatalf("PUT reply has a password field: %s", rec.Body)
	}
	if h.trigger == 0 {
		t.Fatal("a login did not trigger the local agent")
	}
	// The credential reaches the replica's assignment.
	as, err := h.store.AssignmentsForNode(ctx, testNode)
	if err != nil || len(as) != 1 || as[0].Auth == nil || as[0].Auth.Password != apiPassword {
		t.Fatalf("assignments after the login: %+v, %v", as, err)
	}

	// Docker Hub, by another of its names.
	wantStatus(t, h.do(t, http.MethodPut, "/v1/registries/index.docker.io", loginBody("hubuser", "hub-"+apiPassword)), http.StatusOK)

	for _, p := range []string{"/v1/registries", "/v1/apps", "/v1/apps/priv", "/v1/status", "/v1/nodes"} {
		rec := h.do(t, http.MethodGet, p, "")
		wantStatus(t, rec, http.StatusOK)
		checkNoPassword(t, h, rec)
	}
	rec = h.do(t, http.MethodGet, "/v1/registries", "")
	var list RegistryList
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list.Registries) != 2 ||
		list.Registries[0].Host != "docker.io" || list.Registries[1].Host != "ghcr.io" || list.Registries[1].Username != "octocat" {
		t.Fatalf("GET /v1/registries: %s, %v", rec.Body, err)
	}

	rec = h.do(t, http.MethodDelete, "/v1/registries/ghcr.io", "")
	if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Fatalf("DELETE: %d %q", rec.Code, rec.Body)
	}
	rec = h.do(t, http.MethodDelete, "/v1/registries/ghcr.io", "")
	wantStatus(t, rec, http.StatusNotFound)
	if msg := errorOf(t, rec); !strings.Contains(msg, "ghcr.io") {
		t.Fatalf("404 message %q does not name the host", msg)
	}
	if rec := h.do(t, http.MethodDelete, "/v1/registries/registry-1.docker.io", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE by registry-1.docker.io: %d %s", rec.Code, rec.Body)
	}
	rec = h.do(t, http.MethodGet, "/v1/registries", "")
	if strings.TrimSpace(rec.Body.String()) != `{"registries":[]}` {
		t.Fatalf("empty list: %s", rec.Body)
	}
	if as, _ := h.store.AssignmentsForNode(ctx, testNode); len(as) != 1 || as[0].Auth != nil {
		t.Fatalf("assignments after the logout: %+v", as)
	}
}

func TestRegistryRoutesRejectBadInput(t *testing.T) {
	h := newHarness(t)
	cases := []struct {
		name, path, body string
		code             int
		want             string
	}{
		{"Docker Hub namespace", "/v1/registries/myregistry", loginBody("u", apiPassword), 400, "Docker Hub"},
		{"bad port", "/v1/registries/ghcr.io:99999", loginBody("u", apiPassword), 400, "port must be"},
		{"port 0", "/v1/registries/ghcr.io:0", loginBody("u", apiPassword), 400, "port must be"},
		{"space", "/v1/registries/a%20b.io", loginBody("u", apiPassword), 400, "want a host name"},
		{"unknown field", "/v1/registries/ghcr.io", `{"username":"u","password":"` + apiPassword + `","email":"x"}`, 400, "unknown field"},
		{"no password", "/v1/registries/ghcr.io", `{"username":"u"}`, 400, "password is required"},
		{"no username", "/v1/registries/ghcr.io", `{"password":"` + apiPassword + `"}`, 400, "username is required"},
		{"newline", "/v1/registries/ghcr.io", loginBody("u", apiPassword+"\n"), 400, "control characters"},
		{"long password", "/v1/registries/ghcr.io", loginBody("u", apiPassword+strings.Repeat("x", store.MaxRegistryPasswordLen)), 400, "longer than"},
		{"long username", "/v1/registries/ghcr.io", loginBody(strings.Repeat("u", store.MaxRegistryUsernameLen+1), apiPassword), 400, "longer than"},
		{"colon in username", "/v1/registries/ghcr.io", loginBody("a:b", apiPassword), 400, "colon"},
		{"huge body", "/v1/registries/ghcr.io", loginBody("u", apiPassword+strings.Repeat("x", MaxBodyBytes)), 413, "larger than"},
		{"not JSON", "/v1/registries/ghcr.io", `username=u&password=` + apiPassword, 400, "invalid JSON"},
		{"empty", "/v1/registries/ghcr.io", "", 400, "empty"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := h.do(t, http.MethodPut, c.path, c.body)
			wantStatus(t, rec, c.code)
			if msg := errorOf(t, rec); !strings.Contains(msg, c.want) {
				t.Fatalf("error %q, want %q", msg, c.want)
			}
			checkNoPassword(t, h, rec)
		})
	}
	if rs, _ := h.store.ListRegistries(context.Background()); len(rs) != 0 {
		t.Fatalf("refused requests stored %+v", rs)
	}
	// DELETE checks the host too.
	wantStatus(t, h.do(t, http.MethodDelete, "/v1/registries/myregistry", ""), http.StatusBadRequest)
}

// A login without the admin token is refused before the body is read, and
// stores nothing.
func TestRegistryRoutesNeedTheToken(t *testing.T) {
	h := newHarness(t)
	for _, m := range []string{http.MethodPut, http.MethodGet, http.MethodDelete} {
		req := httptest.NewRequest(m, "/v1/registries/ghcr.io", strings.NewReader(loginBody("u", apiPassword)))
		if m == http.MethodGet {
			req = httptest.NewRequest(m, "/v1/registries", nil)
		}
		rec := httptest.NewRecorder()
		h.srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s without the token: %d", m, rec.Code)
		}
		checkNoPassword(t, h, rec)
	}
	if rs, _ := h.store.ListRegistries(context.Background()); len(rs) != 0 {
		t.Fatalf("a request without the token stored %+v", rs)
	}
}
