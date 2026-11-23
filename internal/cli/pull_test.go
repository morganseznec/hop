package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/morganseznec/hop/internal/fakeagent"
)

func TestPull(t *testing.T) {
	f := fakeAWSEnv(t)
	cacheBillingApp(t, 5*time.Minute)
	fakeagent.Install(t)
	// The fake agent runs the session's command here: the "remote" file is local.
	remote := filepath.Join(t.TempDir(), "app.log")
	data := strings.Repeat("GET /health 200\n", 5000)
	os.WriteFile(remote, []byte(data), 0o644)

	dir := t.TempDir()
	t.Chdir(dir)
	stdout, stderr, err := runHop(t, "pull", "acme-b-app", remote)
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "app.log")); string(got) != data {
		t.Fatalf("got %d bytes, want %d", len(got), len(data))
	}
	if stdout != "" {
		t.Errorf("stdout: %q", stdout)
	}
	for _, want := range []string{"hop: acme-b-app:" + remote + " -> app.log\n", "hop: 80.0 KB received, SHA-256 matches on both sides\n"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sessions) != 1 || f.sessions[0]["DocumentName"] != "AWS-StartNonInteractiveCommand" || f.sessions[0]["Target"] != "i-0b01" {
		t.Errorf("StartSession requests: %v", f.sessions)
	}
}

func TestPullChecksBeforeResolving(t *testing.T) {
	fake := fakeAWSEnv(t)
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, "app.log"), 0o700)
	cases := []struct {
		args []string
		want string
	}{
		// "nowhere" matches no instance: resolving it would fail differently, after a
		// refresh.
		{[]string{"nowhere", "var/log/app.log"}, "remote path must be absolute (start with /): var/log/app.log"},
		{[]string{"nowhere", "/var/log/"}, "remote path names a directory: /var/log/"},
		{[]string{"nowhere", "/var/log/app.log", filepath.Join(dir, "missing", "app.log")}, "directory not found: " + filepath.Join(dir, "missing")},
		{[]string{"nowhere", "/var/log/app.log", filepath.Join(dir, "missing") + "/"}, "directory not found: " + filepath.Join(dir, "missing") + "/"},
		{[]string{"nowhere", "/var/log/app.log", dir}, filepath.Join(dir, "app.log") + " is a directory"},
	}
	for _, c := range cases {
		_, _, err := runHop(t, append([]string{"pull"}, c.args...)...)
		if err == nil || err.Error() != c.want {
			t.Errorf("%v: error %v, want %q", c.args, err, c.want)
		}
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.calls) != 0 {
		t.Errorf("AWS called before the checks: %q", fake.calls)
	}
}

func TestPullTarget(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "old.log")
	os.WriteFile(existing, []byte("x"), 0o600)
	cases := []struct{ local, want string }{
		{"", "app.log"},
		{dir, filepath.Join(dir, "app.log")},
		{dir + "/", filepath.Join(dir, "app.log")},
		{existing, existing},
		{filepath.Join(dir, "new.log"), filepath.Join(dir, "new.log")},
		{"new.log", "new.log"},
	}
	for _, c := range cases {
		got, err := pullTarget("/var/log/app.log", c.local)
		if err != nil || got != c.want {
			t.Errorf("%q: %q, %v; want %q", c.local, got, err, c.want)
		}
	}
	if _, err := pullTarget("/var/log/app.log", os.DevNull); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("%s: %v", os.DevNull, err)
	}
}

func TestPullCompletion(t *testing.T) {
	fleetEnv(t)
	cmd := &cobra.Command{}
	pull := newPull(&filters{}).ValidArgsFunction
	if got, _ := pull(cmd, nil, ""); len(got) == 0 {
		t.Errorf("pull, instance: %q", got)
	}
	// The path on the instance starts with "/", which Tab types; what follows is
	// listed on the instance (see TestRemoteCompletion).
	if got, d := pull(cmd, []string{"tools"}, ""); len(got) != 1 || got[0] != "/" || d != cobra.ShellCompDirectiveNoFileComp|cobra.ShellCompDirectiveNoSpace {
		t.Errorf("pull, remote path: %q %v", got, d)
	}
	// From a bucket, the second argument is a local path.
	if got, d := pull(cmd, []string{"s3://acme-logs/app.log"}, ""); got != nil || d != cobra.ShellCompDirectiveDefault {
		t.Errorf("pull from a bucket, local path: %q %v", got, d)
	}
	if got, d := pull(cmd, []string{"tools", "/var/log/app.log"}, ""); got != nil || d != cobra.ShellCompDirectiveDefault {
		t.Errorf("pull, local path: %q %v", got, d)
	}
}

func TestByteSize(t *testing.T) {
	for n, want := range map[int64]string{0: "0 B", 999: "999 B", 1000: "1.0 KB", 80_000: "80.0 KB", 4_250_000: "4.2 MB", 1_500_000_000: "1.5 GB"} {
		if got := byteSize(n); got != want {
			t.Errorf("%d: %q, want %q", n, got, want)
		}
	}
}

