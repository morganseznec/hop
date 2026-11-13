package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/spf13/cobra"

	"github.com/morganseznec/hop/internal/bucket"
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

const pushShort = "Upload a file and verify its SHA-256 (replaces scp)"

func newPush(f *filters) *cobra.Command {
	var force, relay bool
	cmd := &cobra.Command{
		Use:   "push <local-file> <instance> <remote-path>",
		Short: pushShort,
		Long: pushShort + fmt.Sprintf(`.

The file is written as root to an absolute path on the instance, by a command sent
through SSM Run Command (AWS-RunShellScript). An existing file keeps its owner and
mode; a new one belongs to root. Linux and macOS instances only.

Up to %d KB, the file travels in the command itself. Its content then stays in the
SSM command history of the account for about 30 days, base64-encoded: do not push
secrets this way. CloudTrail records the call without the content.

A larger file, or any file with --relay, goes through a relay bucket: hop uploads it
under a random key, the instance downloads it with a URL signed for that one object,
and hop deletes the object once the SHA-256 matches. The bucket must exist and be
named under relay_buckets in hop's configuration: hop never creates one. Your role
needs s3:PutObject, s3:GetObject, s3:DeleteObject and s3:AbortMultipartUpload on it;
the instance needs no S3 permission, only curl or wget, a way to reach S3, and room
for the file twice while it is copied. The SSM command history then keeps the URL,
expired, and not the file.

With s3://bucket/key in place of the instance and the path, the file goes to S3 and
stays there: see hop s3 push.`, session.PushLimit/1000),
		Example: `  hop push ./app.conf tools /etc/app/app.conf
  hop push -p acme-prod ./motd web /etc/motd
  hop push ./release.tar.gz web /opt/app/release.tar.gz   through the relay bucket
  hop push --relay ./api.key tools /etc/app/api.key       a small file, the same way
  hop push ./dump.sql.gz s3://acme-backups/db/            to a bucket (see hop s3 push)`,
		Args: func(cmd *cobra.Command, args []string) error {
			// To a bucket: one file or several, then the place.
			if len(args) >= 2 && bucket.HasScheme(args[len(args)-1]) {
				return nil
			}
			return cobra.ExactArgs(3)(cmd, args)
		},
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
			if bucket.HasScheme(args[len(args)-1]) {
				if relay {
					return errors.New("--relay is for a push to an instance: an s3:// destination is the bucket itself")
				}
				return runS3Push(cmd, f, force, args)
			}
			if force {
				return errors.New("--force is for an s3:// destination: a file pushed to an instance always replaces the one there")
			}
			// A pattern the shell left as it is (quoted, or zsh under noglob): the one
			// file it names.
			if _, err := os.Stat(args[0]); err != nil && bucket.HasMeta(args[0]) {
				switch matches, _ := filepath.Glob(args[0]); len(matches) {
				case 0:
				case 1:
					args[0] = matches[0]
				default:
					return fmt.Errorf("%s matches %s: hop push sends one file to an instance", args[0], plural(len(matches), "file"))
				}
			}
			st, err := os.Stat(args[0])
			if err != nil || st.IsDir() {
				return fmt.Errorf("file not found: %s", args[0])
			}
			if err := session.CheckRemote(args[2]); err != nil {
				return err
			}
			a, err := newApp(*f)
			if err != nil {
				return err
			}
			// Before the picker, a question or a sign-in, which would all be for nothing:
			// a file too large for SSM needs a relay bucket, and so does --relay.
			viaRelay := relay || st.Size() > session.PushLimit
			if viaRelay && len(a.cfg.RelayBuckets) == 0 {
				if relay {
					return fmt.Errorf("--relay needs a bucket: name one under relay_buckets in %s", configFile())
				}
				return session.CheckPush(args[0], args[2])
			}
			in, cfg, err := a.reach(cmd.Context(), args[1:2], forPush)
			if err != nil {
				return err
			}
			a.warnAgent(in)
			a.touch(in)
			if viaRelay {
				return a.pushVia(cmd.Context(), cfg, in, args[0], args[2], st.Size())
			}
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
	cmd.Flags().BoolVar(&force, "force", false, "with s3://, replace an existing object without asking")
	cmd.Flags().BoolVar(&relay, "relay", false, "go through the relay bucket whatever the size: the content stays out of the SSM history")
	return cmd
}

// pushVia copies a file to an instance through the relay bucket of its profile (see
// session.PushVia): the way for a file too large for SSM, or one whose content must
// stay out of the SSM command history.
func (a *app) pushVia(ctx context.Context, cfg aws.Config, in inventory.Instance, local, remote string, size int64) error {
	spec := a.cfg.RelayBucket(in.Profile)
	if spec == "" {
		return fmt.Errorf("%s (%s) is too large for SSM alone (limit %d KB), and relay_buckets in %s names no bucket for the profile %s",
			local, byteSize(size), session.PushLimit/1000, configFile(), in.Profile)
	}
	loc, err := bucket.Parse(spec)
	if err != nil || loc.Bucket == "" {
		return fmt.Errorf("relay_buckets in %s: %q is not a bucket", configFile(), spec)
	}
	if loc.Key != "" && !strings.HasSuffix(loc.Key, "/") {
		loc.Key += "/"
	}
	// The bucket may live in another region than the instance, or in another account.
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	region, err := inventory.BucketRegion(rctx, cfg, loc.Bucket)
	cancel()
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case err != nil:
		return fmt.Errorf("relay bucket %s (relay_buckets in %s): profile %s cannot find it: %s", loc.Bucket, configFile(), in.Profile, shortError(err.Error()))
	}
	relay := session.Relay{Config: cfg.Copy(), Bucket: loc.Bucket, Prefix: loc.Key}
	if region != "" {
		relay.Config.Region = region
	}
	info("%s -> %s:%s, %s through s3://%s", local, in.Name, remote, byteSize(size), loc.Bucket)
	p := &progress{tty: stderrTTY}
	res, err := session.PushVia(ctx, cfg, relay, in.ID, local, remote, p.update, func() {
		p.clear()
		info("uploaded: %s is fetching it (up to %s)", in.Name, session.RelayWait(size))
	})
	p.clear()
	if res.Output != nil && res.Output.Stderr != "" {
		fmt.Fprint(os.Stderr, res.Output.Stderr)
	}
	switch {
	case res.LeftErr != nil:
		warn("the temporary object %s could not be deleted (%s): delete it, or let a lifecycle rule of the bucket do it", res.Object, shortError(res.LeftErr.Error()))
	case res.Left:
		warn("the temporary object %s stays in the bucket, since %s may still be fetching it: delete it afterwards, or let a lifecycle rule of the bucket do it", res.Object, in.Name)
	}
	if err != nil {
		if errors.Is(err, bucket.ErrDenied) {
			hint("your role needs s3:PutObject, s3:GetObject, s3:DeleteObject and s3:AbortMultipartUpload on the relay bucket: %s", iamDocs)
		}
		return err
	}
	info("copied, SHA-256 matches on both sides")
	return nil
}

