package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/spf13/cobra"

	"github.com/morganseznec/hop/internal/atomicfile"
	"github.com/morganseznec/hop/internal/inventory"
	"github.com/morganseznec/hop/internal/match"
	"github.com/morganseznec/hop/internal/session"
)

// Completion of a path on an instance, for hop pull and hop push: Tab lists the
// directory there, as scp does over SSH. Unlike the other completions, which read the
// cache, this one opens a session on the instance (see session.List). So it only does
// for an instance that a command would act on without a question, it never asks
// anything itself, and remote_completion: false turns it off.

// remoteTimeout bounds what one Tab waits for: the credentials, the session and the
// listing. Tests shorten it.
var remoteTimeout = 8 * time.Second

const (
	// remoteFresh is how long a listing answers the next Tab without another session:
	// the time to type a few letters of a name and press Tab again.
	remoteFresh = 30 * time.Second
	// remoteKept is how many listings the cache file holds.
	remoteKept = 8
)

// completeRemote completes a path on the instance that args[at] names.
func completeRemote(f *filters, at int) cobra.CompletionFunc {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if at >= len(args) {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		ctx := cmd.Context()
		if ctx == nil {
			ctx = context.Background()
		}
		paths, why := remotePaths(ctx, f, args[at], toComplete)
		directive := cobra.ShellCompDirectiveNoFileComp
		// A directory is not the end of a path: no space after it. A file alone is.
		if len(paths) != 1 || strings.HasSuffix(paths[0], "/") {
			directive |= cobra.ShellCompDirectiveNoSpace
		}
		if why != "" {
			// Shown under the command line by the shells that show such hints.
			paths = cobra.AppendActiveHelp(paths, why)
		}
		return paths, directive
	}
}

// remotePaths returns the paths on the instance that start with what is typed, and
// why when there is none to offer, or not all of them.
func remotePaths(ctx context.Context, f *filters, query, typed string) (paths []string, why string) {
	switch {
	case typed == "":
		// A path on an instance is absolute: that much can be typed for free.
		return []string{"/"}, ""
	case !strings.HasPrefix(typed, "/"):
		return nil, "a path on the instance starts with /"
	}
	a, err := newApp(*f)
	if err != nil || !a.cfg.CompletesRemotePaths() {
		return nil, ""
	}
	in, why := a.sure(query)
	if why != "" {
		return nil, why
	}
	cut := strings.LastIndex(typed, "/") + 1
	dir, prefix := typed[:cut], typed[cut:]

	names, more, cached := cachedListing(in.Key(), dir, prefix, time.Now())
	if !cached {
		ctx, cancel := context.WithTimeout(ctx, remoteTimeout)
		defer cancel()
		cfg, why := a.quietConfig(ctx, in)
		if why != "" {
			return nil, why
		}
		req := session.Request{Profile: in.Profile, Region: in.Region, InstanceID: in.ID, PreferSSO: a.preferSSO(in.Profile)}
		names, more, err = session.List(ctx, cfg, req, dir, prefix)
		switch {
		case errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil:
			return nil, fmt.Sprintf("%s did not list %s within %s", in.Name, dir, remoteTimeout)
		case err != nil:
			return nil, shortError(err.Error())
		}
		saveListing(dirListing{Instance: in.Key(), Dir: dir, Prefix: prefix, More: more, Names: names, At: time.Now()})
	}
	for _, n := range names {
		// As a shell does: a name that starts with a dot shows once a dot is typed.
		if !strings.HasPrefix(n, prefix) || (strings.HasPrefix(n, ".") && !strings.HasPrefix(prefix, ".")) {
			continue
		}
		paths = append(paths, dir+n)
	}
	if more {
		why = fmt.Sprintf("more than %d names in %s: type the start of one", session.ListLimit, dir)
	}
	return paths, why
}

// sure returns the instance that query names without a doubt: the one a command acts
// on without a question (see decide), read from the cache as it is. A completion has
// nobody to ask, so anything less gets no path, and why says so.
func (a *app) sure(query string) (in inventory.Instance, why string) {
	res := match.Search(a.instances(true), query, a.recents, a.cfg.Label)
	switch v := decide(res.Matches, query, false); {
	case res.Fuzzy || v == verdictNone:
		return in, fmt.Sprintf("no instance named %q: no path to complete", query)
	case v == verdictNotRunning:
		return in, fmt.Sprintf("%s is not running: no path to complete", res.Matches[0].Name)
	case v != verdictAct:
		return in, fmt.Sprintf("%q does not name one instance for sure: type its full name to complete a path there", query)
	}
	in = res.Matches[0]
	if in.GCP() {
		return in, "paths are not completed on GCP instances"
	}
	if err := posixOnly(in); err != nil {
		return in, err.Error()
	}
	return in, ""
}

