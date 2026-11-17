package session

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/morganseznec/hop/internal/fakeagent"
)

// remoteDir fills a directory of the "instance": the fake agent runs the session's
// command on this machine.
func remoteDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"app.log", "app.conf", "it's a $file (1).txt", ".env", "-rf", "*", "tab\there", "two\nlines"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"logs", "app.d", ".cache"} {
		if err := os.Mkdir(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A link to a directory is one to walk into; a link to nothing is still a name.
	os.Symlink(filepath.Join(dir, "logs"), filepath.Join(dir, "current"))
	os.Symlink(filepath.Join(dir, "gone"), filepath.Join(dir, "dangling"))
	return dir
}

func TestList(t *testing.T) {
	out := fakeagent.Install(t)
	fake, cfg := newFake(t, startSessionOK)
	dir := remoteDir(t)

	names, more, err := List(context.Background(), cfg, pullRequest(), dir, "")
	if err != nil {
		t.Fatal(err)
	}
	// Directories and the link to one end with "/", and a link to nothing is a name
	// like another. The names with a tab or a line break are left out: the second
	// half of "two\nlines" is not a name of its own.
	want := []string{"*", "-rf", ".cache/", ".env", "app.conf", "app.d/", "app.log", "current/", "dangling", "it's a $file (1).txt", "logs/"}
	slices.Sort(names)
	if !slices.Equal(names, want) || more {
		t.Errorf("names, more=%v:\n got %q\nwant %q", more, names, want)
	}

	reqs := fake.got("StartSession")
	if len(reqs) != 1 || reqs[0]["DocumentName"] != PullDocument || reqs[0]["Target"] != "i-0aaa" {
		t.Fatalf("StartSession: %v", reqs)
	}
	params, _ := reqs[0]["Parameters"].(map[string]any)
	cmds, _ := params["command"].([]any)
	cmd, _ := cmds[0].(string)
	// One line, the marker only shows up in the output, and the listing changes
	// nothing: no command that writes.
	if !strings.HasPrefix(cmd, "sh -c '") || strings.Contains(cmd, "\n") || regexp.MustCompile(`--hop[0-9a-f]{16}`).MatchString(cmd) {
		t.Errorf("command: %s", cmd)
	}
	if regexp.MustCompile(`\b(rm|mv|cp|tee|touch|mkdir|mkfifo|mktemp|chmod|chown)\b|>[^/&]`).MatchString(cmd) {
		t.Errorf("the listing writes: %s", cmd)
	}
	// dash reads "$((" as arithmetic, even where a command substitution was meant.
	if strings.Contains(strings.ReplaceAll(cmd, "n=$((n+1))", ""), "$((") {
		t.Errorf("a command substitution that dash reads as arithmetic: %s", cmd)
	}
	if args := readFile(t, filepath.Join(out, "args")); !strings.Contains(args, `"DocumentName":"AWS-StartNonInteractiveCommand"`) {
		t.Errorf("plugin arguments: %s", args)
	}
}

func TestListByPrefix(t *testing.T) {
	fakeagent.Install(t)
	_, cfg := newFake(t, startSessionOK)
	dir := remoteDir(t)
	for prefix, want := range map[string][]string{
		"app":   {"app.conf", "app.d/", "app.log"},
		"app.l": {"app.log"},
		".":     {".cache/", ".env"},
		// The prefix is compared as it is: no pattern, no option, no expansion.
		"*":      {"*"},
		"-":      {"-rf"},
		"it's a": {"it's a $file (1).txt"},
		"$(id)":  nil,
		"a*":     nil,
		"zzz":    nil,
		"APP":    nil,
	} {
		names, more, err := List(context.Background(), cfg, pullRequest(), dir, prefix)
		slices.Sort(names)
		if err != nil || more || !slices.Equal(names, want) {
			t.Errorf("prefix %q: %q, more=%v, %v; want %q", prefix, names, more, err, want)
		}
	}
	// With or without its final "/", a directory is the same.
	names, _, err := List(context.Background(), cfg, pullRequest(), dir+"/", "logs")
	if err != nil || !slices.Equal(names, []string{"logs/"}) {
		t.Errorf("with a final /: %q, %v", names, err)
	}
	// A directory with a quote and a space in its name.
	odd := filepath.Join(dir, "it's odd")
	os.Mkdir(odd, 0o755)
	os.WriteFile(filepath.Join(odd, "inside"), nil, 0o644)
	names, _, err = List(context.Background(), cfg, pullRequest(), odd, "")
	if err != nil || !slices.Equal(names, []string{"inside"}) {
		t.Errorf("odd directory: %q, %v", names, err)
	}
	// An empty directory lists nothing, and that is not an error.
	names, more, err := List(context.Background(), cfg, pullRequest(), filepath.Join(dir, "logs"), "")
	if err != nil || more || len(names) != 0 {
		t.Errorf("empty directory: %q, more=%v, %v", names, more, err)
	}
}

func TestListStopsAtTheLimit(t *testing.T) {
	fakeagent.Install(t)
	_, cfg := newFake(t, startSessionOK)
	dir := t.TempDir()
	for i := range ListLimit + 7 {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("part-%05d", i)), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(dir, "zz-last"), nil, 0o644)
	names, more, err := List(context.Background(), cfg, pullRequest(), dir, "")
	if err != nil || !more || len(names) != ListLimit || names[0] != "part-00000" {
		t.Fatalf("%d names, more=%v, %v", len(names), more, err)
	}
	// The limit is on what the prefix keeps, not on what the directory holds.
	names, more, err = List(context.Background(), cfg, pullRequest(), dir, "zz")
	if err != nil || more || !slices.Equal(names, []string{"zz-last"}) {
		t.Errorf("past the limit, by prefix: %q, more=%v, %v", names, more, err)
	}
}

