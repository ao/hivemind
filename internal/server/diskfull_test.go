package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
)

// A write that fails because the disk is full is a 507 with a message that
// says so, not a 500.
func TestDiskFullIs507(t *testing.T) {
	h := newHarness(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("PUT", "/v1/apps/web", nil)
	h.srv.fail(rec, req, fmt.Errorf("put app: %w", syscall.ENOSPC))
	if rec.Code != http.StatusInsufficientStorage {
		t.Fatalf("status %d, want 507", rec.Code)
	}
	if msg := errorOf(t, rec); !strings.Contains(msg, "disk is full") || !strings.Contains(msg, "free space") {
		t.Fatalf("message %q", msg)
	}
	if !strings.Contains(h.logs.String(), "disk of the data directory is full") {
		t.Fatal("not logged")
	}
}
