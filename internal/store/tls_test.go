package store

import (
	"bytes"
	"errors"
	"io/fs"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTLSKeyNamespace(t *testing.T) {
	for _, k := range []string{
		"acme/acme-v02.api.letsencrypt.org-directory/users/ops@example.com/ops.key",
		"certificates/pebble-14000-dir/www.example.com/www.example.com.crt",
		"certificates/pebble-14000-dir/www.example.com/www.example.com.key.compromised",
		"acme/pebble-14000-dir/challenge_tokens/www.example.com.json",
		"ocsp/www.example.com-0123abcd",
		"last_clean.json",
		"rw_test_5577006791947779410",
	} {
		if err := ValidTLSKey(k); err != nil {
			t.Errorf("%q refused: %v", k, err)
		}
	}
	for _, k := range []string{
		"", "acme", "/acme/x", "acme/", "acme//x", "acme/../x", "acme/./x", "acme/x y", "acme/x\x00",
		"hivemind.db", "locks/issue_cert_x", "Certificates/x", "rw_test_", "rw_test_1/x", "last_clean.json/x",
		"acme/" + strings.Repeat("a", MaxTLSKeyLen),
	} {
		if err := ValidTLSKey(k); !errors.Is(err, ErrInvalid) {
			t.Errorf("%q accepted: %v", k, err)
		}
	}
}

func TestTLSStorageMethods(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	s := openT(t, filepath.Join(t.TempDir(), "data", "hivemind.db"), WithClock(clock.Now))
	ctx := ctxT(t)

	const site = "certificates/ca/www.example.com"
	crt, key := site+"/www.example.com.crt", site+"/www.example.com.key"
	if _, err := s.TLSLoad(ctx, crt); !errors.Is(err, fs.ErrNotExist) || !errors.Is(err, ErrNotFound) {
		t.Fatalf("load of a missing key: %v", err)
	}
	if ok, err := s.TLSExists(ctx, crt); ok || err != nil {
		t.Fatalf("exists of a missing key: %v %v", ok, err)
	}
	if _, err := s.TLSList(ctx, "certificates", false); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("list of a missing prefix: %v", err)
	}
	if _, err := s.TLSStat(ctx, crt); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("stat of a missing key: %v", err)
	}

	if err := s.TLSStore(ctx, crt, []byte("cert")); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Second)
	if err := s.TLSStore(ctx, key, []byte("private")); err != nil {
		t.Fatal(err)
	}
	if err := s.TLSStore(ctx, "acme/ca/users/a@example.com/a.json", []byte("{}")); err != nil {
		t.Fatal(err)
	}
	if v, err := s.TLSLoad(ctx, crt); err != nil || string(v) != "cert" {
		t.Fatalf("load: %q %v", v, err)
	}
	if err := s.TLSStore(ctx, crt, []byte("cert2")); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.TLSLoad(ctx, crt); string(v) != "cert2" {
		t.Fatalf("overwrite: %q", v)
	}
	if err := s.TLSStore(ctx, "certificates/ca/empty", nil); err != nil {
		t.Fatal(err)
	}
	if v, err := s.TLSLoad(ctx, "certificates/ca/empty"); err != nil || v == nil || len(v) != 0 {
		t.Fatalf("empty value: %v %v", v, err)
	}

	for _, k := range []string{crt, site, "certificates/ca", "certificates"} {
		if ok, err := s.TLSExists(ctx, k); !ok || err != nil {
			t.Errorf("exists %q: %v %v", k, ok, err)
		}
	}
	// A key that only shares a string prefix is not under it.
	if ok, _ := s.TLSExists(ctx, "certificates/ca/www.example"); ok {
		t.Error("exists matched a string prefix that is not a component")
	}

	info, err := s.TLSStat(ctx, crt)
	if err != nil || !info.Terminal || info.Size != 5 || info.Key != crt {
		t.Fatalf("stat of a value: %+v %v", info, err)
	}
	info, err = s.TLSStat(ctx, site)
	if err != nil || info.Terminal || !info.Modified.Equal(clock.Now()) {
		t.Fatalf("stat of a directory: %+v %v (now %v)", info, err, clock.Now())
	}

	got, err := s.TLSList(ctx, "certificates", false)
	if want := []string{"certificates/ca"}; err != nil || !slices.Equal(got, want) {
		t.Fatalf("list certificates: %v %v", got, err)
	}
	got, _ = s.TLSList(ctx, "certificates/ca", false)
	if want := []string{"certificates/ca/empty", site}; !slices.Equal(got, want) {
		t.Fatalf("list certificates/ca: %v", got)
	}
	got, _ = s.TLSList(ctx, "certificates", true)
	if want := []string{"certificates/ca", "certificates/ca/empty", site, crt, key}; !slices.Equal(got, want) {
		t.Fatalf("recursive list: %v, want %v", got, want)
	}
	got, err = s.TLSList(ctx, crt, false)
	if err != nil || len(got) != 0 {
		t.Fatalf("list of a value: %v %v", got, err)
	}

	if err := s.TLSDelete(ctx, site); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{crt, key, site} {
		if ok, _ := s.TLSExists(ctx, k); ok {
			t.Errorf("%q survived the delete of its directory", k)
		}
	}
	if ok, _ := s.TLSExists(ctx, "certificates/ca/empty"); !ok {
		t.Error("delete of a directory took a sibling")
	}
	if err := s.TLSDelete(ctx, site); err != nil {
		t.Fatalf("delete of a missing key: %v", err)
	}

	big := bytes.Repeat([]byte("x"), MaxTLSValueLen+1)
	if err := s.TLSStore(ctx, crt, big); !errors.Is(err, ErrInvalid) {
		t.Fatalf("value over the limit: %v", err)
	}
	for _, bad := range []string{"hivemind.db", "acme/../../x"} {
		if err := s.TLSStore(ctx, bad, nil); !errors.Is(err, ErrInvalid) {
			t.Errorf("store %q: %v", bad, err)
		}
		if _, err := s.TLSLoad(ctx, bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("load %q: %v", bad, err)
		}
		if err := s.TLSDelete(ctx, bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("delete %q: %v", bad, err)
		}
		if _, err := s.TLSList(ctx, bad, true); !errors.Is(err, ErrInvalid) {
			t.Errorf("list %q: %v", bad, err)
		}
	}
}

