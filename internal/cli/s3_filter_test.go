package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/morganseznec/hop/internal/bucket"
)

func TestS3LsFiltersObjects(t *testing.T) {
	s3Env(t)
	cases := []struct {
		name   string
		args   []string
		stdout []string
		footer string
	}{
		{"a word after the place", []string{"acme-b-logs/2026/11/", "app"},
			[]string{"app.log"}, `1 object, 1.5 KB matching "app" in s3://acme-b-logs/2026/11/ (acme-billing, eu-west-1)`},
		{"a word in another case", []string{"acme-b-logs/2026/11/", "APP"}, []string{"app.log"}, `matching "app"`},
		{"a pattern after the place", []string{"acme-b-logs/2026/11/", "*.log"}, []string{"app.log", "db.log"}, `2 objects, 1.5 KB matching "*.log"`},
		{"a pattern at the end of the place", []string{"acme-b-logs/2026/11/*.log"},
			[]string{"app.log", "db.log"}, `matching "*.log" in s3://acme-b-logs/2026/11/ (`},
		{"a pattern in the middle of a name", []string{"acme-b-logs/2026/11/a*g"}, []string{"app.log"}, `matching "a*g"`},
		{"pattern and words", []string{"acme-b-logs/2026/11/*.log", "db"}, []string{"db.log"}, `matching "*.log db"`},
		{"a prefix is an entry too", []string{"acme-b-logs/2026/", "1"}, []string{"11/"}, `1 prefix matching "1"`},
		// -R is how to find a file somewhere in a bucket.
		{"words among everything below", []string{"-R", "acme-b-logs/", "log"},
			[]string{"2026/11/app.log", "2026/11/db.log"}, `2 objects, 1.5 KB matching "log" in s3://acme-b-logs/ (`},
		{"several words, any order, any case", []string{"-R", "acme-b-logs/", "LOG", "db"}, []string{"2026/11/db.log"}, `matching "log db"`},
		{"a pattern among everything below", []string{"-R", "acme-b-logs/", "*.txt"}, []string{"2026/readme.txt"}, `matching "*.txt"`},
		// A pattern that names several levels reads everything below by itself.
		{"a pattern across levels", []string{"acme-b-logs/*/readme.txt"}, []string{"2026/readme.txt"}, `matching "*/readme.txt" in s3://acme-b-logs/ (`},
		{"names that start with a dot stay out without -a", []string{"-R", "acme-b-logs/", "env"},
			nil, `0 objects matching "env" in s3://acme-b-logs/ (acme-billing, eu-west-1); 1 not shown, starting with a dot: -a lists them`},
		{"and come with -a", []string{"-Ra", "acme-b-logs/", "env"}, []string{"2026/.env"}, `1 object, 9 B matching "env"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stdout, stderr, err := runHop(t, append([]string{"s3", "ls", "-p", "acme-billing"}, c.args...)...)
			if err != nil {
				t.Fatalf("%v\n%s", err, stderr)
			}
			if len(c.stdout) == 0 && stdout != "" {
				t.Errorf("stdout %q", stdout)
			} else if len(c.stdout) > 0 {
				sameLines(t, "stdout", stdout, c.stdout...)
			}
			if !strings.Contains(stderr, c.footer) {
				t.Errorf("stderr lacks %q:\n%s", c.footer, stderr)
			}
		})
	}

	// Nothing at this level: say where else to look.
	stdout, stderr, err := runHop(t, "s3", "ls", "-p", "acme-billing", "acme-b-logs/", "readme")
	if err != nil || stdout != "" || !strings.Contains(stderr, `0 objects matching "readme" in s3://acme-b-logs/`) ||
		!strings.Contains(stderr, "-R looks in every object below s3://acme-b-logs/") {
		t.Errorf("no match at one level: %v, %q\n%s", err, stdout, stderr)
	}
	// With -l, the filtered listing keeps its columns.
	stdout, _, err = runHop(t, "s3", "ls", "-p", "acme-billing", "-lh", "acme-b-logs/2026/11/", "db")
	if err != nil {
		t.Fatal(err)
	}
	sameLines(t, "long", stdout, "SIZE MODIFIED CLASS NAME", "20 B 2026-11-07 14:02 STANDARD db.log")
	// --json is filtered the same way.
	stdout, _, err = runHop(t, "s3", "ls", "-p", "acme-billing", "--json", "-R", "acme-b-logs/", "*.log")
	var rows []bucket.Object
	if err != nil || json.Unmarshal([]byte(stdout), &rows) != nil || len(rows) != 2 || rows[1].Key != "2026/11/db.log" {
		t.Errorf("JSON: %v\n%s", err, stdout)
	}
	if _, _, err := runHop(t, "s3", "ls", "-p", "acme-billing", "acme-b-logs/", "app-[0-9"); err == nil || !strings.Contains(err.Error(), `invalid pattern "app-[0-9"`) {
		t.Errorf("a bracket never closed: %v", err)
	}
}

func TestS3LsBucketsByPattern(t *testing.T) {
	s3Env(t)
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"a pattern for the name", []string{"acme-b-*"}, "acme-b-backups acme-b-logs"},
		{"the end of the name", []string{"*scratch"}, "acme-t-scratch"},
		{"in any case", []string{"ACME-B-*"}, "acme-b-backups acme-b-logs"},
		{"one character", []string{"acme-?-logs"}, "acme-b-logs"},
		{"a pattern and a word", []string{"acme-*", "logs"}, "acme-b-logs"},
		{"a pattern and a word of the line", []string{"*", "us-east-1"}, "acme-b-backups"},
		{"after s3://", []string{"s3://acme-b-*"}, "acme-b-backups acme-b-logs"},
		{"after s3://, with a word", []string{"s3://acme-*", "scratch"}, "acme-t-scratch"},
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
			if strings.Join(got, " ") != c.want {
				t.Errorf("got %v, want %s", got, c.want)
			}
		})
	}
	stdout, stderr, err := runHop(t, "s3", "ls", "-p", "acme-billing", "globex-*")
	if err != nil || stdout != "" || !strings.Contains(stderr, `no buckets match "globex-*"`) {
		t.Errorf("no match: %v, %q\n%s", err, stdout, stderr)
	}
	for args, want := range map[string]string{
		"acme-*/2026/": "a pattern in the bucket name lists buckets, not what one of them holds: hop s3 ls 'acme-*'",
		"acme-[b":      `invalid pattern "acme-[b"`,
	} {
		if _, _, err := runHop(t, "s3", "ls", "-p", "acme-billing", args); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: error %v, want %q", args, err, want)
		}
	}
}

