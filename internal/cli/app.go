package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/morganseznec/hop/internal/awsconf"
	"github.com/morganseznec/hop/internal/config"
	"github.com/morganseznec/hop/internal/inventory"
	"github.com/morganseznec/hop/internal/match"
	"github.com/morganseznec/hop/internal/state"
)

// filters holds the options shared by every command.
type filters struct {
	profiles []string
	regions  []string
	env      string
	refresh  bool
}

type app struct {
	files   *awsconf.Files
	cfg     *config.Config
	inv     *inventory.Inventory
	recents state.Recents
	f       filters
	// fetched holds the profile/region pairs queried during this call: they are never
	// queried twice.
	fetched map[string]bool
	// background is set for the detached refresh started by completion: it must not
	// run credential_process helpers, which may prompt or open a window.
	background bool
}

func newApp(f filters) (*app, error) {
	files, err := awsconf.Load()
	if err != nil {
		return nil, fmt.Errorf("reading AWS configuration: %w", err)
	}
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	a := &app{files: files, cfg: cfg, inv: inventory.Load(), recents: state.LoadRecents(), f: f}
	if err := a.checkProfiles(); err != nil {
		return nil, err
	}
	return a, nil
}

// selectedProfiles applies -p, or else include/exclude from the configuration. SSO
// profiles come first: when two profiles see the same instance, it shows up under the
// SSO profile.
func (a *app) selectedProfiles() []*awsconf.Profile {
	var out []*awsconf.Profile
	for _, p := range a.files.Profiles {
		if !p.Usable() {
			continue
		}
		if len(a.f.profiles) > 0 {
			if matchAny(a.f.profiles, p.Name) {
				out = append(out, p)
			}
			continue
		}
		if a.cfg.Wants(p.Name) {
			out = append(out, p)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Kind.IsSSO() && !out[j].Kind.IsSSO() })
	return out
}

func matchAny(patterns []string, name string) bool {
	for _, p := range patterns {
		if ok, _ := path.Match(p, name); ok || p == name {
			return true
		}
	}
	return false
}

// checkProfiles rejects a -p that matches nothing, suggesting the closest profile.
func (a *app) checkProfiles() error {
	for _, want := range a.f.profiles {
		found := false
		for _, p := range a.files.Profiles {
			if matchAny([]string{want}, p.Name) {
				found = true
				break
			}
		}
		if !found {
			msg := fmt.Sprintf("unknown profile: %s", want)
			if s := closest(want, a.files.Names()); s != "" {
				msg += fmt.Sprintf(" (did you mean %s?)", s)
			}
			return errors.New(msg)
		}
	}
	return nil
}

func (a *app) pairs() []inventory.Pair {
	var out []inventory.Pair
	for _, p := range a.selectedProfiles() {
		regions := a.cfg.Regions(p.Name, p.Region, a.files.DefaultRegion)
		// -r queries the requested region even if the configuration does not list it:
		// `hop ls -p acme-prod -r us-east-1` must work without declaring anything.
		if len(a.f.regions) > 0 {
			regions = a.f.regions
		}
		for _, r := range regions {
			out = append(out, inventory.Pair{Profile: p.Name, Region: r, PreferSSO: p.PreferSSO()})
		}
	}
	return out
}

func (a *app) preferSSO(profile string) bool {
	p := a.files.Profile(profile)
	return p != nil && p.PreferSSO()
}

func (a *app) keys() []string {
	var out []string
	for _, p := range a.pairs() {
		out = append(out, inventory.TargetKey(p.Profile, p.Region))
	}
	return out
}

func (a *app) pairOf(in inventory.Instance) inventory.Pair {
	return inventory.Pair{Profile: in.Profile, Region: in.Region, PreferSSO: a.preferSSO(in.Profile)}
}

// rootProfile follows source_profile from an assume-role profile to the profile that
// holds the credentials: an SSO portal, keys or a credential_process.
func (a *app) rootProfile(name string) *awsconf.Profile {
	p := a.files.Profile(name)
	seen := map[string]bool{}
	for p != nil && p.Kind == awsconf.KindAssumeRole && !seen[p.Name] {
		seen[p.Name] = true
		p = a.files.Profile(p.SourceProfile)
	}
	return p
}

