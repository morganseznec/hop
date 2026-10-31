package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/morganseznec/hop/internal/config"
)

func TestSSHHost(t *testing.T) {
	cases := map[string]string{
		"acme-p-tools.hop": "acme-p-tools",
		"ACME-P-TOOLS.HOP": "ACME-P-TOOLS",
		"i-0abc.hop":       "i-0abc",
		"10.0.1.21.hop":    "10.0.1.21",
		"i-0abc":           "i-0abc",
		"web.hop.hop":      "web.hop",
		"hop":              "hop",
		".hop":             "",
		" .hop":            "",
	}
	for in, want := range cases {
		if got := sshHost(in); got != want {
			t.Errorf("sshHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestQuoteProxyCommand(t *testing.T) {
	cases := map[string]string{
		"/opt/homebrew/bin/hop":        "/opt/homebrew/bin/hop",
		"/home/acme/go/bin/hop":        "/home/acme/go/bin/hop",
		"/Users/acme user/bin/hop":     "'/Users/acme user/bin/hop'",
		"/Users/acme/it's/hop":         `'/Users/acme/it'\''s/hop'`,
		"/Users/acme/$(id)/hop":        "'/Users/acme/$(id)/hop'",
		"/Users/acme/100%/hop":         "/Users/acme/100%%/hop",
		`C:\Tools\hop.exe`:             `C:\Tools\hop.exe`,
		`C:\Program Files\hop\hop.exe`: `"C:\Program Files\hop\hop.exe"`,
	}
	for in, want := range cases {
		if got := quoteProxyCommand(in); got != want {
			t.Errorf("quoteProxyCommand(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestInitSSH(t *testing.T) {
	isolate(t)
	terminal(t, false, false)
	t.Setenv("PATH", t.TempDir())
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	check := func(t *testing.T, hop string) {
		t.Helper()
		out, _, err := runHop(t, "init", "ssh")
		if err != nil {
			t.Fatal(err)
		}
		want := "Host *.hop\n    ProxyCommand " + quoteProxyCommand(hop) + " ssh-proxy -- %h %p\n"
		if !strings.HasSuffix(out, want) {
			t.Errorf("want the block to end with:\n%s\ngot:\n%s", want, out)
		}
		for _, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
			if !strings.HasPrefix(line, "#") && !strings.HasPrefix(line, "Host ") && !strings.HasPrefix(line, "    ProxyCommand ") {
				t.Errorf("unexpected line in ~/.ssh/config: %q", line)
			}
		}
	}
	t.Run("hop not on PATH: this binary", func(t *testing.T) { check(t, self) })
	t.Run("hop on PATH through a link: the link, which survives upgrades", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("symbolic links need privileges on Windows")
		}
		bin := t.TempDir()
		link := filepath.Join(bin, "hop")
		if err := os.Symlink(self, link); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", bin)
		check(t, link)
	})
	t.Run("another hop on PATH: this binary", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("the other hop is a shell script")
		}
		bin := t.TempDir()
		os.WriteFile(filepath.Join(bin, "hop"), []byte("#!/bin/sh\n"), 0o755)
		t.Setenv("PATH", bin)
		check(t, self)
	})
	t.Run("--alias is refused", func(t *testing.T) {
		out, _, err := runHop(t, "init", "ssh", "--alias", "ssm")
		if err == nil || !strings.Contains(err.Error(), "--alias is for shells") || out != "" {
			t.Errorf("%v, %q", err, out)
		}
	})
}

// echoPlugin puts first on PATH a session-manager-plugin that records its arguments,
// then copies its stdin to its stdout, as the real one does with the SSH stream.
func echoPlugin(t *testing.T) (out string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake plugin is a shell script")
	}
	bin, out := t.TempDir(), t.TempDir()
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$HOP_TEST_OUT/args\"\nexec /bin/cat\n"
	if err := os.WriteFile(filepath.Join(bin, "session-manager-plugin"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("HOP_TEST_OUT", out)
	return out
}

func TestSSHProxy(t *testing.T) {
	f := fakeAWSEnv(t)
	cacheBillingApp(t, 5*time.Minute)
	out := echoPlugin(t)
	stdin(t, "SSH-2.0-OpenSSH_9.9\r\nfrom the client\n")

	stdout, stderr, err := runHop(t, "ssh-proxy", "--", "acme-b-app.hop", "2222")
	if err != nil {
		t.Fatal(err)
	}
	// stdout is the SSH stream: what ssh writes comes back untouched, and hop adds
	// nothing to it, nor to stderr, which ssh shows under every scp or rsync.
	if stdout != "SSH-2.0-OpenSSH_9.9\r\nfrom the client\n" {
		t.Errorf("stream changed: %q", stdout)
	}
	if stderr != "" {
		t.Errorf("hop wrote to stderr: %q", stderr)
	}
	f.mu.Lock()
	sessions := f.sessions
	f.mu.Unlock()
	if len(sessions) != 1 {
		t.Fatalf("%d sessions opened", len(sessions))
	}
	s := sessions[0]
	params, _ := s["Parameters"].(map[string]any)
	if s["Target"] != "i-0b01" || s["DocumentName"] != "AWS-StartSSHSession" || len(params) != 1 || strings.Join(toStrings(params["portNumber"]), ",") != "2222" {
		t.Errorf("StartSession request: %v", s)
	}
	if args := readTestFile(t, filepath.Join(out, "args")); !strings.Contains(args, `"DocumentName":"AWS-StartSSHSession"`) {
		t.Errorf("plugin arguments: %s", args)
	}
}

func TestSSHProxyDefaultPort(t *testing.T) {
	f := fakeAWSEnv(t)
	cacheBillingApp(t, 5*time.Minute)
	echoPlugin(t)
	stdin(t, "")
	if _, _, err := runHop(t, "ssh-proxy", "i-0b01"); err != nil {
		t.Fatal(err)
	}
	if len(f.sessions) != 1 || strings.Join(toStrings(f.sessions[0]["Parameters"].(map[string]any)["portNumber"]), ",") != "22" {
		t.Errorf("StartSession requests: %v", f.sessions)
	}
}

func TestSSHProxyRefuses(t *testing.T) {
	cases := []struct {
		name string
		args []string
		cfg  *config.Config
		want string
	}{
		{"a partial name", []string{"ssh-proxy", "acme-b-ap.hop", "22"}, nil, `"acme-b-ap" only partly matches acme-b-app`},
		{"an unknown host", []string{"ssh-proxy", "nowhere.hop", "22"}, nil, `no instance matches "nowhere"`},
		{"an empty host", []string{"ssh-proxy", ".hop", "22"}, nil, `no instance in ".hop"`},
		{"an invalid port", []string{"ssh-proxy", "acme-b-app.hop", "ssh"}, nil, "invalid port: ssh"},
		{"transcript: require", []string{"ssh-proxy", "acme-b-app.hop", "22"}, &config.Config{Transcript: config.TranscriptRequire},
			"ssh refused (transcript: require): Session Manager does not log SSH connections. For a logged shell: hop acme-b-app"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := fakeAWSEnv(t)
			cacheBillingApp(t, 5*time.Minute)
			if c.cfg != nil {
				if err := c.cfg.Save(); err != nil {
					t.Fatal(err)
				}
			}
			echoPlugin(t)
			stdin(t, "SSH-2.0-OpenSSH_9.9\r\n")
			stdout, _, err := runHop(t, c.args...)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %v, want %q", err, c.want)
			}
			if stdout != "" {
				t.Errorf("stdout must stay empty for ssh: %q", stdout)
			}
			if len(f.sessions) != 0 {
				t.Errorf("a session was opened: %v", f.sessions)
			}
		})
	}
}

func toStrings(v any) []string {
	items, _ := v.([]any)
	var out []string
	for _, it := range items {
		s, _ := it.(string)
		out = append(out, s)
	}
	return out
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
