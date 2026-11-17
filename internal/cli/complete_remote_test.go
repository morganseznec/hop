package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/morganseznec/hop/internal/config"
	"github.com/morganseznec/hop/internal/fakeagent"
	"github.com/morganseznec/hop/internal/inventory"
)

// tab is what the shell gets when Tab is pressed after "hop <args>": the completions,
// the hints shown under the command line, and cobra's directive (2: no space after the
// completion, 4: no local files).
func tab(t *testing.T, args ...string) (comps, hints []string, directive int) {
	t.Helper()
	stdout, stderr, err := runHop(t, append([]string{"__complete"}, args...)...)
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	directive, err = strconv.Atoi(strings.TrimPrefix(lines[len(lines)-1], ":"))
	if err != nil {
		t.Fatalf("no directive in %q", stdout)
	}
	for _, l := range lines[:len(lines)-1] {
		if hint, ok := strings.CutPrefix(l, "_activeHelp_ "); ok {
			hints = append(hints, hint)
		} else {
			comps = append(comps, l)
		}
	}
	return comps, hints, directive
}

// remoteHome fills a directory of the "instance": the fake agent runs the session's
// command on this machine.
func remoteHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"app.log", "app.conf", "notes of may.txt", ".bashrc"} {
		os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644)
	}
	os.Mkdir(filepath.Join(dir, "releases"), 0o755)
	os.Mkdir(filepath.Join(dir, ".ssh"), 0o700)
	os.WriteFile(filepath.Join(dir, "releases", "v2.tar.gz"), nil, 0o644)
	return dir + "/"
}

func TestRemoteCompletion(t *testing.T) {
	f := fakeAWSEnv(t)
	cacheBillingApp(t, 5*time.Minute)
	fakeagent.Install(t)
	home := remoteHome(t)
	sessions := func() int {
		f.mu.Lock()
		defer f.mu.Unlock()
		return len(f.sessions)
	}

	// Tab after the directory: what it holds on the instance, a directory with its
	// "/". Names that start with a dot wait for a dot.
	comps, hints, d := tab(t, "pull", "acme-b-app", home)
	want := []string{home + "app.conf", home + "app.log", home + "notes of may.txt", home + "releases/"}
	slices.Sort(comps)
	if !slices.Equal(comps, want) || len(hints) != 0 || d != 6 {
		t.Fatalf("pull %s<Tab>: %q, hints %q, directive %d", home, comps, hints, d)
	}
	f.mu.Lock()
	if len(f.sessions) != 1 || f.sessions[0]["DocumentName"] != "AWS-StartNonInteractiveCommand" || f.sessions[0]["Target"] != "i-0b01" {
		t.Errorf("StartSession requests: %v", f.sessions)
	}
	f.mu.Unlock()

	// A few letters more, Tab again: answered from the listing just made, without a
	// session. One file left: a space may follow it.
	comps, _, d = tab(t, "pull", "acme-b-app", home+"app.l")
	if !slices.Equal(comps, []string{home + "app.log"}) || d != 4 || sessions() != 1 {
		t.Errorf("app.l<Tab>: %q, directive %d, %d sessions", comps, d, sessions())
	}
	// One directory left: no space, the path goes on.
	comps, _, d = tab(t, "pull", "acme-b-app", home+"rel")
	if !slices.Equal(comps, []string{home + "releases/"}) || d != 6 || sessions() != 1 {
		t.Errorf("rel<Tab>: %q, directive %d, %d sessions", comps, d, sessions())
	}
	comps, _, _ = tab(t, "pull", "acme-b-app", home+".")
	slices.Sort(comps)
	if !slices.Equal(comps, []string{home + ".bashrc", home + ".ssh/"}) || sessions() != 1 {
		t.Errorf(".<Tab>: %q, %d sessions", comps, sessions())
	}
	// Nothing starts so: no completion, and no local file offered instead.
	comps, hints, d = tab(t, "pull", "acme-b-app", home+"zzz")
	if len(comps) != 0 || len(hints) != 0 || d != 6 || sessions() != 1 {
		t.Errorf("zzz<Tab>: %q, hints %q, directive %d", comps, hints, d)
	}

	// Another directory is another listing, and so is the same one for hop push.
	comps, _, _ = tab(t, "pull", "acme-b-app", home+"releases/")
	if !slices.Equal(comps, []string{home + "releases/v2.tar.gz"}) || sessions() != 2 {
		t.Errorf("releases/<Tab>: %q, %d sessions", comps, sessions())
	}
	comps, _, _ = tab(t, "push", "./local.conf", "acme-b-app", home+"app.c")
	if !slices.Equal(comps, []string{home + "app.conf"}) || sessions() != 2 {
		t.Errorf("push app.c<Tab>: %q, %d sessions", comps, sessions())
	}

	// The listings are kept next to the inventory, readable by their owner only.
	p, _ := listingsPath()
	if st, err := os.Stat(p); err != nil || st.Mode().Perm() != 0o600 || filepath.Dir(p) != filepath.Dir(mustPath(t)) {
		t.Errorf("%s: %v, %v", p, st, err)
	}

	// Half a minute later, the directory may hold something else: listed again.
	age := func(by time.Duration) {
		b, _ := os.ReadFile(p)
		var file listingsFile
		if err := json.Unmarshal(b, &file); err != nil {
			t.Fatal(err)
		}
		for i := range file.Listings {
			file.Listings[i].At = file.Listings[i].At.Add(-by)
		}
		b, _ = json.Marshal(file)
		os.WriteFile(p, b, 0o600)
	}
	age(remoteFresh + time.Second)
	os.WriteFile(filepath.Join(home, "app.new"), nil, 0o644)
	comps, _, _ = tab(t, "pull", "acme-b-app", home+"app.")
	slices.Sort(comps)
	if !slices.Equal(comps, []string{home + "app.conf", home + "app.log", home + "app.new"}) || sessions() != 3 {
		t.Errorf("after %s: %q, %d sessions", remoteFresh, comps, sessions())
	}
	// A listing made for "app." does not know what else the directory holds.
	comps, _, _ = tab(t, "pull", "acme-b-app", home+"no")
	if !slices.Equal(comps, []string{home + "notes of may.txt"}) || sessions() != 4 {
		t.Errorf("no<Tab> after app.<Tab>: %q, %d sessions", comps, sessions())
	}
	// The names of the listings that are no longer recent leave the file.
	age(remoteFresh + time.Second)
	tab(t, "pull", "acme-b-app", home+"releases/")
	b, _ := os.ReadFile(p)
	if strings.Contains(string(b), "notes of may") || !strings.Contains(string(b), "v2.tar.gz") {
		t.Errorf("listings kept: %s", b)
	}
}

