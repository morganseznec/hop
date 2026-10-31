// Package cli wires up the hop commands.
package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"runtime/debug"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/spf13/cobra"

	"github.com/morganseznec/hop/internal/config"
	"github.com/morganseznec/hop/internal/inventory"
	"github.com/morganseznec/hop/internal/session"
)

// version is set at link time by the release build.
var version = "dev"

// buildVersion returns the version set at link time, or else the module version that
// go install records (go install ...@v1.4.0 or @latest), so that such a build does not
// call itself dev.
func buildVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return version
	}
	return versionOf(version, info.Main.Version)
}

// versionOf returns linked unless it is the default "dev", then the module version
// without its v (as release builds print it), unless go build left "(devel)" there.
func versionOf(linked, module string) string {
	if linked != "dev" || module == "" || module == "(devel)" {
		return linked
	}
	return strings.TrimPrefix(module, "v")
}

// exitError carries an exit code without a message: the remote command's, for
// example.
type exitError struct{ code int }

func (e exitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

func Execute() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	err := newRoot().ExecuteContext(ctx)
	if err == nil {
		return 0
	}
	var ex exitError
	if errors.As(err, &ex) {
		return ex.code
	}
	// Ctrl-C while waiting for a command: say whether it still runs on the instance.
	var ie *session.InterruptedError
	if errors.As(err, &ie) {
		fmt.Fprintln(os.Stderr, paint(stderrTTY, cYellow, "hop: "+err.Error()))
		return 130
	}
	if errors.Is(err, errAborted) || errors.Is(err, context.Canceled) {
		return 130
	}
	fmt.Fprintln(os.Stderr, paint(stderrTTY, cRed, "hop: "+inventory.Redact(err.Error())))
	return 1
}

func newRoot() *cobra.Command {
	f := &filters{}
	root := &cobra.Command{
		Use:   "hop [instance...]",
		Short: "Open a Session Manager shell on any EC2 instance of any of your AWS accounts",
		Long: `Open a Session Manager shell on any EC2 instance of any of your AWS accounts.

hop opens the shell through Session Manager (logged when the account has session
logging), without SSH keys, bastion or instance IDs to copy.

With no argument, it opens a picker over the instances of every AWS profile on this
machine, each queried in its own region plus its extra_regions ("hop scan" finds the
others). With words, it searches: "hop shop app", "hop tools", "hop i-0abc12..." or
"hop 10.0.1.21". A single instance found by its exact name, ID or IP, or by whole
words of its name, connects directly; a looser match asks first.`,
		Example: `  hop                         picker over the whole fleet
  hop shop tools              search by words
  hop -p 'shop-*' -e prod     limit to shop-* profiles, in prod
  hop ls                      list the instances of every account
  hop run tools uptime        one-off command with its output
  hop tunnel tools 3000       port forwarding
  ssh ec2-user@tools.hop      ssh, scp, rsync (set up with hop init ssh)
  hop login                   sign in again to expired SSO portals
  hop doctor                  check this machine's AWS setup`,
		Version:           buildVersion(),
		Args:              cobra.ArbitraryArgs,
		SilenceUsage:      true,
		SilenceErrors:     true,
		ValidArgsFunction: completeQuery(f),
		RunE: func(cmd *cobra.Command, args []string) error {
			return connect(cmd.Context(), *f, args)
		},
	}
	pf := root.PersistentFlags()
	pf.StringSliceVarP(&f.profiles, "profile", "p", nil, "AWS profile(s), patterns allowed (shop-*)")
	pf.StringSliceVarP(&f.regions, "region", "r", nil, "region(s) to query instead of the profile's own")
	pf.StringVarP(&f.env, "env", "e", "", "prod or test (dev, staging, qa, uat... count as test)")
	pf.BoolVar(&f.refresh, "refresh", false, "ignore the cache and query AWS again")
	_ = root.RegisterFlagCompletionFunc("profile", completeProfiles)
	_ = root.RegisterFlagCompletionFunc("env", cobra.FixedCompletions([]string{"prod", "test"}, cobra.ShellCompDirectiveNoFileComp))
	_ = root.RegisterFlagCompletionFunc("region", completeRegions)

	root.AddCommand(newLs(f), newRun(f), newTunnel(f), newPush(f), newLogin(f), newDoctor(f), newScan(f), newSSHProxy(f), newInit(), newRefresh(f))
	return root
}

