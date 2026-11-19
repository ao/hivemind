package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/ao/hivemind/internal/scheduler"
	"github.com/ao/hivemind/internal/spec"
)

// Placement work that repeats. Every sweep and every node's report runs
// placePending, and with a replica that cannot be placed it read every placed
// assignment, decoded each one's spec, and ran the scheduler, to reach the
// answer it reached 2 s before. Three things cut that:
//
//   - A write transaction decodes each (app, generation) spec once
//     (txCache.decode), however many of its queries return it.
//   - placedOn reads the placed assignments once per transaction, and again
//     only when they changed, which a fingerprint of their rows tells.
//   - placePending is skipped when what placement reads is the same as when
//     it last ran and placed nothing: the pending replicas, with their specs
//     and the reasons it wrote, the nodes as the scheduler sees them at this
//     time, and the placed assignments. With a placement function other
//     than the scheduler, which may read anything, it always runs.

// txCache is what one write transaction keeps between its queries. Specs it
// returns share their maps and slices, so callers must not change them.
type txCache struct {
	specs    map[specKey]cachedSpec
	placed   []Assignment
	placedFP [sha256.Size]byte
	placedOK bool
	// routesKept is set by a transaction that leaves the routing table as
	// it is, whatever rows it changes (routecache.go).
	routesKept bool
	// onCommit runs once the transaction has committed.
	onCommit []func()
}

type specKey struct {
	app string
	gen int
}

type cachedSpec struct {
	json string
	spec spec.App
}

// txCaches maps each open transaction of Store.tx to its cache.
var txCaches sync.Map // *sql.Tx -> *txCache

// cacheOf returns the cache of q's transaction, or nil when q is not a
// transaction of Store.tx.
func cacheOf(q querier) *txCache {
	tx, ok := q.(*sql.Tx)
	if !ok {
		return nil
	}
	c, ok := txCaches.Load(tx)
	if !ok {
		return nil
	}
	return c.(*txCache)
}

// decode returns the spec of app at gen stored as js. A nil cache decodes.
func (c *txCache) decode(app string, gen int, js string) (spec.App, error) {
	k := specKey{app, gen}
	if c != nil {
		if e, ok := c.specs[k]; ok && e.json == js {
			return e.spec, nil
		}
	}
	var sp spec.App
	if err := json.Unmarshal([]byte(js), &sp); err != nil {
		return spec.App{}, fmt.Errorf("app %s: stored spec: %w", app, err)
	}
	if c != nil {
		if c.specs == nil {
			c.specs = map[specKey]cachedSpec{}
		}
		c.specs[k] = cachedSpec{js, sp}
	}
	return sp, nil
}

// placedPrint is a fingerprint of what placement reads of the placed
// assignments: each replica on a node and each surge, by app, index, node,
// generation and desired state, and each app's generation and creation time,
// which with the generation fix the spec. Rows read in another order give
// another fingerprint, which only costs a placement.
func placedPrint(ctx context.Context, q querier) ([sha256.Size]byte, error) {
	var reps, surges, apps string
	rows, err := q.QueryContext(ctx, `SELECT
		(SELECT coalesce(group_concat(app || '/' || idx || '/' || node_id || '/' || generation || '/' || desired, ','), '')
			FROM replicas WHERE node_id IS NOT NULL),
		(SELECT coalesce(group_concat(app || '/' || idx || '/' || node_id || '/' || generation || '/' || desired, ','), '')
			FROM surges),
		(SELECT coalesce(group_concat(name || '/' || generation || '/' || created_at || '/' || deleted, ','), '') FROM apps)`)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	defer rows.Close()
	if rows.Next() {
		if err := rows.Scan(&reps, &surges, &apps); err != nil {
			return [sha256.Size]byte{}, err
		}
	}
	if err := rows.Err(); err != nil {
		return [sha256.Size]byte{}, err
	}
	h := sha256.New()
	for _, s := range []string{reps, surges, apps} {
		h.Write(binary.AppendUvarint(nil, uint64(len(s))))
		h.Write([]byte(s))
	}
	var out [sha256.Size]byte
	h.Sum(out[:0])
	return out, nil
}

