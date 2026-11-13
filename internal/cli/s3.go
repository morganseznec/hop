package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/spf13/cobra"

	"github.com/morganseznec/hop/internal/bucket"
	"github.com/morganseznec/hop/internal/inventory"
)

func newS3(f *filters) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "s3 [bucket words... | bucket/prefix]",
		Short: "Pick an S3 bucket across all accounts and browse it; list, download and upload objects",
		Long: `Pick an S3 bucket across all accounts and browse it; list, download and upload objects.

hop lists the buckets of every AWS profile on this machine, so a bucket is named
without saying which account or region holds it: "acme-logs/2026/app.log". The bucket
part is searched like an instance name, among the buckets listed: an exact name, or
whole words of it ("logs/2026/app.log"), is used directly; a looser match asks first.

After s3://, the bucket part is an exact name, as everywhere else in AWS. That is how
to name a bucket of another account, which S3 does not list, with the profile that
reaches it (-p); hop asks before it uploads to such a bucket. It is also what tells a
bucket from an instance on the other commands: hop ls s3://acme-logs/,
hop pull s3://acme-logs/app.log, hop push dump.sql s3://acme-backups/.

With no command, in a terminal, hop s3 opens a picker over those buckets, like hop
does over instances: type to filter, Enter to open one. Inside, the same picker shows
what the bucket holds: Enter on a prefix goes down, ".." goes back up, and Enter on
an object downloads it to the current directory, after a question.

With words, hop s3 searches the buckets first, as hop searches instances: a single
bucket found opens at once, several show in the picker, and only them. That is the
quick way through an account with a hundred buckets: "hop s3 billing logs",
"hop s3 'acme-p-*'". With a place, the picker opens there: "hop s3 logs/2026/".

Patterns work wherever a name is searched: * for any run of characters, ? for one,
[abc] for one of a set. In zsh, quote them, or load hop's integration
(eval "$(hop init zsh)"), which keeps the shell from expanding them before hop.

hop never deletes an object, and asks before replacing one. CloudTrail records
reads and writes of objects only where the account logs S3 data events.`,
		Example: `  hop s3                                      pick a bucket, walk it, download an object
  hop s3 billing logs                         straight to the bucket those words find
  hop s3 logs/2026/                           the picker, opened at that prefix
  hop s3 -p 'acme-*' -e prod                  the picker, over some profiles
  hop s3 ls                                   every bucket of every account
  hop s3 ls -R logs/2026/ error '*.gz'        find objects by words and patterns
  hop s3 ls -lh acme-logs/2026/               what a prefix holds, with sizes and dates
  hop s3 pull acme-logs/2026/app.log          download to ./app.log
  hop s3 push ./dump.sql.gz acme-backups/db/  upload, in parts when the file is large`,
		Args:              cobra.ArbitraryArgs,
		ValidArgsFunction: completeBuckets(f, 0),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Without a terminal there is nobody to choose: the help, or the command
			// that lists instead.
			if !stdinTTY || !stderrTTY {
				if len(args) == 0 {
					return cmd.Help()
				}
				return fmt.Errorf("no terminal: hop s3 %s opens a picker (to list without one: hop s3 ls %s)", strings.Join(args, " "), strings.Join(args, " "))
			}
			a, err := newApp(*f)
			if err != nil {
				return err
			}
			return a.browse(cmd.Context(), args)
		},
	}
	cmd.AddCommand(newS3Ls(f), newS3Pull(f), newS3Push(f))
	return cmd
}

// displayZone is the time zone dates are shown in: the machine's, as ls does. Tests set
// it.
var displayZone = time.Local

// listOptions are the options of a listing, named after those of ls.
type listOptions struct {
	all, long, human, recursive, byTime, bySize, reverse, json bool
	// filter keeps the objects that answer the pattern in the place listed, and the
	// words typed after it. It is not an option of its own.
	filter bucket.Filter
}

