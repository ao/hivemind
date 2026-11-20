package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// A rotation of the cluster CA, as docs/ca-rotation.md describes it. The
// store keeps its state; the CA files are internal/carotate's.

// CARotationSchemaVersion is the first schema version with CA rotation.
const CARotationSchemaVersion = 17

// The phases of a rotation, the CA it goes towards, and the CA of the
// server's own certificate.
const (
	CAPhaseTrust  = "trust"
	CAPhaseRenew  = "renew"
	CAPhaseEnding = "ending"

	CANext    = "next"
	CACurrent = "current"
)

// ErrCARotationExists means a rotation is already in progress.
var ErrCARotationExists = errors.New("a CA rotation is already in progress")

// CARotation is the rotation in progress.
type CARotation struct {
	// CurrentFP is the fingerprint of the CA in ca.crt, and NextFP of the
	// one in ca-next.crt.
	CurrentFP, NextFP [32]byte
	// Phase is CAPhaseTrust, CAPhaseRenew or CAPhaseEnding.
	Phase string
	// Toward is the CA the rotation goes towards: CANext, or CACurrent
	// after an abort in the renew phase.
	Toward string
	// Listener is the CA of the server's own certificate: CACurrent or
	// CANext.
	Listener  string
	StartedAt time.Time
	UpdatedAt time.Time // set by the store
}

func (r CARotation) validate() error {
	switch {
	case r.CurrentFP == r.NextFP:
		return fmt.Errorf("%w: a rotation needs two CAs", ErrInvalid)
	case r.Phase != CAPhaseTrust && r.Phase != CAPhaseRenew && r.Phase != CAPhaseEnding:
		return fmt.Errorf("%w: CA rotation phase %q", ErrInvalid, r.Phase)
	case r.Toward != CANext && r.Toward != CACurrent:
		return fmt.Errorf("%w: CA rotation towards %q", ErrInvalid, r.Toward)
	case r.Listener != CANext && r.Listener != CACurrent:
		return fmt.Errorf("%w: CA rotation listener %q", ErrInvalid, r.Listener)
	}
	return nil
}

// CARotationNode is what the server has seen of one node since the rotation
// started, or turned round.
type CARotationNode struct {
	NodeID string
	// Trusts is true once a request from the node said it trusts the CA
	// the rotation goes towards.
	Trusts bool
	// Renewed is true once the node presented a certificate from that CA.
	Renewed bool
	// OldBuild is true when the node's last request said nothing of what
	// it trusts: a build without rotation.
	OldBuild bool
	// SeenAt is when this record last changed.
	SeenAt time.Time
}

// GetCARotation returns the rotation in progress, or an error wrapping
// ErrNotFound when there is none.
func (s *Store) GetCARotation(ctx context.Context) (CARotation, error) {
	return getCARotation(ctx, s.rdb)
}

func getCARotation(ctx context.Context, q rowQuerier) (CARotation, error) {
	var r CARotation
	var cur, next []byte
	var started, updated int64
	err := q.QueryRowContext(ctx, `SELECT current_fp, next_fp, phase, toward, listener, started_at, updated_at FROM ca_rotation WHERE id = 1`).
		Scan(&cur, &next, &r.Phase, &r.Toward, &r.Listener, &started, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return CARotation{}, fmt.Errorf("CA rotation: %w", ErrNotFound)
	}
	if err != nil {
		return CARotation{}, err
	}
	if len(cur) != 32 || len(next) != 32 {
		return CARotation{}, errors.New("CA rotation: stored fingerprint is not 32 bytes")
	}
	copy(r.CurrentFP[:], cur)
	copy(r.NextFP[:], next)
	r.StartedAt, r.UpdatedAt = time.Unix(0, started), time.Unix(0, updated)
	return r, nil
}