func connect(ctx context.Context, f filters, args []string) error {
	// Without a terminal, the plugin opens the session then fails on EOF: the session
	// stays open on the SSM side and shows up in the logs for no reason.
	if !stdinTTY || !stdoutTTY {
		return errors.New("no terminal: an interactive shell needs one (for a one-off command: hop run <instance> <command>)")
	}
	a, err := newApp(f)
	if err != nil {
		return err
	}
	in, cfg, err := a.reach(ctx, args, forShell)
	if err != nil {
		return err
	}
	if err := a.transcriptGate(ctx, cfg, in); err != nil {
		return err
	}
	a.warnAgent(in)
	a.touch(in)
	return pluginExit(session.Start(ctx, cfg, session.Request{Profile: in.Profile, Region: in.Region, InstanceID: in.ID, PreferSSO: a.preferSSO(in.Profile)}))
}

// pluginExit returns the plugin's exit code as is: it has already printed its error.
func pluginExit(err error) error {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return exitError{ee.ExitCode()}
	}
	return err
}

// transcriptGate shows what will be recorded, based on the account's setting in the
// region rather than on principle: an account with no destination configured records
// nothing.
//
// The setting comes from the cache, up to the TTL old. With transcript: require, it is
// read again from the account (with cfg, the credentials of the instance) before the
// session opens: logging turned off minutes ago must not let a session through.
func (a *app) transcriptGate(ctx context.Context, cfg aws.Config, in inventory.Instance) error {
	policy := a.cfg.TranscriptPolicy()
	var tr inventory.Transcript
	t := a.target(in)
	if t != nil {
		tr = t.Transcript
	}
	if policy == config.TranscriptRequire {
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		tr = inventory.CheckTranscript(cctx, cfg)
		cancel()
		if err := ctx.Err(); err != nil {
			return err
		}
		if t != nil && tr.Checked {
			t.Transcript = tr
		}
	}
	where := fmt.Sprintf("%s (%s, %s, %s)", in.Name, in.ID, in.Profile, in.Region)
	switch {
	case policy == config.TranscriptOff:
		info("session on %s", where)
	case tr.Enabled:
		fmt.Fprintln(os.Stderr, paint(stderrTTY, cBold, fmt.Sprintf("hop: session LOGGED on %s to %s. Everything that follows is recorded.", where, tr.Destination())))
	case !tr.Checked:
		if policy == config.TranscriptRequire {
			msg := fmt.Sprintf("session logging cannot be verified for %s in %s", in.Profile, in.Region)
			if tr.Err != "" {
				msg += " (" + shortError(tr.Err) + ")"
			}
			return errors.New(msg + ": session refused (transcript: require)")
		}
		warn("session logging not verified for %s in %s", in.Profile, in.Region)
		info("session on %s", where)
	default:
		warn("account %s does NOT log sessions in %s: nothing that follows will be recorded.", in.Profile, in.Region)
		hint("the SSM-SessionManagerRunShell document has no CloudWatch log group or S3 bucket")
		if policy == config.TranscriptRequire {
			return errors.New("session refused (transcript: require)")
		}
		if err := ask(ctx, "Open anyway?", false); err != nil {
			return err
		}
		info("session NOT logged on %s", where)
	}
	return nil
}

// newRefresh is the background refresh started by completion, with the -p and -r
// typed so far. It skips credential_process profiles: a detached process must not
// run a password manager or open a window.
func newRefresh(f *filters) *cobra.Command {
	return &cobra.Command{
		Use:    "__refresh",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			release, ok := inventory.Lock()
			defer release()
			if !ok {
				return nil
			}
			a, err := newApp(filters{profiles: f.profiles, regions: f.regions})
			if err != nil {
				return err
			}
			a.background = true
			return a.ensure(cmd.Context(), false)
		},
	}
}
