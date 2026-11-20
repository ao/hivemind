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

// ServerJoinTokens returns the join tokens for servers as a
// pki.TokenStore. They are rows of the join-token table with the server
// role: a server's token is refused for a worker's join, and a worker's
// for a server's, as an unknown token is.
func (s *Store) ServerJoinTokens() pki.TokenStore { return joinTokens{s: s, role: roleServer} }

// Join token roles.
const (
	roleWorker = "worker"
	roleServer = "server"
)

// ErrIsServer is a node that runs a server of the cluster, which node
// removal refuses.
var ErrIsServer = errors.New("the node runs a server of the cluster")
