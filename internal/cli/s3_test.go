package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/morganseznec/hop/internal/bucket"
	"github.com/morganseznec/hop/internal/fakes3"
	"github.com/morganseznec/hop/internal/inventory"
)

// s3Env sets up the fleet with a fake S3. Only acme-billing, a static-key profile,
// can reach it: the SSO profiles have no token in the temporary home. Dates print in
// UTC.
func s3Env(t *testing.T) *fakes3.Server {
	t.Helper()
	fleetEnv(t)
	terminal(t, false, false)
	srv := fakes3.New(t)
	// Every other endpoint stays the closed port isolate set.
	t.Setenv("AWS_ENDPOINT_URL_S3", srv.URL)
	zone := displayZone
	displayZone = time.UTC
	t.Cleanup(func() { displayZone = zone })

	srv.AddBucket("acme-b-logs", "eu-west-1")
	srv.AddBucket("acme-b-backups", "us-east-1")
	srv.AddBucket("acme-t-scratch", "eu-west-3")
	srv.Put("acme-b-logs", "2026/11/app.log", bytes.Repeat([]byte("x"), 1500), "SHA256")
	srv.Put("acme-b-logs", "2026/11/db.log", bytes.Repeat([]byte("y"), 20), "")
	srv.Put("acme-b-logs", "2026/readme.txt", []byte("hello"), "CRC32").Modified = time.Date(2026, 11, 9, 8, 30, 0, 0, time.UTC)
	srv.Put("acme-b-logs", "2026/.env", []byte("KEY=value"), "")
	srv.Put("acme-b-logs", "2026/.git/config", []byte("cfg"), "")
	// The empty object a console creates to stand for the "directory".
	srv.Put("acme-b-logs", "2026/", nil, "")
	srv.Put("acme-b-logs", "top.bin", bytes.Repeat([]byte("z"), 2_500_000), "CRC64NVME").StorageClass = "STANDARD_IA"
	return srv
}

// lines returns the lines of s with their cells one space apart: tables are compared
// without their padding.
func lines(s string) []string {
	var out []string
	for _, l := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		out = append(out, strings.Join(strings.Fields(l), " "))
	}
	return out
}

func sameLines(t *testing.T, what, got string, want ...string) {
	t.Helper()
	if g := lines(got); strings.Join(g, "\n") != strings.Join(want, "\n") {
		t.Errorf("%s:\n got:\n%s\nwant:\n%s", what, strings.Join(g, "\n"), strings.Join(want, "\n"))
	}
}

