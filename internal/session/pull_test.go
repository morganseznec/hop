package session

import (
	"bytes"
	"context"
	"errors"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/morganseznec/hop/internal/fakeagent"
)

func TestMain(m *testing.M) {
	// The fake session-manager-plugin of the pull tests is this binary.
	fakeagent.Main()
	os.Exit(m.Run())
}

// remoteFile writes data to a file of the "instance": the fake agent runs the
// session's command on this machine.
func remoteFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func pullRequest() Request {
	return Request{Profile: "acme-prod", Region: "eu-west-1", InstanceID: "i-0aaa"}
}

// leftovers lists the temporary files Pull left in dir.
func leftovers(t *testing.T, dir string) []string {
	t.Helper()
	m, _ := filepath.Glob(filepath.Join(dir, ".hop-pull-*"))
	return m
}

func TestPull(t *testing.T) {
	out := fakeagent.Install(t)
	fake, cfg := newFake(t, startSessionOK)
	// Every byte value, and more than the plugin sends in one message.
	data := make([]byte, 300_000)
	r := rand.New(rand.NewPCG(1, 2))
	for i := range data {
		data[i] = byte(r.IntN(256))
	}
	remote := remoteFile(t, "it's a $file (1).bin", data)
	dir := t.TempDir()
	local := filepath.Join(dir, "copy.bin")

	var last, total int64
	n, err := Pull(context.Background(), cfg, pullRequest(), remote, local, func(done, size int64) { last, total = done, size })
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(local)
	if n != int64(len(data)) || !bytes.Equal(got, data) {
		t.Fatalf("got %d bytes (n=%d), want %d", len(got), n, len(data))
	}
	if last != n || total != n {
		t.Errorf("progress: %d of %d", last, total)
	}
	if st, _ := os.Stat(local); st.Mode().Perm() != 0o600 {
		t.Errorf("new file mode %v, want 0600", st.Mode().Perm())
	}
	if l := leftovers(t, dir); len(l) > 0 {
		t.Errorf("left behind: %v", l)
	}

	reqs := fake.got("StartSession")
	if len(reqs) != 1 || reqs[0]["DocumentName"] != PullDocument || reqs[0]["Target"] != "i-0aaa" {
		t.Fatalf("StartSession: %v", reqs)
	}
	params, _ := reqs[0]["Parameters"].(map[string]any)
	cmds, _ := params["command"].([]any)
	cmd, _ := cmds[0].(string)
	// One line, and the marker only shows up in the output.
	if !strings.HasPrefix(cmd, "sh -c '") || strings.Contains(cmd, "\n") || regexp.MustCompile(`--hop[0-9a-f]{16}`).MatchString(cmd) {
		t.Errorf("command: %s", cmd)
	}
	if args := readFile(t, filepath.Join(out, "args")); !strings.Contains(args, `"DocumentName":"AWS-StartNonInteractiveCommand"`) {
		t.Errorf("plugin arguments: %s", args)
	}
}

func TestPullKeepsTheMode(t *testing.T) {
	fakeagent.Install(t)
	_, cfg := newFake(t, startSessionOK)
	remote := remoteFile(t, "app.conf", []byte("new\n"))
	local := filepath.Join(t.TempDir(), "app.conf")
	os.WriteFile(local, []byte("old\n"), 0o640)
	if _, err := Pull(context.Background(), cfg, pullRequest(), remote, local, nil); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(local)
	if got := readFile(t, local); got != "new\n" || st.Mode().Perm() != 0o640 {
		t.Errorf("%q, mode %v", got, st.Mode().Perm())
	}
}

func TestPullEmptyFile(t *testing.T) {
	fakeagent.Install(t)
	_, cfg := newFake(t, startSessionOK)
	remote := remoteFile(t, "empty", nil)
	local := filepath.Join(t.TempDir(), "empty")
	n, err := Pull(context.Background(), cfg, pullRequest(), remote, local, nil)
	if err != nil || n != 0 {
		t.Fatalf("%d, %v", n, err)
	}
	if st, err := os.Stat(local); err != nil || st.Size() != 0 {
		t.Errorf("%v, %v", st, err)
	}
}

