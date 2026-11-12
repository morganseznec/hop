package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/morganseznec/hop/internal/bucket"
)

// screen is what the picker showed one time it opened, and the line the cursor started
// on.
type screen struct {
	header string
	lines  []string
	start  int
}

// names returns the first word of each line: the bucket, or the name of the entry.
func (s screen) names() string {
	var out []string
	for _, l := range s.lines {
		out = append(out, strings.Fields(l)[0])
	}
	return strings.Join(out, " ")
}

// scriptPicker stands in for the fuzzy picker, which needs a real terminal. Each time
// the picker opens, it records what is shown and chooses the line whose first word is
// the next of wants; once they are used up, it answers Esc.
func scriptPicker(t *testing.T, wants ...string) *[]screen {
	t.Helper()
	var screens []screen
	saved, width := choose, pickerWidth
	t.Cleanup(func() { choose, pickerWidth = saved, width })
	pickerWidth = func() int { return 120 }
	choose = func(n int, line func(int) string, header string, start int) (int, error) {
		s := screen{header: header, start: start}
		for i := 0; i < n; i++ {
			s.lines = append(s.lines, line(i))
		}
		screens = append(screens, s)
		if len(wants) == 0 {
			return 0, errAborted
		}
		want := wants[0]
		wants = wants[1:]
		for i, l := range s.lines {
			if strings.Fields(l)[0] == want {
				return i, nil
			}
		}
		t.Errorf("no line %q in the picker:\n%s", want, strings.Join(s.lines, "\n"))
		return 0, errAborted
	}
	return &screens
}

func TestS3Browse(t *testing.T) {
	s3Env(t)
	terminal(t, true, true)
	dir := t.TempDir()
	t.Chdir(dir)
	stdin(t, "y\n")
	screens := scriptPicker(t, "acme-b-logs", "2026/", "11/", "app.log")

	stdout, stderr, err := runHop(t, "s3", "-p", "acme-billing")
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	if stdout != "" {
		t.Errorf("stdout %q", stdout)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "app.log")); len(got) != 1500 {
		t.Fatalf("downloaded %d bytes", len(got))
	}
	for _, want := range []string{
		"Download s3://acme-b-logs/2026/11/app.log (1.5 KB) to app.log? [Y/n]",
		"hop: s3://acme-b-logs/2026/11/app.log -> app.log (acme-billing, eu-west-1)\n",
		"1.5 KB received, SHA256 matches on both sides",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}

	s := *screens
	if len(s) != 4 {
		t.Fatalf("the picker opened %d times", len(s))
	}
	// The buckets first, as hop shows the instances.
	if s[0].header != "3 buckets · Enter opens, Esc quits" || s[0].names() != "acme-b-backups acme-b-logs acme-t-scratch" {
		t.Errorf("buckets: %q\n%s", s[0].header, strings.Join(s[0].lines, "\n"))
	}
	if l := s[0].lines[2]; strings.Join(strings.Fields(l), " ") != "acme-t-scratch acme-billing eu-west-3 test" {
		t.Errorf("bucket line %q", l)
	}
	// Then the bucket, one level at a time, with the way back on the first line.
	if s[1].header != "s3://acme-b-logs/ · 1 object, 1 prefix · Enter opens, Esc quits" || s[1].names() != ".. 2026/ top.bin" {
		t.Errorf("top of the bucket: %q\n%s", s[1].header, strings.Join(s[1].lines, "\n"))
	}
	if !strings.Contains(s[1].lines[0], "back to the list of buckets") {
		t.Errorf("first line at the top: %q", s[1].lines[0])
	}
	// The cursor starts on the first bucket, then on the first entry of a listing,
	// not on the way back.
	if s[0].start != 0 || s[1].start != 1 || s[3].start != 1 {
		t.Errorf("cursor starts on lines %d, %d, %d", s[0].start, s[1].start, s[3].start)
	}
	// Names that start with a dot are shown: there is no -a to type here. The empty
	// object that stands for the directory is not an entry.
	if s[2].names() != ".. .env .git/ 11/ readme.txt" || !strings.Contains(s[2].header, "s3://acme-b-logs/2026/ ") || !strings.Contains(s[2].header, "2 objects, 2 prefixes") {
		t.Errorf("2026/: %q\n%s", s[2].header, strings.Join(s[2].lines, "\n"))
	}
	if !strings.Contains(s[2].lines[0], "back to the prefix above") {
		t.Errorf("first line below the top: %q", s[2].lines[0])
	}
	if l := strings.Join(strings.Fields(s[2].lines[4]), " "); l != "readme.txt 5 B 2026-11-09 08:30 STANDARD" {
		t.Errorf("object line %q", l)
	}
	if s[3].names() != ".. app.log db.log" || !strings.Contains(s[3].header, "2 objects ·") {
		t.Errorf("2026/11/: %q\n%s", s[3].header, strings.Join(s[3].lines, "\n"))
	}
	// A storage class other than the usual one shows on the line: an archived object
	// cannot be downloaded as it is.
	if l := strings.Join(strings.Fields(s[1].lines[2]), " "); l != "top.bin 2.5 MB 2026-11-07 14:02 STANDARD_IA" {
		t.Errorf("object line %q", l)
	}
}