// register adds the options of ls to a command, hidden from its help when the command
// lists something else first (hop ls). -h is --human-readable, as in ls, so the help
// keeps only its long form; -h without -l still shows it (see helpAsked).
func (o *listOptions) register(cmd *cobra.Command, hidden bool) {
	fl := cmd.Flags()
	fl.Bool("help", false, "help for "+cmd.Name()+" (and -h on its own)")
	fl.BoolVarP(&o.long, "long", "l", false, "size, date and storage class of each object")
	fl.BoolVarP(&o.human, "human-readable", "h", false, "with -l, sizes in KB, MB and GB")
	fl.BoolVarP(&o.recursive, "recursive", "R", false, "every object below, instead of grouping keys at the next /")
	fl.BoolVarP(&o.byTime, "time", "t", false, "newest first")
	fl.BoolVarP(&o.bySize, "size", "S", false, "largest first")
	fl.BoolVar(&o.reverse, "reverse", false, "reverse the order (-r is --region)")
	if hidden {
		for _, name := range []string{"long", "human-readable", "recursive", "time", "size", "reverse"} {
			_ = fl.MarkHidden(name)
		}
	}
}

// helpAsked reports whether -h stands for the help: without -l it changes nothing in a
// listing, and it is what every other command takes it for.
func (o listOptions) helpAsked() bool { return o.human && !o.long }

func newS3Ls(f *filters) *cobra.Command {
	var o listOptions
	cmd := &cobra.Command{
		Use:   "ls [filter... | bucket/prefix [filter...]]",
		Short: "List buckets across all accounts, or the objects of a bucket",
		Long: `List buckets across all accounts, or the objects of a bucket.

Without argument, or with words, it lists the buckets of every AWS profile in scope:
-p narrows the profiles, -r keeps the buckets of some regions, -e an environment.
That takes one ListBuckets call per profile, cached like the instances (cache_ttl).
Words filter the list like the search does. A profile that cannot be reached keeps
its last known buckets, and the reason is printed on stderr.

An argument with a "/" names a place in a bucket: "acme-logs/" is the top of that
bucket, "acme-logs/2026/" what its keys hold after that prefix. Keys are grouped at
the next "/", like directories, unless -R lists every object below. The bucket part
is searched like an instance name: an exact name, or whole words of it ("logs/"), is
used directly; a looser match asks first. After s3://, it is an exact name
(s3://acme-logs/2026/), which also reaches a bucket of another account with -p.

What comes after the place filters what it holds: a plain word must be part of the
name, in any case, and a word with a wildcard (* ? [abc]) is a pattern for the name,
or for its last part. A pattern may also end the place itself: "acme-logs/2026/*.log".
At one level by default; with -R, among every object below, which is how to find a
file in a bucket: hop s3 ls -R acme-logs/ error 2026-11. The same goes for buckets:
hop s3 ls 'acme-p-*' logs.

The options are those of ls, and combine (-alh): -l for the size, the date and the
storage class, -h for sizes in KB, MB and GB, -a for the names that start with a dot,
left out otherwise, -t and -S to sort. -h on its own shows this help. Reversing the
order is --reverse, since -r is --region.

zsh expands * itself, and stops on a pattern that matches no file here: quote it, or
load hop's zsh integration (hop init zsh), which leaves patterns to hop.`,
		Example: `  hop s3 ls                          every bucket of every account
  hop s3 ls logs                     filter the buckets by words
  hop s3 ls acme-logs/               the top of a bucket
  hop s3 ls 'acme-p-*'               buckets by a pattern
  hop s3 ls -alh acme-logs/2026/     sizes, dates, names that start with a dot
  hop s3 ls acme-logs/2026/ '*.gz'   what fits a pattern at that level
  hop s3 ls -R acme-logs/ error 11   find objects below, by words
  hop s3 ls -R acme-logs/2026/ --json | jq ...`,
		ValidArgsFunction: completeBuckets(f, 0),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runS3Ls(cmd, f, o, args)
		},
	}
	cmd.Flags().BoolVarP(&o.all, "all", "a", false, "also list the names that start with a dot")
	cmd.Flags().BoolVar(&o.json, "json", false, "JSON output")
	o.register(cmd, false)
	return cmd
}

