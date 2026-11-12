package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/mattn/go-runewidth"

	"github.com/morganseznec/hop/internal/bucket"
	"github.com/morganseznec/hop/internal/inventory"
)

// browse is hop s3 with no command, in a terminal: a picker over the buckets in scope,
// as hop opens one over the instances, then over what the chosen bucket holds. It ends
// with a download, or with Esc.
func (a *app) browse(ctx context.Context) error {
	if err := a.bucketScope(); err != nil {
		return err
	}
	if err := a.ensureBuckets(ctx, a.f.refresh); err != nil {
		return err
	}
	buckets := a.buckets()
	if len(buckets) == 0 {
		a.reportBucketProblems()
		msg := "no bucket in scope"
		if n := len(a.inv.BucketsOf(a.bucketProfiles())); n > 0 {
			msg += fmt.Sprintf(": -r and -e leave out %s", plural(n, "bucket"))
		}
		return errors.New(msg)
	}
	for {
		header := fmt.Sprintf("%s · Enter opens, Esc quits", plural(len(buckets), "bucket"))
		chosen, err := a.pickBucket(buckets, header)
		if err != nil {
			return err
		}
		b, cfg, err := a.openBucket(ctx, chosen)
		if err != nil {
			return err
		}
		done, err := a.browseBucket(ctx, cfg, b)
		if err != nil || done {
			return err
		}
	}
}

// upLine is the first line of every listing of the picker: the way back.
const upLine = ".."

// browseBucket walks a bucket in the picker, one level of keys at a time. Enter on a
// prefix goes down, on ".." back up, and from the top of the bucket back to the list of
// buckets (done is then false). Enter on an object offers to download it: a yes ends
// the walk, a no returns to the listing.
func (a *app) browseBucket(ctx context.Context, cfg aws.Config, b inventory.Bucket) (done bool, err error) {
	prefix := ""
	for {
		loc := bucket.Location{Bucket: b.Name, Key: prefix}
		objs, err := bucket.List(ctx, cfg, loc, false, func(n int) {
			if stderrTTY && n >= 1000 {
				fmt.Fprintf(os.Stderr, "\r\033[Khop: %d listed", n)
			}
		})
		if stderrTTY {
			fmt.Fprint(os.Stderr, "\r\033[K")
		}
		if err != nil {
			return false, err
		}
		// Everything, names that start with a dot included: nothing to type -a into.
		entries, _ := listing(objs, prefix, listOptions{all: true})
		// The cursor starts on the first entry: ".." stays one key away, just below.
		i, err := choose(len(entries)+1, entryLines(entries, prefix == ""), browseHeader(entries, loc, pickerWidth()), min(1, len(entries)))
		if err != nil {
			return false, err
		}
		switch {
		case i == 0 && prefix == "":
			return false, nil
		case i == 0:
			prefix = parentPrefix(prefix)
		case entries[i-1].Prefix:
			prefix = entries[i-1].Key
		default:
			got, err := a.offerDownload(ctx, cfg, b, entries[i-1])
			if err != nil || got {
				return got, err
			}
		}
	}
}

// parentPrefix is the prefix one level up: "2026/11/" gives "2026/", and "2026/" the
// top of the bucket.
func parentPrefix(prefix string) string {
	return prefix[:strings.LastIndex(strings.TrimSuffix(prefix, "/"), "/")+1]
}

// browseHeader says where the picker is, what is there, and what the keys do, within
// the width of the terminal: the picker cuts a longer header at its end, where the keys
// are, so the place gives way first, from its start.
func browseHeader(entries []entry, loc bucket.Location, width int) string {
	objects, prefixes := 0, 0
	for _, e := range entries {
		if e.Prefix {
			prefixes++
		} else {
			objects++
		}
	}
	what := "nothing here"
	switch {
	case objects > 0 && prefixes > 0:
		what = plural(objects, "object") + ", " + prefixCount(prefixes)
	case objects > 0:
		what = plural(objects, "object")
	case prefixes > 0:
		what = prefixCount(prefixes)
	}
	const keys = "Enter opens, Esc quits"
	// The picker keeps two columns on each side, and the separators take six.
	room := width - 4 - 6 - runewidth.StringWidth(what) - len(keys)
	return fmt.Sprintf("%s · %s · %s", tail(printable(loc.String()), max(room, 20)), what, keys)
}

// tail shortens s to width columns by cutting its start: in a long key, the end says
// where the picker is.
func tail(s string, width int) string {
	if runewidth.StringWidth(s) <= width {
		return s
	}
	r := []rune(s)
	for len(r) > 0 && runewidth.StringWidth(string(r)) > width-1 {
		r = r[1:]
	}
	return "…" + string(r)
}

func prefixCount(n int) string {
	if n == 1 {
		return "1 prefix"
	}
	return strconv.Itoa(n) + " prefixes"
}

// entryLines returns what the picker shows for a listing: ".." first, which top says
// leaves the bucket, then each entry, an object with its size, its date and its storage
// class.
func entryLines(entries []entry, top bool) func(i int) string {
	width := runewidth.StringWidth(upLine)
	for _, e := range entries {
		width = max(width, runewidth.StringWidth(printable(e.name)))
	}
	up := "back to the prefix above"
	if top {
		up = "back to the list of buckets"
	}
	return func(i int) string {
		if i == 0 {
			return runewidth.FillRight(upLine, width) + "  " + up
		}
		e := entries[i-1]
		if e.Prefix {
			return printable(e.name)
		}
		return strings.TrimRight(fmt.Sprintf("%s  %9s  %s  %s", runewidth.FillRight(printable(e.name), width),
			byteSize(e.Size), e.Modified.In(displayZone).Format("2006-01-02 15:04"), e.StorageClass), " ")
	}
}

// offerDownload asks whether to download the object chosen to the current directory,
// under the last part of its key, and does it on a yes. got is false on a no.
func (a *app) offerDownload(ctx context.Context, cfg aws.Config, b inventory.Bucket, e entry) (got bool, err error) {
	loc := bucket.Location{Bucket: b.Name, Key: e.Key}
	target, err := pullTarget(e.Key, "")
	if err != nil {
		return false, err
	}
	question := fmt.Sprintf("Download %s (%s) to %s?", printable(loc.String()), byteSize(e.Size), target)
	if _, err := os.Stat(target); err == nil {
		question = fmt.Sprintf("Download %s (%s) to %s, which replaces the file there?", printable(loc.String()), byteSize(e.Size), target)
	}
	yes, err := confirm(ctx, question, true)
	if err != nil || !yes {
		return false, err
	}
	return true, a.download(ctx, cfg, b, loc, target)
}