func TestS3LsBuckets(t *testing.T) {
	srv := s3Env(t)
	stdout, stderr, err := runHop(t, "s3", "ls")
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	sameLines(t, "buckets", stdout,
		"NAME PROFILE REGION ENV CREATED",
		"acme-b-backups acme-billing us-east-1 2026-01-02",
		"acme-b-logs acme-billing eu-west-1 2026-01-02",
		"acme-t-scratch acme-billing eu-west-3 test 2026-01-02")
	// The accounts that could not be listed are reported, with the fix, as for instances.
	for _, want := range []string{
		"3 buckets, 1 profile\n",
		"SSO token expired for acme-sso: acme-prod, acme-data not queried",
		"hop login acme-sso",
		"SSO token expired for globex.awsapps.com: globex not queried",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
	// One call per profile that can sign in, kept in the cache with the instances.
	if n := srv.Count("ListBuckets"); n != 1 {
		t.Errorf("%d ListBuckets calls: %v", n, srv.Calls())
	}
	inv := inventory.Load()
	if l := inv.Buckets["acme-billing"]; l == nil || len(l.Buckets) != 3 || l.Err != "" {
		t.Fatalf("bucket list not cached: %+v", l)
	}
	if len(inv.Targets) == 0 {
		t.Error("saving the buckets dropped the instances from the cache")
	}

	// Within the TTL, the list comes from the cache, and says so.
	_, stderr, err = runHop(t, "s3", "ls", "-p", "acme-billing")
	if err != nil || srv.Count("ListBuckets") != 1 {
		t.Errorf("second run: %v, %v", err, srv.Calls())
	}
	if !strings.Contains(stderr, "3 buckets, 1 profile (cached just now, --refresh to query again)") {
		t.Errorf("footer of a cached list:\n%s", stderr)
	}
	if _, _, err := runHop(t, "s3", "ls", "-p", "acme-billing", "--refresh"); err != nil || srv.Count("ListBuckets") != 2 {
		t.Errorf("--refresh: %v, %v", err, srv.Calls())
	}
}

func TestS3LsBucketsNeverListedByHopLs(t *testing.T) {
	srv := s3Env(t)
	// Listing instances must not start calling S3: a role without s3:ListAllMyBuckets
	// would be reported for something nobody asked.
	if _, _, err := runHop(t, "ls"); err != nil {
		t.Fatal(err)
	}
	if len(srv.Calls()) != 0 {
		t.Errorf("hop ls called S3: %v", srv.Calls())
	}
}

func TestS3LsBucketsFilters(t *testing.T) {
	s3Env(t)
	cases := []struct {
		name string
		args []string
		want []string
	}{
		{"words", []string{"logs"}, []string{"acme-b-logs"}},
		{"several words", []string{"acme", "backups"}, []string{"acme-b-backups"}},
		// The profile is searched too, as for instances.
		{"a word of the profile", []string{"billing"}, []string{"acme-b-backups", "acme-b-logs", "acme-t-scratch"}},
		{"the region of the bucket", []string{"-r", "us-east-1"}, []string{"acme-b-backups"}},
		{"environment", []string{"-e", "test"}, []string{"acme-t-scratch"}},
		{"reverse", []string{"--reverse"}, []string{"acme-t-scratch", "acme-b-logs", "acme-b-backups"}},
		// -a, -l and -h are those of ls: accepted, with nothing more to show for a bucket.
		{"ls options", []string{"-alh"}, []string{"acme-b-backups", "acme-b-logs", "acme-t-scratch"}},
		{"every bucket", []string{"s3://"}, []string{"acme-b-backups", "acme-b-logs", "acme-t-scratch"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stdout, stderr, err := runHop(t, append([]string{"s3", "ls", "-p", "acme-billing"}, c.args...)...)
			if err != nil {
				t.Fatalf("%v\n%s", err, stderr)
			}
			var got []string
			for _, l := range lines(stdout)[1:] {
				got = append(got, strings.Fields(l)[0])
			}
			if strings.Join(got, " ") != strings.Join(c.want, " ") {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}

	// A single bucket found by words: say how to look inside.
	_, stderr, _ := runHop(t, "s3", "ls", "-p", "acme-billing", "logs")
	if !strings.Contains(stderr, "hop s3 ls acme-b-logs/ lists what it holds") {
		t.Errorf("no hint for a single bucket:\n%s", stderr)
	}
	stdout, stderr, err := runHop(t, "s3", "ls", "-p", "acme-billing", "nothing-like-it")
	if err != nil || stdout != "" || !strings.Contains(stderr, `no buckets match "nothing-like-it"`) {
		t.Errorf("no match: %v, %q\n%s", err, stdout, stderr)
	}
}

func TestS3LsBucketsJSON(t *testing.T) {
	s3Env(t)
	stdout, stderr, err := runHop(t, "s3", "ls", "--json")
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(stdout), &rows); err != nil {
		t.Fatalf("%v\n%s", err, stdout)
	}
	if len(rows) != 3 {
		t.Fatalf("%d rows", len(rows))
	}
	r := rows[2]
	for k, want := range map[string]any{
		"name": "acme-t-scratch", "profile": "acme-billing", "region": "eu-west-3", "env": "test",
		"created": "2026-01-02T03:04:05Z", "stale": false,
	} {
		if r[k] != want {
			t.Errorf("%s = %v, want %v (%v)", k, r[k], want, r)
		}
	}
	if _, ok := r["fetched_at"]; !ok {
		t.Errorf("no fetched_at: %v", r)
	}
	// A script must still learn that some accounts were not reached.
	if !strings.Contains(stderr, "SSO token expired for acme-sso") {
		t.Errorf("problems not reported with --json:\n%s", stderr)
	}
	// Nothing in scope answers: an empty list, not null.
	stdout, _, err = runHop(t, "s3", "ls", "--json", "-p", "acme-prod")
	if err != nil || strings.TrimSpace(stdout) != "[]" {
		t.Errorf("no bucket: %q, %v", stdout, err)
	}
}

func TestS3LsObjects(t *testing.T) {
	s3Env(t)
	cases := []struct {
		name   string
		args   []string
		stdout []string
		footer string
	}{
		{"names, like ls", []string{"acme-b-logs/2026/"},
			[]string{"11/", "readme.txt"},
			"1 object, 1 prefix, 5 B in s3://acme-b-logs/2026/ (acme-billing, eu-west-1); 2 not shown, starting with a dot: -a lists them"},
		{"-a shows the names that start with a dot", []string{"-a", "acme-b-logs/2026/"},
			[]string{".env", ".git/", "11/", "readme.txt"},
			"2 objects, 2 prefixes, 14 B in s3://acme-b-logs/2026/ (acme-billing, eu-west-1)"},
		{"-l", []string{"-l", "acme-b-logs/2026/"},
			[]string{"SIZE MODIFIED CLASS NAME", "- - - 11/", "5 2026-11-09 08:30 STANDARD readme.txt"}, ""},
		{"-alh", []string{"-alh", "acme-b-logs/2026/"},
			[]string{"SIZE MODIFIED CLASS NAME", "9 B 2026-11-07 14:02 STANDARD .env", "- - - .git/", "- - - 11/", "5 B 2026-11-09 08:30 STANDARD readme.txt"}, ""},
		{"the top of a bucket", []string{"-lh", "acme-b-logs/"},
			[]string{"SIZE MODIFIED CLASS NAME", "- - - 2026/", "2.5 MB 2026-11-07 14:02 STANDARD_IA top.bin"},
			"1 object, 1 prefix, 2.5 MB in s3://acme-b-logs/ (acme-billing, eu-west-1)"},
		{"s3:// in front", []string{"s3://acme-b-logs/2026/11/"},
			[]string{"app.log", "db.log"}, "2 objects, 1.5 KB in s3://acme-b-logs/2026/11/"},
		{"a prefix that stops inside a name", []string{"acme-b-logs/2026/r"},
			[]string{"readme.txt"}, "1 object, 5 B in s3://acme-b-logs/2026/r "},
		{"-R gives whole keys", []string{"-R", "acme-b-logs/2026/"},
			[]string{"2026/", "2026/11/app.log", "2026/11/db.log", "2026/readme.txt"},
			"4 objects, 1.5 KB in s3://acme-b-logs/2026/ (acme-billing, eu-west-1); 2 not shown, starting with a dot: -a lists them"},
		{"-Ra", []string{"-Ra", "acme-b-logs/2026/"},
			[]string{"2026/", "2026/.env", "2026/.git/config", "2026/11/app.log", "2026/11/db.log", "2026/readme.txt"}, ""},
		{"-S, largest first", []string{"-RS", "acme-b-logs/"},
			[]string{"top.bin", "2026/11/app.log", "2026/11/db.log", "2026/readme.txt", "2026/"}, ""},
		{"-t, newest first, prefixes last", []string{"-t", "acme-b-logs/2026/"},
			[]string{"readme.txt", "11/"}, ""},
		{"--reverse", []string{"--reverse", "acme-b-logs/2026/"},
			[]string{"readme.txt", "11/"}, ""},
		{"the bucket by a whole word of its name", []string{"logs/2026/11/"},
			[]string{"app.log", "db.log"}, "in s3://acme-b-logs/2026/11/ (acme-billing, eu-west-1)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stdout, stderr, err := runHop(t, append([]string{"s3", "ls", "-p", "acme-billing"}, c.args...)...)
			if err != nil {
				t.Fatalf("%v\n%s", err, stderr)
			}
			sameLines(t, "stdout", stdout, c.stdout...)
			if !strings.Contains(stderr, c.footer) {
				t.Errorf("stderr lacks %q:\n%s", c.footer, stderr)
			}
		})
	}

	stdout, stderr, err := runHop(t, "s3", "ls", "-p", "acme-billing", "acme-b-logs/nothing/")
	if err != nil || stdout != "" || !strings.Contains(stderr, "0 objects in s3://acme-b-logs/nothing/") {
		t.Errorf("empty prefix: %v, %q\n%s", err, stdout, stderr)
	}
	// What follows the place is what to look for in it, a second place included.
	_, stderr, err = runHop(t, "s3", "ls", "-p", "acme-billing", "acme-b-logs/", "acme-b-backups/")
	if err != nil || !strings.Contains(stderr, `0 objects matching "acme-b-backups/" in s3://acme-b-logs/`) {
		t.Errorf("a second place is a filter: %v\n%s", err, stderr)
	}
}

func TestS3LsObjectsJSON(t *testing.T) {
	s3Env(t)
	stdout, stderr, err := runHop(t, "s3", "ls", "-p", "acme-billing", "--json", "-a", "acme-b-logs/2026/")
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	var rows []bucket.Object
	if err := json.Unmarshal([]byte(stdout), &rows); err != nil {
		t.Fatalf("%v\n%s", err, stdout)
	}
	if len(rows) != 4 {
		t.Fatalf("%d rows: %s", len(rows), stdout)
	}
	// Whole keys, whatever the listing shows.
	if r := rows[3]; r.Key != "2026/readme.txt" || r.Size != 5 || r.StorageClass != "STANDARD" || !r.Modified.Equal(time.Date(2026, 11, 9, 8, 30, 0, 0, time.UTC)) || r.Prefix {
		t.Errorf("object: %+v", r)
	}
	if r := rows[1]; r.Key != "2026/.git/" || !r.Prefix {
		t.Errorf("prefix: %+v", r)
	}
	if !strings.Contains(stdout, `"modified": "2026-11-09T08:30:00Z"`) {
		t.Errorf("date format:\n%s", stdout)
	}
	// An empty listing is an empty list.
	stdout, _, err = runHop(t, "s3", "ls", "-p", "acme-billing", "--json", "acme-b-logs/nothing/")
	if err != nil || strings.TrimSpace(stdout) != "[]" {
		t.Errorf("empty: %q, %v", stdout, err)
	}
}

func TestS3LsKeepsTheTerminalSafe(t *testing.T) {
	srv := s3Env(t)
	// A key may hold anything: an escape sequence must not reach the terminal.
	srv.Put("acme-b-logs", "odd/\x1b[2Jclear\nline", []byte("x"), "")
	stdout, _, err := runHop(t, "s3", "ls", "-p", "acme-billing", "acme-b-logs/odd/")
	if err != nil || stdout != "?[2Jclear?line\n" {
		t.Errorf("stdout %q, %v", stdout, err)
	}
	// --json gives the key as it is, escaped by JSON.
	stdout, _, _ = runHop(t, "s3", "ls", "-p", "acme-billing", "--json", "acme-b-logs/odd/")
	var rows []bucket.Object
	if err := json.Unmarshal([]byte(stdout), &rows); err != nil || len(rows) != 1 || rows[0].Key != "odd/\x1b[2Jclear\nline" || strings.Contains(stdout, "\x1b") {
		t.Errorf("JSON: %v\n%q", err, stdout)
	}
}

func TestListing(t *testing.T) {
	objs := []bucket.Object{
		{Key: "logs/"}, // the object that stands for the directory
		{Key: "logs/.hidden"},
		{Key: "logs/a.txt", Size: 3},
		{Key: "logs/sub/", Prefix: true},
	}
	entries, hidden := listing(objs, "logs/", listOptions{})
	if len(entries) != 2 || hidden != 1 || entries[0].name != "a.txt" || entries[1].name != "sub/" {
		t.Errorf("entries %+v, hidden %d", entries, hidden)
	}
	// Without a "/" in the prefix, names are whole keys.
	entries, _ = listing([]bucket.Object{{Key: "top.txt"}}, "to", listOptions{})
	if len(entries) != 1 || entries[0].name != "top.txt" {
		t.Errorf("entries %+v", entries)
	}
	for rel, want := range map[string]bool{".env": true, "a/.git/config": true, "a/b.c": false, "a.b/c": false} {
		if got := dotted(rel, true); got != want {
			t.Errorf("dotted(%q, recursive) = %v", rel, got)
		}
	}
	if dotted("a/.git/config", false) {
		t.Error("only the name itself counts outside a recursive listing")
	}
}

func TestS3HelpStaysOnH(t *testing.T) {
	srv := s3Env(t)
	// -h is the help everywhere; it means "human-readable" only next to -l.
	for _, args := range [][]string{{"s3", "ls", "-h"}, {"s3", "ls", "--help"}, {"s3", "ls", "-h", "acme-b-logs/"}} {
		stdout, _, err := runHop(t, args...)
		if err != nil || !strings.Contains(stdout, "List buckets across all accounts, or the objects of a bucket.") || !strings.Contains(stdout, "--human-readable") {
			t.Errorf("%v: %v\n%s", args, err, stdout)
		}
	}
	for _, args := range [][]string{{"ls", "-h"}, {"ls", "--help"}} {
		stdout, _, err := runHop(t, args...)
		if err != nil || !strings.Contains(stdout, "List instances across all accounts.") {
			t.Errorf("%v: %v\n%s", args, err, stdout)
		}
		// The options of an S3 listing do not crowd the help of hop ls.
		if strings.Contains(stdout, "--human-readable") || strings.Contains(stdout, "--recursive") {
			t.Errorf("%v: S3 options in the help of hop ls:\n%s", args, stdout)
		}
	}
	if len(srv.Calls()) != 0 {
		t.Errorf("asking for help called S3: %v", srv.Calls())
	}
	stdout, _, err := runHop(t, "s3")
	if err != nil || !strings.Contains(stdout, "hop s3 [command]") {
		t.Errorf("hop s3: %v\n%s", err, stdout)
	}
	if _, _, err := runHop(t, "s3", "nope"); err == nil {
		t.Error("hop s3 nope must fail")
	}
}

func TestLsS3Scheme(t *testing.T) {
	s3Env(t)
	// hop ls s3://... is hop s3 ls ..., with its options.
	want, _, err := runHop(t, "s3", "ls", "-p", "acme-billing", "-alh", "acme-b-logs/2026/")
	if err != nil {
		t.Fatal(err)
	}
	got, stderr, err := runHop(t, "ls", "-p", "acme-billing", "-alh", "s3://acme-b-logs/2026/")
	if err != nil || got != want || !strings.Contains(got, "readme.txt") {
		t.Errorf("%v\n%s\n%s", err, got, stderr)
	}
	stdout, _, err := runHop(t, "ls", "-p", "acme-billing", "s3://")
	if err != nil || !strings.Contains(stdout, "acme-b-backups") || strings.Contains(stdout, "AGENT") {
		t.Errorf("hop ls s3://: %v\n%s", err, stdout)
	}
	// Without s3://, hop ls lists instances, whatever the words.
	stdout, _, err = runHop(t, "ls", "acme-p-tools")
	if err != nil || !strings.Contains(stdout, "AGENT") {
		t.Errorf("hop ls: %v\n%s", err, stdout)
	}
}

func TestS3ResolveBucket(t *testing.T) {
	srv := s3Env(t)
	srv.AddForeignBucket("partner-drop", "eu-central-1")
	srv.Put("partner-drop", "in/file.txt", []byte("x"), "")

	fails := []struct {
		name string
		args []string
		want string
	}{
		{"part of a word", []string{"-p", "acme-billing", "log/"},
			`"log" only partly matches the bucket acme-b-logs (acme-billing, eu-west-1): give its full name (no terminal to confirm)`},
		{"several", []string{"-p", "acme-billing", "acme/"},
			`several buckets match "acme", be more specific: acme-b-backups, acme-b-logs, acme-t-scratch`},
		{"a typo", []string{"-p", "acme-billing", "acme-b-lgos/"}, "did you mean: acme-b-logs"},
		{"nothing close", []string{"-p", "acme-billing", "zzz/"}, `no bucket matches "zzz"`},
		// A search never leaves the buckets listed, even for a name that exists
		// elsewhere: s3:// is how to say "exactly this bucket".
		{"another account's bucket, searched", []string{"-p", "acme-billing", "partner-drop/in/"}, "write its exact name after s3://, with the profile that reaches it (-p)"},
		// With several profiles in scope, hop cannot tell which one reaches a bucket
		// that none of them lists.
		{"another account's bucket, no profile named", []string{"s3://partner-drop/in/"}, "name the profile that reaches it with -p"},
		// After s3://, a name is exact: neither a word of a name, nor a typo, finds one.
		{"a word after s3://", []string{"-p", "acme-billing", "s3://logs/"}, "no bucket named logs that profile acme-billing can reach"},
		{"a typo after s3://", []string{"-p", "acme-billing", "s3://acme-b-lgos/"}, "no bucket named acme-b-lgos that profile acme-billing can reach"},
		{"the region filtered out, exact name", []string{"-p", "acme-billing", "-r", "us-east-1", "s3://acme-t-scratch/"}, "-r us-east-1 leaves out acme-t-scratch, which is in eu-west-3"},
		{"another account's bucket, another region", []string{"-p", "acme-billing", "-r", "us-east-1", "s3://partner-drop/in/"}, "-r us-east-1 leaves out partner-drop, which is in eu-central-1"},
		{"the region filtered out", []string{"-p", "acme-billing", "-r", "us-east-1", "acme-t-scratch/"},
			`no bucket matches "acme-t-scratch": -r us-east-1 leaves out acme-t-scratch, which is in eu-west-3`},
		{"the environment filtered out", []string{"-e", "test", "acme-b-logs/"}, `no bucket matches "acme-b-logs": -e test leaves out acme-b-logs`},
	}
	for _, c := range fails {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := runHop(t, append([]string{"s3", "ls"}, c.args...)...)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %v, want %q", err, c.want)
			}
		})
	}

	// A broad word: the first candidates, and how many are left out.
	for _, n := range []string{"acme-x-1", "acme-x-2", "acme-x-3", "acme-x-4"} {
		srv.AddBucket(n, "eu-west-1")
	}
	_, _, err := runHop(t, "s3", "ls", "-p", "acme-billing", "--refresh", "acme/")
	if err == nil || !strings.Contains(err.Error(), "acme-x-2, and 2 more") {
		t.Errorf("many candidates: %v", err)
	}

	// A bucket of another account: named exactly, with the one profile that reaches it.
	stdout, stderr, err := runHop(t, "s3", "ls", "-p", "acme-billing", "s3://partner-drop/in/")
	if err != nil || stdout != "file.txt\n" || !strings.Contains(stderr, "in s3://partner-drop/in/ (acme-billing, eu-central-1)") {
		t.Errorf("foreign bucket: %v, %q\n%s", err, stdout, stderr)
	}
	// A word that matches no listed bucket is never tried as a name: the bucket "logs"
	// of somebody else must not be read in place of a search that found nothing.
	srv.AddForeignBucket("zzz", "eu-west-1")
	before := srv.Count("HeadBucket")
	if _, _, err := runHop(t, "s3", "ls", "-p", "acme-billing", "zzz/"); err == nil || !strings.Contains(err.Error(), `no bucket matches "zzz"`) {
		t.Errorf("a searched word reached an unlisted bucket: %v", err)
	}
	if srv.Count("HeadBucket") != before {
		t.Errorf("a search asked S3 about a name: %v", srv.Calls())
	}
}