func TestS3BrowseGoesBackUp(t *testing.T) {
	s3Env(t)
	terminal(t, true, true)
	t.Chdir(t.TempDir())
	// Down one level, back to the top, back to the buckets, into another one, then Esc.
	screens := scriptPicker(t, "acme-b-logs", "2026/", "..", "..", "acme-b-backups")
	_, stderr, err := runHop(t, "s3", "-p", "acme-billing")
	if !errors.Is(err, errAborted) {
		t.Fatalf("Esc must end like a canceled picker: %v\n%s", err, stderr)
	}
	var where []string
	for _, s := range *screens {
		where = append(where, strings.Fields(s.header)[0]+" "+strings.Fields(s.header)[1])
	}
	want := []string{"3 buckets", "s3://acme-b-logs/ ·", "s3://acme-b-logs/2026/ ·", "s3://acme-b-logs/ ·", "3 buckets", "s3://acme-b-backups/ ·"}
	if strings.Join(where, "|") != strings.Join(want, "|") {
		t.Errorf("screens:\n got %q\nwant %q", where, want)
	}
	// An empty bucket: only the way back, and the header says so. It lives in another
	// region than its profile.
	last := (*screens)[5]
	if last.names() != ".." || last.start != 0 || last.header != "s3://acme-b-backups/ · nothing here · Enter opens, Esc quits" {
		t.Errorf("empty bucket: %q\n%s", last.header, strings.Join(last.lines, "\n"))
	}
}

func TestS3BrowseDownloadIsAQuestion(t *testing.T) {
	srv := s3Env(t)
	terminal(t, true, true)
	dir := t.TempDir()
	t.Chdir(dir)

	// A no returns to the listing, with nothing downloaded.
	stdin(t, "n\n")
	screens := scriptPicker(t, "acme-b-logs", "top.bin")
	_, stderr, err := runHop(t, "s3", "-p", "acme-billing")
	if !errors.Is(err, errAborted) || len(*screens) != 3 || (*screens)[2].names() != ".. 2026/ top.bin" {
		t.Fatalf("%v, %d screens\n%s", err, len(*screens), stderr)
	}
	if !strings.Contains(stderr, "Download s3://acme-b-logs/top.bin (2.5 MB) to top.bin? [Y/n]") {
		t.Errorf("question:\n%s", stderr)
	}
	if _, err := os.Stat(filepath.Join(dir, "top.bin")); err == nil || srv.Count("GetObject") != 0 {
		t.Errorf("downloaded after a no: %v", srv.Calls())
	}

	// A file of that name is here already: the question says it goes.
	os.WriteFile(filepath.Join(dir, "top.bin"), []byte("mine"), 0o600)
	stdin(t, "\n") // Enter is a yes
	scriptPicker(t, "acme-b-logs", "top.bin")
	_, stderr, err = runHop(t, "s3", "-p", "acme-billing")
	if err != nil || !strings.Contains(stderr, "to top.bin, which replaces the file there? [Y/n]") {
		t.Fatalf("%v\n%s", err, stderr)
	}
	if st, _ := os.Stat(filepath.Join(dir, "top.bin")); st.Size() != 2_500_000 {
		t.Errorf("the file was not replaced: %d bytes", st.Size())
	}
}

