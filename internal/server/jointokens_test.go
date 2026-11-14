package server

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/auth"
	"github.com/ao/hivemind/internal/pki"
)

// newJoinHarness is newHarness with a join-token issuer. The issuer returns
// the given token and records the TTL it was asked for.
func newJoinHarness(t *testing.T, issue func(time.Duration) (string, time.Time, error)) *harness {
	t.Helper()
	h := newHarness(t)
	sessions, err := auth.NewSessions(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	a, err := auth.NewAuthenticator(auth.HashToken(h.token), sessions)
	if err != nil {
		t.Fatal(err)
	}
	h.srv, err = New(Config{Store: h.store, Runtime: h.rt, NodeID: testNode, Auth: a, IssueJoinToken: issue})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.srv.CloseStreams)
	return h
}

func TestJoinTokenWithoutCA(t *testing.T) {
	h := newHarness(t)
	rec := h.do(t, http.MethodPost, "/v1/join-tokens", `{}`)
	wantStatus(t, rec, http.StatusServiceUnavailable)
	if !strings.Contains(errorOf(t, rec), "cluster CA") {
		t.Fatalf("error does not say why: %q", rec.Body.String())
	}
}

func TestJoinTokenIssued(t *testing.T) {
	var asked time.Duration
	exp := time.Unix(1_900_000_000, 0)
	h := newJoinHarness(t, func(ttl time.Duration) (string, time.Time, error) {
		asked = ttl
		return "hivemind-join-v1.test", exp, nil
	})

	rec := h.do(t, http.MethodPost, "/v1/join-tokens", `{"ttl":"1h"}`)
	wantStatus(t, rec, http.StatusCreated)
	if asked != time.Hour {
		t.Fatalf("issuer asked for %s, want 1h", asked)
	}
	var reply JoinTokenReply
	if err := decodeStrict(strings.NewReader(rec.Body.String()), &reply); err != nil {
		t.Fatal(err)
	}
	if reply.Token != "hivemind-join-v1.test" || !reply.ExpiresAt.Equal(exp) {
		t.Fatalf("reply %+v", reply)
	}
}

func TestJoinTokenDefaultTTL(t *testing.T) {
	var asked time.Duration
	h := newJoinHarness(t, func(ttl time.Duration) (string, time.Time, error) {
		asked = ttl
		return "t", time.Now(), nil
	})
	wantStatus(t, h.do(t, http.MethodPost, "/v1/join-tokens", `{}`), http.StatusCreated)
	if asked != pki.DefaultTokenTTL {
		t.Fatalf("default ttl %s, want %s", asked, pki.DefaultTokenTTL)
	}
}

func TestJoinTokenBadInput(t *testing.T) {
	calls := 0
	h := newJoinHarness(t, func(time.Duration) (string, time.Time, error) {
		calls++
		return "t", time.Now(), nil
	})
	for _, body := range []string{
		``,
		`not json`,
		`{}{}`,
		`{"ttl":"soon"}`,
		`{"ttl":"0s"}`,
		`{"ttl":"-1h"}`,
		`{"ttl":"200h"}`,
		`{"ttl":"1h","node":"x"}`,
	} {
		rec := h.do(t, http.MethodPost, "/v1/join-tokens", body)
		if rec.Code != http.StatusBadRequest && rec.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("body %q: status %d, want 400", body, rec.Code)
		}
		errorOf(t, rec)
	}
	if calls != 0 {
		t.Fatalf("issuer ran %d times for bad input", calls)
	}
}
