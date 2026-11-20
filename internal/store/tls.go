package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"slices"
	"strings"
	"time"
)

// TLS storage.
//
// The routers on every node share their certificates, private keys, ACME
// account and challenge tokens through these tables, as certmagic's Storage
// ("Processes running in a cluster should use the same Storage value (with
// the same configuration) in order to share certificates and other TLS
// resources with the cluster", certmagic storage.go). Keys have certmagic's
// file-system semantics: components separated by "/", where a "directory"
// is a key that prefixes others and holds no value itself.
//
// Locks are certmagic's Locker: a named mutex across the cluster, used around
// issuing a certificate and registering an ACME account. A lock is a row with
// an owner and an expiry. Taking it is one transaction that deletes the row
// if it has expired and inserts a new one, so two nodes can never both hold
// it. A holder keeps the expiry ahead of the clock while it works; one that
// dies leaves a row that expires, and is then stale and may be taken, as
// certmagic's FileStorage treats a lock file whose timestamp is too old.

// Limits on TLS storage. certmagic stores a few kilobytes per key.
const (
	MaxTLSKeyLen      = 512
	MaxTLSValueLen    = 128 << 10
	MaxTLSKeys        = 10000
	MaxTLSLockNameLen = 512
	maxTLSOwnerLen    = 128
	maxTLSLockTTL     = time.Hour
)

var (
	tlsComponentRE = regexp.MustCompile(`^[A-Za-z0-9_@.-]+$`)
	tlsTestKeyRE   = regexp.MustCompile(`^rw_test_[0-9]{1,20}$`)
)

// tlsRoots are the first key components certmagic writes under: ACME
// accounts and challenge tokens, certificates and their keys and metadata,
// and OCSP staples.
var tlsRoots = []string{"acme", "certificates", "ocsp"}

// ValidTLSKey checks that key is in certmagic's namespace: at most
// MaxTLSKeyLen bytes, made of non-empty components of letters, digits and
// _ @ . -, none of them "." or "..", starting with acme/, certificates/ or
// ocsp/. Two single keys are allowed as well: last_clean.json, which records
// certmagic's last storage clean, and rw_test_<digits>, which it writes and
// deletes to check the storage before it issues.
func ValidTLSKey(key string) error {
	if key == "" || len(key) > MaxTLSKeyLen {
		return fmt.Errorf("%w: TLS storage key must be 1 to %d bytes", ErrInvalid, MaxTLSKeyLen)
	}
	if key == "last_clean.json" || tlsTestKeyRE.MatchString(key) {
		return nil
	}
	parts := strings.Split(key, "/")
	for _, p := range parts {
		if p == "." || p == ".." || !tlsComponentRE.MatchString(p) {
			return fmt.Errorf("%w: TLS storage key %q: each component is letters, digits and _ @ . - and is not a lone dot or two dots", ErrInvalid, truncKey(key))
		}
	}
	if len(parts) < 2 || !slices.Contains(tlsRoots, parts[0]) {
		return fmt.Errorf("%w: TLS storage key %q is outside certmagic's namespace, which is acme, certificates and ocsp", ErrInvalid, truncKey(key))
	}
	return nil
}

// ValidTLSPrefix checks the key of a List, Delete, Exists or Stat: a valid
// key, or a root component on its own, such as certificates.
func ValidTLSPrefix(prefix string) error { return validTLSPrefix(prefix) }

func validTLSPrefix(prefix string) error {
	if slices.Contains(tlsRoots, prefix) {
		return nil
	}
	return ValidTLSKey(prefix)
}

// ValidTLSLockName checks a lock name. certmagic's names are an operation and
// a name, such as issue_cert_www.example.com; any printable text up to
// MaxTLSLockNameLen bytes is allowed.
func ValidTLSLockName(name string) error {
	if name == "" || len(name) > MaxTLSLockNameLen || strings.IndexFunc(name, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		return fmt.Errorf("%w: TLS lock name must be 1 to %d printable bytes", ErrInvalid, MaxTLSLockNameLen)
	}
	return nil
}

func validTLSOwner(owner string) error {
	if owner == "" || len(owner) > maxTLSOwnerLen || strings.IndexFunc(owner, func(r rune) bool { return r < 0x21 || r == 0x7f }) >= 0 {
		return fmt.Errorf("%w: TLS lock owner must be 1 to %d printable bytes without spaces", ErrInvalid, maxTLSOwnerLen)
	}
	return nil
}

func truncKey(k string) string {
	if len(k) > 64 {
		return k[:64] + "..."
	}
	return k
}

// errTLSNotExist is wrapped by every TLS storage error for a missing key, so
// it matches both ErrNotFound and fs.ErrNotExist, which certmagic checks.
func errTLSNotExist(key string) error {
	return fmt.Errorf("TLS storage key %q: %w (%w)", truncKey(key), ErrNotFound, fs.ErrNotExist)
}

