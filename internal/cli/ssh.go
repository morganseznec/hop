package cli

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/morganseznec/hop/internal/config"
	"github.com/morganseznec/hop/internal/session"
)

// sshSuffix marks the hosts that ssh hands to hop (Host *.hop in ~/.ssh/config).
const sshSuffix = ".hop"

func newSSHProxy(f *filters) *cobra.Command {
	return &cobra.Command{
		Use:   "ssh-proxy <host> [port]",
		Short: "Carry an SSH connection through Session Manager (ProxyCommand for ssh, scp, rsync)",
		Long: `Carry an SSH connection through Session Manager (ProxyCommand for ssh, scp, rsync).

Set it up once with "hop init ssh >> ~/.ssh/config", then reach any instance by
what hop finds it by, followed by .hop:

    ssh ec2-user@tools.hop
    scp ./dump.sql ec2-user@tools.hop:/tmp/
    rsync -a ./site/ ec2-user@web.hop:/srv/site/

The host is what ssh was given, without .hop: a name, an instance ID or a private IP.
ssh gives hop no terminal to ask from, so only an unambiguous match connects (an
exact name, ID or IP, or whole words of a single name). Add -p, -r or -e to the
ProxyCommand to narrow the search.

The instance needs sshd running and your public key in the user's
~/.ssh/authorized_keys. Session Manager does not log what goes through SSH: with
"transcript: require" in hop's config, hop refuses.`,
		Example: `  hop init ssh >> ~/.ssh/config     once
  ssh ec2-user@tools.hop
  ssh -L 5432:db.internal:5432 ec2-user@tools.hop
  git clone ec2-user@tools.hop:/srv/repo.git

  In ~/.ssh/config, to search only some profiles:
    ProxyCommand hop ssh-proxy -p 'acme-*' -- %h %p`,
		Args:              cobra.RangeArgs(1, 2),
		ValidArgsFunction: completeNth(f, 0),
		RunE: func(cmd *cobra.Command, args []string) error {
			port := "22"
			if len(args) == 2 {
				port = args[1]
			}
			if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
				return fmt.Errorf("invalid port: %s", port)
			}
			host := sshHost(args[0])
			if host == "" {
				return fmt.Errorf("no instance in %q", args[0])
			}
			a, err := newApp(*f)
			if err != nil {
				return err
			}
			if a.cfg.TranscriptPolicy() == config.TranscriptRequire {
				return fmt.Errorf("ssh refused (transcript: require): Session Manager does not log SSH connections. For a logged shell: hop %s", host)
			}
			// stdin and stdout carry the SSH stream: resolve never asks or opens the
			// picker there (no terminal), and nothing but the plugin writes to stdout.
			in, cfg, err := a.reach(cmd.Context(), []string{host}, forTunnel)
			if err != nil {
				return err
			}
			a.warnAgent(in)
			a.touch(in)
			return pluginExit(session.Start(cmd.Context(), cfg, session.Request{
				Profile: in.Profile, Region: in.Region, InstanceID: in.ID, PreferSSO: a.preferSSO(in.Profile),
				Document:   "AWS-StartSSHSession",
				Parameters: map[string][]string{"portNumber": {port}},
			}))
		},
	}
}

// sshHost returns the instance ssh asked for: the host without .hop.
func sshHost(host string) string {
	if len(host) >= len(sshSuffix) && strings.EqualFold(host[len(host)-len(sshSuffix):], sshSuffix) {
		host = host[:len(host)-len(sshSuffix)]
	}
	return strings.TrimSpace(host)
}

// sshConfig is the ~/.ssh/config block that sends the *.hop hosts through hop.
func sshConfig(hop string) string {
	return `# hop: SSH through Session Manager. Add it with: hop init ssh >> ~/.ssh/config
# Then: ssh ec2-user@tools.hop, scp ./file ec2-user@tools.hop:/tmp/ (see hop ssh-proxy --help)
Host *` + sshSuffix + `
    ProxyCommand ` + hop + ` ssh-proxy -- %h %p
`
}

// sshCommand is how ~/.ssh/config should run hop: by its full path, since editors and
// other desktop apps start ssh with a short PATH. It keeps the path found on PATH when
// that is this binary, so that a Homebrew symlink survives upgrades.
func sshCommand() string {
	self, err := os.Executable()
	if err != nil {
		return "hop"
	}
	if onPath, err := exec.LookPath("hop"); err == nil && sameFile(onPath, self) {
		self = onPath
	}
	return quoteProxyCommand(self)
}

// safePath is what a POSIX shell takes as one plain word.
var safePath = regexp.MustCompile(`^[A-Za-z0-9_./+@:,=%-]+$`)

// quoteProxyCommand quotes a program path for the ProxyCommand line. ssh runs it with
// the user's shell, except Windows OpenSSH, which starts it directly: there, a path
// with spaces only needs double quotes. % starts an ssh token, %% is a literal one.
func quoteProxyCommand(path string) string {
	path = strings.ReplaceAll(path, "%", "%%")
	switch {
	case isWindowsPath(path):
		if strings.ContainsAny(path, " \t") {
			return `"` + path + `"`
		}
		return path
	case safePath.MatchString(path):
		return path
	}
	return "'" + strings.ReplaceAll(path, "'", `'\''`) + "'"
}

func sameFile(a, b string) bool {
	fa, errA := os.Stat(a)
	fb, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(fa, fb)
}

// isWindowsPath reports a path that starts with a drive letter.
func isWindowsPath(p string) bool {
	return len(p) > 2 && p[1] == ':' && (p[2] == '\\' || p[2] == '/')
}
