package router

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/caddyserver/certmagic"

	"github.com/ao/hivemind/internal/store"
	"github.com/ao/hivemind/internal/tlsstore"
)

// existsFails is the shared storage of a worker whose checks of a key fail
// while its other calls go through, as when the server answers again in
// the middle of a renewal.
type existsFails struct{ tlsstore.Backend }

func (existsFails) Exists(context.Context, string) (bool, error) {
	return false, errors.New(`calling the server: Post "https://10.0.0.1:4484/node/v1/tls/exists": dial tcp 10.0.0.1:4484: i/o timeout`)
}

// A renewal check whose storage cannot say whether the certificate's files
// exist asks the CA for nothing when the stored certificate, renewed by
// another router, is not due. In the sixth soak, the workers' routers, cut
// off from the server, read the failed checks as missing files, logged
// "certificate resources not found in storage for any configured issuer",
// and went down certmagic's obtain path, which orders a new certificate
// once the storage answers again, though it holds one.
func TestRenewalWithFailingChecksOrdersNothing(t *testing.T) {
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "hivemind.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts, err := tlsstore.New(existsFails{tlsstore.StoreBackend{St: st, NodeID: "node-a"}}, tlsstore.Options{Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	const host = "www.example.com"
	routes := func(context.Context) (Table, error) { return Table{host: {}}, nil }
	rt, err := New(Config{Addr: "127.0.0.1", Routes: routes, Storage: ts, Email: "ops@example.com",
		CA: "https://acme.invalid/dir", Logger: log, Metrics: NewMetrics(), Poll: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Shutdown() })
	iss := newTestIssuer(t)
	rt.magic.Issuers = []certmagic.Issuer{iss}
	now := time.Now()
	iss.store(t, ts, host, now.Add(-time.Minute), now.Add(24*time.Hour))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := rt.magic.RenewCertSync(ctx, host, false); err != nil {
		t.Fatalf("renewal: %v", err)
	}
	if got := iss.names(); len(got) != 0 {
		t.Fatalf("the CA was asked for a certificate for %v, which the storage holds", got)
	}
}