func TestS3PushToAnotherAccountAsks(t *testing.T) {
	srv := s3Env(t)
	srv.AddForeignBucket("partner-drop", "eu-central-1")
	local := filepath.Join(t.TempDir(), "report.csv")
	os.WriteFile(local, []byte("a,b\n"), 0o600)

	// Nobody to ask: no upload to a bucket that the profile does not list. A mistyped
	// name can be somebody else's bucket.
	_, _, err := runHop(t, "s3", "push", "-p", "acme-billing", local, "s3://partner-drop/in/")
	if err == nil || !strings.Contains(err.Error(), "s3://partner-drop is not a bucket of the account of acme-billing") || !strings.Contains(err.Error(), "no terminal to confirm") {
		t.Fatalf("error %v", err)
	}
	if srv.Count("PutObject") != 0 {
		t.Fatalf("uploaded without a question: %v", srv.Calls())
	}

	terminal(t, true, true)
	stdin(t, "\n") // Enter is a no
	_, stderr, err := runHop(t, "s3", "push", "-p", "acme-billing", local, "s3://partner-drop/in/")
	if !errors.Is(err, errAborted) || !strings.Contains(stderr, "s3://partner-drop (eu-central-1) is not a bucket of the account of acme-billing. Upload to it? [y/N]") {
		t.Fatalf("%v\n%s", err, stderr)
	}
	stdin(t, "y\n")
	if _, stderr, err = runHop(t, "s3", "push", "-p", "acme-billing", local, "s3://partner-drop/in/"); err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	if srv.Get("partner-drop", "in/report.csv") == nil {
		t.Error("not uploaded after a yes")
	}
	// Reading is what was asked, by its full name: no question.
	terminal(t, false, false)
	t.Chdir(t.TempDir())
	if _, stderr, err := runHop(t, "s3", "pull", "-p", "acme-billing", "s3://partner-drop/in/report.csv"); err != nil {
		t.Fatalf("pull: %v\n%s", err, stderr)
	}
}

