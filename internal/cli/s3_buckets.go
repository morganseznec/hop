package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	fuzzyfinder "github.com/ktr0731/go-fuzzyfinder"
	"github.com/mattn/go-runewidth"

	"github.com/morganseznec/hop/internal/bucket"
	"github.com/morganseznec/hop/internal/inventory"
	"github.com/morganseznec/hop/internal/match"
)

// bucketScope explains a scope without any AWS profile before S3 is queried: buckets
// are listed per AWS profile, whatever GCP projects are in scope.
func (a *app) bucketScope() error {
	if len(a.selectedProfiles()) > 0 {
		return nil
	}
	if err := a.checkScope(); err != nil {
		return err
	}
	return errors.New("no AWS profile in scope: hop s3 lists the buckets of AWS profiles")
}

// bucketProfiles returns the names of the profiles in scope, SSO profiles first.
func (a *app) bucketProfiles() []string {
	var out []string
	for _, p := range a.selectedProfiles() {
		out = append(out, p.Name)
	}
	return out
}

// staleBuckets returns, as pairs to query, the profiles in scope whose bucket list is
// older than maxAge, or all of them with force. The region of a pair is the endpoint
// the call goes to: the profile's main one. A profile already queried during this call
// is never queried again, and one recorded as needing an MFA code stays as is until
// mfa_serial goes from it (see olderThan).
func (a *app) staleBuckets(maxAge time.Duration, force bool) []inventory.Pair {
	now := time.Now()
	var todo []inventory.Pair
	for _, p := range a.selectedProfiles() {
		if a.fetchedBuckets[p.Name] {
			continue
		}
		pair := inventory.Pair{Profile: p.Name, Region: a.mainRegion(p.Name), PreferSSO: p.PreferSSO(), Token: a.tokenKey(p.Name)}
		l, ok := a.inv.Buckets[p.Name]
		switch {
		case ok && l.ErrKind == inventory.ErrMFA:
			if a.files.MFAProfile(p.Name) == nil {
				todo = append(todo, pair)
			}
		case force || !ok || !l.Fresh(maxAge, now):
			todo = append(todo, pair)
		}
	}
	return todo
}

// ensureBuckets lists the buckets of the profiles whose list is older than the TTL (or
// of all of them with force) and saves the cache.
func (a *app) ensureBuckets(ctx context.Context, force bool) error {
	return a.refreshBuckets(ctx, a.staleBuckets(a.cfg.TTL(), force))
}