// runS3Ls lists buckets, or the objects of the location given: it serves "hop s3 ls"
// and "hop ls s3://...".
func runS3Ls(cmd *cobra.Command, f *filters, o listOptions, args []string) error {
	if o.helpAsked() {
		return cmd.Help()
	}
	var loc bucket.Location
	var name bucketName
	if len(args) > 0 && bucket.IsLocation(args[0]) {
		var err error
		if loc, err = bucket.Parse(args[0]); err != nil {
			return err
		}
		switch {
		case bucket.HasMeta(loc.Bucket) && loc.Key != "":
			return fmt.Errorf("a pattern in the bucket name lists buckets, not what one of them holds: hop s3 ls '%s'", loc.Bucket)
		case bucket.HasMeta(loc.Bucket):
			// "s3://acme-*": the buckets that fit, with the words that follow.
			args, loc = append([]string{loc.Bucket}, args[1:]...), bucket.Location{}
		case loc.Bucket != "":
			// The place first, then what to look for in it: a pattern in the key
			// itself ("2026/*.log"), words after it, or both.
			var pattern string
			loc.Key, pattern = bucket.SplitPattern(loc.Key)
			if o.filter, err = bucket.NewFilter(pattern, args[1:]); err != nil {
				return err
			}
			// A pattern that names several levels ("*/app.log") needs every object
			// below to be read.
			o.recursive = o.recursive || strings.Contains(pattern, "/")
			name = nameOf(args[0], loc.Bucket)
		default:
			args = args[1:]
		}
	}
	a, err := newApp(*f)
	if err != nil {
		return err
	}
	if loc.Bucket == "" {
		return a.listBuckets(cmd, o, args)
	}
	return a.listObjects(cmd, o, loc, name)
}

func (a *app) listBuckets(cmd *cobra.Command, o listOptions, words []string) error {
	if err := a.bucketScope(); err != nil {
		return err
	}
	if err := a.ensureBuckets(cmd.Context(), a.f.refresh); err != nil {
		return err
	}
	items := a.buckets()
	total := len(items)
	if len(words) > 0 {
		// The search ranks by relevance; the list keeps the per-account order.
		found, err := a.matchBuckets(items, words)
		if err != nil {
			return err
		}
		keep := map[string]bool{}
		for _, b := range found {
			keep[b.Name] = true
		}
		items = slices.DeleteFunc(items, func(b inventory.Bucket) bool { return !keep[b.Name] })
	}
	if o.byTime {
		sort.SliceStable(items, func(i, j int) bool { return items[i].Created.After(items[j].Created) })
	}
	if o.reverse {
		slices.Reverse(items)
	}
	// As for instances, the problems go to stderr with --json too: a script must not
	// take the last known buckets of an unreachable account for the current ones.
	if o.json {
		if err := a.printBucketsJSON(items); err != nil {
			return err
		}
		a.reportBucketProblems()
		return nil
	}
	switch {
	case len(items) > 0:
		t := &table{head: []string{"NAME", "PROFILE", "REGION", "ENV", "CREATED"}}
		for _, b := range items {
			created := ""
			if !b.Created.IsZero() {
				created = b.Created.In(displayZone).Format("2006-01-02")
			}
			t.add(cell{b.Name, ""}, cell{a.profileLabel(b.Profile), ""}, cell{b.Region, ""}, cell{b.Env, ""}, cell{created, ""})
		}
		t.print(os.Stdout, stdoutTTY)
		a.printBucketsFooter(items)
		if len(items) == 1 && len(words) > 0 {
			hint("hop s3 ls %s/ lists what it holds", items[0].Name)
		}
	case len(words) > 0 && total > 0:
		info("no buckets match %q", strings.Join(words, " "))
	default:
		info("no buckets")
	}
	a.reportBucketProblems()
	return nil
}