func TestS3ResolveBucketAsks(t *testing.T) {
	s3Env(t)
	terminal(t, true, true)
	stdin(t, "y\n")
	stdout, stderr, err := runHop(t, "s3", "ls", "-p", "acme-billing", "log/2026/11/")
	if err != nil || !strings.Contains(stdout, "app.log") {
		t.Fatalf("%v\n%s\n%s", err, stdout, stderr)
	}
	if !strings.Contains(stderr, `"log" only partly matches the bucket acme-b-logs (acme-billing, eu-west-1). List it? [Y/n]`) {
		t.Errorf("question:\n%s", stderr)
	}

	// Before writing, a doubt is a no: Enter refuses.
	stdin(t, "\n")
	local := filepath.Join(t.TempDir(), "f.txt")
	os.WriteFile(local, []byte("x"), 0o600)
	_, stderr, err = runHop(t, "s3", "push", "-p", "acme-billing", local, "log/")
	if !errors.Is(err, errAborted) || !strings.Contains(stderr, "Upload to it? [y/N]") {
		t.Errorf("%v\n%s", err, stderr)
	}
}

func TestS3Pull(t *testing.T) {
	srv := s3Env(t)
	dir := t.TempDir()
	t.Chdir(dir)
	stdout, stderr, err := runHop(t, "s3", "pull", "-p", "acme-billing", "acme-b-logs/2026/11/app.log")
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "app.log")); len(got) != 1500 {
		t.Fatalf("got %d bytes", len(got))
	}
	if stdout != "" {
		t.Errorf("stdout: %q", stdout)
	}
	for _, want := range []string{
		"hop: s3://acme-b-logs/2026/11/app.log -> app.log (acme-billing, eu-west-1)\n",
		"hop: 1.5 KB received, SHA256 matches on both sides\n",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}

	// To a directory, from the instance command: s3:// and the exact name.
	os.Mkdir("logs", 0o700)
	_, stderr, err = runHop(t, "pull", "-p", "acme-billing", "s3://acme-b-logs/2026/11/db.log", "logs")
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	// By a word of the bucket name, without s3://.
	if _, stderr, err := runHop(t, "s3", "pull", "-p", "acme-billing", "logs/2026/11/db.log", filepath.Join(dir, "by-word.log")); err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "logs", "db.log")); len(got) != 20 {
		t.Errorf("got %d bytes", len(got))
	}
	// No checksum of the whole object in S3: hop says what it checked instead.
	if !strings.Contains(stderr, "hop: 20 B received, size checked: S3 holds no checksum of the whole object to compare\n") {
		t.Errorf("stderr:\n%s", stderr)
	}

	// In several ranges, to a path of its own.
	if _, stderr, err = runHop(t, "s3", "pull", "-p", "acme-billing", "acme-b-logs/top.bin", "copy.bin"); err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	if !strings.Contains(stderr, "2.5 MB received, CRC64NVME matches on both sides") {
		t.Errorf("stderr:\n%s", stderr)
	}

	_, stderr, err = runHop(t, "s3", "pull", "-p", "acme-billing", "acme-b-logs/2026/11/nope.log")
	if err == nil || err.Error() != "no such object: s3://acme-b-logs/2026/11/nope.log" || !strings.Contains(stderr, "hop s3 ls acme-b-logs/2026/11/ lists what is there") {
		t.Errorf("missing object: %v\n%s", err, stderr)
	}
	if _, err := os.Stat(filepath.Join(dir, "nope.log")); err == nil {
		t.Error("a file was created for a missing object")
	}

	// A corrupted transfer leaves nothing behind.
	srv.Corrupt = true
	_, _, err = runHop(t, "s3", "pull", "-p", "acme-billing", "acme-b-logs/2026/readme.txt")
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch after download (CRC32)") {
		t.Errorf("corrupted: %v", err)
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "*readme*")); len(m) != 0 {
		t.Errorf("files left: %v", m)
	}
}