func TestS3BrowseNeedsATerminal(t *testing.T) {
	srv := s3Env(t)
	screens := scriptPicker(t)
	// Nobody to choose: the help, and nothing asked to S3.
	stdout, _, err := runHop(t, "s3")
	if err != nil || !strings.Contains(stdout, "hop s3 [command]") || !strings.Contains(stdout, "opens a picker over those buckets") {
		t.Errorf("%v\n%s", err, stdout)
	}
	if len(*screens) != 0 || len(srv.Calls()) != 0 {
		t.Errorf("the picker opened, or S3 was called: %v", srv.Calls())
	}
}

func TestS3BrowseWithoutBuckets(t *testing.T) {
	s3Env(t)
	terminal(t, true, true)
	screens := scriptPicker(t)
	// The only profile in scope cannot sign in: say why, rather than open an empty picker.
	_, stderr, err := runHop(t, "s3", "-p", "acme-prod")
	if err == nil || err.Error() != "no bucket in scope" || !strings.Contains(stderr, "SSO token expired for acme-sso") {
		t.Errorf("%v\n%s", err, stderr)
	}
	_, _, err = runHop(t, "s3", "-p", "acme-billing", "-r", "ap-south-1")
	if err == nil || err.Error() != "no bucket in scope: -r and -e leave out 3 buckets" {
		t.Errorf("filtered out: %v", err)
	}
	if len(*screens) != 0 {
		t.Error("the picker opened without a bucket to show")
	}
}

func TestBrowseHeaderFitsTheTerminal(t *testing.T) {
	loc := bucket.Location{Bucket: "acme-production-application-logs-eu-west-1", Key: "services/checkout/2026/11/12/"}
	entries := []entry{{Object: bucket.Object{Key: loc.Key + "a.log"}}, {Object: bucket.Object{Key: loc.Key + "old/", Prefix: true}}}
	for _, width := range []int{80, 100, 200} {
		h := browseHeader(entries, loc, width)
		// The picker cuts what goes past the width, less two columns on each side: the
		// keys, at the end, must never be what goes.
		if n := len([]rune(h)); n > width-4 || !strings.HasSuffix(h, " · 1 object, 1 prefix · Enter opens, Esc quits") {
			t.Errorf("width %d: header of %d columns: %q", width, n, h)
		}
		// The end of the place, where the picker is, stays.
		if !strings.Contains(h, "/2026/11/12/ ·") {
			t.Errorf("width %d: the end of the place is gone: %q", width, h)
		}
	}
	if h := browseHeader(entries, loc, 200); !strings.HasPrefix(h, "s3://acme-production-application-logs-eu-west-1/services/") {
		t.Errorf("a wide terminal shows the whole place: %q", h)
	}
}

func TestTailKeepsTheEndOfALongPlace(t *testing.T) {
	if got := tail("s3://acme-logs/2026/", 60); got != "s3://acme-logs/2026/" {
		t.Errorf("short: %q", got)
	}
	long := "s3://acme-production-application-logs-eu-west-1/services/checkout/2026/11/12/"
	got := tail(long, 40)
	if !strings.HasPrefix(got, "…") || !strings.HasSuffix(got, "/checkout/2026/11/12/") || len([]rune(got)) != 40 {
		t.Errorf("long: %q (%d)", got, len([]rune(got)))
	}
}

func TestParentPrefix(t *testing.T) {
	for prefix, want := range map[string]string{"2026/11/": "2026/", "2026/": "", "a/b/c/": "a/b/", "a//": "a/"} {
		if got := parentPrefix(prefix); got != want {
			t.Errorf("parentPrefix(%q) = %q, want %q", prefix, got, want)
		}
	}
}
