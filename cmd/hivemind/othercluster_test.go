//go:build unix

package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/ao/hivemind/internal/auth"
)

// otherUserHelperEnv makes the test binary run the CLI once, with the
// arguments in otherUserArgsEnv (one per line) and rootDataDir set to the
// directory it names, and print the error. The test runs it as another user.
const (
	otherUserHelperEnv = "HIVEMIND_TEST_OTHER_USER_ROOT_DATA_DIR"
	otherUserArgsEnv   = "HIVEMIND_TEST_OTHER_USER_ARGS"
)

func otherUserHelper() int {
	rootDataDir = os.Getenv(otherUserHelperEnv)
	var out bytes.Buffer
	root := newRoot(strings.NewReader(""), &out, &out)
	root.SetArgs(strings.Split(os.Getenv(otherUserArgsEnv), "\n"))
	if err := root.Execute(); err != nil {
		fmt.Println("error:", err)
		return 1
	}
	fmt.Print(out.String())
	return 0
}

// nobody is the user the test runs the CLI as.
const nobody = 65534

// On a machine where root set up hivemind, as docs/systemd.md does with sudo
// hivemind init, the CLI run by another user has no token. It says to run
// the command with sudo, and not to run init, which would make a second
// cluster. It finds out from the files it cannot read: the data directory,
// root's client config where sudo kept this user's HOME, and the token file
// a readable client config names. The test needs root, to make the files
// and to run the CLI as nobody, so that the kernel refuses the reads.
func TestCLIAsAnotherUserWhereRootSetUpHivemind(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("needs root, to run the CLI as another user")
	}
	isolate(t)
	// Not t.TempDir: its parents are not open to nobody.
	base, err := os.MkdirTemp("", "hm-other-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	must(t, os.Chmod(base, 0o755))
	bin := filepath.Join(base, "hivemind.test")
	copyFile(t, bin, 0o755)
	data := filepath.Join(base, "var-lib-hivemind")
	if _, _, err := run(t, "", "init", "--data-dir", data); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(base, "home")
	must(t, os.MkdirAll(filepath.Join(home, ".config"), 0o755))
	must(t, os.Chown(home, nobody, nobody))
	must(t, os.Chown(filepath.Join(home, ".config"), nobody, nobody))

	asNobody := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(bin)
		cmd.Dir = base
		cmd.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin", otherUserHelperEnv + "=" + data, otherUserArgsEnv + "=" + strings.Join(args, "\n")}
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: nobody, Gid: nobody, Groups: []uint32{}}}
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("%v as nobody succeeded: %s", args, out)
		}
		return string(out)
	}
	check := func(args []string, path, sudo string) {
		t.Helper()
		out := asNobody(args...)
		t.Logf("%v as nobody: %s", args, strings.TrimSpace(out))
		for _, want := range []string{"hivemind on this machine was set up by root", "this user cannot read " + path + ",", "run " + sudo + ", or give the token with --token or HIVEMIND_TOKEN"} {
			if !strings.Contains(out, want) {
				t.Errorf("%v as nobody: %s\nwant %q", args, out, want)
			}
		}
		if strings.Contains(out, "hivemind init") {
			t.Errorf("%v as nobody says to run init: %s", args, out)
		}
	}

	// The data directory, mode 0700.
	check([]string{"status"}, data, "sudo hivemind status")
	check([]string{"logs", "web", "2"}, data, "sudo hivemind logs web 2")
	check([]string{"deploy", "--name", "web", "--image", "nginx:alpine"}, data, "sudo hivemind deploy ...")

	// Root's client config in this user's home, in a directory of root's,
	// as sudo hivemind init writes it when sudo keeps HOME.
	cfgDir := filepath.Join(home, ".config", "hivemind")
	must(t, os.Mkdir(cfgDir, 0o700))
	cfg := filepath.Join(cfgDir, "config.yaml")
	must(t, os.WriteFile(cfg, []byte("server: http://127.0.0.1:4483\ntoken-file: "+filepath.Join(data, auth.TokenFileName)+"\n"), 0o600))
	check([]string{"status"}, cfg, "sudo hivemind status")

	// A client config this user can read, naming a token file it cannot.
	must(t, os.Chmod(cfgDir, 0o755))
	must(t, os.Chmod(cfg, 0o644))
	check([]string{"nodes"}, filepath.Join(data, auth.TokenFileName), "sudo hivemind nodes")

	// A directory of this user's own that it cannot read is not someone
	// else's cluster: the plain advice stands.
	must(t, os.Remove(cfg))
	must(t, os.Chmod(data, 0o755))
	own := filepath.Join(home, ".local", "state", "hivemind")
	must(t, os.MkdirAll(own, 0o700))
	must(t, os.Chown(filepath.Join(home, ".local"), nobody, nobody))
	must(t, os.Chown(filepath.Join(home, ".local", "state"), nobody, nobody))
	must(t, os.Chown(own, nobody, nobody))
	must(t, os.Chmod(own, 0o000))
	must(t, os.Chmod(filepath.Join(data, auth.TokenFileName), 0o644))
	if out := asNobody("status"); strings.Contains(out, "set up by") || !strings.Contains(out, "no API token") {
		t.Errorf("status as nobody, with its own data directory unreadable and root's readable: %s", out)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// copyFile copies the running test binary to dst.
func copyFile(t *testing.T, dst string, perm os.FileMode) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	src, err := os.Open(exe)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(f, src); err != nil {
		f.Close()
		t.Fatal(err)
	}
	must(t, f.Close())
	must(t, os.Chmod(dst, perm))
}

// commandLine repeats a command's path and arguments, and puts ... for its
// flags, whose values may be secrets.
func TestCommandLine(t *testing.T) {
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"status"}, "hivemind status"},
		{[]string{"logs", "web", "2"}, "hivemind logs web 2"},
		{[]string{"status", "--json"}, "hivemind status ..."},
		{[]string{"deploy", "--name", "x", "--image", "busybox", "--env", "PASSWORD=secret", "--", "sleep", "60"}, "hivemind deploy ..."},
		{[]string{"delete", "my app"}, `hivemind delete "my app"`},
		{[]string{"node", "drain", "node-1"}, "hivemind node drain node-1"},
	} {
		root := newRoot(strings.NewReader(""), io.Discard, io.Discard)
		cmd, rest, err := root.Find(c.args)
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.ParseFlags(rest); err != nil {
			t.Fatal(err)
		}
		if got := commandLine(cmd); got != c.want {
			t.Errorf("%v: %q, want %q", c.args, got, c.want)
		}
	}
}