// StartCARotation records a new rotation from the CA with fingerprint
// current to the one with next, in phase trust, with the server's
// certificate from the current CA, and forgets what was seen of the nodes
// in any earlier rotation. A rotation in progress is ErrCARotationExists.
func (s *Store) StartCARotation(ctx context.Context, current, next [32]byte) (CARotation, error) {
	now := s.now()
	r := CARotation{CurrentFP: current, NextFP: next, Phase: CAPhaseTrust, Toward: CANext, Listener: CACurrent,
		StartedAt: now, UpdatedAt: now}
	if err := r.validate(); err != nil {
		return CARotation{}, err
	}
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM ca_rotation`).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return ErrCARotationExists
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM ca_rotation_nodes`); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO ca_rotation (id, current_fp, next_fp, phase, toward, listener, started_at, updated_at)
			VALUES (1, ?, ?, ?, ?, ?, ?, ?)`, current[:], next[:], r.Phase, r.Toward, r.Listener, now.UnixNano(), now.UnixNano())
		return err
	})
	if err != nil {
		return CARotation{}, err
	}
	return r, nil
}

// SetCARotation writes the phase, direction and listener of the rotation
// in progress, which must have the same fingerprints as r. resetNodes
// forgets what was seen of the nodes, in the same transaction, as when the
// rotation turns round. No rotation is ErrNotFound.
func (s *Store) SetCARotation(ctx context.Context, r CARotation, resetNodes bool) error {
	if err := r.validate(); err != nil {
		return err
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE ca_rotation SET phase = ?, toward = ?, listener = ?, updated_at = ?
			WHERE id = 1 AND current_fp = ? AND next_fp = ?`, r.Phase, r.Toward, r.Listener, s.now().UnixNano(), r.CurrentFP[:], r.NextFP[:])
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			return fmt.Errorf("CA rotation with these CAs: %w", ErrNotFound)
		}
		if resetNodes {
			_, err = tx.ExecContext(ctx, `DELETE FROM ca_rotation_nodes`)
		}
		return err
	})
}

// EndCARotation deletes the rotation and what was seen of the nodes. No
// rotation is not an error.
func (s *Store) EndCARotation(ctx context.Context) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM ca_rotation`); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM ca_rotation_nodes`)
		return err
	})
}

// CARotationNodes returns what was seen of each node, by node ID.
func (s *Store) CARotationNodes(ctx context.Context) (map[string]CARotationNode, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT node_id, trusts, renewed, old_build, seen_at FROM ca_rotation_nodes`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]CARotationNode{}
	for rows.Next() {
		var n CARotationNode
		var seen int64
		if err := rows.Scan(&n.NodeID, &n.Trusts, &n.Renewed, &n.OldBuild, &seen); err != nil {
			return nil, err
		}
		n.SeenAt = time.Unix(0, seen)
		out[n.NodeID] = n
	}
	return out, rows.Err()
}

// NoteCARotationNode writes what was seen of a node, while a rotation is
// in progress. With none, it writes nothing: a note that races the end of
// a rotation leaves nothing behind.
func (s *Store) NoteCARotationNode(ctx context.Context, n CARotationNode) error {
	if n.NodeID == "" {
		return fmt.Errorf("%w: a node ID is required", ErrInvalid)
	}
	// A transaction of its own, as every write is, so that it is replicated
	// with the rest when the cluster has several servers (docs/ha.md). The
	// marks are not in the routing table.
	return s.tx(ctx, func(tx *sql.Tx) error {
		keepsRoutes(tx)
		_, err := tx.ExecContext(ctx, `INSERT INTO ca_rotation_nodes (node_id, trusts, renewed, old_build, seen_at)
			SELECT ?, ?, ?, ?, ? WHERE EXISTS (SELECT 1 FROM ca_rotation)
			ON CONFLICT (node_id) DO UPDATE SET trusts = excluded.trusts, renewed = excluded.renewed,
				old_build = excluded.old_build, seen_at = excluded.seen_at`,
			n.NodeID, n.Trusts, n.Renewed, n.OldBuild, s.now().UnixNano())
		return err
	})
}
