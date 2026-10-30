package cli

import (
	"strings"

	"github.com/spf13/cobra"

	"github.com/morganseznec/hop/internal/awsconf"
)

// Completions read the cache without waiting for the network. Options already typed
// (-p, -r, -e) are taken into account: cobra parses them before calling these functions.

func instanceNames(f *filters) []string {
	a, err := newApp(*f)
	if err != nil {
		return nil
	}
	a.ensureInBackground()
	seen := map[string]bool{}
	var out []string
	for _, in := range a.instances(false) {
		if seen[in.Name] {
			continue
		}
		seen[in.Name] = true
		out = append(out, in.Name+"\t"+joinNonEmpty(", ", a.profileLabel(in.Profile), joinNonEmpty(" ", in.Region, in.Env)))
	}
	return out
}

// joinNonEmpty joins the parts that are not empty: a description without an
// environment must not end with a space.
func joinNonEmpty(sep string, parts ...string) string {
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, sep)
}

func completeQuery(f *filters) cobra.CompletionFunc {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) > 0 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return instanceNames(f), cobra.ShellCompDirectiveNoFileComp
	}
}

// completeNth completes an instance name at position n, and nothing elsewhere.
func completeNth(f *filters, n int) cobra.CompletionFunc {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) != n {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return instanceNames(f), cobra.ShellCompDirectiveNoFileComp
	}
}

func completeProfiles(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	files, err := awsconf.Load()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	var out []string
	for _, p := range files.Profiles {
		if p.Usable() {
			out = append(out, p.Name+"\t"+joinNonEmpty(", ", p.Kind.String(), p.Region))
		}
	}
	return out, cobra.ShellCompDirectiveNoFileComp
}

// knownRegions are the regions of the commercial partition, as listed by the endpoints
// data of the SDK (aws-sdk-go-v2 internal/endpoints/awsrulesfn/partitions.json).
var knownRegions = strings.Fields(`eu-west-1 eu-west-2 eu-west-3 eu-central-1 eu-central-2 eu-north-1 eu-south-1
eu-south-2 us-east-1 us-east-2 us-west-1 us-west-2 af-south-1 me-south-1 me-central-1 il-central-1
ca-central-1 ca-west-1 mx-central-1 sa-east-1 ap-south-1 ap-south-2 ap-east-1 ap-east-2 ap-southeast-1
ap-southeast-2 ap-southeast-3 ap-southeast-4 ap-southeast-5 ap-southeast-6 ap-southeast-7 ap-northeast-1
ap-northeast-2 ap-northeast-3`)

func completeRegions(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	return knownRegions, cobra.ShellCompDirectiveNoFileComp
}
