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
		{"powershell", []string{"init", "powershell"}, "for PowerShell, see hop completion powershell --help"},
		{"unknown shell", []string{"init", "tcsh"}, `unknown target "tcsh": zsh, bash, fish or ssh`},
		{"alias like an option", []string{"init", "bash", "--alias=-f"}, "invalid alias name"},
		{"alias like a long option", []string{"init", "fish", "--alias", "--help"}, "invalid alias name"},
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

func TestInitBashAndFish(t *testing.T) {
	isolate(t)
	terminal(t, false, false)
	cases := []struct {
		shell, header, registered, alias string
	}{
		{"bash", "# hop: eval \"$(hop init bash)\" in ~/.bashrc\n", "complete -o default -F __start_hop hop",
			"unset -f -- ssm 2>/dev/null\nalias -- ssm=hop\n"},
		{"fish", "# hop: hop init fish | source, in ~/.config/fish/config.fish\n", "complete -c hop ",
			"function ssm --wraps hop --description 'alias ssm=hop'\n    hop $argv\nend\n"},
	}
	for _, c := range cases {
		t.Run(c.shell, func(t *testing.T) {
			out, _, err := runHop(t, "init", c.shell)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(out, c.header) || !strings.Contains(out, c.registered) {
				t.Errorf("header or completion missing:\n%.300s", out)
			}
			if strings.Contains(out, "ssm") {
				t.Errorf("alias defined without --alias")
			}
			withAlias, _, err := runHop(t, "init", c.shell, "--alias", "ssm")
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(withAlias, out) || !strings.Contains(withAlias, c.alias) {
				t.Errorf("want the same script plus the alias %q, got the end:\n%s", c.alias, withAlias[len(out)-min(len(out), 200):])
			}
		})
	}
}

// bashRun runs code in a bash that reads no startup file, after before and hop init
// bash with args.
func bashRun(t *testing.T, before, code string, args ...string) (string, error) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("bash is not available on every Windows runner")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not installed")
	}
	path := os.Getenv("PATH")
	isolate(t)
	terminal(t, false, false)
	out, _, err := runHop(t, append([]string{"init", "bash"}, args...)...)
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(t.TempDir(), "hop.bash")
	if err := os.WriteFile(script, []byte(out), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bash, "--norc", "--noprofile", "-c", before+"\nsource '"+script+"'\n"+code)
	cmd.Env = append(os.Environ(), "PATH="+path)
	b, err := cmd.CombinedOutput()
	return string(b), err
}

func TestInitBashBehavior(t *testing.T) {
	t.Run("sourcing is silent and registers the completion", func(t *testing.T) {
		out, err := bashRun(t, "", "complete -p hop")
		if err != nil || !strings.HasSuffix(out, " -F __start_hop hop\n") || strings.Count(out, "\n") != 1 {
			t.Errorf("%v: %q", err, out)
		}
	})
	t.Run("the alias replaces a function of the same name", func(t *testing.T) {
		out, err := bashRun(t, "shopt -s expand_aliases; ssm() { echo function; }",
			`type -t ssm; alias ssm; a=$(complete -p ssm); b=$(complete -p hop); [ "${a% ssm}" = "${b% hop}" ] && echo same completion`, "--alias", "ssm")
		if err != nil || out != "alias\nalias ssm='hop'\nsame completion\n" {
			t.Errorf("%v: %q", err, out)
		}
	})
	t.Run("the alias completes like hop", func(t *testing.T) {
		// A hop that answers __complete as cobra does: the completion runs the word
		// typed, which the alias turns into hop.
		bin := t.TempDir()
		os.WriteFile(filepath.Join(bin, "hop"), []byte("#!/bin/sh\n[ \"$1 $2\" = \"__complete l\" ] && printf 'ls\\tList the instances\\nlogin\\tSign in\\n:4\\n'\n"), 0o755)
		t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
		out, err := bashRun(t, "shopt -s expand_aliases",
			`COMP_WORDS=(ssm l); COMP_CWORD=1; COMP_LINE="ssm l"; COMP_POINT=5; __start_hop ssm 2>/dev/null; printf '%s\n' "${COMPREPLY[@]}"`, "--alias", "ssm")
		if err != nil || out != "ls\nlogin\n" {
			t.Errorf("%v: %q", err, out)
		}
	})
	t.Run("with the bash-completion package, its word splitting applies", func(t *testing.T) {
		bin := t.TempDir()
		os.WriteFile(filepath.Join(bin, "hop"), []byte("#!/bin/sh\n[ \"$1 $2\" = \"__complete l\" ] && printf 'ls\\n:4\\n'\n"), 0o755)
		t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
		stub := `_get_comp_words_by_ref() { called="bash-completion $*"; cur=l; prev=hop; words=(hop l); cword=1; }`
		// compopt complains outside a real completion: its message is left out.
		out, err := bashRun(t, stub, `COMP_WORDS=(hop l); COMP_CWORD=1; __start_hop hop 2>/dev/null; echo "$called"; printf '%s\n' "${COMPREPLY[@]}"`)
		if err != nil || out != "bash-completion -n =: cur prev words cword\nls\n" {
			t.Errorf("%v: %q", err, out)
		}
	})
}

func TestInitFishBehavior(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fish does not run on Windows")
	}
	fish, err := exec.LookPath("fish")
	if err != nil {
		t.Skip("fish not installed")
	}
	path := os.Getenv("PATH")
	isolate(t)
	terminal(t, false, false)
	out, _, err := runHop(t, "init", "fish", "--alias", "ssm")
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(t.TempDir(), "hop.fish")
	os.WriteFile(script, []byte(out), 0o600)
	// A hop that answers __complete as cobra does.
	bin := t.TempDir()
	os.WriteFile(filepath.Join(bin, "hop"), []byte("#!/bin/sh\n[ \"$1 $2\" = \"__complete l\" ] && printf 'ls\\tList the instances\\nlogin\\tSign in\\n:4\\n'\n"), 0o755)

	code := "function ssm; echo function; end; source " + script + "; functions -D ssm >/dev/null; and complete -C 'ssm l'; complete -C 'hop l'"
	cmd := exec.Command(fish, "--no-config", "-c", code)
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+path, "HOME="+t.TempDir())
	b, err := cmd.CombinedOutput()
	// fish sorts the candidates.
	want := "login\tSign in\nls\tList the instances\n"
	if err != nil || string(b) != want+want {
		t.Errorf("%v: %q", err, b)
	}
}