func TestS3PullChecksBeforeResolving(t *testing.T) {
	srv := s3Env(t)
	t.Chdir(t.TempDir())
	for args, want := range map[string]string{
		"acme-b-logs/2026/":                     "names a prefix, not an object",
		"acme-b-logs":                           "names a prefix, not an object",
		"s3://":                                 "no bucket in",
		"acme-b-logs/2026/readme.txt missing/x": "directory not found: missing",
	} {
		_, _, err := runHop(t, append([]string{"s3", "pull"}, strings.Fields(args)...)...)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: error %v, want %q", args, err, want)
		}
	}
	if len(srv.Calls()) != 0 {
		t.Errorf("S3 was called: %v", srv.Calls())
	}
	// The instance form keeps its own arguments.
	if _, _, err := runHop(t, "pull", "tools"); err == nil || !strings.Contains(err.Error(), "accepts between 2 and 3 arg(s)") {
		t.Errorf("hop pull tools: %v", err)
	}
	if _, _, err := runHop(t, "pull", "s3://a/b", "c", "d"); err == nil || !strings.Contains(err.Error(), "accepts between 1 and 2 arg(s)") {
		t.Errorf("hop pull s3://a/b c d: %v", err)
	}
}

func TestS3Push(t *testing.T) {
	srv := s3Env(t)
	dir := t.TempDir()
	local := filepath.Join(dir, "dump.sql")
	os.WriteFile(local, bytes.Repeat([]byte("row\n"), 500), 0o600)

	// A key that ends with "/" keeps the name of the file.
	stdout, stderr, err := runHop(t, "s3", "push", "-p", "acme-billing", local, "acme-b-backups/db/")
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	if o := srv.Get("acme-b-backups", "db/dump.sql"); o == nil || len(o.Data) != 2000 {
		t.Fatalf("object: %+v", o)
	}
	if stdout != "" {
		t.Errorf("stdout: %q", stdout)
	}
	for _, want := range []string{
		"hop: " + local + " -> s3://acme-b-backups/db/dump.sql (acme-billing, us-east-1)\n",
		"hop: 2.0 KB sent, CRC32 matches on both sides\n",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}

	// Never replaced without a yes: refused where nobody can answer.
	os.WriteFile(local, []byte("another dump"), 0o600)
	_, _, err = runHop(t, "s3", "push", "-p", "acme-billing", local, "acme-b-backups/db/dump.sql")
	if !errors.Is(err, bucket.ErrExists) || !strings.Contains(err.Error(), "s3://acme-b-backups/db/dump.sql already exists: --force replaces it (no terminal to confirm)") {
		t.Errorf("existing object: %v", err)
	}
	if o := srv.Get("acme-b-backups", "db/dump.sql"); len(o.Data) != 2000 {
		t.Fatal("the object was replaced")
	}
	if srv.Count("PutObject") != 1 {
		t.Errorf("a second upload was attempted: %v", srv.Calls())
	}
	if _, stderr, err = runHop(t, "s3", "push", "-p", "acme-billing", "--force", local, "acme-b-backups/db/dump.sql"); err != nil {
		t.Fatalf("--force: %v\n%s", err, stderr)
	}
	if o := srv.Get("acme-b-backups", "db/dump.sql"); string(o.Data) != "another dump" {
		t.Error("--force did not replace the object")
	}

	// The bucket alone, and the instance command with s3://.
	if _, stderr, err = runHop(t, "push", "-p", "acme-billing", local, "s3://acme-t-scratch"); err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	if srv.Get("acme-t-scratch", "dump.sql") == nil {
		t.Errorf("not at the top of the bucket: %v", srv.Calls())
	}

	for args, want := range map[string]string{
		filepath.Join(dir, "missing") + " acme-b-backups/": "file not found",
		dir + " acme-b-backups/":                           "file not found",
		local + " s3://":                                   "no bucket in",
	} {
		_, _, err := runHop(t, append([]string{"s3", "push", "-p", "acme-billing"}, strings.Fields(args)...)...)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: error %v, want %q", args, err, want)
		}
	}
	// --force means nothing for an instance, where a push always replaces the file.
	if _, _, err := runHop(t, "push", "--force", local, "tools", "/etc/motd"); err == nil || !strings.Contains(err.Error(), "--force is for an s3:// destination") {
		t.Errorf("--force to an instance: %v", err)
	}
	if _, _, err := runHop(t, "push", local, "tools"); err == nil || !strings.Contains(err.Error(), "accepts 3 arg(s)") {
		t.Errorf("hop push file instance: %v", err)
	}
}