// TLSKeyInfo describes one key.
type TLSKeyInfo struct {
	Key      string
	Modified time.Time
	Size     int64
	// Terminal is false for a directory: a key that only prefixes others.
	Terminal bool
}

// TLSStore puts value at key, replacing what was there. The table holds at
// most MaxTLSKeys keys.
func (s *Store) TLSStore(ctx context.Context, key string, value []byte) error {
	if err := ValidTLSKey(key); err != nil {
		return err
	}
	if len(value) > MaxTLSValueLen {
		return fmt.Errorf("%w: TLS storage value is %d bytes, more than %d", ErrInvalid, len(value), MaxTLSValueLen)
	}
	if value == nil {
		value = []byte{}
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM tls_storage WHERE key != ?`, key).Scan(&n); err != nil {
			return err
		}
		if n >= MaxTLSKeys {
			return fmt.Errorf("%w: TLS storage holds %d keys, the most it takes", ErrConflict, MaxTLSKeys)
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO tls_storage (key, value, modified) VALUES (?, ?, ?)
			ON CONFLICT (key) DO UPDATE SET value = excluded.value, modified = excluded.modified`, key, value, s.now().UnixNano())
		return err
	})
}

// TLSLoad returns the value at key. A missing key is an error that matches
// ErrNotFound and fs.ErrNotExist.
func (s *Store) TLSLoad(ctx context.Context, key string) ([]byte, error) {
	if err := ValidTLSKey(key); err != nil {
		return nil, err
	}
	var v []byte
	err := s.rdb.QueryRowContext(ctx, `SELECT value FROM tls_storage WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errTLSNotExist(key)
	}
	if err == nil && v == nil {
		v = []byte{} // SQLite gives an empty blob back as nil
	}
	return v, err
}

// TLSDelete deletes key and every key under it. A key that does not exist is
// not an error.
func (s *Store) TLSDelete(ctx context.Context, key string) error {
	if err := validTLSPrefix(key); err != nil {
		return err
	}
	lo, hi := descendants(key)
	// A transaction of its own, as every write is, so that it is replicated
	// with the rest when the cluster has several servers (docs/ha.md). The
	// TLS storage is not in the routing table.
	return s.tx(ctx, func(tx *sql.Tx) error {
		keepsRoutes(tx)
		_, err := tx.ExecContext(ctx, `DELETE FROM tls_storage WHERE key = ? OR (key >= ? AND key < ?)`, key, lo, hi)
		return err
	})
}

// TLSExists reports whether key holds a value or prefixes other keys.
func (s *Store) TLSExists(ctx context.Context, key string) (bool, error) {
	if err := validTLSPrefix(key); err != nil {
		return false, err
	}
	lo, hi := descendants(key)
	var n int
	err := s.rdb.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM tls_storage WHERE key = ? OR (key >= ? AND key < ?))`, key, lo, hi).Scan(&n)
	return n == 1, err
}

// TLSStat describes key: a value, or a directory whose modified time is the
// latest of the keys under it.
func (s *Store) TLSStat(ctx context.Context, key string) (TLSKeyInfo, error) {
	if err := validTLSPrefix(key); err != nil {
		return TLSKeyInfo{}, err
	}
	var size, mod int64
	err := s.rdb.QueryRowContext(ctx, `SELECT length(value), modified FROM tls_storage WHERE key = ?`, key).Scan(&size, &mod)
	if err == nil {
		return TLSKeyInfo{Key: key, Modified: time.Unix(0, mod), Size: size, Terminal: true}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return TLSKeyInfo{}, err
	}
	lo, hi := descendants(key)
	var latest sql.NullInt64
	if err := s.rdb.QueryRowContext(ctx, `SELECT max(modified) FROM tls_storage WHERE key >= ? AND key < ?`, lo, hi).Scan(&latest); err != nil {
		return TLSKeyInfo{}, err
	}
	if !latest.Valid {
		return TLSKeyInfo{}, errTLSNotExist(key)
	}
	return TLSKeyInfo{Key: key, Modified: time.Unix(0, latest.Int64)}, nil
}