// quietConfig loads the credentials to reach an instance, as awsConfig does, but
// never offers to sign in: a completion asks nothing. why says what is missing.
func (a *app) quietConfig(ctx context.Context, in inventory.Instance) (cfg aws.Config, why string) {
	if a.files.MFAProfile(in.Profile) != nil {
		return cfg, fmt.Sprintf("profile %s needs an MFA code: no path to complete", in.Profile)
	}
	cfg, err := inventory.AWSConfig(ctx, in.Profile, in.Region, a.preferSSO(in.Profile))
	if err == nil {
		_, err = cfg.Credentials.Retrieve(ctx)
	}
	if err == nil {
		return cfg, ""
	}
	if pt, ok := a.ssoPortal(in.Profile); ok && inventory.Classify(err) == inventory.ErrSSOExpired {
		return cfg, fmt.Sprintf("SSO token expired for %s: hop login %s", pt.Label(), pt.Label())
	}
	return cfg, "no credentials for profile " + in.Profile + ": hop doctor"
}

// dirListing is what a directory of an instance held when it was listed.
type dirListing struct {
	// Instance is the key of the instance (profile, region, ID).
	Instance string `json:"instance"`
	Dir      string `json:"dir"`
	// Prefix is what the names listed start with, and More that some were left out.
	Prefix string    `json:"prefix,omitempty"`
	More   bool      `json:"more,omitempty"`
	Names  []string  `json:"names"`
	At     time.Time `json:"at"`
}

// answers reports whether the listing holds every name of dir that starts with
// prefix, and is recent enough to be believed.
func (l dirListing) answers(instance, dir, prefix string, now time.Time) bool {
	age := now.Sub(l.At)
	return l.Instance == instance && l.Dir == dir && age >= 0 && age < remoteFresh &&
		strings.HasPrefix(prefix, l.Prefix) && (!l.More || prefix == l.Prefix)
}

// listingsFile is the file the last listings are kept in, next to the inventory.
type listingsFile struct {
	Version  int          `json:"version"`
	Listings []dirListing `json:"listings"`
}

func listingsPath() (string, error) {
	dir, err := inventory.CacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "paths.json"), nil
}

// freshListings reads the listings that are still recent. A missing or unreadable
// file is no listing: it is only a cache.
func freshListings(now time.Time) []dirListing {
	p, err := listingsPath()
	if err != nil {
		return nil
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	var f listingsFile
	if json.Unmarshal(b, &f) != nil || f.Version != 1 {
		return nil
	}
	var fresh []dirListing
	for _, l := range f.Listings {
		if age := now.Sub(l.At); age >= 0 && age < remoteFresh {
			fresh = append(fresh, l)
		}
	}
	return fresh
}

func cachedListing(instance, dir, prefix string, now time.Time) (names []string, more, ok bool) {
	for _, l := range freshListings(now) {
		if l.answers(instance, dir, prefix, now) {
			return l.Names, l.More, true
		}
	}
	return nil, false, false
}

// saveListing keeps a listing for the next Tab, with the few others that are still
// recent: the names of older ones leave the disk here. Best effort: without the file,
// the next Tab lists again.
func saveListing(l dirListing) {
	p, err := listingsPath()
	if err != nil {
		return
	}
	if os.MkdirAll(filepath.Dir(p), 0o700) != nil {
		return
	}
	keep := []dirListing{l}
	for _, old := range freshListings(l.At) {
		if len(keep) < remoteKept && (old.Instance != l.Instance || old.Dir != l.Dir) {
			keep = append(keep, old)
		}
	}
	b, err := json.Marshal(listingsFile{Version: 1, Listings: keep})
	if err != nil {
		return
	}
	tmp, err := atomicfile.Create(p, ".paths-*")
	if err != nil {
		return
	}
	defer tmp.Discard()
	if _, err := tmp.Write(b); err != nil {
		return
	}
	_ = tmp.Commit()
}