func TestS3BrowseFromWords(t *testing.T) {
	s3Env(t)
	terminal(t, true, true)
	t.Chdir(t.TempDir())

	// One bucket found: the picker opens in it, without showing the list first.
	screens := scriptPicker(t)
	_, stderr, err := runHop(t, "s3", "-p", "acme-billing", "scratch")
	if !errors.Is(err, errAborted) || len(*screens) != 1 || !strings.HasPrefix((*screens)[0].header, "s3://acme-t-scratch/ · nothing here") {
		t.Fatalf("one match: %v, %+v\n%s", err, *screens, stderr)
	}

	// Several: the picker shows them, and only them. ".." at the top of the one chosen
	// leads back to the whole list.
	screens = scriptPicker(t, "acme-b-logs", "..")
	_, stderr, err = runHop(t, "s3", "-p", "acme-billing", "acme-b-")
	if !errors.Is(err, errAborted) || len(*screens) != 3 {
		t.Fatalf("several matches: %v, %d screens\n%s", err, len(*screens), stderr)
	}
	s := *screens
	if s[0].header != `2 buckets for "acme-b-" · Enter opens, Esc quits` || s[0].names() != "acme-b-backups acme-b-logs" {
		t.Errorf("matches: %q\n%s", s[0].header, strings.Join(s[0].lines, "\n"))
	}
	if !strings.HasPrefix(s[1].header, "s3://acme-b-logs/ ·") || s[2].header != "3 buckets · Enter opens, Esc quits" {
		t.Errorf("then: %q, %q", s[1].header, s[2].header)
	}

	// A pattern finds buckets like words do.
	screens = scriptPicker(t)
	if _, _, err = runHop(t, "s3", "-p", "acme-billing", "acme-b-*"); !errors.Is(err, errAborted) || (*screens)[0].header != `2 buckets for "acme-b-*" · Enter opens, Esc quits` {
		t.Errorf("pattern: %v, %+v", err, *screens)
	}
	screens = scriptPicker(t)
	if _, _, err = runHop(t, "s3", "-p", "acme-billing", "*-logs"); !errors.Is(err, errAborted) || !strings.HasPrefix((*screens)[0].header, "s3://acme-b-logs/ ·") {
		t.Errorf("pattern with one match: %v, %+v", err, *screens)
	}
}

