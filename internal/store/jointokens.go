package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/ao/hivemind/internal/pki"
)

// tokenTimeout bounds one token-table write. pki.TokenStore has no context.
const tokenTimeout = 10 * time.Second

// JoinTokens returns the join-token table as a pki.TokenStore. Each call is
// one transaction, and the database runs IMMEDIATE transactions, so two
// concurrent Consume calls for one token can't both succeed.
func (s *Store) JoinTokens() pki.TokenStore { return joinTokens{s} }

type joinTokens struct{ s *Store }

// Add records an unused token hash that expires at expires. A hash that is
// already stored is an error.
func (j joinTokens) Add(hash [32]byte, expires time.Time) error {
	ctx, cancel := context.WithTimeout(context.Background(), tokenTimeout)
	defer cancel()
	return j.s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO join_tokens (hash, expires, used_at) VALUES (?, ?, 0)`,
			hash[:], expires.UnixNano())
		return err
	})
}

// Consume marks the hash used. It returns pki.ErrInvalidToken for an unknown
// hash, pki.ErrTokenExpired at or after expiry, and pki.ErrTokenUsed when it
// was used before. The expiry is checked first, as pki.MemoryStore does.
func (j joinTokens) Consume(hash [32]byte, now time.Time) error {
	ctx, cancel := context.WithTimeout(context.Background(), tokenTimeout)
	defer cancel()
	return j.s.tx(ctx, func(tx *sql.Tx) error {
		var expires, usedAt int64
		err := tx.QueryRowContext(ctx, `SELECT expires, used_at FROM join_tokens WHERE hash = ?`, hash[:]).
			Scan(&expires, &usedAt)
		if errors.Is(err, sql.ErrNoRows) {
			return pki.ErrInvalidToken
		}
		if err != nil {
			return err
		}
		if now.UnixNano() >= expires {
			return pki.ErrTokenExpired
		}
		if usedAt != 0 {
			return pki.ErrTokenUsed
		}
		_, err = tx.ExecContext(ctx, `UPDATE join_tokens SET used_at = ? WHERE hash = ?`, now.UnixNano(), hash[:])
		return err
	})
}

// PruneJoinTokens deletes the tokens that have expired by now, used or not.
// It returns how many it deleted. A pruned token reads as pki.ErrInvalidToken,
// the same as an unknown one, so a joining node can't tell the two apart.
func (s *Store) PruneJoinTokens(ctx context.Context, now time.Time) (int64, error) {
	var n int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM join_tokens WHERE expires <= ?`, now.UnixNano())
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	return n, err
}