const pullShort = "Download a file and verify its SHA-256 (replaces scp from the instance)"

func newPull(f *filters) *cobra.Command {
	return &cobra.Command{
		Use:   "pull <instance> <remote-path> [local-path]",
		Short: pullShort,
		Long: pullShort + `.

The file goes through a Session Manager session that runs a single command
(AWS-StartNonInteractiveCommand): no SSH key, and no size limit. It is read as the
session user (ssm-user by default), or with sudo -n when that user cannot read it.
Without a local path, it lands in the current directory under its own name; given a
directory, it lands there under its own name.

hop writes it under a temporary name, then renames it once the SHA-256 matches on
both sides: an interrupted download leaves nothing behind. A new file is readable by
you only; an existing one is replaced and keeps its mode.

While it streams, the SSM agent keeps the output on the instance's disk, in base64:
a third more than the file. CloudTrail records the StartSession call, and when the
account logs sessions, the log may keep a copy of the file, base64-encoded. Linux
and macOS instances only.

With s3://bucket/key in place of the instance and the path, the object comes from S3
instead: see hop s3 pull.`,
		Example: `  hop pull tools /var/log/app.log                 to ./app.log
  hop pull tools /etc/nginx/nginx.conf conf/      to conf/nginx.conf
  hop pull -p acme-prod db /var/backups/db.sql.gz /tmp/prod.sql.gz
  hop pull s3://acme-logs/2026/app.log            from a bucket (see hop s3 pull)`,
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 && bucket.HasScheme(args[0]) {
				return cobra.RangeArgs(1, 2)(cmd, args)
			}
			return cobra.RangeArgs(2, 3)(cmd, args)
		},
		ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			switch len(args) {
			case 0:
				return completeNth(f, 0)(cmd, args, toComplete)
			case 1:
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			return nil, cobra.ShellCompDirectiveDefault
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if bucket.HasScheme(args[0]) {
				return runS3Pull(cmd, f, args)
			}
			remote, local := args[1], ""
			if len(args) == 3 {
				local = args[2]
			}
			// Before the picker, a question or a sign-in, which would all be for nothing.
			if err := session.CheckPull(remote); err != nil {
				return err
			}
			local, err := pullTarget(remote, local)
			if err != nil {
				return err
			}
			a, err := newApp(*f)
			if err != nil {
				return err
			}
			in, cfg, err := a.reach(cmd.Context(), args[:1], forPull)
			if err != nil {
				return err
			}
			a.warnAgent(in)
			a.touch(in)
			info("%s:%s -> %s", in.Name, remote, local)
			req := session.Request{Profile: in.Profile, Region: in.Region, InstanceID: in.ID, PreferSSO: a.preferSSO(in.Profile)}
			p := &progress{tty: stderrTTY}
			n, err := session.Pull(cmd.Context(), cfg, req, remote, local, p.update)
			p.clear()
			if err != nil {
				return err
			}
			info("%s received, SHA-256 matches on both sides", byteSize(n))
			return nil
		},
	}
}

