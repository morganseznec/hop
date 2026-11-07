// Package fakeagent plays session-manager-plugin and the SSM agent in tests of
// non-interactive sessions (hop pull). The test binary, started as the plugin, runs the
// session's command with the local sh and prints its output, stdout and stderr
// together, between the lines the real plugin prints.
package fakeagent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

const env = "HOP_FAKE_AGENT"

// Install puts first on PATH a session-manager-plugin that starts the test binary as
// the fake agent; TestMain must call Main first. The fake writes its arguments to
// $HOP_TEST_OUT/args. Variables change what it does:
//   - HOP_FAKE_AGENT_CUT=n stops after n bytes of output, like a session cut off;
//   - HOP_FAKE_AGENT_FLIP=n changes the nth base64 character after the begin line;
//   - HOP_FAKE_AGENT_WAIT=1 waits a minute before running the command.
func Install(t testing.TB) (out string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake plugin is a shell script")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin, out := t.TempDir(), t.TempDir()
	script := "#!/bin/sh\n" + env + "=1 exec '" + strings.ReplaceAll(self, "'", `'\''`) + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "session-manager-plugin"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOP_TEST_OUT", out)
	return out
}

// Main plays the plugin and exits when the test binary was started as one, and returns
// otherwise.
func Main() {
	if os.Getenv(env) != "1" {
		return
	}
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if out := os.Getenv("HOP_TEST_OUT"); out != "" {
		os.WriteFile(filepath.Join(out, "args"), []byte(strings.Join(args, "\n")+"\n"), 0o600)
	}
	// The arguments hop gives: the variable that holds the StartSession response, the
	// region, "StartSession", the profile, the request, the endpoint.
	if len(args) != 6 {
		fmt.Fprintf(os.Stderr, "fake plugin: %d arguments\n", len(args))
		return 2
	}
	var req struct {
		DocumentName string
		Parameters   map[string][]string
	}
	if err := json.Unmarshal([]byte(args[4]), &req); err != nil || len(req.Parameters["command"]) != 1 {
		fmt.Fprintf(os.Stderr, "fake plugin: request %s\n", args[4])
		return 2
	}
	fmt.Print("\nStarting session with SessionId: acme-user-0abc\n")

	// The real plugin ends the session when its stdin closes.
	go func() {
		io.Copy(io.Discard, os.Stdin)
		fmt.Fprintln(os.Stderr, "Unable read from Stdin: EOF")
		os.Exit(1)
	}()
	if os.Getenv("HOP_FAKE_AGENT_WAIT") == "1" {
		time.Sleep(time.Minute)
	}

	w := &output{w: os.Stdout, cut: -1}
	if n, err := strconv.Atoi(os.Getenv("HOP_FAKE_AGENT_CUT")); err == nil {
		w.cut = n
	}
	if n, err := strconv.Atoi(os.Getenv("HOP_FAKE_AGENT_FLIP")); err == nil {
		w.flip = n
	}
	// The agent splits the command into words, as a shell does, and runs them, with a
	// PATH of its own: the tests often empty the one of hop. Theirs comes first, so
	// that a test can replace a tool (sudo).
	cmd := exec.Command("/bin/sh", "-c", req.Parameters["command"][0])
	cmd.Env = append(os.Environ(), "PATH="+os.Getenv("PATH")+":/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin")
	cmd.Stdout, cmd.Stderr = w, w
	var ee *exec.ExitError
	if err := cmd.Run(); err != nil && !errors.As(err, &ee) {
		fmt.Fprintf(w, "fake agent: %v\n", err)
	}
	if w.cut >= 0 {
		return 0
	}
	fmt.Print("\n\nExiting session with sessionId: acme-user-0abc.\n\n")
	return 0
}

// output passes the command's output on, cut or altered as asked.
type output struct {
	w     io.Writer
	cut   int    // bytes to pass on, -1 for all
	flip  int    // the base64 character to change, counted from 1 after the begin line
	head  []byte // the output up to the begin line
	begun bool
	after int // bytes written after the begin line
	n     int // bytes passed on
}

func (o *output) Write(p []byte) (int, error) {
	q := append([]byte(nil), p...)
	for i, c := range q {
		if !o.begun {
			o.head = append(o.head, c)
			o.begun = bytes.HasSuffix(o.head, []byte("-begin\n"))
			continue
		}
		o.after++
		if o.after == o.flip {
			if c == 'A' {
				q[i] = 'B'
			} else {
				q[i] = 'A'
			}
		}
	}
	if o.cut >= 0 {
		if o.n >= o.cut {
			o.n += len(q)
			return len(p), nil
		}
		if o.n+len(q) > o.cut {
			q = q[:o.cut-o.n]
		}
	}
	o.n += len(q)
	o.w.Write(q)
	return len(p), nil
}