// ssoPortal returns the SSO portal a profile signs in with, directly or through the
// source_profile of an assumed role.
func (a *app) ssoPortal(profile string) (awsconf.Portal, bool) {
	p := a.rootProfile(profile)
	if p == nil || !p.Kind.IsSSO() {
		return awsconf.Portal{}, false
	}
	return a.files.PortalOf(p.Name)
}

// invalidatePortal forces the next refresh of every profile that signs in through the
// portal: its SSO profiles, and the assumed roles whose source_profile chain ends at
// one of them. Called after a login, so that none of them keeps reporting the expired
// token.
func (a *app) invalidatePortal(pt awsconf.Portal) {
	for _, p := range a.files.Profiles {
		if root := a.rootProfile(p.Name); root != nil && root.Kind.IsSSO() && root.PortalKey() == pt.Key {
			a.inv.Invalidate(p.Name)
		}
	}
}

// usesProcess reports whether a profile gets its credentials from a credential_process,
// directly or through the source_profile of an assumed role.
func (a *app) usesProcess(profile string) bool {
	p := a.rootProfile(profile)
	return p != nil && p.Kind == awsconf.KindProcess
}

// freshWindow is how old the inventory of a pair may be for hop to act on it as is.
// Past it, a search that misses queries the pair again (see resolve), and an instance
// found there is looked up again before hop acts on it (see verify).
const freshWindow = time.Minute

// stale returns the pairs to query: those older than the TTL, or all of them with
// force.
func (a *app) stale(force bool) []inventory.Pair { return a.olderThan(a.cfg.TTL(), force) }

// olderThan returns the pairs in scope whose data is older than maxAge, or all of them
// with force. A pair already queried during this call is never queried again, and a
// background refresh leaves out credential_process profiles.
func (a *app) olderThan(maxAge time.Duration, force bool) []inventory.Pair {
	now := time.Now()
	var todo []inventory.Pair
	for _, p := range a.pairs() {
		key := inventory.TargetKey(p.Profile, p.Region)
		if a.fetched[key] || (a.background && a.usesProcess(p.Profile)) {
			continue
		}
		t, ok := a.inv.Targets[key]
		if force || !ok || !t.Fresh(maxAge, now) {
			todo = append(todo, p)
		}
	}
	return todo
}

// ensure refreshes stale profile/region pairs (or all of them with force) and saves the
// cache.
func (a *app) ensure(ctx context.Context, force bool) error {
	return a.refresh(ctx, a.stale(force))
}

