package auth

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// TokenFileName is the name of the admin token file inside the data directory.
const TokenFileName = "admin.token"

// ErrClusterExists means the data directory already holds an admin token, so
// a new one must not be written over it.
var ErrClusterExists = errors.New("auth: a cluster already exists in this data directory (admin.token is present); refusing to overwrite it")

// ErrInsecureTokenFile means the token file is readable by its group or by
// other users.
var ErrInsecureTokenFile = errors.New("auth: admin token file is accessible to group or others; expected mode 0600")

// WriteAdminToken writes token to <dir>/admin.token with mode 0600. The file
// is created with O_EXCL, so an existing token is never replaced. If a token
// file already exists, the error wraps ErrClusterExists and the file is left
// as it was. A failed write removes the partial file.
func WriteAdminToken(dir, token string) error {
	if err := ParseToken(token); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("auth: create data directory: %w", err)
	}
	path := filepath.Join(dir, TokenFileName)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%w: %s", ErrClusterExists, path)
		}
		return fmt.Errorf("auth: create admin token file: %w", err)
	}
	if _, err := io.WriteString(f, token+"\n"); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return fmt.Errorf("auth: write admin token file: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return fmt.Errorf("auth: sync admin token file: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("auth: close admin token file: %w", err)
	}
	return nil
}

// ReadAdminToken reads and validates <dir>/admin.token. It refuses a path that
// is a symlink or is not a regular file, a file readable by group or others,
// and content that is not a well-formed token. The mode is checked on the
// open file descriptor, not on the path.
func ReadAdminToken(dir string) (string, error) {
	path := filepath.Join(dir, TokenFileName)
	li, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("auth: read admin token: %w", err)
	}
	if !li.Mode().IsRegular() {
		return "", fmt.Errorf("auth: %s is not a regular file", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("auth: open admin token file: %w", err)
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("auth: stat admin token file: %w", err)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return "", fmt.Errorf("%w: %s has mode %04o", ErrInsecureTokenFile, path, perm)
	}
	// A token is 46 bytes; the limit stops a huge file from being read whole.
	b, err := io.ReadAll(io.LimitReader(f, 256))
	if err != nil {
		return "", fmt.Errorf("auth: read admin token file: %w", err)
	}
	token := strings.TrimSpace(string(b))
	if err := ParseToken(token); err != nil {
		return "", fmt.Errorf("auth: %s: %w", path, err)
	}
	return token, nil
}
