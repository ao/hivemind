package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/auth"
	"github.com/ao/hivemind/internal/compose"
	"github.com/ao/hivemind/internal/server"
	"github.com/ao/hivemind/internal/spec"
	"github.com/ao/hivemind/internal/store"
)

const testCompose = `services:
  web:
    image: nginx:alpine
    ports: ["8080:80", "0.0.0.0:8443:443"]
    environment:
      GREETING: hello ${WHO}
    deploy: {replicas: 2}
  db:
    image: postgres:16
    environment: ["POSTGRES_PASSWORD=${DB_PASSWORD}"]
    volumes: [pgdata:/var/lib/postgresql/data]
volumes:
  pgdata:
`

const testDotEnv = "WHO=world\nDB_PASSWORD=from-dotenv\n"

const testConverted = `name: db
image: postgres:16
replicas: 1
env:
  POSTGRES_PASSWORD: from-dotenv
volumes:
  - name: pgdata
    path: /var/lib/postgresql/data
---
name: web
image: nginx:alpine
replicas: 2
env:
  GREETING: hello everyone
ports:
  - bind: 127.0.0.1
    host: 8080
    container: 80
    protocol: tcp
  - bind: 0.0.0.0
    host: 8443
    container: 443
    protocol: tcp
`

const testNote = `note: services.web.ports[0]: published on 127.0.0.1:8080 only, the hivemind default; Compose would publish it on every interface. To publish elsewhere, write "0.0.0.0:8080:80" or another address` + "\n"

// writeCompose writes a compose file, and a .env beside it unless dotenv is
// empty, into a new directory, and returns the file's path.
func writeCompose(t *testing.T, file, dotenv string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "compose.yaml")
	if err := os.WriteFile(path, []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}
	if dotenv != "" {
		if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(dotenv), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func TestComposeConvert(t *testing.T) {
	isolate(t)
	t.Setenv("WHO", "everyone") // the environment wins over .env
	path := writeCompose(t, testCompose, testDotEnv)

	out, errOut, err := run(t, "", "compose", "convert", path)
	if err != nil {
		t.Fatal(err)
	}
	if out != testConverted {
		t.Errorf("stdout:\n%s\nwant:\n%s", out, testConverted)
	}
	if errOut != testNote {
		t.Errorf("stderr:\n%q\nwant:\n%q", errOut, testNote)
	}

	out, _, err = run(t, "", "compose", "convert", "--prefix", "shop", path)
	if err != nil || !strings.Contains(out, "name: shop-db\n") || !strings.Contains(out, "name: shop-web\n") {
		t.Fatalf("--prefix: %v\n%s", err, out)
	}

	// - reads standard input, and .env from the working directory.
	t.Chdir(filepath.Dir(path))
	if out, _, err = run(t, testCompose, "compose", "convert", "-"); err != nil || out != testConverted {
		t.Fatalf("convert -: %v\n%s", err, out)
	}
}

func TestComposeConvertErrors(t *testing.T) {
	isolate(t)
	for _, k := range []string{"WHO", "DB_PASSWORD"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	dir := t.TempDir()
	refused := writeCompose(t, "services:\n  web: {image: nginx, depends_on: [db], build: .}\n", "")
	unset := writeCompose(t, testCompose, "")
	secret := writeCompose(t, "services:\n  web:\n    image: nginx\n    environment: {PASSWORD: s3cr3t-VALUE, EMPTY: }\n", "")
	large := filepath.Join(dir, "large.yaml")
	if err := os.WriteFile(large, make([]byte, maxSpecFile+1), 0o600); err != nil {
		t.Fatal(err)
	}
	badEnv := writeCompose(t, testCompose, "WHO=world\nDB_PASSWORD='s3cr3t-VALUE\n")
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"compose", "convert"}, "accepts 1 arg"},
		{[]string{"compose", "convert", filepath.Join(dir, "missing.yaml")}, "no such file"},
		{[]string{"compose", "convert", refused}, refused + ": 2 problems:\n  services.web.build: not supported: build and push the image, then name it in image\n  services.web.depends_on: not supported: hivemind starts each app on its own"},
		{[]string{"compose", "convert", unset}, unset + ": 2 problems:\n  services.db.environment[0]: variable DB_PASSWORD is not set"},
		{[]string{"compose", "convert", secret}, "services.web.environment.EMPTY: give a value"},
		{[]string{"compose", "convert", large}, "large.yaml is larger than 1048576 bytes"},
		{[]string{"compose", "convert", badEnv}, ".env line 2: the single quote is not closed"},
		{[]string{"compose", "convert", "--prefix", "Shop", unset}, `services.db: the app name "Shop-db" is not valid`},
		{[]string{"compose", "deploy", refused, "--token", "x"}, "services.web.depends_on: not supported"},
		{[]string{"compose", "delete", large, "--token", "x"}, "is larger than"},
	} {
		_, _, err := run(t, "", c.args...)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: %v, want %q", c.args, err, c.want)
		}
		if err != nil && strings.Contains(err.Error(), "s3cr3t") {
			t.Errorf("%v: the error holds an environment value: %v", c.args, err)
		}
	}
}

