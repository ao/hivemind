package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// RecordNodeCert records that node id holds a certificate valid until
// notAfter. A record only moves forward: an older expiry than the one
// stored leaves the record as it is, so a connection that still presents
// the certificate a node renewed does not undo the renewal. A removed node
// is refused with ErrNodeRemoved, in the same transaction, so a renewal that
// races a removal does not hand out a certificate.
func (s *Store) RecordNodeCert(ctx context.Context, id string, notAfter time.Time) error {
	if id == "" || notAfter.IsZero() {
		return fmt.Errorf("%w: a certificate record needs a node ID and an expiry", ErrInvalid)
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		if removed, err := isRemoved(ctx, tx, id); err != nil {
			return err
		} else if removed {
			return fmt.Errorf("node %q: %w", id, ErrNodeRemoved)
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO node_certs (id, not_after, issued_at) VALUES (?, ?, ?)
			ON CONFLICT (id) DO UPDATE SET not_after = excluded.not_after, issued_at = excluded.issued_at
			WHERE excluded.not_after > node_certs.not_after`,
			id, notAfter.UnixNano(), s.now().UnixNano())
		return err
	})
}

// NodeCertExpiry returns the recorded expiry of node id's certificate, or an
// error wrapping ErrNotFound when there is no record.
func (s *Store) NodeCertExpiry(ctx context.Context, id string) (time.Time, error) {
	var na int64
	err := s.db.QueryRowContext(ctx, `SELECT not_after FROM node_certs WHERE id = ?`, id).Scan(&na)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, fmt.Errorf("certificate of node %q: %w", id, ErrNotFound)
	}
	if err != nil {
		return time.Time{}, err
	}
	return time.Unix(0, na), nil
}
