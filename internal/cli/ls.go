package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/morganseznec/hop/internal/inventory"
	"github.com/morganseznec/hop/internal/match"
)

func newLs(f *filters) *cobra.Command {
	var all, asJSON bool
	cmd := &cobra.Command{
		Use:   "ls [filter...]",
		Short: "List instances across all accounts",
		Long: `List instances across all accounts.

Each profile is queried in its own region plus its extra_regions, in parallel, and
the result is cached (cache_ttl, 15 minutes by default). Words filter the list like
the search does. An unreachable account keeps its last known instances, and the
reason is printed on stderr.

AGENT is the SSM agent: online, lost (with its last contact), inactive, none, or ?
when it could not be read; a stopped instance (--all) shows its state there. LOGGING
says whether the account logs sessions in that region: yes, NO, or ? when it could
not be checked. Both show - for a GCP instance (experimental, see gcp_projects).`,
		Example: `  hop ls
  hop ls shop               filter by words, like the search
  hop ls -e test --all      test instances, stopped ones included
  hop ls --json | jq ...`,
		ValidArgsFunction: completeQuery(f),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := newApp(*f)
			if err != nil {
				return err
			}
			if err := a.checkScope(); err != nil {
				return err
			}
			if err := a.ensure(cmd.Context(), f.refresh); err != nil {
				return err
			}
			items := a.instances(all)
			words := len(items) > 0 && len(args) > 0
			if len(args) > 0 {
				// Search ranks by relevance; ls keeps the per-account order.
				keep := map[string]bool{}
				for _, m := range match.Search(items, strings.Join(args, " "), a.recents, a.cfg.Label).Matches {
					keep[m.Key()] = true
				}
				var filtered []inventory.Instance
				for _, in := range items {
					if keep[in.Key()] {
						filtered = append(filtered, in)
					}
				}
				items = filtered
			}
			// The problems go to stderr with --json too: a script must not take the last
			// known instances of an unreachable account for the current ones.
			if asJSON {
				if err := a.printJSON(items); err != nil {
					return err
				}
				a.reportProblems()
				return nil
			}
			switch {
			case len(items) > 0:
				a.printTable(items)
				a.printFooter(items)
			case words:
				info("no instances match %q", strings.Join(args, " "))
			default:
				msg := "no instances"
				if r := a.emptyReason(all); r != "" {
					msg += ": " + r
				}
				info("%s", msg)
			}
			a.reportProblems()
			return nil
		},
	}
	cmd.Flags().BoolVarP(&all, "all", "a", false, "include stopped instances")
	cmd.Flags().BoolVar(&asJSON, "json", false, "JSON output")
	return cmd
}

// printJSON writes the instances as a JSON list. Each row says how old its data is:
// fetched_at is the last successful query of its profile/region pair, stale reports a
// pair whose last query failed (error) or whose data is older than the TTL.
func (a *app) printJSON(items []inventory.Instance) error {
	type row struct {
		inventory.Instance
		Transcript inventory.Transcript `json:"transcript"`
		Label      string               `json:"label,omitempty"`
		FetchedAt  time.Time            `json:"fetched_at,omitzero"`
		Stale      bool                 `json:"stale"`
		Error      string               `json:"error,omitempty"`
	}
	now := time.Now()
	out := []row{}
	for _, in := range items {
		r := row{Instance: in, Label: a.cfg.Label(in.Profile), Stale: true}
		if t := a.target(in); t != nil {
			r.Transcript = t.Transcript
			// Whole seconds in UTC: 2026-10-30T09:15:00Z.
			r.FetchedAt = t.FetchedAt.UTC().Truncate(time.Second)
			r.Stale = t.Err != "" || t.FetchedAt.IsZero() || now.Sub(t.FetchedAt) >= a.cfg.TTL()
			if t.Err != "" {
				r.Error = shortError(t.Err)
				if k := t.ErrKind; k != "" && k != inventory.ErrOther && k != inventory.ErrMFA {
					r.Error = k.Explain() + ": " + r.Error
				}
			}
		}
		out = append(out, r)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func (a *app) printTable(items []inventory.Instance) {
	t := &table{head: []string{"NAME", "PROFILE", "REGION", "ENV", "IP", "AGENT", "LOGGING"}}
	for _, in := range items {
		name := cell{in.Name, ""}
		agent := a.agentCell(in)
		if !in.Running() {
			name.color = cDim
			agent = cell{in.State, cDim}
		}
		t.add(name, cell{a.profileLabel(in.Profile), ""}, cell{in.Region, ""}, cell{in.Env, ""},
			cell{in.PrivateIP, ""}, agent, a.transcriptCell(in))
	}
	t.print(os.Stdout, stdoutTTY)
}

func (a *app) agentCell(in inventory.Instance) cell {
	if in.GCP() {
		return cell{"-", cDim}
	}
	switch {
	case in.Online():
		return cell{"online", cGreen}
	case in.Agent == "ConnectionLost":
		return cell{"lost " + ago(in.LastPing), cYellow}
	case in.Agent != "":
		return cell{strings.ToLower(in.Agent), cYellow}
	}
	if t := a.target(in); t != nil && t.AgentsKnown {
		return cell{"none", cDim}
	}
	return cell{"?", cDim}
}

func (a *app) transcriptCell(in inventory.Instance) cell {
	if in.GCP() {
		return cell{"-", cDim}
	}
	t := a.target(in)
	switch {
	case t == nil || !t.Transcript.Checked:
		return cell{"?", cDim}
	case t.Transcript.Enabled:
		return cell{"yes", cGreen}
	}
	return cell{"NO", cRed}
}

func (a *app) printFooter(items []inventory.Instance) {
	profiles := map[string]bool{}
	for _, in := range items {
		profiles[in.Profile] = true
	}
	msg := fmt.Sprintf("%s, %s, regions %s", plural(len(items), "instance"), plural(len(profiles), "profile"), strings.Join(regionsOf(items), ", "))
	// The age of the oldest data not queried during this call.
	var oldest time.Time
	for _, k := range a.keys() {
		if t, ok := a.inv.Targets[k]; ok && !a.fetched[k] && !t.FetchedAt.IsZero() && (oldest.IsZero() || t.FetchedAt.Before(oldest)) {
			oldest = t.FetchedAt
		}
	}
	if !oldest.IsZero() {
		msg += fmt.Sprintf(" (cached %s, --refresh to query again)", ago(oldest))
	}
	fmt.Fprintln(os.Stderr, paint(stderrTTY, cDim, msg))
}