// The security review's file: ${HIVEMIND_TOKEN} in environment copied the
// admin token into the container and into what convert printed. It is
// refused, by convert and by deploy before anything is sent, and the token
// is in no output.
func TestComposeRefusesHivemindVariables(t *testing.T) {
	isolate(t)
	const token = "hm_admin-token-from-the-environment"
	t.Setenv(envToken, token)
	t.Setenv("HARMLESS", "x")
	path := writeCompose(t, "services:\n  web:\n    image: nginx\n    environment:\n      HARMLESS_LOOKING: ${HIVEMIND_TOKEN}\n", "")
	want := "services.web.environment.HARMLESS_LOOKING: variable HIVEMIND_TOKEN: a compose file cannot use the variables that start with HIVEMIND_"
	for _, args := range [][]string{{"compose", "convert", path}, {"compose", "deploy", path, "--server", "http://127.0.0.1:1"}} {
		out, errOut, err := run(t, "", args...)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%v: %v, want %q", args, err, want)
		}
		if strings.Contains(out+errOut+fmt.Sprint(err), token) {
			t.Errorf("%v: the output holds the token:\n%s%s%v", args, out, errOut, err)
		}
	}
	dotenv := writeCompose(t, "services:\n  web: {image: nginx, environment: {A: $A}}\n", "HIVEMIND_SERVER=https://other:4483\nA=1\n")
	if _, _, err := run(t, "", "compose", "convert", dotenv); err == nil || !strings.Contains(err.Error(), ".env line 1: HIVEMIND_SERVER: .env cannot set") {
		t.Errorf(".env with HIVEMIND_SERVER: %v", err)
	}
}

func TestComposeMaxFileIsDeploys(t *testing.T) {
	if compose.MaxFileBytes != maxSpecFile {
		t.Fatalf("compose.MaxFileBytes %d, deploy -f limit %d", compose.MaxFileBytes, maxSpecFile)
	}
}

// composeAPI serves the real API handler, with a fake runtime, and points
// the client at it.
func composeAPI(t *testing.T) *store.Store {
	t.Helper()
	return composeAPIWith(t, nil)
}

