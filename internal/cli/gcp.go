package cli

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/morganseznec/hop/internal/config"
	"github.com/morganseznec/hop/internal/inventory"
	"github.com/morganseznec/hop/internal/session"
)

// GCP support is experimental: gcp_projects in the configuration adds the Compute
// Engine instances of those projects to the inventory, under gcp:<project>, and hop
// opens shells on them through IAP. run, tunnel, push and ssh-proxy stay AWS only.

// gcpOn reports whether gcp_projects turns GCP on.
func (a *app) gcpOn() bool { return len(a.cfg.GCPProjects) > 0 }

// gcpInScope reports whether the GCP projects are queried: GCP is on, -r (AWS regions)
// is not set, and -p, if set, names gcp or gcp:<project>.
func (a *app) gcpInScope(regions []string) bool {
	if !a.gcpOn() || len(regions) > 0 {
		return false
	}
	return len(a.f.profiles) == 0 || gcpPattern(a.f.profiles)
}

// gcpPattern reports whether a -p pattern selects GCP instances: gcp for all of them,
// or gcp:<project pattern>.
func gcpPattern(patterns []string) bool {
	for _, p := range patterns {
		if matchAny([]string{p}, inventory.GCP) || strings.HasPrefix(p, inventory.GCP+":") {
			return true
		}
	}
	return false
}

// gcpFiltered reports whether -p leaves out a GCP instance: -p gcp:alpha keeps the
// instances of the project alpha only.
func (a *app) gcpFiltered(in inventory.Instance) bool {
	return len(a.f.profiles) > 0 && !matchAny(a.f.profiles, inventory.GCP) && !matchAny(a.f.profiles, in.Profile)
}

// reachGCP checks a GCP instance before a shell, as verify does for EC2: when the
// inventory is older than freshWindow, the instance is read again, and hop refuses one
// that is gone, stopped or replaced by another of the same name (gcloud targets the
// name). The other commands are not available there yet.
func (a *app) reachGCP(ctx context.Context, in inventory.Instance, why purpose) (inventory.Instance, error) {
	where := fmt.Sprintf("%s (%s, %s)", in.Name, in.Profile, in.Region)
	if why != forShell {
		return in, fmt.Errorf("%s is a GCP instance: hop only opens shells there for now (GCP support is experimental). With gcloud: %s",
			where, gcpAlternative(why, in))
	}
	t := a.target(in)
	if t == nil || time.Since(t.FetchedAt) < freshWindow || (t.Err == "" && a.fetched[t.Key()]) {
		return in, nil
	}
	lctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	cur, found, err := inventory.LookupGCP(lctx, in)
	cancel()
	if ctx.Err() != nil {
		return in, ctx.Err()
	}
	switch {
	case err != nil:
		// A shell targets the name: gcloud fails cleanly if the instance is gone.
		return in, nil
	case !found:
		return in, fmt.Errorf("%s no longer exists: hop ls --refresh", where)
	case cur.ID != in.ID:
		return in, fmt.Errorf("%s is now another instance of the same name: hop ls --refresh, then try again", where)
	case !cur.Running():
		return in, fmt.Errorf("%s is now %s", where, cur.State)
	}
	return cur, nil
}

// gcpAlternative is the gcloud command that does what hop does not do on GCP yet.
func gcpAlternative(why purpose, in inventory.Instance) string {
	args := strings.Join(session.GCPArgs(in.Project(), in.Region), " ")
	switch why {
	case forTunnel:
		return fmt.Sprintf("gcloud compute start-iap-tunnel %s <port> --local-host-port=localhost:<port> --project=%s --zone=%s", in.Name, in.Project(), in.Region)
	case forPush:
		return fmt.Sprintf("gcloud compute scp <file> %s:<path> %s", in.Name, args)
	}
	return fmt.Sprintf("gcloud compute ssh %s %s --command '<command>'", in.Name, args)
}

// reportGCP explains a failed GCP inventory: most often, gcloud wants a new login.
func (a *app) reportGCP(err string) {
	warn("GCP projects not queried: %s", shortError(err))
	if !strings.Contains(err, "gcloud not found") {
		hint("gcloud auth login, then hop ls --refresh")
	}
}

// diagnoseGCP checks, for hop doctor, what the GCP support needs: gcloud, and a last
// inventory that worked.
func (a *app) diagnoseGCP(add func(finding)) {
	if _, err := exec.LookPath("gcloud"); err != nil {
		add(finding{lvl: lvlCrit, title: "gcloud not found: the GCP projects of gcp_projects cannot be listed",
			fix: []string{"install the Google Cloud CLI (https://cloud.google.com/sdk/docs/install), then gcloud auth login"}})
		return
	}
	if t := a.inv.Targets[inventory.GCPPair().Key()]; t != nil && t.Err != "" {
		add(finding{lvl: lvlWarn, title: "the last GCP inventory failed", detail: []string{shortError(t.Err)},
			fix: []string{"gcloud auth login, then hop ls --refresh"}})
		return
	}
	add(finding{lvl: lvlInfo, title: "GCP support on (experimental): shells only, through IAP, not recorded by GCP"})
}

// shellGCP opens the shell with gcloud compute ssh. GCP records the IAP connection in
// its audit logs, not what is typed: transcript: require refuses, warn says so.
func (a *app) shellGCP(in inventory.Instance) error {
	switch a.cfg.TranscriptPolicy() {
	case config.TranscriptRequire:
		return errors.New("session refused (transcript: require): GCP does not record shell sessions")
	case config.TranscriptOff:
	default:
		warn("GCP does not record shell sessions: nothing that follows on %s is recorded.", in.Name)
	}
	info("session on %s (%s, %s) through IAP, with gcloud (GCP support is experimental)", in.Name, in.Profile, in.Region)
	a.touch(in)
	return pluginExit(session.StartGCP(in.Project(), in.Region, in.Name))
}