// refresh queries the given pairs and saves the cache. When ctx is canceled (Ctrl-C),
// nothing is recorded: the pairs cut short are not errors, and the caller gets the
// context error.
func (a *app) refresh(ctx context.Context, todo []inventory.Pair) error {
	if len(todo) == 0 {
		return nil
	}
	progress := func(done, total int) {
		if stderrTTY {
			fmt.Fprintf(os.Stderr, "\r\033[Khop: inventory %d/%d", done, total)
		}
	}
	results := inventory.FetchAll(ctx, todo, 25*time.Second, progress)
	if stderrTTY {
		fmt.Fprint(os.Stderr, "\r\033[K")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if a.fetched == nil {
		a.fetched = map[string]bool{}
	}
	for _, t := range results {
		a.inv.Put(t)
		a.fetched[t.Key()] = true
	}
	if err := a.inv.Save(); err != nil {
		warn("cache not saved: %v", err)
	}
	return nil
}

// ensureInBackground serves completion, which must answer right away: it reads the
// cache as is and hands the refresh over to a detached process, with the same -p and
// -r.
func (a *app) ensureInBackground() {
	a.background = true
	if len(a.stale(false)) == 0 {
		return
	}
	self, err := os.Executable()
	if err != nil {
		return
	}
	cmd := exec.Command(self, a.refreshArgs()...)
	detach(cmd)
	_ = cmd.Start() // best effort: the next command refreshes anyway
}

// refreshArgs are the arguments of the background refresh: the filters typed so far,
// so that `hop -r us-east-1 <TAB>` refreshes the pairs it shows.
func (a *app) refreshArgs() []string {
	args := []string{"__refresh"}
	for _, p := range a.f.profiles {
		args = append(args, "--profile="+p)
	}
	for _, r := range a.f.regions {
		args = append(args, "--region="+r)
	}
	return args
}

// instances returns the inventory filtered by -e, and by state unless all is set.
func (a *app) instances(all bool) []inventory.Instance {
	var out []inventory.Instance
	for _, in := range a.inv.Instances(a.keys()) {
		if a.f.env != "" && in.Env != a.f.env {
			continue
		}
		if !all && !in.Running() {
			continue
		}
		out = append(out, in)
	}
	return out
}

func (a *app) target(in inventory.Instance) *inventory.Target {
	return a.inv.Targets[inventory.TargetKey(in.Profile, in.Region)]
}

// reportProblems summarizes unreachable profile/region pairs, grouped by cause, with
// the command that fixes them, or the raw error when there is no fix to offer.
func (a *app) reportProblems() {
	sso := map[string][]string{} // portal -> profiles
	var portals []string
	other := map[inventory.ErrKind][]failure{}
	var kinds []inventory.ErrKind
	seen := map[string]bool{}
	for _, k := range a.keys() {
		t, ok := a.inv.Targets[k]
		if !ok || t.Err == "" {
			continue
		}
		label := t.Profile
		if t.Region != a.mainRegion(t.Profile) {
			label += " (" + t.Region + ")"
		}
		if t.ErrKind == inventory.ErrSSOExpired {
			if pt, ok := a.ssoPortal(t.Profile); ok {
				if !seen[pt.Label()] {
					seen[pt.Label()] = true
					portals = append(portals, pt.Label())
				}
				sso[pt.Label()] = append(sso[pt.Label()], label)
				continue
			}
		}
		if _, ok := other[t.ErrKind]; !ok {
			kinds = append(kinds, t.ErrKind)
		}
		other[t.ErrKind] = append(other[t.ErrKind], failure{label, shortError(t.Err)})
	}
	for _, pt := range portals {
		warn("SSO token expired for %s: %s not queried", pt, strings.Join(sso[pt], ", "))
		hint("hop login %s", pt)
	}
	for _, k := range kinds {
		var labels []string
		for _, f := range other[k] {
			labels = append(labels, f.label)
		}
		warn("%s: %s", k.Explain(), strings.Join(labels, ", "))
		switch k {
		case inventory.ErrBadKeys:
			hint("hop doctor for details on static-key profiles")
			continue
		case inventory.ErrLoginExpired:
			hint(`sign in again with "aws login --profile <profile>", then hop ls --refresh`)
			continue
		case inventory.ErrThrottled:
			hint("AWS is rate limiting these calls: retry in a minute")
			continue
		}
		printCauses(other[k])
	}
}

type failure struct{ label, err string }

// printCauses shows the raw errors of one cause. Pairs that fail the same way share a
// line; a single error needs no label, the warning above already names the pairs.
func printCauses(fs []failure) {
	var errs []string
	byErr := map[string][]string{}
	for _, f := range fs {
		if _, ok := byErr[f.err]; !ok {
			errs = append(errs, f.err)
		}
		byErr[f.err] = append(byErr[f.err], f.label)
	}
	if len(errs) == 1 {
		hint("%s", errs[0])
		return
	}
	const most = 3
	for _, e := range errs[:min(most, len(errs))] {
		hint("%s: %s", strings.Join(byErr[e], ", "), e)
	}
	if len(errs) > most {
		hint("and %d more", len(errs)-most)
	}
}

// requestID is the part of SDK errors that only AWS support can use.
var requestID = regexp.MustCompile(`https response error StatusCode: \d+, RequestID: [^,]*, `)

// shortError puts a raw error on one line, without the request ID, and cuts it to
// what a terminal line can show.
func shortError(s string) string {
	s = requestID.ReplaceAllString(strings.Join(strings.Fields(inventory.Redact(s)), " "), "")
	if r := []rune(s); len(r) > 200 {
		s = string(r[:197]) + "..."
	}
	return s
}

func (a *app) mainRegion(profile string) string {
	p := a.files.Profile(profile)
	if p == nil {
		return ""
	}
	return a.cfg.Regions(p.Name, p.Region, a.files.DefaultRegion)[0]
}

// purpose is what an instance is resolved for.
type purpose int

const (
	forShell purpose = iota
	forTunnel
	forRun
	forPush
)

// sendsCommand reports whether the purpose changes the machine with nobody watching
// the result (run and push, through AWS-RunShellScript).
func (p purpose) sendsCommand() bool { return p == forRun || p == forPush }

// confirmDefault is the answer Enter gives when hop asks about a loose match. For run
// and push, a doubt is a no.
func (p purpose) confirmDefault() bool { return !p.sendsCommand() }

func (p purpose) question() string {
	switch p {
	case forTunnel:
		return "Open the tunnel?"
	case forRun:
		return "Run the command there?"
	case forPush:
		return "Copy the file there?"
	}
	return "Connect?"
}

// verdict is what resolve does with the instances a query matches.
type verdict int

const (
	verdictNone       verdict = iota // nothing matches
	verdictNotRunning                // no candidate is running
	verdictAct                       // one candidate, running and strongly matched: go
	verdictConfirm                   // one candidate, loosely matched: ask first
	verdictPick                      // several candidates: the picker
	verdictRefuse                    // anything short of verdictAct, without a terminal
)

// decide is the rule resolve follows. It acts without asking only when a single
// instance matches, stopped ones included, and that instance is running and strongly
// matched (see match.Strong): a partial name, a profile or a region never connects on
// its own, and neither does a name that also belongs to a stopped instance, or the
// name or IP of a hybrid node.
func decide(matches []inventory.Instance, query string, interactive bool) verdict {
	running := 0
	for _, in := range matches {
		if in.Running() {
			running++
		}
	}
	switch {
	case len(matches) == 0:
		return verdictNone
	case running == 0:
		return verdictNotRunning
	case len(matches) == 1 && match.Strong(matches[0], query):
		return verdictAct
	case !interactive:
		return verdictRefuse
	case len(matches) == 1:
		return verdictConfirm
	}
	return verdictPick
}

// resolve turns what was typed into an instance: directly for a single strong match,
// after a question or through the picker otherwise (see decide). asked reports that
// the user chose the instance, rather than hop on its own.
func (a *app) resolve(ctx context.Context, args []string, why purpose) (in inventory.Instance, asked bool, err error) {
	if err := a.ensure(ctx, a.f.refresh); err != nil {
		return inventory.Instance{}, false, err
	}
	query := strings.TrimSpace(strings.Join(args, " "))
	if query == "" {
		running := a.instances(false)
		if len(running) == 0 {
			a.reportProblems()
			return inventory.Instance{}, false, errors.New("no running instance in scope")
		}
		in, err := a.pick(match.Search(running, "", a.recents, a.cfg.Label).Matches, "")
		return in, true, err
	}

	interactive := stdinTTY && stderrTTY
	res := match.Search(a.instances(true), query, a.recents, a.cfg.Label)
	v := decide(res.Matches, query, interactive)
	// A miss may come from a cache that predates the instance, or its state: the pairs
	// not refreshed within freshWindow are queried again, and the search runs once
	// more. A strong match is not a miss (reach looks that one instance up before
	// acting), and neither is a word search that opens the picker: there the user
	// chooses from what is shown.
	miss := v == verdictNotRunning || (!res.Exact && v != verdictAct && (res.Fuzzy || v != verdictPick))
	if miss {
		if err := a.refresh(ctx, a.olderThan(freshWindow, false)); err != nil {
			return inventory.Instance{}, false, err
		}
		res = match.Search(a.instances(true), query, a.recents, a.cfg.Label)
		v = decide(res.Matches, query, interactive)
	}

	notFound := func(suggestions []string) error {
		a.reportProblems()
		msg := "no instance matches \"" + query + "\""
		if res.Literal != "" {
			msg = "no instance with " + res.Literal + " " + query
		}
		if len(suggestions) > 0 {
			msg += "\n     did you mean: " + strings.Join(suggestions, ", ")
		}
		return errors.New(msg)
	}
	// A fuzzy match is only a suggestion: always ask, and without a terminal just
	// suggest it.
	if res.Fuzzy {
		switch v {
		case verdictNotRunning, verdictRefuse:
			var names []string
			for i := 0; i < len(res.Matches) && i < 3; i++ {
				names = append(names, res.Matches[i].Name)
			}
			return inventory.Instance{}, false, notFound(names)
		case verdictConfirm:
			in := res.Matches[0]
			if !confirm(fmt.Sprintf("\"%s\" does not exist. Did you mean %s (%s, %s)?", query, in.Name, in.Profile, in.Region), why.confirmDefault()) {
				return inventory.Instance{}, false, errAborted
			}
			return in, true, nil
		}
		in, err := a.pickRunning(res.Matches, fmt.Sprintf("\"%s\" does not exist, close matches", query))
		return in, true, err
	}

	switch v {
	case verdictNone:
		return inventory.Instance{}, false, notFound(res.Suggestions)
	case verdictNotRunning:
		return inventory.Instance{}, false, notRunning(query, res.Matches)
	case verdictAct:
		in := res.Matches[0]
		// A private IP is unique within a network, not across accounts: with several
		// profiles in scope, the address may also belong to a machine of another
		// account, absent from the inventory or not. run and push ask first.
		if n := len(a.selectedProfiles()); res.Literal == "IP address" && why.sendsCommand() && n > 1 {
			if !interactive {
				return inventory.Instance{}, false, fmt.Errorf("%s is a private IP and %d profiles are in scope: other accounts may use it too. Narrow with -p %s (no terminal to confirm)",
					query, n, in.Profile)
			}
			if !confirm(fmt.Sprintf("%s is %s (%s, %s), but private IPs repeat across accounts. %s", query, in.Name, in.Profile, in.Region, why.question()), false) {
				return inventory.Instance{}, false, errAborted
			}
			return in, true, nil
		}
		return in, false, nil
	case verdictRefuse:
		if len(res.Matches) == 1 {
			in := res.Matches[0]
			// Only a hybrid node matches exactly without matching strongly.
			if res.Exact {
				return inventory.Instance{}, false, fmt.Errorf("%s (%s, %s) is a hybrid node: its name and IP are reported by the node itself, give its ID %s (no terminal to confirm)",
					in.Name, in.Profile, in.Region, in.ID)
			}
			return inventory.Instance{}, false, fmt.Errorf("\"%s\" only partly matches %s (%s, %s): give its full name, ID or IP, or narrow with -p, -r or -e (no terminal to confirm)",
				query, in.Name, in.Profile, in.Region)
		}
		return inventory.Instance{}, false, severalMatch(res.Matches)
	case verdictConfirm:
		in := res.Matches[0]
		question := fmt.Sprintf("\"%s\" only partly matches %s (%s, %s). %s", query, in.Name, in.Profile, in.Region, why.question())
		if res.Exact { // a hybrid node, as above
			question = fmt.Sprintf("%s (%s, %s, %s) is a hybrid node: its name and IP are reported by the node itself. %s", in.Name, in.ID, in.Profile, in.Region, why.question())
		}
		if !confirm(question, why.confirmDefault()) {
			return inventory.Instance{}, false, errAborted
		}
		return in, true, nil
	}
	header := fmt.Sprintf("%d instances for \"%s\"", len(res.Matches), query)
	if res.Exact {
		header = fmt.Sprintf("\"%s\" exists %d times", query, len(res.Matches))
	}
	in, err = a.pickRunning(res.Matches, header)
	return in, true, err
}

// reach resolves what was typed (see resolve) and returns the instance with the
// credentials to act on it (see awsConfig). An instance whose data is older than
// freshWindow is looked up again first (see verify): if it is no longer the one meant,
// hop says what changed and searches once more, with the pair of that instance
// refreshed.
func (a *app) reach(ctx context.Context, args []string, why purpose) (inventory.Instance, aws.Config, error) {
	query := strings.TrimSpace(strings.Join(args, " "))
	for retried := false; ; retried = true {
		in, asked, err := a.resolve(ctx, args, why)
		if err != nil {
			return in, aws.Config{}, err
		}
		// A Windows instance is refused before signing in or looking it up.
		if why.sendsCommand() {
			if err := posixOnly(in); err != nil {
				return in, aws.Config{}, err
			}
		}
		cfg, err := a.awsConfig(ctx, in)
		if err != nil {
			return in, cfg, err
		}
		cur, changed, err := a.verify(ctx, cfg, in, query, why, asked)
		if err != nil || changed == "" {
			return cur, cfg, err
		}
		if retried {
			return in, cfg, errors.New(changed)
		}
		warn("%s: searching again", changed)
		if err := a.refresh(ctx, []inventory.Pair{a.pairOf(in)}); err != nil {
			return in, cfg, err
		}
	}
}

// verify makes sure that the instance is still the one meant, when the inventory of
// its pair is older than freshWindow and was not queried during this call: it is
// looked up again in its account, and the instance returned is the one AWS describes
// now. changed says what differs when it is gone, no longer running, or no longer
// matches what was typed.
//
// When the lookup fails, a shell or a tunnel goes ahead: the session targets the ID and
// fails cleanly if the instance is gone. run and push, which act with nobody watching,
// turn a choice hop made on its own into a question, and refuse without a terminal.
func (a *app) verify(ctx context.Context, cfg aws.Config, in inventory.Instance, query string, why purpose, asked bool) (cur inventory.Instance, changed string, err error) {
	var fetchedAt time.Time
	t := a.target(in)
	if t != nil {
		fetchedAt = t.FetchedAt
	}
	if time.Since(fetchedAt) < freshWindow || (t != nil && t.Err == "" && a.fetched[t.Key()]) {
		return in, "", nil
	}
	lctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	cur, found, err := inventory.Lookup(lctx, cfg, in)
	cancel()
	if ctx.Err() != nil {
		return in, "", ctx.Err()
	}
	where := fmt.Sprintf("%s (%s, %s, %s)", in.Name, in.ID, in.Profile, in.Region)
	switch {
	case err != nil:
		if !why.sendsCommand() || asked {
			return in, "", nil
		}
		doubt := fmt.Sprintf("%s could not be checked again (%s; inventory from %s)", where, inventory.Classify(err).Explain(), ago(fetchedAt))
		if !stdinTTY || !stderrTTY {
			return in, "", fmt.Errorf("%s: not acting on it without a terminal to confirm", doubt)
		}
		if !confirm(doubt+". "+why.question(), false) {
			return in, "", errAborted
		}
		return in, "", nil
	case !found:
		return in, where + " no longer exists", nil
	case !cur.Running():
		return in, where + " is now " + cur.State, nil
	case !a.stillMatches(cur, query, asked):
		switch {
		case cur.Name != in.Name:
			return in, where + " is now named " + cur.Name, nil
		case cur.PrivateIP != in.PrivateIP:
			return in, fmt.Sprintf("%s now has the private IP %q", where, cur.PrivateIP), nil
		}
		return in, fmt.Sprintf("%s no longer matches \"%s\"", where, query), nil
	}
	return cur, "", nil
}

// stillMatches reports whether the instance, as AWS describes it now, still answers
// what was typed: as strongly as before when hop chose it on its own.
func (a *app) stillMatches(in inventory.Instance, query string, asked bool) bool {
	switch {
	case query == "":
		return true
	case !asked:
		return match.Strong(in, query)
	}
	return len(match.Search([]inventory.Instance{in}, query, nil, a.cfg.Label).Matches) > 0
}

// pickRunning opens the picker over every candidate, stopped ones included so that
// nothing is hidden, and refuses a stopped choice.
func (a *app) pickRunning(items []inventory.Instance, header string) (inventory.Instance, error) {
	in, err := a.pick(items, header)
	if err != nil {
		return in, err
	}
	if !in.Running() {
		return inventory.Instance{}, notRunning(in.Name, []inventory.Instance{in})
	}
	return in, nil
}

func notRunning(query string, items []inventory.Instance) error {
	if len(items) == 1 {
		in := items[0]
		return fmt.Errorf("%s (%s, %s) is %s: start it first", in.Name, in.Profile, in.Region, in.State)
	}
	return fmt.Errorf("no running instance matches \"%s\": %s", query, describeAll(items))
}

func severalMatch(items []inventory.Instance) error {
	return fmt.Errorf("several instances match, be more specific: %s", describeAll(items))
}

// describeAll lists instances on one line, with the state of those not running.
func describeAll(items []inventory.Instance) string {
	var out []string
	for _, in := range items {
		s := in.Name + " (" + in.Profile + ", " + in.Region
		if !in.Running() {
			s += ", " + in.State
		}
		out = append(out, s+")")
	}
	return strings.Join(out, "; ")
}

// awsConfig loads the instance's profile and checks its credentials before handing over
// to the plugin. On an expired SSO token, it offers to open the portal right away.
func (a *app) awsConfig(ctx context.Context, in inventory.Instance) (aws.Config, error) {
	cfg, err := inventory.AWSConfig(ctx, in.Profile, in.Region, a.preferSSO(in.Profile))
	if err != nil {
		return cfg, err
	}
	_, err = cfg.Credentials.Retrieve(ctx)
	if err == nil {
		return cfg, nil
	}
	// Only a profile that signs in through a portal can be fixed by signing in again.
	// Classify reads error messages: for any other profile, the raw error says more.
	pt, ok := a.ssoPortal(in.Profile)
	if !ok || inventory.Classify(err) != inventory.ErrSSOExpired {
		return cfg, fmt.Errorf("credentials for profile %s: %w", in.Profile, inventory.RedactError(err))
	}
	if !confirm(fmt.Sprintf("SSO token expired for %s. Sign in again now?", pt.Label()), true) {
		return cfg, fmt.Errorf("SSO token expired for %s: hop login %s", pt.Label(), pt.Label())
	}
	if err := ssoLogin(pt); err != nil {
		return cfg, err
	}
	a.invalidatePortal(pt)
	if err := a.inv.Save(); err != nil {
		warn("cache not saved: %v", err)
	}
	cfg, err = inventory.AWSConfig(ctx, in.Profile, in.Region, a.preferSSO(in.Profile))
	if err != nil {
		return cfg, err
	}
	if _, err := cfg.Credentials.Retrieve(ctx); err != nil {
		return cfg, fmt.Errorf("still no credentials for %s: %w", in.Profile, err)
	}
	return cfg, nil
}

// warnAgent warns when the SSM agent was not responding at the last inventory. It tries
// anyway: the agent may have come back since.
func (a *app) warnAgent(in inventory.Instance) {
	t := a.target(in)
	switch {
	case in.Online():
	case in.Agent != "":
		warn("SSM agent %s on %s (last contact %s): the session may fail", in.Agent, in.Name, ago(in.LastPing))
	case t != nil && t.AgentsKnown:
		warn("no SSM agent registered on %s: the session may fail", in.Name)
	}
}

func closest(s string, names []string) string {
	res := match.Search(fakeInstances(names), s, nil, func(string) string { return "" })
	if len(res.Matches) > 0 {
		return res.Matches[0].Name
	}
	if len(res.Suggestions) > 0 {
		return res.Suggestions[0]
	}
	return ""
}

func fakeInstances(names []string) []inventory.Instance {
	var out []inventory.Instance
	for _, n := range names {
		out = append(out, inventory.Instance{Name: n, ID: n})
	}
	return out
}

// regionsOf returns the distinct regions, in order of appearance.
func regionsOf(items []inventory.Instance) []string {
	var out []string
	for _, in := range items {
		if !slices.Contains(out, in.Region) {
			out = append(out, in.Region)
		}
	}
	return out
}
