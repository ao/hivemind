package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ao/hivemind/internal/spec"
)

// Limits on registry credentials. A password holds tokens as well as
// passwords: an ECR token from aws ecr get-login-password is a few
// kilobytes.
const (
	MaxRegistryUsernameLen = 256
	MaxRegistryPasswordLen = 16 << 10
	MaxRegistries          = 100
)

// Registry is a stored registry credential, without its password. Nothing
// that lists credentials returns a password.
type Registry struct {
	// Host is the registry, in the form spec.ParseRegistryHost gives.
	Host      string
	Username  string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// RegistryAuth is the credential a pull of an image from Host sends.
type RegistryAuth struct {
	Host     string
	Username string
	Password string
}

// ValidateRegistryCredential checks a username and password: each is
// non-empty, valid UTF-8 without control characters, and within its
// limit, and the username has no colon, which HTTP basic authentication
// cannot carry (RFC 7617, section 2). Errors never contain either value.
func ValidateRegistryCredential(username, password string) error {
	switch {
	case username == "":
		return fmt.Errorf("%w: username is required", ErrInvalid)
	case len(username) > MaxRegistryUsernameLen:
		return fmt.Errorf("%w: username is longer than %d bytes", ErrInvalid, MaxRegistryUsernameLen)
	case !printable(username):
		return fmt.Errorf("%w: username must be UTF-8 text without control characters", ErrInvalid)
	case strings.Contains(username, ":"):
		return fmt.Errorf("%w: username must not contain a colon", ErrInvalid)
	case password == "":
		return fmt.Errorf("%w: password is required", ErrInvalid)
	case len(password) > MaxRegistryPasswordLen:
		return fmt.Errorf("%w: password is longer than %d bytes", ErrInvalid, MaxRegistryPasswordLen)
	case !printable(password):
		return fmt.Errorf("%w: password must be UTF-8 text without control characters, such as a newline", ErrInvalid)
	}
	return nil
}

func printable(s string) bool {
	return utf8.ValidString(s) && strings.IndexFunc(s, unicode.IsControl) < 0
}

// registryHost checks a credential's host and returns its canonical form.
func registryHost(host string) (string, error) {
	h, err := spec.ParseRegistryHost(host)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return h, nil
}

// PutRegistry stores the credential for a registry, or replaces the one
// stored. Docker Hub's names (docker.io, index.docker.io and
// registry-1.docker.io) are one host. A new host past MaxRegistries is
// ErrConflict. The change reaches the next pull only: running containers
// are not touched.
func (s *Store) PutRegistry(ctx context.Context, host, username, password string) (Registry, error) {
	h, err := registryHost(host)
	if err != nil {
		return Registry{}, err
	}
	if err := ValidateRegistryCredential(username, password); err != nil {
		return Registry{}, err
	}
	var out Registry
	err = s.tx(ctx, func(tx *sql.Tx) error {
		now := s.now().UnixNano()
		var created int64
		err := tx.QueryRowContext(ctx, `SELECT created_at FROM registries WHERE host = ?`, h).Scan(&created)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM registries`).Scan(&n); err != nil {
				return err
			}
			if n >= MaxRegistries {
				return fmt.Errorf("%w: %d registries are stored, the most there can be; log out of one first", ErrConflict, n)
			}
			created = now
			_, err = tx.ExecContext(ctx, `INSERT INTO registries (host, username, password, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?)`, h, username, password, created, now)
		case err != nil:
			return err
		default:
			_, err = tx.ExecContext(ctx, `UPDATE registries SET username = ?, password = ?, updated_at = ? WHERE host = ?`,
				username, password, now, h)
		}
		if err != nil {
			return err
		}
		out = Registry{Host: h, Username: username, CreatedAt: time.Unix(0, created), UpdatedAt: time.Unix(0, now)}
		return nil
	})
	return out, err
}

// ListRegistries returns every stored credential by host, without the
// passwords.
func (s *Store) ListRegistries(ctx context.Context) ([]Registry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT host, username, created_at, updated_at FROM registries ORDER BY host`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Registry
	for rows.Next() {
		var r Registry
		var created, updated int64
		if err := rows.Scan(&r.Host, &r.Username, &created, &updated); err != nil {
			return nil, err
		}
		r.CreatedAt, r.UpdatedAt = time.Unix(0, created), time.Unix(0, updated)
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeleteRegistry removes the credential for a registry. A host with none
// is ErrNotFound. Containers that run an image from it keep running; the
// next pull from it sends no credential.
func (s *Store) DeleteRegistry(ctx context.Context, host string) error {
	h, err := registryHost(host)
	if err != nil {
		return err
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM registries WHERE host = ?`, h)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			return fmt.Errorf("%w: no credential is stored for registry %s", ErrNotFound, h)
		}
		return nil
	})
}

// attachRegistryAuth sets Auth on each assignment that is to run an image
// from a registry with a stored credential. Replicas marked for removal pull
// nothing, and get none.
func attachRegistryAuth(ctx context.Context, q querier, as []Assignment) error {
	need := false
	for _, a := range as {
		if a.Desired == Running {
			need = true
			break
		}
	}
	if !need {
		return nil
	}
	rows, err := q.QueryContext(ctx, `SELECT host, username, password FROM registries`)
	if err != nil {
		return err
	}
	defer rows.Close()
	creds := map[string]RegistryAuth{}
	for rows.Next() {
		var c RegistryAuth
		if err := rows.Scan(&c.Host, &c.Username, &c.Password); err != nil {
			return err
		}
		creds[c.Host] = c
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(creds) == 0 {
		return nil
	}
	for i := range as {
		if as[i].Desired != Running {
			continue
		}
		host, err := spec.RegistryHost(as[i].Spec.Image)
		if err != nil {
			// Stored specs are validated, so their images parse.
			continue
		}
		if c, ok := creds[host]; ok {
			as[i].Auth = &c
		}
	}
	return nil
}
