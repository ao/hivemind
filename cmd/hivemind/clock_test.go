package main

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/pki"
)

type fixedSkew struct{ d time.Duration }

func (f fixedSkew) ClockSkew() (time.Duration, bool) { return f.d, true }

// A worker whose clock is hours off the server's says so, which way and by
// how much; one within a minute says nothing.
func TestWatchClockWarns(t *testing.T) {
	for _, c := range []struct {
		skew time.Duration
		want string
	}{
		{3 * time.Hour, "3h0m0s ahead of the server's"},
		{-2 * time.Hour, "2h0m0s behind the server's"},
		{20 * time.Second, ""},
		{-40 * time.Second, ""},
	} {
		var buf lockedBuf
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		watchClock(ctx, fixedSkew{c.skew}, slog.New(slog.NewTextHandler(&buf, nil)), 10*time.Millisecond)
		cancel()
		got := buf.String()
		switch {
		case c.want == "" && got != "":
			t.Errorf("skew %s: logged %q", c.skew, got)
		case c.want != "" && (!strings.Contains(got, c.want) || !strings.Contains(got, "NTP")):
			t.Errorf("skew %s: logged %q, want %q", c.skew, got, c.want)
		case c.want != "" && strings.Count(got, "level=WARN") != 1:
			t.Errorf("skew %s: warned %d times in 50 ms, want once an hour", c.skew, strings.Count(got, "level=WARN"))
		}
	}
}

// A renewed certificate from a server whose clock is hours ahead is not
// valid yet here; the error says the clocks disagree.
func TestRenewedCertFromTheFutureNamesTheClock(t *testing.T) {
	ca := newCA(t)
	key, err := pki.NewNodeKey()
	if err != nil {
		t.Fatal(err)
	}
	csr, err := pki.NewCSR(key)
	if err != nil {
		t.Fatal(err)
	}
	der, err := ca.IssueNodeCert(csr, pki.NodeCertOptions{NodeID: "node-abc123", Now: time.Now().Add(3 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = checkRenewedCert(ca, "node-abc123", key, pemEncodeCert(der), time.Now())
	if err == nil || !strings.Contains(err.Error(), "this machine's clock reads") || !strings.Contains(err.Error(), "NTP") {
		t.Fatalf("certificate from the future: %v", err)
	}
	// The same certificate checked at the server's time is fine.
	if _, err := checkRenewedCert(ca, "node-abc123", key, pemEncodeCert(der), time.Now().Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
}

// lockedBuf is a buffer a logger in another goroutine may write to.
type lockedBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
