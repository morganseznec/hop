package cli

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/morganseznec/hop/internal/inventory"
	"github.com/morganseznec/hop/internal/session"
)

func newRun(f *filters) *cobra.Command {
	var wait time.Duration
	cmd := &cobra.Command{
		Use:   "run <instance> <command...>",
		Short: "Run a command and print its output (replaces ssh host 'cmd')",
		Long: `Run a command and print its output (replaces ssh host 'cmd').

The command runs as root, through SSM Run Command (AWS-RunShellScript), and its
output is printed once it ends: up to 24,000 characters per stream, where SSM cuts
it. The remote exit code becomes hop's exit code. Options placed after the instance
name belong to the command: "hop run tools ls -la". Linux and macOS instances only.

Session logging does not apply: the command and its output stay in the SSM command
history of the account, for about 30 days. CloudTrail records who sent the command
and where, without its text. A command that has not reached the instance when hop
stops waiting is canceled; one already running goes on.`,
		Example: `  hop run tools uptime
  hop run tools df -h /                       options after the instance go to the command
  hop run tools 'journalctl -u app | tail'    quote pipes and redirections
  hop run --timeout 15m tools /opt/backup.sh  wait longer than the default 2m`,
		Args:              cobra.MinimumNArgs(2),
		ValidArgsFunction: completeNth(f, 0),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := newApp(*f)
			if err != nil {
				return err
			}
			in, cfg, err := a.reach(cmd.Context(), args[:1], forRun)
			if err != nil {
				return err
			}
			a.warnAgent(in)
			a.touch(in)
			// Always say where the command is going, even from a script.
			info("%s (%s, %s, %s)", in.Name, in.ID, in.Profile, in.Region)
			out, err := session.Run(cmd.Context(), cfg, in.ID, []string{strings.Join(args[1:], " ")}, wait)
			var te *session.TimeoutError
			if errors.As(err, &te) {
				hint("hop run --timeout 10m to wait longer")
				return err
			}
			if err != nil {
				return err
			}
			fmt.Fprint(os.Stdout, out.Stdout)
			fmt.Fprint(os.Stderr, out.Stderr)
			if out.Truncated {
				warn("output truncated by SSM at 24,000 characters")
			}
			if out.Status != "Success" {
				if out.ExitCode > 0 {
					return exitError{int(out.ExitCode)}
				}
				return fmt.Errorf("status %s", out.Status)
			}
			return nil
		},
	}
	cmd.Flags().SetInterspersed(false)
	cmd.Flags().DurationVar(&wait, "timeout", 2*time.Minute, "how long to wait for the output")
	return cmd
}

var pushShort = fmt.Sprintf("Upload a file of up to %d KB and verify its SHA-256 (replaces scp)", session.PushLimit/1000)

func newPush(f *filters) *cobra.Command {
	return &cobra.Command{
		Use:   "push <local-file> <instance> <remote-path>",
		Short: pushShort,
		Long: pushShort + `.

The file goes through SSM Run Command (AWS-RunShellScript) and is written as root to
an absolute path on the instance. An existing file keeps its owner and mode; a new
one belongs to root. The content stays in the SSM command history of the account for
about 30 days, base64-encoded: do not push secrets this way. CloudTrail records the
call without the content. Linux and macOS instances only; for larger files, go
through S3.`,
		Example: `  hop push ./app.conf tools /etc/app/app.conf
  hop push -p acme-prod ./motd web /etc/motd`,
		Args: cobra.ExactArgs(3),
		ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			switch len(args) {
			case 0:
				return nil, cobra.ShellCompDirectiveDefault
			case 1:
				return completeNth(f, 1)(cmd, args, toComplete)
			}
			return nil, cobra.ShellCompDirectiveNoFileComp
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if st, err := os.Stat(args[0]); err != nil || st.IsDir() {
				return fmt.Errorf("file not found: %s", args[0])
			}
			// Before the picker, a question or a sign-in, which would all be for nothing.
			if err := session.CheckPush(args[0], args[2]); err != nil {
				return err
			}
			a, err := newApp(*f)
			if err != nil {
				return err
			}
			in, cfg, err := a.reach(cmd.Context(), args[1:2], forPush)
			if err != nil {
				return err
			}
			a.warnAgent(in)
			a.touch(in)
			info("%s -> %s:%s", args[0], in.Name, args[2])
			out, err := session.Push(cmd.Context(), cfg, in.ID, args[0], args[2])
			// The remote shell says why a copy failed (permission denied, no such
			// directory); the status alone does not.
			if out != nil && out.Stderr != "" {
				fmt.Fprint(os.Stderr, out.Stderr)
			}
			if err != nil {
				return err
			}
			info("copied, SHA-256 matches on both sides")
			return nil
		},
	}
}