// printBucketsJSON writes the buckets as a JSON list, each with the age of its data,
// like hop ls --json does for instances.
func (a *app) printBucketsJSON(items []inventory.Bucket) error {
	type row struct {
		inventory.Bucket
		Label     string    `json:"label,omitempty"`
		FetchedAt time.Time `json:"fetched_at,omitzero"`
		Stale     bool      `json:"stale"`
		Error     string    `json:"error,omitempty"`
	}
	now := time.Now()
	out := []row{}
	for _, b := range items {
		r := row{Bucket: b, Label: a.cfg.Label(b.Profile), Stale: true}
		if l := a.inv.Buckets[b.Profile]; l != nil {
			r.FetchedAt = l.FetchedAt.UTC().Truncate(time.Second)
			r.Stale = l.Err != "" || l.FetchedAt.IsZero() || now.Sub(l.FetchedAt) >= a.cfg.TTL()
			if l.Err != "" {
				r.Error = shortError(l.Err)
				if k := l.ErrKind; k != "" && k != inventory.ErrOther && k != inventory.ErrMFA {
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

func (a *app) printBucketsFooter(items []inventory.Bucket) {
	profiles := map[string]bool{}
	for _, b := range items {
		profiles[b.Profile] = true
	}
	msg := fmt.Sprintf("%s, %s", plural(len(items), "bucket"), plural(len(profiles), "profile"))
	// The age of the oldest list not read during this call.
	var oldest time.Time
	for _, p := range a.bucketProfiles() {
		if l := a.inv.Buckets[p]; l != nil && !a.fetchedBuckets[p] && !l.FetchedAt.IsZero() && (oldest.IsZero() || l.FetchedAt.Before(oldest)) {
			oldest = l.FetchedAt
		}
	}
	if !oldest.IsZero() {
		msg += fmt.Sprintf(" (cached %s, --refresh to query again)", ago(oldest))
	}
	fmt.Fprintln(os.Stderr, paint(stderrTTY, cDim, msg))
}

// entry is a line of an object listing: the object, and the name it is shown under.
type entry struct {
	bucket.Object
	name string
}

func (a *app) listObjects(cmd *cobra.Command, o listOptions, loc bucket.Location, name bucketName) error {
	ctx := cmd.Context()
	b, cfg, err := a.reachBucket(ctx, name, forListing)
	if err != nil {
		return err
	}
	loc.Bucket = b.Name
	progress := func(n int) {
		if stderrTTY && n >= 1000 {
			fmt.Fprintf(os.Stderr, "\r\033[Khop: %d listed", n)
		}
	}
	objs, err := bucket.List(ctx, cfg, loc, o.recursive, progress)
	if stderrTTY {
		fmt.Fprint(os.Stderr, "\r\033[K")
	}
	if err != nil {
		return err
	}
	entries, hidden := listing(objs, loc.Key, o)
	if o.json {
		out := make([]bucket.Object, len(entries))
		for i, e := range entries {
			out[i] = e.Object
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			return err
		}
	} else {
		printEntries(entries, o)
	}
	objectsFooter(entries, hidden, loc, b, o.filter)
	// A search that finds nothing at this level may find below.
	if len(entries) == 0 && !o.filter.Empty() && !o.recursive {
		hint("-R looks in every object below %s", printable(loc.String()))
	}
	return nil
}

// listing turns what S3 returned under prefix into the lines to show, in order, and
// counts those left out for starting with a dot. An object is named by what follows the
// last "/" of the prefix, like a file in its directory, or by its whole key with -R.
// The empty object some tools create to stand for a directory is the directory itself:
// it has no name of its own there.
func listing(objs []bucket.Object, prefix string, o listOptions) (entries []entry, hidden int) {
	base := prefix[:strings.LastIndex(prefix, "/")+1]
	for _, obj := range objs {
		rel := strings.TrimPrefix(obj.Key, base)
		e := entry{Object: obj, name: rel}
		if o.recursive {
			e.name = obj.Key
		} else if rel == "" {
			continue
		}
		if !o.filter.Match(rel) {
			continue
		}
		if !o.all && dotted(rel, o.recursive) {
			hidden++
			continue
		}
		entries = append(entries, e)
	}
	sort.SliceStable(entries, func(i, j int) bool {
		x, y := entries[i], entries[j]
		switch {
		// A prefix has neither a date nor a size: it comes after the objects.
		case (o.byTime || o.bySize) && x.Prefix != y.Prefix:
			return y.Prefix
		case o.byTime && !x.Modified.Equal(y.Modified):
			return x.Modified.After(y.Modified)
		case o.bySize && x.Size != y.Size:
			return x.Size > y.Size
		}
		return x.Key < y.Key
	})
	if o.reverse {
		slices.Reverse(entries)
	}
	return entries, hidden
}

// dotted reports whether a name starts with a dot, which ls leaves out without -a. In
// a recursive listing, a dot at the start of any part of the path counts: ls -R does
// not go down a hidden directory.
func dotted(rel string, recursive bool) bool {
	if !recursive {
		return strings.HasPrefix(rel, ".")
	}
	for part := range strings.SplitSeq(rel, "/") {
		if strings.HasPrefix(part, ".") {
			return true
		}
	}
	return false
}

// printable replaces what a key must not send to a terminal: control characters, which
// could rewrite the lines above, and the marks that reorder what is displayed. A key
// may hold any character; --json gives it as it is.
func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r) {
			return '?'
		}
		return r
	}, s)
}

func printEntries(entries []entry, o listOptions) {
	if !o.long {
		for _, e := range entries {
			fmt.Fprintln(os.Stdout, printable(e.name))
		}
		return
	}
	if len(entries) == 0 {
		return
	}
	sizes := make([]string, len(entries))
	width := len("SIZE")
	for i, e := range entries {
		switch {
		case e.Prefix:
			sizes[i] = "-"
		case o.human:
			sizes[i] = byteSize(e.Size)
		default:
			sizes[i] = strconv.FormatInt(e.Size, 10)
		}
		width = max(width, len(sizes[i]))
	}
	// Sizes line up on the right, like in ls -l.
	t := &table{head: []string{fmt.Sprintf("%*s", width, "SIZE"), "MODIFIED", "CLASS", "NAME"}}
	for i, e := range entries {
		modified, class, name := "-", "-", cell{printable(e.name), ""}
		if e.Prefix {
			name.color = cCyan
		} else {
			modified = e.Modified.In(displayZone).Format("2006-01-02 15:04")
			if e.StorageClass != "" {
				class = e.StorageClass
			}
		}
		t.add(cell{fmt.Sprintf("%*s", width, sizes[i]), ""}, cell{modified, ""}, cell{class, ""}, name)
	}
	t.print(os.Stdout, stdoutTTY)
}

// objectsFooter says on stderr what the listing holds, where, and what -a would add.
func objectsFooter(entries []entry, hidden int, loc bucket.Location, b inventory.Bucket, filter bucket.Filter) {
	objects, prefixes, size := 0, 0, int64(0)
	for _, e := range entries {
		if e.Prefix {
			prefixes++
			continue
		}
		objects++
		size += e.Size
	}
	var parts []string
	if objects > 0 || prefixes == 0 {
		parts = append(parts, plural(objects, "object"))
	}
	switch {
	case prefixes == 1:
		parts = append(parts, "1 prefix")
	case prefixes > 1:
		parts = append(parts, fmt.Sprintf("%d prefixes", prefixes))
	}
	if objects > 0 {
		parts = append(parts, byteSize(size))
	}
	what := strings.Join(parts, ", ")
	if !filter.Empty() {
		what += fmt.Sprintf(" matching %q", printable(filter.String()))
	}
	msg := fmt.Sprintf("%s in %s (%s, %s)", what, printable(loc.String()), b.Profile, b.Region)
	if hidden > 0 {
		msg += fmt.Sprintf("; %d not shown, starting with a dot: -a lists them", hidden)
	}
	fmt.Fprintln(os.Stderr, paint(stderrTTY, cDim, msg))
}

const s3PullShort = "Download an object from a bucket and check it"

func newS3Pull(f *filters) *cobra.Command {
	return &cobra.Command{
		Use:   "pull <bucket>/<key|pattern> [local-path]",
		Short: s3PullShort,
		Long: s3PullShort + `.

The object is named by its bucket and key; hop finds which profile and region hold
the bucket, searched like an instance name, or taken as an exact name after s3://.
Without a local path, the file lands in the current directory under the last part of
the key; given a directory, it lands there.

A large object comes as ranges of 8 MB, five at a time. hop writes them under a
temporary name, then renames the file once it is complete: an interrupted download
leaves nothing behind. A new file is readable by you only; an existing one is replaced
and keeps its mode.

The size is compared with what S3 announced, and the content with the checksum S3
holds for the whole object, when it has one: hop says which, or that there was none
(an object sent without a checksum, or in parts by a tool that kept only a checksum of
its parts). An archived object (Glacier) must be restored first.

A key with a wildcard (* ? [abc]) downloads every object of that prefix that fits it,
each under its own name, into the current directory or the one given:
hop s3 pull 'acme-logs/2026/11/*.gz' logs/. hop says how many and how much first, and
asks when there are several.`,
		Example: `  hop s3 pull acme-logs/2026/app.log                 to ./app.log
  hop s3 pull acme-logs/2026/app.log logs/           to logs/app.log
  hop s3 pull 'acme-logs/2026/11/*.gz' logs/         every .gz of that prefix
  hop s3 pull -p acme-prod s3://acme-backups/db.sql.gz /tmp/prod.sql.gz`,
		Args: cobra.RangeArgs(1, 2),
		ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			if len(args) == 0 {
				return completeBuckets(f, 0)(cmd, args, toComplete)
			}
			return nil, cobra.ShellCompDirectiveDefault
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return runS3Pull(cmd, f, args)
		},
	}
}