func TestS3BrowseFromAPlace(t *testing.T) {
	s3Env(t)
	terminal(t, true, true)
	t.Chdir(t.TempDir())

	// The picker opens at the prefix, found by a word of the bucket name; ".." walks
	// back up to the top, then to the buckets.
	screens := scriptPicker(t, "..", "..", "..")
	_, stderr, err := runHop(t, "s3", "-p", "acme-billing", "logs/2026/11")
	if !errors.Is(err, errAborted) || len(*screens) != 4 {
		t.Fatalf("%v, %d screens\n%s", err, len(*screens), stderr)
	}
	var where []string
	for _, s := range *screens {
		where = append(where, strings.Fields(s.header)[0])
	}
	if got := strings.Join(where, " "); got != "s3://acme-b-logs/2026/11/ s3://acme-b-logs/2026/ s3://acme-b-logs/ 3" {
		t.Errorf("screens: %s", got)
	}
	if (*screens)[0].names() != ".. app.log db.log" {
		t.Errorf("first screen:\n%s", strings.Join((*screens)[0].lines, "\n"))
	}

	// After s3://, the exact name.
	screens = scriptPicker(t)
	if _, _, err = runHop(t, "s3", "-p", "acme-billing", "s3://acme-b-logs/2026/"); !errors.Is(err, errAborted) || !strings.HasPrefix((*screens)[0].header, "s3://acme-b-logs/2026/ ·") {
		t.Errorf("exact place: %v, %+v", err, *screens)
	}
	// s3:// alone is every bucket.
	screens = scriptPicker(t)
	if _, _, err = runHop(t, "s3", "-p", "acme-billing", "s3://"); !errors.Is(err, errAborted) || (*screens)[0].header != "3 buckets · Enter opens, Esc quits" {
		t.Errorf("s3://: %v, %+v", err, *screens)
	}
}

func TestS3BrowseRefusals(t *testing.T) {
	srv := s3Env(t)
	terminal(t, true, true)
	screens := scriptPicker(t)
	for args, want := range map[string]string{
		"nothing-like-it":          `no bucket matches "nothing-like-it": hop s3 ls lists them all`,
		"acme-b-logs/2026/*.log":   "the picker walks prefixes, it does not take a pattern for keys: hop s3 ls 'acme-b-logs/2026/*.log' lists what fits",
		"acme-b-logs/ readme":      "the picker opens one place: to filter what acme-b-logs/ holds, hop s3 ls acme-b-logs/ readme",
		"-r us-east-1 acme-b-logs": `no bucket matches "acme-b-logs": -r us-east-1 leaves out acme-b-logs, which is in eu-west-1`,
	} {
		_, _, err := runHop(t, append([]string{"s3", "-p", "acme-billing"}, strings.Fields(args)...)...)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: error %v, want %q", args, err, want)
		}
	}
	if len(*screens) != 0 {
		t.Errorf("the picker opened: %+v", *screens)
	}

	// Words without a terminal: nobody to choose, and the command that lists instead.
	terminal(t, false, false)
	before := len(srv.Calls())
	_, _, err := runHop(t, "s3", "billing", "logs")
	if err == nil || err.Error() != "no terminal: hop s3 billing logs opens a picker (to list without one: hop s3 ls billing logs)" {
		t.Errorf("no terminal: %v", err)
	}
	if len(srv.Calls()) != before {
		t.Errorf("S3 was called: %v", srv.Calls())
	}
}