// composeAPIWith is composeAPI with the handler passed through wrap, when
// it is set.
func composeAPIWith(t *testing.T, wrap func(*store.Store, http.Handler) http.Handler) *store.Store {
	t.Helper()
	home := isolate(t)
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(home, "d", storeFileName), store.WithMinReady(0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.UpsertNode(ctx, store.Node{ID: "n1", Name: "n1"}); err != nil {
		t.Fatal(err)
	}
	tok, _ := auth.NewToken()
	sess, _ := auth.NewSessions(time.Hour)
	authn, _ := auth.NewAuthenticator(auth.HashToken(tok), sess)
	api, err := server.New(server.Config{Store: st, Runtime: fakeRuntime{}, NodeID: "n1", Auth: authn,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	h := api.Handler()
	if wrap != nil {
		h = wrap(st, h)
	}
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	t.Setenv(envServer, ts.URL)
	t.Setenv(envToken, tok)
	return st
}

func TestComposeDeployAndDelete(t *testing.T) {
	st := composeAPI(t)
	ctx := context.Background()
	t.Setenv("WHO", "everyone")
	path := writeCompose(t, testCompose, testDotEnv)

	out, errOut, err := run(t, "", "compose", "deploy", path)
	if err != nil {
		t.Fatal(err)
	}
	if want := "db: created, generation 1, 1 replica\nweb: created, generation 1, 2 replicas\n"; out != want {
		t.Fatalf("deploy:\n%s\nwant:\n%s", out, want)
	}
	// The one node takes one replica of web: the second one's host ports are
	// the first one's.
	if want := testNote + "web: warning: replica 1 has no node: host port 0.0.0.0:8443/tcp is in use on every eligible node; " +
		"held by another replica of web\n"; errOut != want {
		t.Errorf("deploy stderr:\n%q\nwant:\n%q", errOut, want)
	}
	web, err := st.GetApp(ctx, "web")
	if err != nil {
		t.Fatal(err)
	}
	if w := web.Spec; w.Env["GREETING"] != "hello everyone" || w.Replicas != 2 || len(w.Ports) != 2 ||
		w.Ports[0].Bind != "127.0.0.1" || w.Ports[0].Host != 8080 || w.Ports[1].Bind != "0.0.0.0" {
		t.Fatalf("stored web: %+v", w)
	}
	db, err := st.GetApp(ctx, "db")
	if err != nil || db.Spec.Env["POSTGRES_PASSWORD"] != "from-dotenv" || len(db.Spec.Volumes) != 1 || db.Spec.Volumes[0].Name != "pgdata" {
		t.Fatalf("stored db: %+v %v", db.Spec, err)
	}

	// Again, unchanged, and as JSON.
	if out, _, err = run(t, "", "compose", "deploy", path); err != nil || out != "db: unchanged, generation 1, 1 replica\nweb: unchanged, generation 1, 2 replicas\n" {
		t.Fatalf("second deploy: %v\n%s", err, out)
	}
	out, _, err = run(t, "", "compose", "deploy", path, "--json")
	var prs []server.PutResult
	if err != nil || json.Unmarshal([]byte(out), &prs) != nil || len(prs) != 2 || prs[0].App.Name != "db" || prs[1].Created || !prs[0].Unchanged || !prs[1].Unchanged {
		t.Fatalf("deploy --json: %v\n%s", err, out)
	}

	// One bad service, and nothing is sent.
	bad := writeCompose(t, "services:\n  aaa: {image: nginx}\n  zzz: {image: nginx, restart: no}\n", "")
	if _, _, err = run(t, "", "compose", "deploy", bad); err == nil || !strings.Contains(err.Error(), "services.zzz.restart: not supported") {
		t.Fatalf("deploy of a bad file: %v", err)
	}
	if _, err := st.GetApp(ctx, "aaa"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a refused file deployed aaa: %v", err)
	}

	// A prefix makes separate apps.
	if out, _, err = run(t, "", "compose", "deploy", "--prefix", "shop", path); err != nil || !strings.HasPrefix(out, "shop-db: created") {
		t.Fatalf("deploy --prefix: %v\n%s", err, out)
	}

	// A wrong token is refused.
	other, _ := auth.NewToken()
	if _, _, err = run(t, "", "compose", "deploy", path, "--token", other); err == nil || !strings.Contains(err.Error(), "reading app db before deploying: unauthorized") {
		t.Fatalf("wrong token: %v", err)
	}

	// delete reads only the names and the project, so unset variables and
	// refused keys do not matter. The file is in another directory, so it
	// names the project of the one deployed.
	names := writeCompose(t, "name: '"+compose.ProjectName(filepath.Dir(path))+"'\nservices:\n  web: {image: '${UNSET}', depends_on: [db]}\n  db: {build: .}\n", "")
	out, _, err = run(t, "", "compose", "delete", names)
	if want := "db: deleting; its containers stop on the next reconcile pass\nweb: deleting; its containers stop on the next reconcile pass\n"; err != nil || out != want {
		t.Fatalf("delete: %v\n%s", err, out)
	}
	for _, name := range []string{"db", "web"} {
		if _, err := st.GetApp(ctx, name); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("%s after delete: %v", name, err)
		}
	}
	if out, _, err = run(t, "", "compose", "delete", path); err != nil || out != "db: not found\nweb: not found\n" {
		t.Fatalf("second delete: %v\n%s", err, out)
	}
	out, _, err = run(t, "", "compose", "delete", "--prefix", "shop", path, "--json")
	var dels []composeDeleted
	if err != nil || json.Unmarshal([]byte(out), &dels) != nil || len(dels) != 2 || dels[0] != (composeDeleted{"shop-db", "deleting"}) {
		t.Fatalf("delete --json: %v\n%s", err, out)
	}
	if _, _, err = run(t, "", "compose", "delete", path, "--token", other); err == nil || !strings.Contains(err.Error(), "unauthorized") {
		t.Fatalf("delete with a wrong token: %v", err)
	}
}

// TestComposeStopsAtAServerError checks that deploy and delete stop at the
// first app the server refuses, and name the apps already done.
func TestComposeStopsAtAServerError(t *testing.T) {
	isolate(t)
	var n atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			// compose deploy's check before the first write: no such app.
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"error":"app not found"}`))
			return
		}
		if n.Add(1) > 1 {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"error":"the store is read-only"}`))
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/v1/apps/")
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusAccepted)
			json.NewEncoder(w).Encode(map[string]string{"name": name, "status": "deleting"})
			return
		}
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(server.PutResult{App: server.AppView{Name: name, Generation: 1, Replicas: 1}, Created: true})
	}))
	defer ts.Close()
	t.Setenv(envServer, ts.URL)
	t.Setenv(envToken, "test")
	path := writeCompose(t, "services:\n  a: {image: x}\n  b: {image: x}\n  c: {image: x}\n", "")

	out, _, err := run(t, "", "compose", "deploy", "--prefix", "p", path)
	if err == nil || err.Error() != "deploying app p-b (service b): the store is read-only; already deployed: p-a" || out != "p-a: created, generation 1, 1 replica\n" {
		t.Fatalf("deploy: %v\n%s", err, out)
	}
	n.Store(0)
	out, _, err = run(t, "", "compose", "delete", path)
	if err == nil || err.Error() != "deleting app b: the store is read-only; already deleted: a" || !strings.HasPrefix(out, "a: deleting") {
		t.Fatalf("delete: %v\n%s", err, out)
	}
	n.Store(1)
	if _, _, err = run(t, "", "compose", "deploy", path); err == nil || err.Error() != "deploying app a: the store is read-only" {
		t.Fatalf("first deploy fails: %v", err)
	}
}

