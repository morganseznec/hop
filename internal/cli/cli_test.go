package cli

import (
	"net"
	"strconv"
	"strings"
	"testing"
	"unicode"

	"github.com/spf13/cobra"
)

func TestVersionLess(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"1.2.764.0", "1.2.497.0", false},
		{"1.2.463.0", "1.2.497.0", true},
		{"1.2.497.0", "1.2.497.0", false},
		{"1.3", "1.2.999.0", false},
	}
	for _, c := range cases {
		if got := versionLess(c.a, c.b); got != c.want {
			t.Errorf("versionLess(%s, %s) = %v", c.a, c.b, got)
		}
	}
}

func TestPortFallback(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip(err)
	}
	defer l.Close()
	busy := strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
	if portFree(busy) {
		t.Fatalf("port %s is in use but reported free", busy)
	}
	next := nextFreePort(busy, portFree)
	if next == "" || next == busy || !portFree(next) {
		t.Errorf("fallback port: %q", next)
	}
}

func TestPlural(t *testing.T) {
	cases := []struct {
		n    int
		word string
		want string
	}{
		{1, "profile", "1 profile"},
		{3, "profile", "3 profiles"},
		{0, "instance", "0 instances"},
		{2, "profile/region pair", "2 profile/region pairs"},
	}
	for _, c := range cases {
		if got := plural(c.n, c.word); got != c.want {
			t.Errorf("plural(%d, %q) = %q, want %q", c.n, c.word, got, c.want)
		}
	}
}

// Cobra shows Long instead of Short in a command's help: each Long starts with its
// Short, so that the summary is not lost. Shorts start with a capitalized verb.
func TestHelpTexts(t *testing.T) {
	isolate(t)
	root := newRoot()
	cmds := append([]*cobra.Command{root}, root.Commands()...)
	withExamples := map[string]bool{"push": true, "run": true, "login": true, "scan": true, "doctor": true}
	for _, c := range cmds {
		// Cobra's own commands (help, completion) keep its wording.
		if c.Hidden || c.Name() == "help" || c.Name() == "completion" {
			continue
		}
		if c.Short == "" || !unicode.IsUpper([]rune(c.Short)[0]) || strings.HasSuffix(c.Short, ".") {
			t.Errorf("%s: Short %q must start with a capital and have no final period", c.Name(), c.Short)
		}
		if c.Long != "" && !strings.HasPrefix(c.Long, c.Short+".") {
			t.Errorf("%s: Long must start with the Short sentence, got %.80q", c.Name(), c.Long)
		}
		if withExamples[c.Name()] && c.Example == "" {
			t.Errorf("%s: no Example", c.Name())
		}
		if strings.ContainsRune(c.Short+c.Long+c.Example, '\u2014') {
			t.Errorf("%s: em-dash in the help text", c.Name())
		}
	}
}

// Options after the instance name belong to the remote command, as "hop run --help"
// shows: "-h" there must not print hop's help.
func TestRunPassesOptionsToTheCommand(t *testing.T) {
	isolate(t)
	terminal(t, false, false)
	stdout, _, err := runHop(t, "run", "tools", "df", "-h", "/")
	if strings.Contains(stdout, "Usage:") {
		t.Errorf("-h after the instance printed the help:\n%s", stdout)
	}
	// No profile in the isolated home: run stops before any AWS call.
	if err == nil || !strings.Contains(err.Error(), "no AWS profile with credentials") {
		t.Errorf("error %v", err)
	}
}