func TestS3PullByPattern(t *testing.T) {
	srv := s3Env(t)
	dir := t.TempDir()
	t.Chdir(dir)
	os.Mkdir("out", 0o700)

	// Without a terminal, a pattern is what was asked: hop says what it found, and goes.
	_, stderr, err := runHop(t, "s3", "pull", "-p", "acme-billing", "acme-b-logs/2026/11/*.log", "out")
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	for name, size := range map[string]int64{"app.log": 1500, "db.log": 20} {
		if st, err := os.Stat(filepath.Join(dir, "out", name)); err != nil || st.Size() != size {
			t.Errorf("%s: %v", name, err)
		}
	}
	for _, want := range []string{
		"hop: 2 objects (1.5 KB) matching s3://acme-b-logs/2026/11/*.log\n",
		"hop: s3://acme-b-logs/2026/11/app.log -> " + filepath.Join("out", "app.log") + " (acme-billing, eu-west-1)\n",
		"hop: 1.5 KB received, SHA256 matches on both sides\n",
		"hop: 2 objects downloaded to out\n",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}

	// One object fits: no need of a count, and the current directory by default.
	if _, stderr, err = runHop(t, "s3", "pull", "-p", "acme-billing", "acme-b-logs/2026/read*"); err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "readme.txt")); string(b) != "hello" {
		t.Errorf("readme.txt: %q", b)
	}

	before := srv.Count("GetObject")
	for args, want := range map[string]string{
		"acme-b-logs/2026/11/*.gz":        "no object matches s3://acme-b-logs/2026/11/*.gz",
		"acme-b-logs/*/readme.txt":        `the pattern "*/readme.txt" spans several levels: hop s3 pull takes the objects of one prefix at a time`,
		"acme-b-logs/2026/11/*.log nope":  "nope is not a directory: the objects a pattern matches need one to land in",
		"acme-b-*/2026/readme.txt":        "a pattern in the bucket name is for hop s3 ls",
		"acme-b-logs/2026/11/[ab.log out": "invalid pattern",
	} {
		_, _, err := runHop(t, append([]string{"s3", "pull", "-p", "acme-billing"}, strings.Fields(args)...)...)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: error %v, want %q", args, err, want)
		}
	}
	if srv.Count("GetObject") != before {
		t.Errorf("something was downloaded: %v", srv.Calls())
	}
}

func TestS3PullByPatternAsksFirst(t *testing.T) {
	srv := s3Env(t)
	terminal(t, true, true)
	dir := t.TempDir()
	t.Chdir(dir)

	// Several files land at once: a question, where somebody can answer.
	stdin(t, "n\n")
	_, stderr, err := runHop(t, "s3", "pull", "-p", "acme-billing", "acme-b-logs/2026/11/*.log")
	if !errors.Is(err, errAborted) || !strings.Contains(stderr, "Download 2 objects (1.5 KB) matching s3://acme-b-logs/2026/11/*.log to .? [Y/n]") {
		t.Fatalf("%v\n%s", err, stderr)
	}
	if srv.Count("GetObject") != 0 {
		t.Errorf("downloaded after a no: %v", srv.Calls())
	}
	stdin(t, "y\n")
	if _, stderr, err = runHop(t, "s3", "pull", "-p", "acme-billing", "acme-b-logs/2026/11/*.log"); err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "*.log")); len(m) != 2 {
		t.Errorf("files: %v", m)
	}
}

