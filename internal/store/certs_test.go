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
