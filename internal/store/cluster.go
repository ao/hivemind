package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrClusterExists means the store already holds a cluster record.
var ErrClusterExists = errors.New("a cluster already exists in this store")

// Cluster is the record that hivemind init writes once.
type Cluster struct {
	// AdminTokenHash is the SHA-256 digest of the admin token. The store
	// never holds the token itself.
	AdminTokenHash [32]byte
	// NodeID is the ID of the node that runs the server.
	NodeID    string
	CreatedAt time.Time // set by the store
	// Epoch counts the restores this cluster's store has been through: 0
	// from init, and one more than the backup's after each restore
	// (BumpEpoch). A store from before schema version 16 is at 0. InitCluster
	// ignores it.
	Epoch int64
}

// InitCluster writes the cluster record, at epoch 0. It fails with
// ErrClusterExists if one is already there, and leaves it as it was.
func (s *Store) InitCluster(ctx context.Context, c Cluster) error {
	if c.AdminTokenHash == ([32]byte{}) || c.NodeID == "" {
		return fmt.Errorf("%w: cluster needs an admin token hash and a node ID", ErrInvalid)
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM cluster`).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return ErrClusterExists
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO cluster (id, admin_token_hash, node_id, created_at) VALUES (1, ?, ?, ?)`,
			c.AdminTokenHash[:], c.NodeID, s.now().UnixNano())
		return err
	})
}

// GetCluster returns the cluster record, or an error wrapping ErrNotFound if
// init has not run.
func (s *Store) GetCluster(ctx context.Context) (Cluster, error) {
	var c Cluster
	var hash []byte
	var created int64
	err := s.rdb.QueryRowContext(ctx, `SELECT admin_token_hash, node_id, created_at, epoch FROM cluster WHERE id = 1`).
		Scan(&hash, &c.NodeID, &created, &c.Epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return Cluster{}, fmt.Errorf("cluster: %w", ErrNotFound)
	}
	if err != nil {
		return Cluster{}, err
	}
	if len(hash) != len(c.AdminTokenHash) {
		return Cluster{}, fmt.Errorf("cluster: stored token hash has %d bytes", len(hash))
	}
	copy(c.AdminTokenHash[:], hash)
	c.CreatedAt = time.Unix(0, created)
	return c, nil
}

// BumpEpoch raises the cluster's epoch by one, as restore does to the store
// it restored, and returns the new epoch. A store without a cluster record
// is an error wrapping ErrNotFound.
func (s *Store) BumpEpoch(ctx context.Context) (int64, error) {
	var epoch int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE cluster SET epoch = epoch + 1 WHERE id = 1`)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			return fmt.Errorf("cluster: %w", ErrNotFound)
		}
		return tx.QueryRowContext(ctx, `SELECT epoch FROM cluster WHERE id = 1`).Scan(&epoch)
	})
	return epoch, err
}