func TestS3PushSeveralFiles(t *testing.T) {
	srv := s3Env(t)
	dir := t.TempDir()
	for name, content := range map[string]string{"a.txt": "aaa", "b.txt": "bbbb", "c.log": "c"} {
		os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600)
	}
	// A directory that fits the pattern is not a file to send.
	os.Mkdir(filepath.Join(dir, "sub.txt"), 0o700)
	a, b := filepath.Join(dir, "a.txt"), filepath.Join(dir, "b.txt")

	// Files given one by one, as a shell expands a pattern.
	_, stderr, err := runHop(t, "s3", "push", "-p", "acme-billing", a, b, "acme-b-backups/in/")
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	if srv.Get("acme-b-backups", "in/a.txt") == nil || string(srv.Get("acme-b-backups", "in/b.txt").Data) != "bbbb" {
		t.Fatalf("objects: %v", srv.Keys("acme-b-backups"))
	}
	if !strings.Contains(stderr, "hop: 2 of 2 files sent to s3://acme-b-backups/in/\n") {
		t.Errorf("stderr:\n%s", stderr)
	}

	// A pattern the shell left alone: hop reads it, files only.
	if _, stderr, err = runHop(t, "s3", "push", "-p", "acme-billing", filepath.Join(dir, "*.txt"), "acme-b-backups/pat/"); err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	if got := strings.Join(srv.Keys("acme-b-backups"), " "); got != "in/a.txt in/b.txt pat/a.txt pat/b.txt" {
		t.Errorf("keys: %s", got)
	}
	// From the command for instances, with s3://, and to the top of a bucket.
	if _, stderr, err = runHop(t, "push", "-p", "acme-billing", a, b, "s3://acme-t-scratch"); err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	if got := strings.Join(srv.Keys("acme-t-scratch"), " "); got != "a.txt b.txt" {
		t.Errorf("keys: %s", got)
	}

	// Nobody to ask, and the objects are there: nothing replaced, and the count says so.
	puts := srv.Count("PutObject")
	_, _, err = runHop(t, "s3", "push", "-p", "acme-billing", a, b, "acme-b-backups/in/")
	if !errors.Is(err, bucket.ErrExists) || !strings.Contains(err.Error(), "(0 of 2 files sent)") || srv.Count("PutObject") != puts {
		t.Errorf("existing objects: %v", err)
	}

	for args, want := range map[string]string{
		a + " " + b + " acme-b-backups/in":                           "2 files need a destination that ends with /, where each keeps its name: acme-b-backups/in/",
		filepath.Join(dir, "*.zip") + " acme-b-backups/":             "no file matches",
		a + " " + filepath.Join(dir, "missing") + " acme-b-backups/": "file not found",
		a + " acme-b-backups/*.txt":                                  "holds a wildcard: a destination is one bucket and one key",
	} {
		_, _, err := runHop(t, append([]string{"s3", "push", "-p", "acme-billing"}, strings.Fields(args)...)...)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: error %v, want %q", args, err, want)
		}
	}
}

func TestS3PushSeveralFilesSkipsOnANo(t *testing.T) {
	srv := s3Env(t)
	terminal(t, true, true)
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.txt"), filepath.Join(dir, "b.txt")
	os.WriteFile(a, []byte("new a"), 0o600)
	os.WriteFile(b, []byte("new b"), 0o600)
	srv.Put("acme-b-backups", "in/a.txt", []byte("in place"), "")

	// A no to replacing one object is not a no to the next file.
	stdin(t, "n\n")
	_, stderr, err := runHop(t, "s3", "push", "-p", "acme-billing", a, b, "acme-b-backups/in/")
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	if string(srv.Get("acme-b-backups", "in/a.txt").Data) != "in place" || string(srv.Get("acme-b-backups", "in/b.txt").Data) != "new b" {
		t.Errorf("a.txt must stay, b.txt must go")
	}
	for _, want := range []string{"s3://acme-b-backups/in/a.txt exists", "hop: s3://acme-b-backups/in/a.txt left as it is\n", "hop: 1 of 2 files sent to s3://acme-b-backups/in/\n"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
}

func TestPushToAnInstanceReadsAPattern(t *testing.T) {
	fakeAWSEnv(t)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "app.conf"), []byte("listen 8080\n"), 0o600)

	// A pattern that names one file, left alone by the shell: that file.
	_, stderr, err := runHop(t, "push", filepath.Join(dir, "app*.conf"), "acme-b-app", "/root/app.conf")
	if err == nil || !strings.Contains(stderr, "hop: "+filepath.Join(dir, "app.conf")+" -> acme-b-app:/root/app.conf\n") {
		t.Errorf("one match: %v\n%s", err, stderr)
	}
	os.WriteFile(filepath.Join(dir, "app2.conf"), []byte("x"), 0o600)
	if _, _, err := runHop(t, "push", filepath.Join(dir, "app*.conf"), "acme-b-app", "/root/app.conf"); err == nil || !strings.Contains(err.Error(), "matches 2 files: hop push sends one file to an instance") {
		t.Errorf("two matches: %v", err)
	}
	if _, _, err := runHop(t, "push", filepath.Join(dir, "*.zip"), "acme-b-app", "/root/app.conf"); err == nil || !strings.Contains(err.Error(), "file not found") {
		t.Errorf("no match: %v", err)
	}
}
