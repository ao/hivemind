package auth

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTokenOrFail(t *testing.T) string {
	t.Helper()
	tok, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestWriteAdminTokenMode0600(t *testing.T) {
	dir := t.TempDir()
	tok := newTokenOrFail(t)
	if err := WriteAdminToken(dir, tok); err != nil {
		t.Fatalf("WriteAdminToken: %v", err)
	}
	fi, err := os.Stat(filepath.Join(dir, TokenFileName))
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Fatalf("admin.token mode %04o, want 0600", got)
	}
	got, err := ReadAdminToken(dir)
	if err != nil {
		t.Fatalf("ReadAdminToken: %v", err)
	}
	if got != tok {
		t.Fatalf("read back a different token")
	}
}

func TestWriteAdminTokenRefusesToOverwrite(t *testing.T) {
	dir := t.TempDir()
	first := newTokenOrFail(t)
	second := newTokenOrFail(t)
	if err := WriteAdminToken(dir, first); err != nil {
		t.Fatal(err)
	}
	err := WriteAdminToken(dir, second)
	if !errors.Is(err, ErrClusterExists) {
		t.Fatalf("second write error = %v, want ErrClusterExists", err)
	}
	got, err := ReadAdminToken(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != first {
		t.Fatal("refused write still changed the existing token")
	}
}

func TestWriteAdminTokenRefusesSymlinkInPlace(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "elsewhere")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, TokenFileName)); err != nil {
		t.Fatal(err)
	}
	if err := WriteAdminToken(dir, newTokenOrFail(t)); !errors.Is(err, ErrClusterExists) {
		t.Fatalf("write over a symlink: %v, want ErrClusterExists", err)
	}
	b, err := os.ReadFile(target)
	if err != nil || string(b) != "keep" {
		t.Fatalf("symlink target was changed: %q, %v", b, err)
	}
}

func TestWriteAdminTokenRejectsMalformedToken(t *testing.T) {
	dir := t.TempDir()
	if err := WriteAdminToken(dir, "not-a-token"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("error = %v, want ErrInvalidToken", err)
	}
	if _, err := os.Stat(filepath.Join(dir, TokenFileName)); !os.IsNotExist(err) {
		t.Fatalf("a malformed token left a file behind: %v", err)
	}
}

func TestReadAdminTokenRefusesLooseMode(t *testing.T) {
	for _, mode := range []os.FileMode{0o644, 0o640, 0o604, 0o620} {
		t.Run(mode.String(), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, TokenFileName)
			if err := os.WriteFile(path, []byte(newTokenOrFail(t)+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, mode); err != nil {
				t.Fatal(err)
			}
			_, err := ReadAdminToken(dir)
			if !errors.Is(err, ErrInsecureTokenFile) {
				t.Fatalf("mode %04o: error = %v, want ErrInsecureTokenFile", mode.Perm(), err)
			}
		})
	}
}

func TestReadAdminTokenAcceptsOwnerOnlyModes(t *testing.T) {
	dir := t.TempDir()
	tok := newTokenOrFail(t)
	path := filepath.Join(dir, TokenFileName)
	if err := os.WriteFile(path, []byte(tok), 0o400); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadAdminToken(dir); err != nil || got != tok {
		t.Fatalf("mode 0400: got %v, %v", got, err)
	}
}

func TestReadAdminTokenRefusesMissingAndMalformed(t *testing.T) {
	dir := t.TempDir()
	if _, err := ReadAdminToken(dir); err == nil {
		t.Fatal("missing token file: no error")
	}
	path := filepath.Join(dir, TokenFileName)
	if err := os.WriteFile(path, []byte("hm_truncated"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := ReadAdminToken(dir)
	if !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("malformed file: error = %v, want ErrInvalidToken", err)
	}
	if strings.Contains(err.Error(), "hm_truncated") {
		t.Fatal("error message echoes the token file content")
	}
}

func TestReadAdminTokenRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	tok := newTokenOrFail(t)
	target := filepath.Join(dir, "real")
	if err := os.WriteFile(target, []byte(tok), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, TokenFileName)); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadAdminToken(dir); err == nil {
		t.Fatal("symlinked token file was read")
	}
}