func TestListFails(t *testing.T) {
	dir := remoteDir(t)
	closed := filepath.Join(t.TempDir(), "closed")
	os.Mkdir(closed, 0o700)
	os.WriteFile(filepath.Join(closed, "secret"), nil, 0o600)
	os.Chmod(closed, 0)
	t.Cleanup(func() { os.Chmod(closed, 0o700) })
	// sudo asks for a password: sudo -n fails.
	noSudo := t.TempDir()
	os.WriteFile(filepath.Join(noSudo, "sudo"), []byte("#!/bin/sh\nexit 1\n"), 0o755)

	cases := []struct {
		name string
		dir  string
		env  map[string]string
		want string
	}{
		{"missing directory", "/nonexistent/hop/dir", nil, "cannot list the directory on the instance: no such directory, or no permission to see it"},
		{"a file", filepath.Join(dir, "app.log"), nil, "cannot list the directory on the instance: no such directory, or no permission to see it"},
		{"unreadable, no sudo", closed, map[string]string{"PATH": noSudo}, "cannot list the directory on the instance: permission denied, and sudo -n is not allowed"},
		{"session cut before the listing", dir, map[string]string{"HOP_FAKE_AGENT_CUT": "20"}, "the session ended before the listing started"},
		{"session cut in the listing", dir, map[string]string{"HOP_FAKE_AGENT_CUT": "120"}, "the session ended before the end of the listing"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.name == "unreadable, no sudo" && os.Geteuid() == 0 {
				t.Skip("root reads every directory")
			}
			fakeagent.Install(t)
			for k, v := range c.env {
				if k == "PATH" {
					v += string(os.PathListSeparator) + os.Getenv("PATH")
				}
				t.Setenv(k, v)
			}
			fake, cfg := newFake(t, startSessionOK)
			names, _, err := List(context.Background(), cfg, pullRequest(), c.dir, "")
			if err == nil || !strings.Contains(err.Error(), c.want) || names != nil {
				t.Fatalf("got %q, %v; want %q", names, err, c.want)
			}
			if len(fake.got("StartSession")) != 1 {
				t.Errorf("StartSession calls: %v", fake.got("StartSession"))
			}
		})
	}
}