func newTunnel(f *filters) *cobra.Command {
	var host string
	cmd := &cobra.Command{
		Use:   "tunnel <instance> <remote-port> [local-port]",
		Short: "Forward a port to the instance or a host it can reach (replaces ssh -L)",
		Long: `Forward a port to the instance or a host it can reach (replaces ssh -L).

Opens no shell, so Session Manager logs nothing of what goes through it. With --host,
the instance relays to another machine on its network (RDS, database, internal
service). Without a local port, a remote port below 1024 is opened 8000 above, since
lower ports usually need administrator rights.`,
		Example: `  hop tunnel tools 3000                       http://localhost:3000
  hop tunnel web 443                          https://localhost:8443
  hop tunnel tools 5432 15432 --host db.internal
  hop tunnel tools 22                         then: scp -P 8022 file user@localhost:/tmp`,
		Args:              cobra.RangeArgs(2, 3),
		ValidArgsFunction: completeNth(f, 0),
		RunE: func(cmd *cobra.Command, args []string) error {
			remote, explicit := args[1], ""
			if len(args) == 3 {
				explicit = args[2]
			}
			for _, p := range args[1:] {
				if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
					return fmt.Errorf("invalid port: %s", p)
				}
			}
			local, err := localPort(remote, explicit, portFree)
			if err != nil {
				return err
			}
			a, err := newApp(*f)
			if err != nil {
				return err
			}
			in, cfg, err := a.reach(cmd.Context(), args[:1], forTunnel)
			if err != nil {
				return err
			}
			a.warnAgent(in)
			a.touch(in)
			req := session.Request{
				Profile: in.Profile, Region: in.Region, InstanceID: in.ID, PreferSSO: a.preferSSO(in.Profile),
				Document:   "AWS-StartPortForwardingSession",
				Parameters: map[string][]string{"portNumber": {remote}, "localPortNumber": {local}},
			}
			dest := in.Name
			if host != "" {
				req.Document = "AWS-StartPortForwardingSessionToRemoteHost"
				req.Parameters["host"] = []string{host}
				dest = host + " via " + in.Name
			}
			info("localhost:%s -> %s:%s (Ctrl-C to close)", local, dest, remote)
			return pluginExit(session.Start(cmd.Context(), cfg, req))
		},
	}
	cmd.Flags().StringVar(&host, "host", "", "host to reach from the instance, instead of the instance itself")
	return cmd
}

// posixOnly refuses Windows instances: run and push send a POSIX shell script through
// AWS-RunShellScript, which Windows does not run.
func posixOnly(in inventory.Instance) error {
	// EC2 says "Windows" or "Windows with SQL Server ..."; the SSM agent says
	// "Microsoft Windows Server 2022 Datacenter".
	if strings.Contains(strings.ToLower(in.Platform), "windows") {
		return fmt.Errorf("%s runs %s: hop run and hop push support Linux and macOS instances; use a shell session (hop %s)", in.Name, in.Platform, in.Name)
	}
	return nil
}

// localPort chooses the local end of a tunnel. An explicit port is used as given, or
// refused with an alternative. By default it is the remote port, except below 1024,
// where listening usually needs administrator rights: 443 becomes 8443, 80 becomes
// 8080. A default port in use moves to the next free one.
func localPort(remote, explicit string, free func(string) bool) (string, error) {
	if explicit != "" {
		if free(explicit) {
			return explicit, nil
		}
		msg := "local port " + explicit + " already in use"
		if n, _ := strconv.Atoi(explicit); n < 1024 {
			msg = "local port " + explicit + " cannot be opened: it may be reserved to administrators (below 1024) or in use"
		}
		if alt := alternativePort(explicit, free); alt != "" {
			msg += " (try " + alt + ")"
		}
		return "", errors.New(msg)
	}
	n, _ := strconv.Atoi(remote)
	if n >= 1024 && free(remote) {
		return remote, nil
	}
	alt := alternativePort(remote, free)
	switch {
	case alt == "":
		return "", fmt.Errorf("no free local port found for %s: give one as the third argument", remote)
	case n >= 1024:
		info("local port %s already in use, using %s", remote, alt)
	}
	return alt, nil
}

// alternativePort returns a free local port to use instead of p: p+8000 for a port
// below 1024 (or the next free one after it), the next free one after p otherwise.
func alternativePort(p string, free func(string) bool) string {
	n, _ := strconv.Atoi(p)
	if n < 1024 {
		if c := strconv.Itoa(n + 8000); free(c) {
			return c
		}
		return nextFreePort(strconv.Itoa(n+8000), free)
	}
	return nextFreePort(p, free)
}

func portFree(p string) bool {
	l, err := net.Listen("tcp", "127.0.0.1:"+p)
	if err != nil {
		return false
	}
	l.Close()
	return true
}

func nextFreePort(from string, free func(string) bool) string {
	n, _ := strconv.Atoi(from)
	for p := n + 1; p <= min(n+100, 65535); p++ {
		if free(strconv.Itoa(p)) {
			return strconv.Itoa(p)
		}
	}
	return ""
}
