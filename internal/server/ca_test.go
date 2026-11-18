package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ao/hivemind/internal/carotate"
	"github.com/ao/hivemind/internal/pki"
	"github.com/ao/hivemind/internal/store"
)

// caHarness is a harness whose server holds a CA rotation manager, on a CA
// in a directory of its own, and a worker node beside the server's.
func caHarness(t *testing.T) *harness {
	t.Helper()
	return newHarness(t, func(c *Config) {
		dir := t.TempDir()
		if _, err := pki.CreateCA(dir); err != nil {
			t.Fatal(err)
		}
		if err := c.Store.UpsertNode(context.Background(), store.Node{ID: "node-w1", Name: "w1", CPU: 1, Memory: 1 << 30}); err != nil {
			t.Fatal(err)
		}
		m, err := carotate.Open(context.Background(), carotate.Options{Dir: dir, Store: c.Store, LocalNode: testNode,
			Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
		if err != nil {
			t.Fatal(err)
		}
		c.CA = m
	})
}

func decodeCA(t *testing.T, rec *httptest.ResponseRecorder) carotate.Status {
	t.Helper()
	var st carotate.Status
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("%s: %v", rec.Body.String(), err)
	}
	return st
}

func TestCARoutes(t *testing.T) {
	h := caHarness(t)
	rec := h.do(t, http.MethodGet, "/v1/ca", "")
	wantStatus(t, rec, http.StatusOK)
	st := decodeCA(t, rec)
	if st.Current == nil || st.Rotation != nil || st.Listener != st.Current.Fingerprint {
		t.Fatalf("status before a rotation: %+v", st)
	}

	rec = h.do(t, http.MethodPost, "/v1/ca/rotation/start", "")
	wantStatus(t, rec, http.StatusOK)
	st = decodeCA(t, rec)
	if st.Rotation == nil || st.Rotation.Phase != "trust" || st.Next == nil || len(st.Nodes) != 1 || st.Rotation.NextStep != "renew" {
		t.Fatalf("status after start: %+v", st)
	}
	wantStatus(t, h.do(t, http.MethodPost, "/v1/ca/rotation/start", ""), http.StatusConflict)

	// renew waits for node-w1, by name, unless forced.
	rec = h.do(t, http.MethodPost, "/v1/ca/rotation/renew", `{}`)
	wantStatus(t, rec, http.StatusConflict)
	var refusal CARefusal
	if err := json.Unmarshal(rec.Body.Bytes(), &refusal); err != nil || len(refusal.Waiting) != 1 || refusal.Waiting[0].Node != "node-w1" ||
		!strings.Contains(refusal.Error, "node-w1") {
		t.Fatalf("renew's refusal: %s", rec.Body.String())
	}
	wantStatus(t, h.do(t, http.MethodPost, "/v1/ca/rotation/renew", `{"force":true}`), http.StatusOK)
	// finish waits too; abort turns the rotation round.
	wantStatus(t, h.do(t, http.MethodPost, "/v1/ca/rotation/finish", ""), http.StatusConflict)
	rec = h.do(t, http.MethodPost, "/v1/ca/rotation/abort", "")
	wantStatus(t, rec, http.StatusOK)
	if st := decodeCA(t, rec); st.Rotation == nil || st.Rotation.Toward != "current" {
		t.Fatalf("after abort in phase renew: %+v", st.Rotation)
	}
	wantStatus(t, h.do(t, http.MethodPost, "/v1/ca/rotation/abort", ""), http.StatusConflict)
	rec = h.do(t, http.MethodPost, "/v1/ca/rotation/finish", `{"force":true}`)
	wantStatus(t, rec, http.StatusOK)
	if st := decodeCA(t, rec); st.Rotation != nil || st.Next != nil {
		t.Fatalf("after finish: %+v", st)
	}

	// Bad requests.
	wantStatus(t, h.do(t, http.MethodPost, "/v1/ca/rotation/start", `{"force":true}`), http.StatusBadRequest)
	wantStatus(t, h.do(t, http.MethodPost, "/v1/ca/rotation/start", `{"what":1}`), http.StatusBadRequest)
	wantStatus(t, h.do(t, http.MethodPost, "/v1/ca/rotation/now", ""), http.StatusNotFound)
	wantStatus(t, h.do(t, http.MethodPost, "/v1/ca/rotation/finish", ""), http.StatusConflict)

	// The routes need the admin token, like every other.
	for _, r := range []struct{ method, path string }{{http.MethodGet, "/v1/ca"}, {http.MethodPost, "/v1/ca/rotation/start"}} {
		rec := httptest.NewRecorder()
		h.srv.Handler().ServeHTTP(rec, httptest.NewRequest(r.method, r.path, nil))
		wantStatus(t, rec, http.StatusUnauthorized)
	}
}

func TestCARoutesWithoutACA(t *testing.T) {
	h := newHarness(t)
	wantStatus(t, h.do(t, http.MethodGet, "/v1/ca", ""), http.StatusServiceUnavailable)
	wantStatus(t, h.do(t, http.MethodPost, "/v1/ca/rotation/start", ""), http.StatusServiceUnavailable)
}