// TLSList returns the keys under prefix, sorted, as full keys. Without
// recursive it returns the immediate children, values and directories; with
// it, every key and directory below prefix. This is what certmagic's
// FileStorage returns from a walk of the directory. A prefix that is a value
// has no children; one that does not exist is an error that matches
// ErrNotFound and fs.ErrNotExist.
func (s *Store) TLSList(ctx context.Context, prefix string, recursive bool) ([]string, error) {
	if err := validTLSPrefix(prefix); err != nil {
		return nil, err
	}
	lo, hi := descendants(prefix)
	rows, err := s.rdb.QueryContext(ctx, `SELECT key FROM tls_storage WHERE key >= ? AND key < ? ORDER BY key`, lo, hi)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := map[string]bool{}
	var out []string
	add := func(k string) {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		rest := strings.TrimPrefix(k, prefix+"/")
		parts := strings.Split(rest, "/")
		if !recursive {
			add(prefix + "/" + parts[0])
			continue
		}
		for i := range parts {
			add(prefix + "/" + strings.Join(parts[:i+1], "/"))
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		var n int
		if err := s.rdb.QueryRowContext(ctx, `SELECT count(*) FROM tls_storage WHERE key = ?`, prefix).Scan(&n); err != nil {
			return nil, err
		}
		if n == 0 {
			return nil, errTLSNotExist(prefix)
		}
		return []string{}, nil
	}
	slices.Sort(out)
	return out, nil
}

// descendants returns the key range of everything under key: from key + "/"
// up to, not including, key + "0", the byte after "/".
func descendants(key string) (string, string) { return key + "/", key + "0" }

// MaxTLSLocksPerHolder is the most locks one holder may have at once. The
// holder is the part of the owner before its first "/", which the node API
// sets to the node's ID; certmagic takes a lock or two at a time.
const MaxTLSLocksPerHolder = 1000

// ErrTLSLockLimit is wrapped, with ErrConflict, by TLSTryLock's error for a
// holder that has MaxTLSLocksPerHolder locks.
var ErrTLSLockLimit = errors.New("too many TLS locks")

// tlsHolder returns the holder of owner: the part before its first "/", or
// all of it.
func tlsHolder(owner string) string {
	h, _, _ := strings.Cut(owner, "/")
	return h
}

// TLSTryLock takes the lock name for owner until ttl from now, if no one
// holds it or its holder's expiry has passed. It reports whether it took it.
// The check and the take are one transaction. Every expired lock is deleted
// first, whatever its name, so that names used once do not pile up; a
// holder that has MaxTLSLocksPerHolder locks gets an error wrapping
// ErrConflict and ErrTLSLockLimit.
func (s *Store) TLSTryLock(ctx context.Context, name, owner string, ttl time.Duration) (bool, error) {
	if err := ValidTLSLockName(name); err != nil {
		return false, err
	}
	if err := validTLSOwner(owner); err != nil {
		return false, err
	}
	if ttl <= 0 || ttl > maxTLSLockTTL {
		return false, fmt.Errorf("%w: lock lifetime %s is not in (0, %s]", ErrInvalid, ttl, maxTLSLockTTL)
	}
	took := false
	err := s.tx(ctx, func(tx *sql.Tx) error {
		now := s.now().UnixNano()
		if _, err := tx.ExecContext(ctx, `DELETE FROM tls_locks WHERE expires <= ?`, now); err != nil {
			return err
		}
		var held int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM tls_locks WHERE name = ?`, name).Scan(&held); err != nil {
			return err
		}
		if held > 0 {
			return nil
		}
		holder := tlsHolder(owner)
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM tls_locks WHERE owner = ? OR (owner >= ? AND owner < ?)`,
			holder, holder+"/", holder+"0").Scan(&held); err != nil {
			return err
		}
		if held >= MaxTLSLocksPerHolder {
			return fmt.Errorf("%w: %w: %s holds %d TLS locks, the most one may hold; it must release one first",
				ErrConflict, ErrTLSLockLimit, holder, held)
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO tls_locks (name, owner, expires) VALUES (?, ?, ?) ON CONFLICT (name) DO NOTHING`,
			name, owner, now+int64(ttl))
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		took = n == 1
		return nil
	})
	return took && err == nil, err
}

// TLSRenewLock moves the expiry of a lock that owner holds to ttl from now.
// A lock that owner does not hold, because it expired and was taken or was
// never held, is ErrNotFound.
func (s *Store) TLSRenewLock(ctx context.Context, name, owner string, ttl time.Duration) error {
	if err := ValidTLSLockName(name); err != nil {
		return err
	}
	if ttl <= 0 || ttl > maxTLSLockTTL {
		return fmt.Errorf("%w: lock lifetime %s is not in (0, %s]", ErrInvalid, ttl, maxTLSLockTTL)
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		keepsRoutes(tx)
		now := s.now().UnixNano()
		res, err := tx.ExecContext(ctx, `UPDATE tls_locks SET expires = ? WHERE name = ? AND owner = ? AND expires > ?`,
			now+int64(ttl), name, owner, now)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("TLS lock %q is not held by this owner: %w", name, ErrNotFound)
		}
		return nil
	})
}

// TLSUnlock releases a lock that owner holds. A lock held by someone else is
// left alone, and the call is ErrNotFound.
func (s *Store) TLSUnlock(ctx context.Context, name, owner string) error {
	if err := ValidTLSLockName(name); err != nil {
		return err
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		keepsRoutes(tx)
		res, err := tx.ExecContext(ctx, `DELETE FROM tls_locks WHERE name = ? AND owner = ?`, name, owner)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("TLS lock %q is not held by this owner: %w", name, ErrNotFound)
		}
		return nil
	})
}
