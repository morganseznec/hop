package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"

	"github.com/morganseznec/hop/internal/awsconf"
)

func newLogin(f *filters) *cobra.Command {
	var statusOnly bool
	cmd := &cobra.Command{
		Use:   "login [portal|profile...]",
		Short: "Reopen expired SSO portals, one login per portal",
		Long: `SSO profiles are grouped by token: all profiles on the same portal open with
a single login. With no argument, only expired portals are reopened. The sign-in
itself is handed to "aws sso login", which drives the browser.`,
		ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			files, err := awsconf.Load()
			if err != nil {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			var out []string
			for _, pt := range files.Portals(nil) {
				out = append(out, fmt.Sprintf("%s\t%s", pt.Label(), pt.Token()))
			}
			return out, cobra.ShellCompDirectiveNoFileComp
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := newApp(*f)
			if err != nil {
				return err
			}
			selected := map[string]bool{}
			for _, p := range a.selectedProfiles() {
				selected[p.Name] = true
			}
			// A portal named explicitly opens even if its profiles are excluded.
			keep := func(name string) bool { return len(args) > 0 || selected[name] }
			portals := a.files.Portals(keep)
			if len(portals) == 0 {
				info("no SSO profiles in scope")
				return nil
			}
			printPortals(portals)
			if statusOnly {
				return nil
			}

			var todo []awsconf.Portal
			if len(args) == 0 {
				for _, pt := range portals {
					if pt.Token().State != awsconf.TokenValid {
						todo = append(todo, pt)
					}
				}
				if len(todo) == 0 {
					info("all SSO tokens are valid")
					return nil
				}
			}
			for _, arg := range args {
				pt, ok := findPortal(portals, arg)
				if !ok {
					return fmt.Errorf("not an SSO portal or profile: %s", arg)
				}
				todo = append(todo, pt)
			}
			for _, pt := range todo {
				info("signing in to SSO portal %s (%s)", pt.Label(), strings.Join(pt.Profiles, ", "))
				if err := ssoLogin(pt); err != nil {
					return err
				}
				for _, p := range pt.Profiles {
					a.inv.Invalidate(p)
				}
			}
			return a.inv.Save()
		},
	}
	cmd.Flags().BoolVar(&statusOnly, "status", false, "show token status without signing in")
	return cmd
}

func findPortal(portals []awsconf.Portal, arg string) (awsconf.Portal, bool) {
	for _, pt := range portals {
		if pt.Label() == arg || pt.Key == arg || pt.SessionName == arg {
			return pt, true
		}
		for _, p := range pt.Profiles {
			if p == arg {
				return pt, true
			}
		}
	}
	return awsconf.Portal{}, false
}

func printPortals(portals []awsconf.Portal) {
	t := &table{head: []string{"PORTAL", "TOKEN", "PROFILES"}}
	for _, pt := range portals {
		tok := pt.Token()
		color := cRed
		if tok.State == awsconf.TokenValid {
			color = cGreen
		}
		profiles := strings.Join(pt.Profiles, ", ")
		if len(pt.Profiles) > 3 {
			profiles = strings.Join(pt.Profiles[:3], ", ") + fmt.Sprintf(" +%d", len(pt.Profiles)-3)
		}
		t.add(cell{pt.Label(), ""}, cell{tok.String(), color}, cell{profiles, ""})
	}
	t.print(os.Stderr, stderrTTY)
}

// ssoLogin delegates to `aws sso login`. Its output goes to stderr, so stdout stays
// clean when a login is triggered in the middle of another command.
func ssoLogin(pt awsconf.Portal) error {
	bin, err := exec.LookPath("aws")
	if err != nil {
		return errors.New("aws CLI not found, run it yourself: aws " + strings.Join(pt.LoginArgs(), " "))
	}
	cmd := exec.Command(bin, pt.LoginArgs()...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stderr, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("aws %s: %w", strings.Join(pt.LoginArgs(), " "), err)
	}
	return nil
}
