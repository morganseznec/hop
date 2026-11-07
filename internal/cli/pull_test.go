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
	if got, d := pull(cmd, []string{"tools"}, ""); got != nil || d != cobra.ShellCompDirectiveNoFileComp {
		t.Errorf("pull, remote path: %q %v", got, d)
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
