package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/ao/hivemind/internal/pki"
)

// The servers of a cluster with more than one (docs/ha.md). The Raft
// configuration says which servers vote; this table says where each one
// is, for the workers, which dial a server's node listener, and for
// hivemind servers. It is empty for a cluster of one.

// Server is one server of the cluster.
type Server struct {
	// ID is the server's node ID.
	ID string
	// Advertise is the address the server advertises, an IP address or a
	// DNS name, which its certificate names, and NodePort its node
	// listener's port. Workers dial Advertise:NodePort.
	Advertise string
	NodePort  int
	// RaftAddr is the server's address on the server port, host:port.
	RaftAddr string
	AddedAt  time.Time
}

// NodeAddr is the host:port workers dial for the server.
func (sv Server) NodeAddr() string { return net.JoinHostPort(sv.Advertise, strconv.Itoa(sv.NodePort)) }

// MaxServers is the most servers a cluster takes.
const MaxServers = 7

// PutServer adds the server, or replaces its row.
func (s *Store) PutServer(ctx context.Context, sv Server) error {
	if err := pki.ValidateNodeID(sv.ID); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if sv.Advertise == "" || sv.RaftAddr == "" || sv.NodePort < 1 || sv.NodePort > 65533 {
		return fmt.Errorf("%w: a server needs an advertise address, a node port in 1-65533 and a Raft address", ErrInvalid)
	}
	if len(sv.Advertise) > 253 || len(sv.RaftAddr) > MaxNodeDialsLen {
		return fmt.Errorf("%w: the server's address is too long", ErrInvalid)
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		keepsRoutes(tx)
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM servers WHERE id != ?`, sv.ID).Scan(&n); err != nil {
			return err
		}
		if n >= MaxServers {
			return fmt.Errorf("%w: the cluster has %d servers, the most it takes", ErrConflict, n)
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO servers (id, advertise, node_port, raft_addr, added_at) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (id) DO UPDATE SET advertise = excluded.advertise, node_port = excluded.node_port, raft_addr = excluded.raft_addr`,
			sv.ID, sv.Advertise, sv.NodePort, sv.RaftAddr, s.now().UnixNano())
		return err
	})
}

// DeleteServer removes the server's row. An unknown server is ErrNotFound.
func (s *Store) DeleteServer(ctx context.Context, id string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		keepsRoutes(tx)
		res, err := tx.ExecContext(ctx, `DELETE FROM servers WHERE id = ?`, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("server %q: %w", id, ErrNotFound)
		}
		return nil
	})
}

// ListServers returns the servers, the earliest added first.
func (s *Store) ListServers(ctx context.Context) ([]Server, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT id, advertise, node_port, raft_addr, added_at FROM servers ORDER BY added_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Server
	for rows.Next() {
		var sv Server
		var added int64
		if err := rows.Scan(&sv.ID, &sv.Advertise, &sv.NodePort, &sv.RaftAddr, &added); err != nil {
			return nil, err
		}
		sv.AddedAt = time.Unix(0, added)
		out = append(out, sv)
	}
	return out, rows.Err()
}

// ReplicasToMove counts what a drain of the node would move: the replicas
// without a volume that should run on it, and the surges placed on it.
// hivemind server remove refuses a server whose node has any: a removed
// server's serve stops with its agent, its containers keep running, and
// once its node is lost they are placed again elsewhere and run twice. A
// drained node has none, and neither has a lost one, whose replicas the
// loss has placed again already.
func (s *Store) ReplicasToMove(ctx context.Context, node string) (int, error) {
	reps, err := queryAssignments(ctx, s.rdb, `r.node_id = ? AND r.desired = 'running'`, node)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range reps {
		if len(r.Spec.Volumes) == 0 {
			n++
		}
	}
	var surges int
	if err := s.rdb.QueryRowContext(ctx, `SELECT count(*) FROM surges WHERE node_id = ?`, node).Scan(&surges); err != nil {
		return 0, err
	}
	return n + surges, nil
}

// ServerJoinTokens returns the join tokens for servers as a
// pki.TokenStore. They are rows of the join-token table with the server
// role: a server's token is refused for a worker's join, and a worker's
// for a server's, with a *TokenRoleError, and is left unused.
func (s *Store) ServerJoinTokens() pki.TokenStore { return joinTokens{s: s, role: roleServer} }

// Join token roles.
const (
	roleWorker = "worker"
	roleServer = "server"
)

// ErrIsServer is a node that runs a server of the cluster, which node
// removal refuses.
var ErrIsServer = errors.New("the node runs a server of the cluster")

// HASchemaVersion is the first schema version with the server table.
const HASchemaVersion = 18

// MakeClusterOfOneFile makes the store file at path the store of a cluster
// of one, as restore does to the store it has just written
// (docs/ha.md#compatibility): it empties the server table and sets the
// applied index to 0, for a Raft log that a later server join starts
// afresh. Like BumpEpochFile it does not open the file as a Store, so the
// file keeps its schema version; one from before HASchemaVersion has
// neither, and is left as it is. It returns how many servers it took out.
func MakeClusterOfOneFile(ctx context.Context, path string) (int64, error) {
	db, err := openExisting(path)
	if err != nil {
		return 0, err
	}
	defer db.Close()
	v, err := schemaVersion(ctx, db)
	if err != nil {
		return 0, err
	}
	if v < HASchemaVersion {
		return 0, nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `DELETE FROM servers`)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE raft_applied SET idx = 0 WHERE id = 1`); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	if err := db.Close(); err != nil {
		return 0, err
	}
	return n, syncFile(path)
}

// CountServers is how many servers the server table lists.
func (s *Store) CountServers(ctx context.Context) (int, error) {
	var n int
	err := s.rdb.QueryRowContext(ctx, `SELECT count(*) FROM servers`).Scan(&n)
	return n, err
}

// RunAsNodeFile makes the store file at path, which restore has just
// written, the store of the server that runs as node: the cluster record
// names it from then on, as serve reads it. A backup taken on a server that
// joined a cluster of several names that server's node, and the record
// names the first server's. A node on the removed list is ErrNodeRemoved:
// serve would refuse it. Like MakeClusterOfOneFile it does not open the
// file as a Store.
func RunAsNodeFile(ctx context.Context, path, node string) error {
	if err := pki.ValidateNodeID(node); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	db, err := openExisting(path)
	if err != nil {
		return err
	}
	defer db.Close()
	v, err := schemaVersion(ctx, db)
	if err != nil {
		return err
	}
	if v >= removalSchemaVersion {
		removed, err := isRemoved(ctx, db, node)
		if err != nil {
			return err
		}
		if removed {
			return fmt.Errorf("node %s: %w", node, ErrNodeRemoved)
		}
	}
	var cur string
	if err := db.QueryRowContext(ctx, `SELECT node_id FROM cluster WHERE id = 1`).Scan(&cur); err != nil {
		return err
	}
	if cur == node {
		return nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE cluster SET node_id = ? WHERE id = 1`, node); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if err := db.Close(); err != nil {
		return err
	}
	return syncFile(path)
}

// removalSchemaVersion is the first schema version with removed_nodes.
const removalSchemaVersion = 6
