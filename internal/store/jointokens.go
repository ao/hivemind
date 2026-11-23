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
func (s *Store) JoinTokens() pki.TokenStore { return joinTokens{s: s, role: roleWorker} }

type joinTokens struct {
	s    *Store
	role string
}

// Add records an unused token hash that expires at expires. A hash that is
// already stored is an error.
func (j joinTokens) Add(hash [32]byte, expires time.Time) error {
	ctx, cancel := context.WithTimeout(context.Background(), tokenTimeout)
	defer cancel()
	return j.s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO join_tokens (hash, expires, used_at, role) VALUES (?, ?, 0, ?)`,
			hash[:], expires.UnixNano(), j.role)
		return err
	})
}

// TokenRoleError is a join token that the cluster issued for the other
// role: a server's token given to a worker's join, or a worker's given to
// a server's. The token is left as it was. It is a pki.ErrInvalidToken, so
// that what refuses an unknown token refuses it too. Saying which role the
// token has tells its holder nothing they could not learn by using it.
type TokenRoleError struct {
	// Server is true for a server's token.
	Server bool
}

func (e *TokenRoleError) Error() string {
	if e.Server {
		return "pki: invalid join token: it is a server's join token"
	}
	return "pki: invalid join token: it is a worker's join token"
}

func (e *TokenRoleError) Unwrap() error { return pki.ErrInvalidToken }

// Consume marks the hash used. It returns pki.ErrInvalidToken for an unknown
// hash, a *TokenRoleError for a hash of the other role, pki.ErrTokenExpired
// at or after expiry, and pki.ErrTokenUsed when it was used before. The
// expiry is checked first, as pki.MemoryStore does.
func (j joinTokens) Consume(hash [32]byte, now time.Time) error {
	ctx, cancel := context.WithTimeout(context.Background(), tokenTimeout)
	defer cancel()
	return j.s.tx(ctx, func(tx *sql.Tx) error {
		var expires, usedAt int64
		err := tx.QueryRowContext(ctx, `SELECT expires, used_at FROM join_tokens WHERE hash = ? AND role = ?`, hash[:], j.role).
			Scan(&expires, &usedAt)
		if errors.Is(err, sql.ErrNoRows) {
			var role string
			switch err := tx.QueryRowContext(ctx, `SELECT role FROM join_tokens WHERE hash = ?`, hash[:]).Scan(&role); {
			case err == nil:
				return &TokenRoleError{Server: role == roleServer}
			case !errors.Is(err, sql.ErrNoRows):
				return err
			}
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