func TestS3PushAsksBeforeReplacing(t *testing.T) {
	srv := s3Env(t)
	srv.Put("acme-b-backups", "app.conf", []byte("in place"), "")
	local := filepath.Join(t.TempDir(), "app.conf")
	os.WriteFile(local, []byte("new"), 0o600)
	terminal(t, true, true)

	stdin(t, "n\n")
	_, stderr, err := runHop(t, "s3", "push", "-p", "acme-billing", local, "acme-b-backups/")
	if !errors.Is(err, errAborted) || !strings.Contains(stderr, "s3://acme-b-backups/app.conf exists (8 B, modified ") || !strings.Contains(stderr, "Replace it? [y/N]") {
		t.Fatalf("%v\n%s", err, stderr)
	}
	if string(srv.Get("acme-b-backups", "app.conf").Data) != "in place" {
		t.Fatal("replaced after a no")
	}
	stdin(t, "y\n")
	if _, stderr, err = runHop(t, "s3", "push", "-p", "acme-billing", local, "acme-b-backups/"); err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	if string(srv.Get("acme-b-backups", "app.conf").Data) != "new" {
		t.Error("not replaced after a yes")
	}
}

func TestS3PushWithoutTheRightToLook(t *testing.T) {
	srv := s3Env(t)
	srv.DenyHead = true // a role that may write, not read
	local := filepath.Join(t.TempDir(), "app.conf")
	os.WriteFile(local, []byte("new"), 0o600)
	if _, stderr, err := runHop(t, "s3", "push", "-p", "acme-billing", local, "acme-b-backups/"); err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	// hop could not see the object: S3 itself refuses to write over it.
	os.WriteFile(local, []byte("newer"), 0o600)
	_, stderr, err := runHop(t, "s3", "push", "-p", "acme-billing", local, "acme-b-backups/")
	if !errors.Is(err, bucket.ErrExists) || !strings.Contains(stderr, "--force replaces it") {
		t.Errorf("%v\n%s", err, stderr)
	}
	if string(srv.Get("acme-b-backups", "app.conf").Data) != "new" {
		t.Error("the object was replaced")
	}
}