// pendingRow is a replica placePending is to place, with its stored spec.
type pendingRow struct {
	as   Assignment
	json string
}

// queryPending returns the running replicas with no node that are not held,
// with their stored specs.
func queryPending(ctx context.Context, q querier) ([]pendingRow, error) {
	c := cacheOf(q)
	rows, err := q.QueryContext(ctx, `SELECT r.app, r.idx, r.generation, r.desired, r.pending_reason, p.spec
		FROM replicas r JOIN app_specs p ON p.app = r.app AND p.generation = r.generation
		WHERE r.desired = 'running' AND r.node_id IS NULL AND r.held = 0 ORDER BY r.app, r.idx`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []pendingRow
	for rows.Next() {
		var p pendingRow
		if err := rows.Scan(&p.as.App, &p.as.Index, &p.as.Generation, &p.as.Desired, &p.as.PendingReason, &p.json); err != nil {
			return nil, err
		}
		p.as.specLen = len(p.json)
		if p.as.Spec, err = c.decode(p.as.App, p.as.Generation, p.json); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// placeInputs is what placement reads, as placePending would see it at now with
// the pending replicas' reasons: reasons[i] for pending[i].
func (s *Store) placeInputs(nodes []Node, pending []pendingRow, reasons []string, placed [sha256.Size]byte, now time.Time) [sha256.Size]byte {
	h := sha256.New()
	var buf []byte
	str := func(v string) {
		buf = binary.AppendUvarint(buf[:0], uint64(len(v)))
		h.Write(buf)
		h.Write([]byte(v))
	}
	num := func(v int64) {
		buf = binary.AppendVarint(buf[:0], v)
		h.Write(buf)
	}
	flag := func(b bool) {
		if b {
			num(1)
		} else {
			num(0)
		}
	}
	num(int64(len(nodes)))
	for _, n := range nodes {
		str(n.ID)
		num(int64(n.CPU))
		num(n.Memory)
		flag(n.Eligible)
		str(n.Address)
		// What the scheduler derives from the lease and readiness times.
		flag(now.Sub(n.LastLease) <= scheduler.DefaultLeaseTimeout)
		flag(s.minReady == 0 || now.Sub(n.ReadySince) >= s.minReady)
	}
	num(int64(len(pending)))
	for i, p := range pending {
		str(p.as.App)
		num(int64(p.as.Index))
		num(int64(p.as.Generation))
		str(reasons[i])
		str(p.json)
	}
	h.Write(placed[:])
	var out [sha256.Size]byte
	h.Sum(out[:0])
	return out
}

// placeSkip reports whether key is the key of the last placement that placed
// nothing.
func (s *Store) placeSkip(key [sha256.Size]byte) bool {
	s.placeMu.Lock()
	defer s.placeMu.Unlock()
	return s.placeSet && s.placeKey == key
}

// placeDone records key as the key of a placement that placed nothing.
func (s *Store) placeDone(key [sha256.Size]byte) {
	s.placeMu.Lock()
	defer s.placeMu.Unlock()
	s.placeKey, s.placeSet = key, true
}

// placedFrom returns the placed assignments, as placedOn does, given their
// fingerprint fp when the caller has it.
func placedFrom(ctx context.Context, q querier, fp *[sha256.Size]byte) ([]Assignment, error) {
	c := cacheOf(q)
	if c != nil {
		if fp == nil {
			f, err := placedPrint(ctx, q)
			if err != nil {
				return nil, err
			}
			fp = &f
		}
		if c.placedOK && c.placedFP == *fp {
			return slices.Clone(c.placed), nil
		}
	}
	as, err := queryAssignments(ctx, q, `r.node_id IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	surges, err := querySurges(ctx, q, `1`)
	if err != nil {
		return nil, err
	}
	out := append(as, surges...)
	if c != nil {
		c.placed, c.placedFP, c.placedOK = slices.Clone(out), *fp, true
	}
	return out, nil
}