func mustPath(t *testing.T) string {
	t.Helper()
	p, err := inventory.Path()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRemoteCompletionNeedsASureInstance(t *testing.T) {
	f := fakeAWSEnv(t)
	cacheBillingApp(t, 5*time.Minute)
	fakeagent.Install(t)
	home := remoteHome(t)
	// With a terminal, where a command would ask: a completion never does.
	terminal(t, true, true)
	stdin(t, "")

	for _, c := range []struct {
		args []string
		hint string
	}{
		// The same name in two accounts.
		{[]string{"pull", "tools", home}, `"tools" does not name one instance for sure: type its full name to complete a path there`},
		// Part of a word of a name: a command would ask first.
		{[]string{"pull", "acme-b-ap", home}, `"acme-b-ap" does not name one instance for sure: type its full name to complete a path there`},
		{[]string{"pull", "acme-p-proxy", home}, "acme-p-proxy is not running: no path to complete"},
		{[]string{"pull", "nowhere", home}, `no instance named "nowhere": no path to complete`},
		// A typo is only a suggestion.
		{[]string{"pull", "acmebapp", home}, `no instance named "acmebapp": no path to complete`},
		{[]string{"push", "./f", "tools", home}, `"tools" does not name one instance for sure: type its full name to complete a path there`},
		// A path on an instance is absolute.
		{[]string{"pull", "acme-b-app", "var/log"}, "a path on the instance starts with /"},
		{[]string{"pull", "acme-b-app", "~/"}, "a path on the instance starts with /"},
		// The profile signs in through a portal whose token has expired: say so, and
		// do not offer to sign in.
		{[]string{"pull", "acme-p-tools", home}, "SSO token expired for acme-sso: hop login acme-sso"},
	} {
		comps, hints, d := tab(t, c.args...)
		if len(comps) != 0 || !slices.Equal(hints, []string{c.hint}) || d != 6 {
			t.Errorf("%v: %q, hints %q, directive %d", c.args, comps, hints, d)
		}
	}
	// Whole words of the name of one instance are as good as its name, as for a
	// command, and -p narrows the search to one account.
	comps, hints, _ := tab(t, "pull", "-p", "acme-billing", "b app", home+"rel")
	if !slices.Equal(comps, []string{home + "releases/"}) || len(hints) != 0 {
		t.Errorf("by words, with -p: %q, hints %q", comps, hints)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sessions) != 1 {
		t.Errorf("sessions opened: %v", f.sessions)
	}
}

func TestRemoteCompletionSaysWhatFailed(t *testing.T) {
	f := fakeAWSEnv(t)
	cacheBillingApp(t, 5*time.Minute)
	fakeagent.Install(t)
	home := remoteHome(t)

	comps, hints, d := tab(t, "pull", "acme-b-app", "/nonexistent/hop/")
	if len(comps) != 0 || !slices.Equal(hints, []string{"cannot list the directory on the instance: no such directory, or no permission to see it"}) || d != 6 {
		t.Errorf("missing directory: %q, hints %q, directive %d", comps, hints, d)
	}
	// A file is not a directory to list.
	_, hints, _ = tab(t, "pull", "acme-b-app", home+"app.log/")
	if len(hints) != 1 || !strings.Contains(hints[0], "no such directory") {
		t.Errorf("a file: %q", hints)
	}
	// A failure is not kept: the next Tab asks again.
	f.mu.Lock()
	n := len(f.sessions)
	f.mu.Unlock()
	tab(t, "pull", "acme-b-app", "/nonexistent/hop/")
	f.mu.Lock()
	if len(f.sessions) != n+1 {
		t.Errorf("%d sessions after a second Tab on a missing directory, %d before", len(f.sessions), n)
	}
	f.mu.Unlock()

	// An instance that does not answer: Tab gives up, and says so.
	saved := remoteTimeout
	remoteTimeout = 300 * time.Millisecond
	t.Cleanup(func() { remoteTimeout = saved })
	t.Setenv("HOP_FAKE_AGENT_WAIT", "1")
	start := time.Now()
	comps, hints, _ = tab(t, "pull", "acme-b-app", home+"releases/")
	if len(comps) != 0 || !slices.Equal(hints, []string{"acme-b-app did not list " + home + "releases/ within 300ms"}) {
		t.Errorf("no answer: %q, hints %q", comps, hints)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("Tab took %s", took)
	}
}

func TestRemoteCompletionSaysWhenThereIsMore(t *testing.T) {
	fakeAWSEnv(t)
	cacheBillingApp(t, 5*time.Minute)
	fakeagent.Install(t)
	dir := t.TempDir() + "/"
	for i := range 2003 {
		os.WriteFile(dir+"part-"+strconv.Itoa(10000+i), nil, 0o644)
	}
	os.WriteFile(dir+"zz-last", nil, 0o644)
	comps, hints, _ := tab(t, "pull", "acme-b-app", dir)
	if len(comps) != 2000 || !slices.Equal(hints, []string{"more than 2000 names in " + dir + ": type the start of one"}) {
		t.Fatalf("%d completions, hints %q", len(comps), hints)
	}
	// The listing was cut: it cannot answer for a name it may have left out.
	comps, hints, _ = tab(t, "pull", "acme-b-app", dir+"zz")
	if !slices.Equal(comps, []string{dir + "zz-last"}) || len(hints) != 0 {
		t.Errorf("zz<Tab>: %q, hints %q", comps, hints)
	}
}

func TestRemoteCompletionCanBeTurnedOff(t *testing.T) {
	f := fakeAWSEnv(t)
	cacheBillingApp(t, 5*time.Minute)
	fakeagent.Install(t)
	home := remoteHome(t)
	p, _ := config.Path()
	os.WriteFile(p, []byte("remote_completion: false\n"), 0o600)

	comps, hints, d := tab(t, "pull", "acme-b-app", home)
	if len(comps) != 0 || len(hints) != 0 || d != 6 {
		t.Errorf("turned off: %q, hints %q, directive %d", comps, hints, d)
	}
	// The "/" every path starts with costs nothing, and stays.
	if comps, _, _ := tab(t, "pull", "acme-b-app", ""); !slices.Equal(comps, []string{"/"}) {
		t.Errorf("first Tab: %q", comps)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sessions) != 0 {
		t.Errorf("sessions opened: %v", f.sessions)
	}
}

func TestListingAnswers(t *testing.T) {
	now := time.Now()
	l := dirListing{Instance: "acme|eu-west-1|i-01", Dir: "/var/log/", Prefix: "app", At: now.Add(-10 * time.Second)}
	for _, c := range []struct {
		name                  string
		instance, dir, prefix string
		more                  bool
		at                    time.Time
		want                  bool
	}{
		{"the same Tab again", l.Instance, l.Dir, "app", false, now, true},
		{"more letters", l.Instance, l.Dir, "app.l", false, now, true},
		{"fewer letters", l.Instance, l.Dir, "ap", false, now, false},
		{"other letters", l.Instance, l.Dir, "nginx", false, now, false},
		{"another directory", l.Instance, "/var/", "app", false, now, false},
		{"another instance", "acme|eu-west-1|i-02", l.Dir, "app", false, now, false},
		{"too old", l.Instance, l.Dir, "app", false, now.Add(remoteFresh), false},
		{"from the future", l.Instance, l.Dir, "app", false, now.Add(-time.Minute), false},
		{"cut, the same letters", l.Instance, l.Dir, "app", true, now, true},
		{"cut, more letters", l.Instance, l.Dir, "app.l", true, now, false},
	} {
		l.More = c.more
		if got := l.answers(c.instance, c.dir, c.prefix, c.at); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}