// writeComposeIn writes a compose file into a new directory called dir, and
// returns its path.
func writeComposeIn(t *testing.T, dir, file string) string {
	t.Helper()
	d := filepath.Join(t.TempDir(), dir)
	if err := os.Mkdir(d, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(d, "compose.yaml")
	if err := os.WriteFile(path, []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// compose deploy updates only the apps its project deployed. It refuses an
// app of the same name that hivemind deploy or another project made, and
// changes nothing of it, unless --replace; after that the project owns it,
// and deploying again needs no flag. The project is the file's top-level
// name, or its directory's name.
func TestComposeDeployRefusesAnAppItDidNotDeploy(t *testing.T) {
	st := composeAPI(t)
	ctx := context.Background()
	if out, _, err := run(t, "", "deploy", "--name", "web", "--image", "httpd:2"); err != nil || !strings.HasPrefix(out, "web: created") {
		t.Fatalf("deploy web: %q, %v", out, err)
	}
	shop := writeComposeIn(t, "Shop.App", "services:\n  web: {image: nginx:alpine}\n  api: {image: nginx:alpine}\n")
	out, _, err := run(t, "", "compose", "deploy", shop)
	want := "conflict: app web was deployed with hivemind deploy or the API, not from compose project shopapp; " +
		"hivemind compose deploy --replace (replace=true in the API) updates it anyway; nothing was deployed"
	if err == nil || err.Error() != want || out != "" {
		t.Fatalf("compose deploy over an app it did not deploy: %q, %v\nwant the error %q", out, err, want)
	}
	if web, err := st.GetApp(ctx, "web"); err != nil || web.Spec.Image != "httpd:2" || web.Generation != 1 || web.Origin != "" {
		t.Fatalf("web after the refused deploy: %+v, %v; want it unchanged", web, err)
	}
	// api comes before web, and is new, but the file was refused whole.
	if _, err := st.GetApp(ctx, "api"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("api after the refused deploy: %v; want it not deployed", err)
	}

	out, _, err = run(t, "", "compose", "deploy", shop, "--replace")
	if err != nil || out != "api: created, generation 1, 1 replica\nweb: updated, generation 2, 1 replica\n" {
		t.Fatalf("compose deploy --replace: %q, %v", out, err)
	}
	if web, err := st.GetApp(ctx, "web"); err != nil || web.Spec.Image != "nginx:alpine" || web.Origin != "compose:shopapp" {
		t.Fatalf("web after --replace: %+v, %v", web, err)
	}
	if out, _, err = run(t, "", "compose", "deploy", shop); err != nil || out != "api: unchanged, generation 1, 1 replica\nweb: unchanged, generation 2, 1 replica\n" {
		t.Fatalf("compose deploy again: %q, %v", out, err)
	}

	// hivemind deploy may update it, and the project keeps it.
	if _, _, err := run(t, "", "deploy", "--name", "web", "--image", "nginx:1.27"); err != nil {
		t.Fatal(err)
	}
	if out, _, err = run(t, "", "compose", "deploy", shop); err != nil || !strings.Contains(out, "web: updated") {
		t.Fatalf("compose deploy after hivemind deploy: %q, %v", out, err)
	}

	// Another project with a service web is refused; its top-level name is
	// its project.
	other := writeComposeIn(t, "shop.app", "name: blog\nservices:\n  web: {image: caddy:2}\n")
	if _, _, err := run(t, "", "compose", "deploy", other); err == nil ||
		!strings.Contains(err.Error(), "app web was deployed from compose project shopapp, not from compose project blog") {
		t.Fatalf("compose deploy of another project: %v", err)
	}
	if v, _, err := run(t, "", "compose", "deploy", writeComposeIn(t, "x", "name: Blog\nservices:\n  web: {image: caddy:2}\n")); err == nil ||
		!strings.Contains(err.Error(), "name: a project name is 1 to 64 lowercase letters") {
		t.Fatalf("compose deploy with a bad project name: %q, %v", v, err)
	}

	// Every app that belongs to something else is named, and none of the
	// file is deployed.
	if _, _, err := run(t, "", "deploy", "--name", "cache", "--image", "redis:7"); err != nil {
		t.Fatal(err)
	}
	both := writeComposeIn(t, "blog", "services:\n  a-new: {image: caddy:2}\n  cache: {image: redis:7}\n  web: {image: caddy:2}\n")
	out, _, err = run(t, "", "compose", "deploy", both)
	want = "conflict: app cache was deployed with hivemind deploy or the API, not from compose project blog; " +
		"app web was deployed from compose project shopapp, not from compose project blog; " +
		"hivemind compose deploy --replace (replace=true in the API) updates them anyway; nothing was deployed"
	if err == nil || err.Error() != want || out != "" {
		t.Fatalf("compose deploy over two apps it did not deploy: %q, %v\nwant the error %q", out, err, want)
	}
	if _, err := st.GetApp(ctx, "a-new"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a-new after the refused deploy: %v", err)
	}
}

// The check before the first write does not replace the store's own: an app
// that hivemind deploy takes between the check and the write is still
// refused, and the error names the apps deployed before it.
func TestComposeDeployRefusesAnAppTakenAfterTheCheck(t *testing.T) {
	var took atomic.Bool
	st := composeAPIWith(t, func(st *store.Store, h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPut && r.URL.Path == "/v1/apps/web" && took.CompareAndSwap(false, true) {
				if _, err := st.PutApp(r.Context(), spec.App{Name: "web", Image: "httpd:2", Replicas: 1}); err != nil {
					t.Error(err)
				}
			}
			h.ServeHTTP(w, r)
		})
	})
	shop := writeComposeIn(t, "shop", "services:\n  web: {image: nginx:alpine}\n  api: {image: nginx:alpine}\n")
	out, _, err := run(t, "", "compose", "deploy", shop)
	want := "deploying app web: conflict: app web was deployed with hivemind deploy or the API, not from compose project shop; " +
		"hivemind compose deploy --replace (replace=true in the API) updates it anyway; already deployed: api"
	if err == nil || err.Error() != want || out != "api: created, generation 1, 1 replica\n" {
		t.Fatalf("compose deploy racing hivemind deploy: %q, %v\nwant the error %q", out, err, want)
	}
	if web, err := st.GetApp(context.Background(), "web"); err != nil || web.Spec.Image != "httpd:2" || web.Origin != "" {
		t.Fatalf("web after the race: %+v, %v", web, err)
	}
}

// compose delete deletes only the apps its project deployed, as deploy
// updates only those. It refuses the whole file when any app belongs to
// something else, naming each one, and deletes nothing; --force deletes
// them. The server checks again at each delete.
func TestComposeDeleteRefusesAnAppItDidNotDeploy(t *testing.T) {
	st := composeAPI(t)
	ctx := context.Background()
	if out, _, err := run(t, "", "deploy", "--name", "web", "--image", "httpd:2"); err != nil || !strings.HasPrefix(out, "web: created") {
		t.Fatalf("deploy web: %q, %v", out, err)
	}
	shop := writeComposeIn(t, "shop", "services:\n  api: {image: nginx:alpine}\n")
	if _, _, err := run(t, "", "compose", "deploy", shop); err != nil {
		t.Fatal(err)
	}
	// A file of the same project that also names web.
	both := writeComposeIn(t, "x", "name: shop\nservices:\n  api: {image: nginx:alpine}\n  web: {image: nginx:alpine}\n")
	out, _, err := run(t, "", "compose", "delete", both)
	want := "conflict: app web was deployed with hivemind deploy or the API, not from compose project shop; " +
		"hivemind compose delete --force deletes it anyway; nothing was deleted"
	if err == nil || err.Error() != want || out != "" {
		t.Fatalf("compose delete over an app it did not deploy: %q, %v\nwant the error %q", out, err, want)
	}
	for _, name := range []string{"api", "web"} {
		if _, err := st.GetApp(ctx, name); err != nil {
			t.Fatalf("%s after the refused delete: %v", name, err)
		}
	}
	// Another project's file deletes nothing of shop's.
	blog := writeComposeIn(t, "blog", "services:\n  api: {image: caddy:2}\n")
	if _, _, err := run(t, "", "compose", "delete", blog); err == nil ||
		!strings.Contains(err.Error(), "app api was deployed from compose project shop, not from compose project blog; hivemind compose delete --force deletes it anyway") {
		t.Fatalf("compose delete of another project: %v", err)
	}

	out, _, err = run(t, "", "compose", "delete", both, "--force")
	if err != nil || out != "api: deleting; its containers stop on the next reconcile pass\nweb: deleting; its containers stop on the next reconcile pass\n" {
		t.Fatalf("compose delete --force: %q, %v", out, err)
	}
	for _, name := range []string{"api", "web"} {
		if _, err := st.GetApp(ctx, name); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("%s after --force: %v", name, err)
		}
	}
}

