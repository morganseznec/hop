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
		Short: "Inventory of instances across all accounts",
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
			if err := a.ensure(cmd.Context(), f.refresh); err != nil {
				return err
			}
			items := a.instances(all)
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
			if asJSON {
				return a.printJSON(items)
			}
			if len(items) == 0 {
				info("no instances")
			} else {
				a.printTable(items)
				a.printFooter(items)
			}
			a.reportProblems()
			return nil
		},
	}
	cmd.Flags().BoolVarP(&all, "all", "a", false, "include stopped instances")
	cmd.Flags().BoolVar(&asJSON, "json", false, "JSON output")
	return cmd
}

func (a *app) printJSON(items []inventory.Instance) error {
	type row struct {
		inventory.Instance
		Transcript inventory.Transcript `json:"transcript"`
		Label      string               `json:"label,omitempty"`
	}
	out := []row{}
	for _, in := range items {
		r := row{Instance: in, Label: a.cfg.Label(in.Profile)}
		if t := a.target(in); t != nil {
			r.Transcript = t.Transcript
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