// pullTarget is the file pull writes: local as given, or the remote file's name in the
// directory local, or in the current directory when local is empty. It must be a
// regular file, or not exist yet in a directory that does.
func pullTarget(remote, local string) (string, error) {
	// The remote path is a POSIX one, whatever this machine runs.
	name := path.Base(remote)
	target := local
	if st, err := os.Stat(local); local == "" || (err == nil && st.IsDir()) {
		target = filepath.Join(local, name)
	} else if strings.HasSuffix(local, "/") || strings.HasSuffix(local, string(filepath.Separator)) {
		return "", fmt.Errorf("directory not found: %s", local)
	}
	st, err := os.Stat(target)
	switch {
	case err == nil && st.IsDir():
		return "", fmt.Errorf("%s is a directory", target)
	case err == nil && !st.Mode().IsRegular():
		return "", fmt.Errorf("%s is not a regular file", target)
	case err == nil:
		return target, nil
	case !errors.Is(err, os.ErrNotExist):
		return "", err
	}
	if dir := filepath.Dir(target); dir != "." {
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			return "", fmt.Errorf("directory not found: %s", dir)
		}
	}
	return target, nil
}

// progress shows on stderr, when it is a terminal, how much of a download has arrived,
// at most every 200 ms.
type progress struct {
	tty   bool
	last  time.Time
	shown bool
}

func (p *progress) update(done, total int64) {
	if !p.tty || time.Since(p.last) < 200*time.Millisecond {
		return
	}
	p.last, p.shown = time.Now(), true
	msg := byteSize(done)
	if total > 0 {
		msg += fmt.Sprintf(" of %s (%d%%)", byteSize(total), done*100/total)
	}
	fmt.Fprintf(os.Stderr, "\rhop: %s\033[K", msg)
}

// clear erases the progress line, if one was shown.
func (p *progress) clear() {
	if p.shown {
		fmt.Fprint(os.Stderr, "\r\033[K")
	}
}

// byteSize writes n in B, KB, MB or GB, in powers of 1,000.
func byteSize(n int64) string {
	switch {
	case n < 1000:
		return fmt.Sprintf("%d B", n)
	case n < 1_000_000:
		return fmt.Sprintf("%.1f KB", float64(n)/1e3)
	case n < 1_000_000_000:
		return fmt.Sprintf("%.1f MB", float64(n)/1e6)
	}
	return fmt.Sprintf("%.1f GB", float64(n)/1e9)
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

// posixOnly refuses Windows instances: run, push and pull send a POSIX shell script,
// which Windows does not run.
func posixOnly(in inventory.Instance) error {
	// EC2 says "Windows" or "Windows with SQL Server ..."; the SSM agent says
	// "Microsoft Windows Server 2022 Datacenter".
	if strings.Contains(strings.ToLower(in.Platform), "windows") {
		return fmt.Errorf("%s runs %s: hop run, push and pull support Linux and macOS instances; use a shell session (hop %s)", in.Name, in.Platform, in.Name)
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