// refreshBuckets lists the buckets of the given profiles and saves the cache. As for
// instances (see refresh), nothing is recorded when ctx is canceled, and a profile that
// needs an MFA code is recorded in error without being queried.
func (a *app) refreshBuckets(ctx context.Context, todo []inventory.Pair) error {
	if len(todo) == 0 {
		return nil
	}
	var query []inventory.Pair
	var results []*inventory.BucketList
	for _, p := range todo {
		if m := a.files.MFAProfile(p.Profile); m != nil {
			results = append(results, &inventory.BucketList{Profile: p.Profile,
				Err: mfaError(p.Profile, m).Error(), ErrKind: inventory.ErrMFA, ErrAt: time.Now()})
			continue
		}
		query = append(query, p)
	}
	if len(query) > 0 {
		progress := func(done, total int) {
			if stderrTTY {
				fmt.Fprintf(os.Stderr, "\r\033[Khop: buckets %d/%d", done, total)
			}
		}
		results = append(results, inventory.FetchBuckets(ctx, query, 25*time.Second, progress)...)
		if stderrTTY {
			fmt.Fprint(os.Stderr, "\r\033[K")
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if a.fetchedBuckets == nil {
		a.fetchedBuckets = map[string]bool{}
	}
	for _, l := range results {
		a.inv.PutBuckets(l)
		a.fetchedBuckets[l.Profile] = true
	}
	if err := a.inv.Save(); err != nil {
		warn("cache not saved: %v", err)
	}
	return nil
}

// buckets returns the buckets of the profiles in scope, narrowed by -r (the region of
// the bucket, whatever the region of its profile) and -e.
func (a *app) buckets() []inventory.Bucket {
	var out []inventory.Bucket
	for _, b := range a.inv.BucketsOf(a.bucketProfiles()) {
		if len(a.f.regions) > 0 && !slices.Contains(a.f.regions, b.Region) {
			continue
		}
		if a.f.env != "" && b.Env != a.f.env {
			continue
		}
		out = append(out, b)
	}
	return out
}

// reportBucketProblems summarizes the profiles whose buckets could not be listed, like
// reportProblems does for instances.
func (a *app) reportBucketProblems() {
	var problems []problem
	for _, p := range a.bucketProfiles() {
		if l := a.inv.Buckets[p]; l != nil && l.Err != "" {
			problems = append(problems, problem{p, p, l.ErrKind, l.Err})
		}
	}
	a.report(problems, "hop s3 ls --refresh")
}

// asInstances presents buckets to the search, which ranks instances: the name is what
// gets searched, and every bucket counts as running.
func asInstances(bs []inventory.Bucket) []inventory.Instance {
	out := make([]inventory.Instance, len(bs))
	for i, b := range bs {
		out[i] = inventory.Instance{ID: b.Name, Name: b.Name, Profile: b.Profile, Region: b.Region, Env: b.Env, State: "running"}
	}
	return out
}

// searchBuckets ranks the buckets against a query, like the search does for instances,
// and returns the matching buckets in that order.
func (a *app) searchBuckets(bs []inventory.Bucket, query string) (match.Result, []inventory.Bucket) {
	res := match.Search(asInstances(bs), query, nil, a.cfg.Label)
	byName := map[string]inventory.Bucket{}
	for _, b := range bs {
		byName[b.Name] = b
	}
	out := make([]inventory.Bucket, len(res.Matches))
	for i, m := range res.Matches {
		out[i] = byName[m.Name]
	}
	return res, out
}

// bucketUse is what a bucket is resolved for: it words the question asked about a loose
// match, and a doubt before writing is a no.
type bucketUse struct {
	question string
	write    bool
}

var (
	forListing  = bucketUse{question: "List it?"}
	forDownload = bucketUse{question: "Download from it?"}
	forUpload   = bucketUse{question: "Upload to it?", write: true}
)

// bucketName is the bucket part of a place, and how it was written. After s3://, it is
// the exact name of a bucket, as everywhere else in AWS. On its own, it is searched like
// an instance name, among the buckets hop lists.
type bucketName struct {
	name  string
	exact bool
}

// nameOf reads the bucket part of the argument a place was parsed from.
func nameOf(arg, name string) bucketName {
	return bucketName{name: name, exact: bucket.HasScheme(arg)}
}

// resolveBucket turns the bucket part of a place into a bucket.
//
// An exact name (s3://) is looked up as it is: see exactBucket. Otherwise the buckets
// listed are searched with the rule instances follow (see decide): a single bucket
// found by its exact name, or by whole words of its name, is used directly; a looser
// match asks first, several open the picker, and without a terminal hop lists the
// candidates and stops. A search never leaves the buckets listed: a word that matches
// none of them is not tried as the name of somebody else's bucket.
func (a *app) resolveBucket(ctx context.Context, n bucketName, use bucketUse) (inventory.Bucket, error) {
	if err := a.ensureBuckets(ctx, a.f.refresh); err != nil {
		return inventory.Bucket{}, err
	}
	if n.exact {
		return a.exactBucket(ctx, n.name, use)
	}
	name := n.name
	interactive := stdinTTY && stderrTTY
	res, found := a.searchBuckets(a.buckets(), name)
	v := decide(res.Matches, name, interactive)
	// A miss may come from a list that predates the bucket: the lists not refreshed
	// within freshWindow are read again, and the search runs once more.
	if v != verdictAct && !res.Exact && (res.Fuzzy || v != verdictPick) {
		if err := a.refreshBuckets(ctx, a.staleBuckets(freshWindow, false)); err != nil {
			return inventory.Bucket{}, err
		}
		res, found = a.searchBuckets(a.buckets(), name)
		v = decide(res.Matches, name, interactive)
	}
	where := func(b inventory.Bucket) string { return b.Profile + ", " + regionOr(b.Region) }

	notFound := func(suggestions []string) error {
		a.reportBucketProblems()
		if err := a.filteredOut(name); err != nil {
			return err
		}
		msg := fmt.Sprintf("no bucket matches %q", name)
		if len(suggestions) > 0 {
			msg += "\n     did you mean: " + strings.Join(suggestions, ", ")
		}
		return errors.New(msg + "\n     a bucket of another account is not listed: write its exact name after s3://, with the profile that reaches it (-p)")
	}
	names := func(n int) []string {
		var out []string
		for i := 0; i < len(found) && i < n; i++ {
			out = append(out, found[i].Name)
		}
		return out
	}
	// The first few, then how many more: a broad word matches dozens of buckets.
	several := func() string {
		const most = 5
		s := strings.Join(names(most), ", ")
		if len(found) > most {
			s += fmt.Sprintf(", and %d more", len(found)-most)
		}
		return s
	}
	switch {
	case len(found) == 0:
		return inventory.Bucket{}, notFound(res.Suggestions)
	case v == verdictAct:
		return found[0], nil
	case !interactive && res.Fuzzy:
		return inventory.Bucket{}, notFound(names(3))
	case !interactive && len(found) == 1:
		return inventory.Bucket{}, fmt.Errorf("%q only partly matches the bucket %s (%s): give its full name (no terminal to confirm)", name, found[0].Name, where(found[0]))
	case !interactive:
		return inventory.Bucket{}, fmt.Errorf("several buckets match %q, be more specific: %s", name, several())
	case len(found) == 1:
		b := found[0]
		question := fmt.Sprintf("%q only partly matches the bucket %s (%s). %s", name, b.Name, where(b), use.question)
		if res.Fuzzy {
			question = fmt.Sprintf("No bucket is named %q. Did you mean %s (%s)?", name, b.Name, where(b))
		}
		if err := ask(ctx, question, !use.write); err != nil {
			return inventory.Bucket{}, err
		}
		return b, nil
	}
	header := fmt.Sprintf("%d buckets for %q", len(found), name)
	if res.Fuzzy {
		header = fmt.Sprintf("no bucket is named %q, close matches", name)
	}
	return a.pickBucket(found, header)
}

// filteredOut explains a name that a listed bucket carries, when -r or -e leaves that
// bucket out: better than "not found", or than suggesting another one.
func (a *app) filteredOut(name string) error {
	for _, b := range a.inv.BucketsOf(a.bucketProfiles()) {
		if !strings.EqualFold(b.Name, name) {
			continue
		}
		if len(a.f.regions) > 0 && !slices.Contains(a.f.regions, b.Region) {
			return fmt.Errorf("no bucket matches %q: -r %s leaves out %s, which is in %s", name, strings.Join(a.f.regions, ","), b.Name, regionOr(b.Region))
		}
		if a.f.env != "" && b.Env != a.f.env {
			return fmt.Errorf("no bucket matches %q: -e %s leaves out %s", name, a.f.env, b.Name)
		}
	}
	return nil
}

// exactBucket finds the bucket that carries exactly this name: among the buckets
// listed, or else by asking S3 with the one profile in scope, since S3 lists the buckets
// an account owns and not those it may use.
//
// A bucket found that way belongs to another account. hop reads from it, as it was
// named in full; before writing to it, hop asks, and refuses without a terminal: a
// mistyped name can be somebody else's bucket.
func (a *app) exactBucket(ctx context.Context, name string, use bucketUse) (inventory.Bucket, error) {
	listed := func() (inventory.Bucket, bool) {
		for _, b := range a.buckets() {
			if b.Name == name {
				return b, true
			}
		}
		return inventory.Bucket{}, false
	}
	if b, ok := listed(); ok {
		return b, nil
	}
	if err := a.refreshBuckets(ctx, a.staleBuckets(freshWindow, false)); err != nil {
		return inventory.Bucket{}, err
	}
	if b, ok := listed(); ok {
		return b, nil
	}
	if err := a.filteredOut(name); err != nil {
		return inventory.Bucket{}, err
	}
	ps := a.bucketProfiles()
	if len(ps) != 1 {
		a.reportBucketProblems()
		return inventory.Bucket{}, fmt.Errorf("no bucket named %s in the accounts of the %d profiles in scope\n     a bucket of another account is not listed: name the profile that reaches it with -p", name, len(ps))
	}
	b, err := a.probeBucket(ctx, ps[0], name)
	if err != nil {
		return inventory.Bucket{}, err
	}
	if use.write {
		if !stdinTTY || !stderrTTY {
			return inventory.Bucket{}, fmt.Errorf("s3://%s is not a bucket of the account of %s: hop uploads without asking only to a bucket that its profile lists (no terminal to confirm)", name, ps[0])
		}
		if err := ask(ctx, fmt.Sprintf("s3://%s (%s) is not a bucket of the account of %s. %s", name, b.Region, ps[0], use.question), false); err != nil {
			return inventory.Bucket{}, err
		}
	}
	return b, nil
}

// probeBucket asks S3, with the credentials of a profile, where the bucket of that exact
// name lives: it is how a bucket the profile may use without owning it is found.
func (a *app) probeBucket(ctx context.Context, profile, name string) (inventory.Bucket, error) {
	main := a.mainRegion(profile)
	cfg, err := a.awsConfig(ctx, profile, main)
	if err != nil {
		return inventory.Bucket{}, err
	}
	lctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	region, err := inventory.BucketRegion(lctx, cfg, name)
	switch {
	case ctx.Err() != nil:
		return inventory.Bucket{}, ctx.Err()
	case err != nil:
		return inventory.Bucket{}, fmt.Errorf("no bucket named %s that profile %s can reach", name, profile)
	}
	if region == "" {
		region = main
	}
	// -r narrows what hop acts on, listed or not.
	if len(a.f.regions) > 0 && !slices.Contains(a.f.regions, region) {
		return inventory.Bucket{}, fmt.Errorf("-r %s leaves out %s, which is in %s", strings.Join(a.f.regions, ","), name, region)
	}
	return inventory.Bucket{Name: name, Profile: profile, Region: region, Env: inventory.DeriveEnv(nil, name, profile)}, nil
}

// reachBucket resolves the bucket part of a location (see resolveBucket) and returns
// the bucket with the credentials of its profile, set to the region the bucket lives
// in. A region the list did not give is asked to S3.
func (a *app) reachBucket(ctx context.Context, name bucketName, use bucketUse) (inventory.Bucket, aws.Config, error) {
	if err := a.bucketScope(); err != nil {
		return inventory.Bucket{}, aws.Config{}, err
	}
	b, err := a.resolveBucket(ctx, name, use)
	if err != nil {
		return b, aws.Config{}, err
	}
	region := b.Region
	if region == "" {
		region = a.mainRegion(b.Profile)
	}
	cfg, err := a.awsConfig(ctx, b.Profile, region)
	if err != nil {
		return b, cfg, err
	}
	if b.Region == "" {
		lctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		found, err := inventory.BucketRegion(lctx, cfg, b.Name)
		cancel()
		switch {
		case ctx.Err() != nil:
			return b, cfg, ctx.Err()
		case err != nil:
			return b, cfg, fmt.Errorf("no bucket named %s that profile %s can reach (%s)", b.Name, b.Profile, shortError(err.Error()))
		case found != "":
			region = found
		}
		b.Region = region
		cfg.Region = region
	}
	return b, cfg, nil
}

func regionOr(region string) string {
	if region == "" {
		return "region unknown"
	}
	return region
}

// pickBucket opens the fuzzy picker over buckets: name, profile, region and
// environment on each line, all of them searchable.
func (a *app) pickBucket(items []inventory.Bucket, header string) (inventory.Bucket, error) {
	wName, wProf := 0, 0
	for _, b := range items {
		wName = max(wName, runewidth.StringWidth(b.Name))
		wProf = max(wProf, runewidth.StringWidth(a.profileLabel(b.Profile)))
	}
	line := func(i int) string {
		b := items[i]
		return strings.TrimRight(fmt.Sprintf("%s  %s  %-14s %s",
			runewidth.FillRight(b.Name, wName), runewidth.FillRight(a.profileLabel(b.Profile), wProf), b.Region, b.Env), " ")
	}
	i, err := fuzzyfinder.Find(items, line,
		fuzzyfinder.WithPromptString("hop> "),
		fuzzyfinder.WithHeader(header+" · Enter to choose, Esc to cancel"))
	if errors.Is(err, fuzzyfinder.ErrAbort) {
		return inventory.Bucket{}, errAborted
	}
	if err != nil {
		return inventory.Bucket{}, err
	}
	return items[i], nil
}