// runS3Pull downloads the object args[0] names, to args[1] when given: it serves
// "hop s3 pull" and "hop pull s3://...".
func runS3Pull(cmd *cobra.Command, f *filters, args []string) error {
	loc, err := bucket.Parse(args[0])
	switch {
	case err != nil:
		return err
	case loc.Bucket == "":
		return fmt.Errorf("no bucket in %q: expected bucket/key", args[0])
	case bucket.HasMeta(loc.Bucket):
		return fmt.Errorf("a pattern in the bucket name is for hop s3 ls: name the bucket, and put the pattern in the key (%s)", "bucket/2026/*.log")
	case loc.Key == "" || strings.HasSuffix(loc.Key, "/"):
		return fmt.Errorf("%s names a prefix, not an object: hop s3 ls %s lists what it holds", args[0], args[0])
	}
	local := ""
	if len(args) == 2 {
		local = args[1]
	}
	if bucket.HasMeta(loc.Key) {
		return runS3PullMany(cmd, f, args[0], loc, local)
	}
	// Before the picker, a question or a sign-in, which would all be for nothing.
	target, err := pullTarget(loc.Key, local)
	if err != nil {
		return err
	}
	a, err := newApp(*f)
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	b, cfg, err := a.reachBucket(ctx, nameOf(args[0], loc.Bucket), forDownload)
	if err != nil {
		return err
	}
	loc.Bucket = b.Name
	return a.download(ctx, cfg, b, loc, target)
}

