package store

import (
	"errors"
	"testing"
	"time"
)

func TestRecordNodeCertOnlyMovesForward(t *testing.T) {
	s := newT(t)
	ctx := ctxT(t)
	if _, err := s.NodeCertExpiry(ctx, node1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no record: %v, want ErrNotFound", err)
	}
	t1 := time.Unix(2_000_000_000, 0)
	if err := s.RecordNodeCert(ctx, node1, t1); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordNodeCert(ctx, node1, t1.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got, err := s.NodeCertExpiry(ctx, node1); err != nil || !got.Equal(t1) {
		t.Fatalf("after an older record: %s, %v; want %s", got, err, t1)
	}
	t2 := t1.Add(24 * time.Hour)
	if err := s.RecordNodeCert(ctx, node1, t2); err != nil {
		t.Fatal(err)
	}
	nodes, err := s.ListNodes(ctx)
	if err != nil || len(nodes) != 1 || !nodes[0].CertNotAfter.Equal(t2) {
		t.Fatalf("ListNodes: %+v, %v; want CertNotAfter %s", nodes, err, t2)
	}
	if err := s.RecordNodeCert(ctx, "", t2); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty ID: %v", err)
	}
	// Removing the node drops its record.
	if _, err := s.RemoveNode(ctx, node1, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.NodeCertExpiry(ctx, node1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("record after removal: %v, want ErrNotFound", err)
	}
}

// A record keeps the certificate's issue time. A record of the same
// certificate with an earlier issue time corrects it, as an older server
// recorded the time it first saw the certificate; a later one does not.
func TestRecordNodeCertValidityKeepsTheIssueTime(t *testing.T) {
	s := newT(t)
	ctx := ctxT(t)
	issued := time.Unix(1_900_000_000, 0)
	notAfter := issued.Add(365 * 24 * time.Hour)
	seen := issued.Add(100 * 24 * time.Hour)
	issuedAt := func() time.Time {
		t.Helper()
		nodes, err := s.ListNodes(ctx)
		if err != nil || len(nodes) != 1 {
			t.Fatalf("ListNodes: %+v, %v", nodes, err)
		}
		return nodes[0].CertIssuedAt
	}
	if err := s.RecordNodeCertValidity(ctx, node1, seen, notAfter); err != nil {
		t.Fatal(err)
	}
	if got := issuedAt(); !got.Equal(seen) {
		t.Fatalf("issue time %s, want %s", got, seen)
	}
	if err := s.RecordNodeCertValidity(ctx, node1, issued, notAfter); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordNodeCertValidity(ctx, node1, seen, notAfter); err != nil {
		t.Fatal(err)
	}
	if got := issuedAt(); !got.Equal(issued) {
		t.Fatalf("issue time after the certificate's own: %s, want %s", got, issued)
	}
	if err := s.RecordNodeCertValidity(ctx, node1, time.Time{}, notAfter); !errors.Is(err, ErrInvalid) {
		t.Fatalf("no issue time: %v", err)
	}
}