func TestPullFails(t *testing.T) {
	data := bytes.Repeat([]byte("line of a log file\n"), 2000)
	dirOnRemote := t.TempDir()
	unreadable := remoteFile(t, "secret", []byte("x"))
	os.Chmod(unreadable, 0)
	// sudo asks for a password: sudo -n fails.
	noSudo := t.TempDir()
	os.WriteFile(filepath.Join(noSudo, "sudo"), []byte("#!/bin/sh\nexit 1\n"), 0o755)

	cases := []struct {
		name   string
		remote string
		env    map[string]string
		want   string
	}{
		{"missing file", "/nonexistent/hop/file", nil, "cannot read the file on the instance: no such file, or no permission to see it"},
		{"directory", dirOnRemote, nil, "cannot read the file on the instance: is a directory"},
		{"unreadable, no sudo", unreadable, map[string]string{"PATH": noSudo}, "cannot read the file on the instance: permission denied, and sudo -n is not allowed"},
		{"session cut", remoteFile(t, "app.log", data), map[string]string{"HOP_FAKE_AGENT_CUT": "2000"}, "the session ended after"},
		{"data changed on the way", remoteFile(t, "app.log", data), map[string]string{"HOP_FAKE_AGENT_FLIP": "10"}, "checksum mismatch after download"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.name == "unreadable, no sudo" && os.Geteuid() == 0 {
				t.Skip("root reads every file")
			}
			fakeagent.Install(t)
			for k, v := range c.env {
				if k == "PATH" {
					v += string(os.PathListSeparator) + os.Getenv("PATH")
				}
				t.Setenv(k, v)
			}
			fake, cfg := newFake(t, startSessionOK)
			dir := t.TempDir()
			local := filepath.Join(dir, "copy")
			_, err := Pull(context.Background(), cfg, pullRequest(), c.remote, local, nil)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want %q", err, c.want)
			}
			if _, err := os.Stat(local); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("local file written: %v", err)
			}
			if l := leftovers(t, dir); len(l) > 0 {
				t.Errorf("left behind: %v", l)
			}
			if len(fake.got("StartSession")) != 1 {
				t.Errorf("StartSession calls: %v", fake.got("StartSession"))
			}
		})
	}
}

func TestPullInterrupted(t *testing.T) {
	fakeagent.Install(t)
	t.Setenv("HOP_FAKE_AGENT_WAIT", "1")
	fake, cfg := newFake(t, startSessionOK)
	remote := remoteFile(t, "app.log", []byte("x"))
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(300*time.Millisecond, cancel)
	start := time.Now()
	_, err := Pull(ctx, cfg, pullRequest(), remote, filepath.Join(dir, "app.log"), nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Errorf("took %s: the plugin was not stopped", time.Since(start))
	}
	// The session would otherwise stay open on the SSM side.
	if len(fake.got("TerminateSession")) != 1 {
		t.Errorf("TerminateSession calls: %v", fake.got("TerminateSession"))
	}
	if l := leftovers(t, dir); len(l) > 0 {
		t.Errorf("left behind: %v", l)
	}
}

func TestPullRefusesBeforeCallingAWS(t *testing.T) {
	for remote, want := range map[string]string{
		"var/log/app.log": "remote path must be absolute",
		"/var/log/":       "remote path names a directory",
	} {
		_, err := Pull(context.Background(), aws.Config{}, pullRequest(), remote, filepath.Join(t.TempDir(), "x"), nil)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", remote, err)
		}
	}
}

func TestReceiveExplainsASessionThatNeverStarts(t *testing.T) {
	stream := "\nStarting session with SessionId: acme-user-0abc\n\n\nSessionId: acme-user-0abc : Unable to start command: exec: \"sh\": not found\n\n"
	_, _, err := receive(strings.NewReader(stream), "--hop0123-", &bytes.Buffer{}, nil)
	if err == nil || err.Error() != `the session ended before the transfer started: SessionId: acme-user-0abc : Unable to start command: exec: "sh": not found` {
		t.Errorf("%v", err)
	}
}

func TestReceiveReportsTheRemoteError(t *testing.T) {
	stream := "--hop0123-size 12\n--hop0123-begin\nYWJj\n--hop0123-stderr cat: /var/log/app.log: Input/output error\n--hop0123-end 1 \n"
	_, _, err := receive(strings.NewReader(stream), "--hop0123-", &bytes.Buffer{}, nil)
	if err == nil || err.Error() != "reading the file failed on the instance (exit code 1): cat: /var/log/app.log: Input/output error" {
		t.Errorf("%v", err)
	}
}