// download fetches the object loc of the bucket b to the file target, and says what it
// did and how it was checked.
func (a *app) download(ctx context.Context, cfg aws.Config, b inventory.Bucket, loc bucket.Location, target string) error {
	info("%s -> %s (%s, %s)", printable(loc.String()), target, b.Profile, b.Region)
	p := &progress{tty: stderrTTY}
	res, err := bucket.Download(ctx, cfg, loc, target, p.update)
	p.clear()
	if err != nil {
		if errors.Is(err, bucket.ErrNotFound) {
			hint("hop s3 ls %s/%s lists what is there", loc.Bucket, printable(loc.Key[:strings.LastIndex(loc.Key, "/")+1]))
		}
		return err
	}
	if res.Checksum == "" {
		info("%s received, size checked: S3 holds no checksum of the whole object to compare", byteSize(res.Size))
		return nil
	}
	info("%s received, %s matches on both sides", byteSize(res.Size), res.Checksum)
	return nil
}

const s3PushShort = "Upload a file to a bucket, in parts when it is large, and check it"

func newS3Push(f *filters) *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "push <local-file>... <bucket>[/<key>]",
		Short: s3PushShort,
		Long: s3PushShort + `.

The destination is a bucket and a key; hop finds which profile and region hold the
bucket, searched like an instance name, or taken as an exact name after s3://. A key
that ends with "/", or no key at all, keeps the name of the file: "acme-backups/db/"
receives db/dump.sql.gz. Several files, or a pattern (*.gz), go one after the other to
a destination that ends with "/". To a bucket of another account (s3://, with -p), hop asks
first, and refuses without a terminal: a mistyped name can be somebody else's bucket.

hop never replaces an object without asking: when the key exists, it asks first, and
without a terminal it refuses unless --force is given. S3 itself refuses the write if
the object appeared meanwhile.

From 16 MB on, the file goes as a multipart upload, in parts of 8 MB, five at a time
(larger parts past 80 GB). An upload that fails or is interrupted is aborted, so its
parts do not stay in the bucket. S3 verifies a CRC32 of each request and returns the
CRC32 of the whole object, which hop compares with the one of the file.`,
		Example: `  hop s3 push ./dump.sql.gz acme-backups/db/             to db/dump.sql.gz
  hop s3 push ./app.conf acme-config/prod/app.conf
  hop s3 push ./logs/*.gz acme-backups/logs/             several files, each under its name
  hop s3 push --force ./index.html s3://acme-site/index.html`,
		Args: cobra.MinimumNArgs(2),
		ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			if len(args) == 1 {
				return completeBuckets(f, 1)(cmd, args, toComplete)
			}
			return nil, cobra.ShellCompDirectiveDefault
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return runS3Push(cmd, f, force, args)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "replace an existing object without asking")
	return cmd
}

