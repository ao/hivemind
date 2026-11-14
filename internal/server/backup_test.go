package server

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/auth"
	"github.com/ao/hivemind/internal/store"
)

// backupServer is a server whose Backup writes body, or fails with err
// after writing body.
func backupServer(t *testing.T, body string, err error) (*Server, string, *int) {
	t.Helper()
	st, oerr := store.Open(context.Background(), filepath.Join(t.TempDir(), "hivemind.db"))
	if oerr != nil {
		t.Fatal(oerr)
	}
	t.Cleanup(func() { st.Close() })
	tok, _ := auth.NewToken()
	sessions, _ := auth.NewSessions(time.Hour)
	a, aerr := auth.NewAuthenticator(auth.HashToken(tok), sessions)
	if aerr != nil {
		t.Fatal(aerr)
	}
	calls := 0
	srv, serr := New(Config{Store: st, Runtime: &fakeRuntime{}, NodeID: testNode, Auth: a,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Backup: func(_ context.Context, w io.Writer) error {
			calls++
			if body != "" {
				if _, err := io.WriteString(w, body); err != nil {
					return err
				}
			}
			return err
		}})
	if serr != nil {
		t.Fatal(serr)
	}
	return srv, tok, &calls
}

func backupRequest(tok, remote string, viaTLS bool) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/backup", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.RemoteAddr = remote
	if viaTLS {
		req.TLS = &tls.ConnectionState{}
	}
	return req
}

func TestBackupRouteStreamsTheArchive(t *testing.T) {
	for _, c := range []struct {
		remote string
		tls    bool
	}{{"127.0.0.1:5000", false}, {"[::1]:5000", false}, {"10.0.0.9:5000", true}} {
		srv, tok, calls := backupServer(t, "archive bytes", nil)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, backupRequest(tok, c.remote, c.tls))
		if rec.Code != http.StatusOK || rec.Body.String() != "archive bytes" || *calls != 1 {
			t.Fatalf("%s (tls %v): %d %q, %d calls", c.remote, c.tls, rec.Code, rec.Body.String(), *calls)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/x-tar" {
			t.Errorf("Content-Type %q", ct)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
			t.Errorf("Cache-Control %q", cc)
		}
	}
}

// Over plain HTTP, only a loopback client gets the archive: it holds the CA
// key.
func TestBackupRouteRefusesPlaintextFromAnotherMachine(t *testing.T) {
	srv, tok, calls := backupServer(t, "archive bytes", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, backupRequest(tok, "10.0.0.9:5000", false))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "TLS") || *calls != 0 {
		t.Fatalf("plain HTTP from 10.0.0.9: %d %q, %d calls", rec.Code, rec.Body.String(), *calls)
	}
}

func TestBackupRouteWithoutToken(t *testing.T) {
	srv, _, calls := backupServer(t, "archive bytes", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, backupRequest("hm_wrong", "127.0.0.1:5000", false))
	if rec.Code != http.StatusUnauthorized || *calls != 0 {
		t.Fatalf("no valid token: %d, %d calls", rec.Code, *calls)
	}
}

func TestBackupRouteErrors(t *testing.T) {
	// Nothing written yet: an error reply.
	srv, tok, _ := backupServer(t, "", errors.New("disk full"))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, backupRequest(tok, "127.0.0.1:5000", false))
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "disk full") {
		t.Fatalf("failure before the archive: %d %q", rec.Code, rec.Body.String())
	}

	// Part of the archive sent: the connection is aborted.
	srv, tok, _ = backupServer(t, "partial", errors.New("read error"))
	rec = httptest.NewRecorder()
	func() {
		defer func() {
			if p := recover(); p != http.ErrAbortHandler {
				t.Fatalf("failure while streaming: recovered %v, want http.ErrAbortHandler", p)
			}
		}()
		srv.Handler().ServeHTTP(rec, backupRequest(tok, "127.0.0.1:5000", false))
	}()

	// No backup function: 503.
	h := newHarness(t)
	if rec := h.do(t, http.MethodPost, "/v1/backup", ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no Backup: %d", rec.Code)
	}
}