func TestTLSStorageKeyLimit(t *testing.T) {
	s := newT(t)
	ctx := ctxT(t)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := range MaxTLSKeys {
		if _, err := tx.ExecContext(ctx, `INSERT INTO tls_storage (key, value, modified) VALUES (?, x'', 0)`, "ocsp/k"+strconv.Itoa(i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := s.TLSStore(ctx, "ocsp/one-more", []byte("x")); !errors.Is(err, ErrConflict) {
		t.Fatalf("store past the key limit: %v", err)
	}
	// Replacing a key that exists is still allowed.
	if err := s.TLSStore(ctx, "ocsp/k0", []byte("x")); err != nil {
		t.Fatalf("overwrite at the limit: %v", err)
	}
}

func TestTLSLockContentionAndExpiry(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	var mu sync.Mutex
	now := func() time.Time { mu.Lock(); defer mu.Unlock(); return clock.Now() }
	advance := func(d time.Duration) { mu.Lock(); defer mu.Unlock(); clock.Advance(d) }
	s := openT(t, filepath.Join(t.TempDir(), "data", "hivemind.db"), WithClock(now))
	ctx := ctxT(t)
	const name = "issue_cert_www.example.com"

	ok, err := s.TLSTryLock(ctx, name, "node-a:1", 30*time.Second)
	if !ok || err != nil {
		t.Fatalf("first lock: %v %v", ok, err)
	}
	// A second holder cannot take it, nor can the first take it again.
	for _, owner := range []string{"node-b:1", "node-a:1"} {
		if ok, err := s.TLSTryLock(ctx, name, owner, 30*time.Second); ok || err != nil {
			t.Fatalf("%s took a held lock: %v %v", owner, ok, err)
		}
	}
	// Another name is free.
	if ok, _ := s.TLSTryLock(ctx, "issue_cert_api.example.com", "node-b:1", 30*time.Second); !ok {
		t.Fatal("a different lock name was refused")
	}
	// Only the holder unlocks or renews.
	if err := s.TLSUnlock(ctx, name, "node-b:1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unlock by another owner: %v", err)
	}
	if err := s.TLSRenewLock(ctx, name, "node-b:1", time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatalf("renew by another owner: %v", err)
	}

	// A holder that renews keeps the lock past its first expiry.
	advance(20 * time.Second)
	if err := s.TLSRenewLock(ctx, name, "node-a:1", 30*time.Second); err != nil {
		t.Fatal(err)
	}
	advance(20 * time.Second)
	if ok, _ := s.TLSTryLock(ctx, name, "node-b:1", 30*time.Second); ok {
		t.Fatal("took a lock that was renewed")
	}
	// One that stops renewing loses it once it expires: the lock is stale.
	advance(11 * time.Second)
	if ok, err := s.TLSTryLock(ctx, name, "node-b:1", 30*time.Second); !ok || err != nil {
		t.Fatalf("stale lock not taken: %v %v", ok, err)
	}
	if err := s.TLSRenewLock(ctx, name, "node-a:1", 30*time.Second); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the old holder renewed a lock it lost: %v", err)
	}
	if err := s.TLSUnlock(ctx, name, "node-a:1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the old holder unlocked the new holder's lock: %v", err)
	}
	if err := s.TLSUnlock(ctx, name, "node-b:1"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.TLSTryLock(ctx, name, "node-a:2", 30*time.Second); !ok {
		t.Fatal("an unlocked lock was refused")
	}

	for _, c := range []struct {
		name, owner string
		ttl         time.Duration
	}{
		{"", "o", time.Second}, {"x\n", "o", time.Second}, {strings.Repeat("x", MaxTLSLockNameLen+1), "o", time.Second},
		{"x", "", time.Second}, {"x", "has space", time.Second}, {"x", "o", 0}, {"x", "o", 2 * time.Hour},
	} {
		if _, err := s.TLSTryLock(ctx, c.name, c.owner, c.ttl); !errors.Is(err, ErrInvalid) {
			t.Errorf("lock %q %q %s: %v", c.name, c.owner, c.ttl, err)
		}
	}
}

// TestTLSLockOneHolderAtATime races many holders for one lock on the real
// clock: at most one holds it at any moment, and every one gets it.
func TestTLSLockOneHolderAtATime(t *testing.T) {
	s := newT(t)
	ctx := ctxT(t)
	var mu sync.Mutex
	holding, most, done := 0, 0, 0
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func(owner string) {
			defer wg.Done()
			for {
				ok, err := s.TLSTryLock(ctx, "issue_cert_x", owner, time.Minute)
				if err != nil {
					t.Error(err)
					return
				}
				if ok {
					break
				}
				time.Sleep(time.Millisecond)
			}
			mu.Lock()
			holding++
			most = max(most, holding)
			mu.Unlock()
			time.Sleep(2 * time.Millisecond)
			mu.Lock()
			holding--
			done++
			mu.Unlock()
			if err := s.TLSUnlock(ctx, "issue_cert_x", owner); err != nil {
				t.Error(err)
			}
		}("owner-" + strconv.Itoa(i))
	}
	wg.Wait()
	if most != 1 || done != 8 {
		t.Fatalf("at most %d holders at once, %d of 8 done", most, done)
	}
}