// runS3Push uploads the files named by every argument but the last to the place the
// last one names: it serves "hop s3 push" and "hop push <file>... s3://...".
func runS3Push(cmd *cobra.Command, f *filters, force bool, args []string) error {
	dest := args[len(args)-1]
	files, err := localFiles(args[:len(args)-1])
	if err != nil {
		return err
	}
	loc, err := bucket.Parse(dest)
	switch {
	case err != nil:
		return err
	case loc.Bucket == "":
		return fmt.Errorf("no bucket in %q: expected bucket/key", dest)
	case bucket.HasMeta(loc.Bucket) || bucket.HasMeta(loc.Key):
		return fmt.Errorf("%s holds a wildcard: a destination is one bucket and one key", dest)
	case len(files) > 1 && loc.Key != "" && !strings.HasSuffix(loc.Key, "/"):
		return fmt.Errorf("%s need a destination that ends with /, where each keeps its name: %s/", plural(len(files), "file"), dest)
	}
	a, err := newApp(*f)
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	b, cfg, err := a.reachBucket(ctx, nameOf(dest, loc.Bucket), forUpload)
	if err != nil {
		return err
	}
	sent := 0
	for _, local := range files {
		to := bucket.Location{Bucket: b.Name, Key: loc.Key}
		if to.Key == "" || strings.HasSuffix(to.Key, "/") {
			to.Key += filepath.Base(local)
		}
		err := a.upload(ctx, cfg, b, local, to, force)
		switch {
		case err == nil:
			sent++
		case len(files) == 1:
			return err
		case errors.Is(err, errAborted) && ctx.Err() == nil:
			// A no to replacing one object is not a no to the files that follow.
			info("%s left as it is", printable(to.String()))
		default:
			return fmt.Errorf("%w (%d of %s sent)", err, sent, plural(len(files), "file"))
		}
	}
	if len(files) > 1 {
		info("%d of %s sent to %s", sent, plural(len(files), "file"), printable(bucket.Location{Bucket: b.Name, Key: loc.Key}.String()))
	}
	return nil
}

// localFiles returns the files an upload was given. An argument that names no file
// and holds a wildcard is a pattern, read here: the shell did not expand it, quoted or
// not (zsh passes it on under noglob, which hop's zsh integration sets). Directories a
// pattern matches are left out.
func localFiles(args []string) ([]string, error) {
	var out []string
	for _, arg := range args {
		st, err := os.Stat(arg)
		switch {
		case err == nil && st.Mode().IsRegular():
			out = append(out, arg)
		case err == nil && !st.IsDir():
			return nil, fmt.Errorf("%s is not a regular file", arg)
		case err != nil && bucket.HasMeta(arg):
			matches, _ := filepath.Glob(arg)
			n := len(out)
			for _, m := range matches {
				if st, err := os.Stat(m); err == nil && st.Mode().IsRegular() {
					out = append(out, m)
				}
			}
			if len(out) == n {
				return nil, fmt.Errorf("no file matches %s", arg)
			}
		default:
			return nil, fmt.Errorf("file not found: %s", arg)
		}
	}
	return out, nil
}