// An app that another project takes between the check and the delete is
// still refused, by the server, and the error names the apps deleted before
// it.
func TestComposeDeleteRefusesAnAppTakenAfterTheCheck(t *testing.T) {
	var took atomic.Bool
	st := composeAPIWith(t, func(st *store.Store, h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodDelete && r.URL.Path == "/v1/apps/web" && took.CompareAndSwap(false, true) {
				if _, err := st.PutAppWith(r.Context(), spec.App{Name: "web", Image: "caddy:2", Replicas: 1},
					store.PutOptions{Origin: "compose:blog", Replace: true}); err != nil {
					t.Error(err)
				}
			}
			h.ServeHTTP(w, r)
		})
	})
	shop := writeComposeIn(t, "shop", "services:\n  web: {image: nginx:alpine}\n  api: {image: nginx:alpine}\n")
	if _, _, err := run(t, "", "compose", "deploy", shop); err != nil {
		t.Fatal(err)
	}
	out, _, err := run(t, "", "compose", "delete", shop)
	want := "deleting app web: conflict: app web was deployed from compose project blog, not from compose project shop; " +
		"hivemind compose delete --force deletes it anyway; already deleted: api"
	if err == nil || err.Error() != want || out != "api: deleting; its containers stop on the next reconcile pass\n" {
		t.Fatalf("compose delete racing another project: %q, %v\nwant the error %q", out, err, want)
	}
	if web, err := st.GetApp(context.Background(), "web"); err != nil || web.Origin != "compose:blog" {
		t.Fatalf("web after the race: %+v, %v", web, err)
	}
}
