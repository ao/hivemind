package nodeapi

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The client measures how far the server's clock is from this machine's,
// from the Date header of its replies.
func TestClientMeasuresClockSkew(t *testing.T) {
	for _, off := range []time.Duration{3 * time.Hour, -2 * time.Hour, 0} {
		ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Date", time.Now().Add(off).UTC().Format(http.TimeFormat))
			w.WriteHeader(http.StatusNoContent)
		}))
		c, err := NewClient(ts.URL, ts.Client().Transport.(*http.Transport).TLSClientConfig)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := c.ClockSkew(); ok {
			t.Fatal("a skew before any reply")
		}
		if err := c.Register(context.Background(), "node-abc123", Register{Name: "n"}); err != nil {
			t.Fatal(err)
		}
		d, ok := c.ClockSkew()
		// This machine is ahead when the server's clock is behind.
		if want := -off; !ok || d < want-2*time.Second || d > want+2*time.Second {
			t.Errorf("server %s off: skew %s, %v, want about %s", off, d, ok, want)
		}
		ts.Close()
	}
}

// A handshake that fails because a certificate is outside its validity at
// this machine's time says to check the clock; other errors are untouched.
func TestClockHint(t *testing.T) {
	exp := fmt.Errorf("tls: failed to verify certificate: %w", x509.CertificateInvalidError{Reason: x509.Expired, Detail: "current time is before"})
	got := clockHint(exp)
	if !strings.Contains(got.Error(), "this machine's clock reads") || !strings.Contains(got.Error(), "NTP") || !errors.Is(got, exp) {
		t.Fatalf("hint: %v", got)
	}
	other := errors.New("connection refused")
	if clockHint(other) != other {
		t.Fatal("an error with no clock in it was changed")
	}
}