// upload sends one file to loc in the bucket b, after a look at what is there: an
// existing object is replaced only after a yes, or with force.
func (a *app) upload(ctx context.Context, cfg aws.Config, b inventory.Bucket, local string, loc bucket.Location, force bool) error {
	// Always say where the file is going, even from a script.
	info("%s -> %s (%s, %s)", local, printable(loc.String()), b.Profile, b.Region)

	replace := force
	if !force {
		hctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		cur, err := bucket.Head(hctx, cfg, loc)
		cancel()
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case err == nil:
			if !stdinTTY || !stderrTTY {
				return fmt.Errorf("%s %w: --force replaces it (no terminal to confirm)", printable(loc.String()), bucket.ErrExists)
			}
			question := fmt.Sprintf("%s exists (%s, modified %s). Replace it?", printable(loc.String()), byteSize(cur.Size), ago(cur.Modified))
			if err := ask(ctx, question, false); err != nil {
				return err
			}
			replace = true
		case errors.Is(err, bucket.ErrNotFound), errors.Is(err, bucket.ErrDenied):
			// Nothing there, or a role that may write without reading: S3 refuses
			// the write itself if the object exists.
		default:
			return err
		}
	}
	p := &progress{tty: stderrTTY}
	res, err := bucket.Upload(ctx, cfg, local, loc, bucket.UploadOptions{Replace: replace, Progress: p.update})
	p.clear()
	if err != nil {
		if errors.Is(err, bucket.ErrExists) {
			hint("--force replaces it")
		}
		return err
	}
	if res.Checksum == "" {
		info("%s sent: S3 returned no checksum to compare", byteSize(res.Size))
		return nil
	}
	info("%s sent, %s matches on both sides", byteSize(res.Size), res.Checksum)
	return nil
}

// runS3PullMany downloads every object of one prefix that fits the pattern in the key
// of a place ("acme-logs/2026/*.log"), into a directory, each under its own name.
func runS3PullMany(cmd *cobra.Command, f *filters, arg string, loc bucket.Location, dir string) error {
	prefix, pattern := bucket.SplitPattern(loc.Key)
	if strings.Contains(pattern, "/") {
		return fmt.Errorf("the pattern %q spans several levels: hop s3 pull takes the objects of one prefix at a time (hop s3 ls '%s' shows what it matches)", pattern, arg)
	}
	filter, err := bucket.NewFilter(pattern, nil)
	if err != nil {
		return err
	}
	if dir == "" {
		dir = "."
	}
	// Before the picker, a question or a sign-in, which would all be for nothing.
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return fmt.Errorf("%s is not a directory: the objects a pattern matches need one to land in", dir)
	}
	a, err := newApp(*f)
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	b, cfg, err := a.reachBucket(ctx, nameOf(arg, loc.Bucket), forDownload)
	if err != nil {
		return err
	}
	loc.Bucket, loc.Key = b.Name, prefix
	objs, err := bucket.List(ctx, cfg, loc, false, nil)
	if err != nil {
		return err
	}
	var found []bucket.Object
	size := int64(0)
	for _, o := range objs {
		if !o.Prefix && filter.Match(strings.TrimPrefix(o.Key, prefix)) {
			found = append(found, o)
			size += o.Size
		}
	}
	place := printable(loc.String() + pattern)
	if len(found) == 0 {
		return fmt.Errorf("no object matches %s", place)
	}
	what := fmt.Sprintf("%s (%s) matching %s", plural(len(found), "object"), byteSize(size), place)
	// Several files land at once, and replace those of the same name: worth a look
	// first, where somebody can answer.
	if len(found) > 1 && stdinTTY && stderrTTY {
		if err := ask(ctx, fmt.Sprintf("Download %s to %s?", what, dir), true); err != nil {
			return err
		}
	} else {
		info("%s", what)
	}
	for i, o := range found {
		target, err := pullTarget(o.Key, dir)
		if err == nil {
			err = a.download(ctx, cfg, b, bucket.Location{Bucket: b.Name, Key: o.Key}, target)
		}
		if err != nil {
			return fmt.Errorf("%w (%d of %s downloaded)", err, i, plural(len(found), "object"))
		}
	}
	if len(found) > 1 {
		info("%s downloaded to %s", plural(len(found), "object"), dir)
	}
	return nil
}

// completeBuckets completes a bucket name at position n, from the cache, with the "/"
// that starts a key. Keys are not completed: that would take a call to S3.
func completeBuckets(f *filters, n int) cobra.CompletionFunc {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) != n || strings.Contains(strings.TrimPrefix(strings.ToLower(toComplete), bucket.Scheme), "/") {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		a, err := newApp(*f)
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		prefix := ""
		if bucket.HasScheme(toComplete) {
			prefix = toComplete[:len(bucket.Scheme)]
		}
		var out []string
		for _, b := range a.buckets() {
			out = append(out, prefix+b.Name+"/\t"+joinNonEmpty(", ", a.profileLabel(b.Profile), joinNonEmpty(" ", b.Region, b.Env)))
		}
		return out, cobra.ShellCompDirectiveNoFileComp | cobra.ShellCompDirectiveNoSpace
	}
}
