package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// runHop runs the hop command tree with args and returns stdout, stderr and the error.
func runHop(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	var err error
	var stderr string
	stdout := capture(t, &os.Stdout, func() {
		stderr = capture(t, &os.Stderr, func() {
			root := newRoot()
			root.SetArgs(args)
			err = root.ExecuteContext(context.Background())
		})
	})
	return stdout, stderr, err
}

func TestInitZsh(t *testing.T) {
	isolate(t)
	terminal(t, false, false)
	cases := []struct {
		name  string
		args  []string
		alias string
	}{
		{"without alias", []string{"init", "zsh"}, ""},
		{"with alias", []string{"init", "zsh", "--alias", "ssm"}, "ssm"},
		{"alias with digits, dash and underscore", []string{"init", "zsh", "--alias=my_hop-2"}, "my_hop-2"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, _, err := runHop(t, c.args...)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(out, "# hop: eval \"$(hop init zsh)\" in ~/.zshrc\n") {
				t.Errorf("header missing:\n%.200s", out)
			}
			aliasLines := "if (( $+functions[" + c.alias + "] )); then unfunction -- " + c.alias + "; fi\nalias -- " + c.alias + "=hop\n"
			if c.alias != "" && !strings.Contains(out, aliasLines) {
				t.Errorf("alias lines missing:\n%.400s", out)
			}
			if c.alias == "" && strings.Contains(out, "alias --") {
				t.Errorf("alias defined without --alias:\n%.400s", out)
			}
			// The completion is guarded: compdef only exists after compinit.
			guard := "if (( $+functions[compdef] )); then\n"
			if !strings.Contains(out, guard) || !strings.HasSuffix(out, "fi\n") {
				t.Errorf("completion not guarded:\n%s", out)
			}
			if !strings.Contains(out, "compdef _hop hop") {
				t.Errorf("completion function not registered")
			}
			if i, j := strings.Index(out, guard), strings.Index(out, "compdef _hop hop"); i > j {
				t.Errorf("compdef called before the guard")
			}
		})
	}
}

func TestInitRejects(t *testing.T) {
	isolate(t)
	terminal(t, false, false)
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"other shell", []string{"init", "bash"}, "only zsh is supported"},
		{"no shell", []string{"init"}, "accepts 1 arg"},
		{"command injection", []string{"init", "zsh", "--alias", "ssm; rm -rf ~"}, "invalid alias name"},
		{"command substitution", []string{"init", "zsh", "--alias", "$(id)"}, "invalid alias name"},
		{"space", []string{"init", "zsh", "--alias", "my hop"}, "invalid alias name"},
		{"quote", []string{"init", "zsh", "--alias", "x'y"}, "invalid alias name"},
		{"trailing newline", []string{"init", "zsh", "--alias", "ssm\n"}, "invalid alias name"},
		{"non-ASCII letter", []string{"init", "zsh", "--alias", "hôp"}, "invalid alias name"},
		{"equal sign", []string{"init", "zsh", "--alias", "ssm=ls"}, "invalid alias name"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, _, err := runHop(t, c.args...)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %v, want %q", err, c.want)
			}
			if out != "" {
				t.Errorf("nothing must be printed on error, got:\n%.200s", out)
			}
		})
	}
}

// zshScript writes the output of hop init zsh to a file and returns zsh's path.
func zshScript(t *testing.T, args ...string) (zsh, script string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("zsh is not available on Windows")
	}
	zsh, err := exec.LookPath("zsh")
	if err != nil {
		t.Skip("zsh not installed")
	}
	isolate(t)
	terminal(t, false, false)
	out, _, err := runHop(t, append([]string{"init", "zsh"}, args...)...)
	if err != nil {
		t.Fatal(err)
	}
	script = filepath.Join(t.TempDir(), "hop.zsh")
	if err := os.WriteFile(script, []byte(out), 0o600); err != nil {
		t.Fatal(err)
	}
	return zsh, script
}

// zshRun runs code in a zsh that reads no startup file.
func zshRun(t *testing.T, zsh, code string) (string, error) {
	t.Helper()
	cmd := exec.Command(zsh, "-f", "-c", code)
	cmd.Env = append(os.Environ(), "ZDOTDIR="+t.TempDir())
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestInitZshSyntax(t *testing.T) {
	for _, args := range [][]string{nil, {"--alias", "ssm"}} {
		zsh, script := zshScript(t, args...)
		if out, err := exec.Command(zsh, "-n", script).CombinedOutput(); err != nil {
			t.Errorf("zsh -n %v: %v\n%s", args, err, out)
		}
	}
}

func TestInitZshBehavior(t *testing.T) {
	zsh, script := zshScript(t, "--alias", "ssm")

	t.Run("without compinit, sourcing is silent and harmless", func(t *testing.T) {
		out, err := zshRun(t, zsh, "source '"+script+"' && print ok")
		if err != nil || out != "ok\n" {
			t.Errorf("%v: %q", err, out)
		}
	})
	t.Run("the alias replaces a function of the same name", func(t *testing.T) {
		out, err := zshRun(t, zsh, "ssm() { print function }; source '"+script+"'; whence -w ssm; alias ssm")
		if err != nil || out != "ssm: alias\nssm=hop\n" {
			t.Errorf("%v: %q", err, out)
		}
	})
	t.Run("with compinit, the completion is registered", func(t *testing.T) {
		out, err := zshRun(t, zsh, "autoload -Uz compinit && compinit -u -D && source '"+script+"' && print -r -- ${_comps[hop]}")
		if err != nil || out != "_hop\n" {
			t.Errorf("%v: %q", err, out)
		}
	})
}