func TestPullSeveral(t *testing.T) {
	f := fakeAWSEnv(t)
	cacheBillingApp(t, 5*time.Minute)
	fakeagent.Install(t)
	// The fake agent runs the session's commands here: the "remote" files are local.
	remote := t.TempDir()
	for name, data := range map[string]string{"app.17.log": "seventeen\n", "app.18.log": "eighteen\n", "app.19.log": "nineteen\n", "other.txt": "x\n", ".app.1.log": "hidden\n"} {
		os.WriteFile(filepath.Join(remote, name), []byte(data), 0o644)
	}
	os.Mkdir(filepath.Join(remote, "app.dir.log"), 0o755)

	// A pattern, expanded on the instance, into a directory.
	dest := t.TempDir()
	_, stderr, err := runHop(t, "pull", "acme-b-app", remote+"/app.*.log", dest)
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	entries, _ := os.ReadDir(dest)
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	if strings.Join(got, " ") != "app.17.log app.18.log app.19.log" {
		t.Errorf("pattern: files %q\n%s", got, stderr)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "app.18.log")); string(b) != "eighteen\n" {
		t.Errorf("app.18.log: %q", b)
	}
	if !strings.Contains(stderr, "hop: 3 of 3 files received, 28 B in all\n") {
		t.Errorf("stderr:\n%s", stderr)
	}
	f.mu.Lock()
	// One session lists the directory, then one per file.
	if len(f.sessions) != 4 {
		t.Errorf("%d sessions, want 4", len(f.sessions))
	}
	f.mu.Unlock()

	// Several paths, as a brace gives them, into the current directory; one fails,
	// the others still come.
	cwd := t.TempDir()
	t.Chdir(cwd)
	_, stderr, err = runHop(t, "pull", "acme-b-app", remote+"/app.17.log", remote+"/missing.log", remote+"/app.19.log")
	if err == nil || err.Error() != "1 of 3 files failed: "+remote+"/missing.log" {
		t.Errorf("error %v\n%s", err, stderr)
	}
	for _, name := range []string{"app.17.log", "app.19.log"} {
		if _, err := os.Stat(filepath.Join(cwd, name)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}

	// A pattern that matches nothing.
	_, _, err = runHop(t, "pull", "acme-b-app", remote+"/nothing*")
	if err == nil || err.Error() != "no file on the instance matches "+remote+"/nothing*" {
		t.Errorf("no match: %v", err)
	}
}

func TestPullSeveralChecksBeforeResolving(t *testing.T) {
	fake := fakeAWSEnv(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "old.log")
	os.WriteFile(file, []byte("x"), 0o600)
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"nowhere", "/var/*/app.log"}, "a pattern on the instance goes in the file name only, not in the directory: /var/*/app.log"},
		{[]string{"nowhere", "/var/log/app[.log"}, "invalid pattern: /var/log/app[.log"},
		{[]string{"nowhere", "/var/log/a.log", "var/log/b.log", "/var/log/c.log"}, "remote path must be absolute (start with /): var/log/b.log"},
		{[]string{"nowhere", "/var/log/*.log", file}, "several files land in a directory, and " + file + " is not one"},
		{[]string{"nowhere", "/var/log/*.log", filepath.Join(dir, "missing") + "/"}, "directory not found: " + filepath.Join(dir, "missing") + "/"},
	}
	for _, c := range cases {
		_, _, err := runHop(t, append([]string{"pull"}, c.args...)...)
		if err == nil || err.Error() != c.want {
			t.Errorf("%v: error %v, want %q", c.args, err, c.want)
		}
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.calls) != 0 {
		t.Errorf("AWS called before the checks: %q", fake.calls)
	}
}

func TestSplitPull(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		paths   []string
		remotes string
		local   string
	}{
		{[]string{"/var/log/a.log"}, "/var/log/a.log", ""},
		{[]string{"/var/log/a.log", "a.log"}, "/var/log/a.log", "a.log"},
		{[]string{"/var/log/a.log", dir}, "/var/log/a.log", dir},
		{[]string{"/var/log/a.log", "out/"}, "/var/log/a.log", "out/"},
		// Another directory, or one that exists here: a local path, as before.
		{[]string{"/var/log/a.log", "/hop-nowhere/b.log"}, "/var/log/a.log", "/hop-nowhere/b.log"},
		{[]string{dir + "/a.log", dir + "/b.log"}, dir + "/a.log", dir + "/b.log"},
		// The same directory, which does not exist here: a brace, two remote files.
		{[]string{"/hop-nowhere/a.log", "/hop-nowhere/b.log"}, "/hop-nowhere/a.log /hop-nowhere/b.log", ""},
		{[]string{"/var/log/a.log", "/var/log/*.gz"}, "/var/log/a.log /var/log/*.gz", ""},
		{[]string{"/l/a.log", "/l/b.log", "/l/c.log"}, "/l/a.log /l/b.log /l/c.log", ""},
		{[]string{"/l/a.log", "/l/b.log", dir}, "/l/a.log /l/b.log", dir},
		{[]string{"/l/a.log", "/l/b.log", "."}, "/l/a.log /l/b.log", "."},
	}
	for _, c := range cases {
		remotes, local := splitPull(c.paths)
		if strings.Join(remotes, " ") != c.remotes || local != c.local {
			t.Errorf("%q: %q %q, want %q %q", c.paths, remotes, local, c.remotes, c.local)
		}
	}
}

func TestPullTargetsSameName(t *testing.T) {
	_, err := pullTargets([]string{"/a/app.log", "/b/app.log"}, "")
	if err == nil || err.Error() != "/a/app.log and /b/app.log would both land in app.log: download them one at a time" {
		t.Errorf("%v", err)
	}
}