func TestS3Completion(t *testing.T) {
	s3Env(t)
	// Completion reads the cache: fill it.
	if _, _, err := runHop(t, "s3", "ls", "-p", "acme-billing"); err != nil {
		t.Fatal(err)
	}
	f := &filters{}
	got, dir := completeBuckets(f, 0)(nil, nil, "")
	if dir != cobra.ShellCompDirectiveNoFileComp|cobra.ShellCompDirectiveNoSpace {
		t.Errorf("directive %v", dir)
	}
	if len(got) != 3 || got[1] != "acme-b-logs/\tacme-billing, eu-west-1" {
		t.Errorf("completions %q", got)
	}
	if got, _ := completeBuckets(f, 0)(nil, nil, "s3://ac"); len(got) != 3 || !strings.HasPrefix(got[0], "s3://acme-b-backups/") {
		t.Errorf("with s3://: %q", got)
	}
	// Keys are not completed, and neither is another argument.
	if got, _ := completeBuckets(f, 0)(nil, nil, "acme-b-logs/20"); got != nil {
		t.Errorf("keys: %q", got)
	}
	if got, _ := completeBuckets(f, 1)(nil, nil, ""); got != nil {
		t.Errorf("another position: %q", got)
	}
}

func TestS3NeedsAnAWSProfile(t *testing.T) {
	isolate(t)
	terminal(t, false, false)
	_, _, err := runHop(t, "s3", "ls")
	if err == nil || !strings.Contains(err.Error(), "no AWS profile with credentials") {
		t.Errorf("no profile at all: %v", err)
	}
}

func TestShortErrorDropsTheHostID(t *testing.T) {
	in := "operation error S3: GetObject, https response error StatusCode: 403, RequestID: ABC123, HostID: aBcD/eF+g=, api error AccessDenied: Access Denied"
	if got := shortError(in); got != "operation error S3: GetObject, api error AccessDenied: Access Denied" {
		t.Errorf("%q", got)
	}
	// The other services have no host ID.
	in = "operation error EC2: DescribeInstances, https response error StatusCode: 403, RequestID: ABC123, api error UnauthorizedOperation: denied"
	if got := shortError(in); got != "operation error EC2: DescribeInstances, api error UnauthorizedOperation: denied" {
		t.Errorf("%q", got)
	}
}