func TestListReadsWithSudo(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every directory")
	}
	fakeagent.Install(t)
	_, cfg := newFake(t, startSessionOK)
	// A home the session user cannot open, and a sudo that needs no password: this
	// one runs the command as it is, on a copy the test can read.
	home := filepath.Join(t.TempDir(), "ec2-user")
	os.Mkdir(home, 0o700)
	os.WriteFile(filepath.Join(home, "notes.txt"), nil, 0o600)
	os.Mkdir(filepath.Join(home, "releases"), 0o700)
	os.Chmod(home, 0)
	t.Cleanup(func() { os.Chmod(home, 0o700) })

	bin, log := t.TempDir(), filepath.Join(t.TempDir(), "sudo.log")
	sudo := fmt.Sprintf("#!/bin/sh\necho \"$1 $2\" >> '%s'\n[ \"$1\" = -n ] || exit 1\nshift\n[ \"$1\" = true ] && exit 0\nchmod 700 '%s'\n\"$@\"\nrc=$?\nchmod 0 '%s'\nexit $rc\n", log, home, home)
	os.WriteFile(filepath.Join(bin, "sudo"), []byte(sudo), 0o755)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	names, more, err := List(context.Background(), cfg, pullRequest(), home, "")
	if err != nil || more || !slices.Equal(names, []string{"notes.txt", "releases/"}) {
		t.Fatalf("%q, more=%v, %v", names, more, err)
	}
	// sudo -n, which never asks for a password: to see whether it is allowed, to test
	// the directory twice, and for the shell that reads it.
	if got := strings.Fields(readFile(t, log)); !slices.Equal(got, []string{"-n", "true", "-n", "test", "-n", "test", "-n", "sh"}) {
		t.Errorf("sudo ran: %q", got)
	}
}

func TestCheckList(t *testing.T) {
	_, cfg := newFake(t, startSessionOK)
	for _, dir := range []string{"", "var/log", "./logs", "~"} {
		if err := CheckList(dir); err == nil || !strings.Contains(err.Error(), "must be absolute") {
			t.Errorf("%q: %v", dir, err)
		}
		// Refused before any call.
		if _, _, err := List(context.Background(), cfg, pullRequest(), dir, ""); err == nil {
			t.Errorf("List(%q) went ahead", dir)
		}
	}
	if err := CheckList("/"); err != nil {
		t.Errorf("/: %v", err)
	}
}

func TestListable(t *testing.T) {
	for name, want := range map[string]bool{
		"app.log": true, "logs/": true, "it's a $file": true, "é.txt": true, "-rf": true,
		"": false, "/": false, "a/b": false, "a/b/": false, "tab\there": false, "bell\a": false, "latin\xe9": false, "esc\x1b[0m": false,
	} {
		if got := listable(name); got != want {
			t.Errorf("listable(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestReadList(t *testing.T) {
	const m = "--hop0123456789abcdef-"
	for _, c := range []struct {
		name, out string
		want      []string
		more      bool
		err       string
	}{
		{"names", "\nStarting session with SessionId: x\n" + m + "begin\napp.log\nlogs/\n" + m + "end\n\nExiting session with sessionId: x.\n", []string{"app.log", "logs/"}, false, ""},
		{"line ends of a terminal", m + "begin\r\napp.log\r\n" + m + "end\r\n", []string{"app.log"}, false, ""},
		{"more", m + "begin\na\n" + m + "more\n" + m + "end\n", []string{"a"}, true, ""},
		{"nothing", m + "begin\n" + m + "end\n", nil, false, ""},
		// What comes before the listing is not a name, and neither is another marker.
		{"noise first", "app.log\n" + m + "begin\n" + m + "other x\nb\n" + m + "end\nc\n", []string{"b"}, false, ""},
		{"error", m + "error no such directory, or no permission to see it\n", nil, false, "cannot list the directory on the instance: no such directory"},
		{"cut", m + "begin\napp.log\n", nil, false, "the session ended before the end of the listing"},
		{"never started", "\nSessionId: x : ----------ERROR-------\nEncountered error while initiating handshake\n", nil, false, "the session ended before the listing started: SessionId: x : ----------ERROR------- / Encountered error while initiating handshake"},
		{"empty", "", nil, false, "the session ended before the listing started"},
		// A name that looks like the marker of another session ends nothing.
		{"foreign marker", m + "begin\n--hopffffffffffffffff-end\n" + m + "end\n", []string{"--hopffffffffffffffff-end"}, false, ""},
	} {
		names, more, err := readList(strings.NewReader(c.out), m)
		switch {
		case c.err != "":
			if err == nil || !strings.Contains(err.Error(), c.err) || names != nil {
				t.Errorf("%s: %q, %v; want error %q", c.name, names, err, c.err)
			}
		case err != nil || more != c.more || !slices.Equal(names, c.want):
			t.Errorf("%s: %q, more=%v, %v; want %q, more=%v", c.name, names, more, err, c.want, c.more)
		}
	}
}
