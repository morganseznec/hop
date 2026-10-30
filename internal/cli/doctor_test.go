package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/credentials/ssocreds"

	"github.com/morganseznec/hop/internal/awsconf"
	"github.com/morganseznec/hop/internal/config"
	"github.com/morganseznec/hop/internal/inventory"
	"github.com/morganseznec/hop/internal/state"
)

const doctorConfig = `[sso-session acme-sso]
sso_start_url = https://acme.awsapps.com/start
sso_region = eu-west-1

[profile acme-prod]
sso_session = acme-sso
sso_account_id = 111111111111
region = eu-west-1

[profile acme-legacy]
sso_start_url = https://acme.awsapps.com/start
sso_region = eu-west-1
sso_account_id = 222222222222
region = eu-west-1

[profile acme-data]
sso_session = acme-sso
sso_account_id = 333333333333
region = eu-west-1
`

const doctorCredentials = `# aws_access_key_id = AKIAOLDOLDOLDOLD0000
[acme-data]
aws_access_key_id = AKIAFAKEFAKEFAKE0001
aws_secret_access_key = fictional-secret

[acme-billing]
aws_access_key_id = AKIAFAKEFAKEFAKE0002
aws_secret_access_key = fictional-secret

[acme-temp]
aws_access_key_id = ASIAFAKEFAKEFAKE0003
aws_secret_access_key = fictional-secret
aws_session_token = fictional-token
`

// doctorApp builds an app for diagnose: no tool on PATH, a temporary home, files with
// a known set of problems.
func doctorApp(t *testing.T, zshrc string) *app {
	t.Helper()
	home := isolate(t)
	t.Setenv("PATH", t.TempDir())
	// The region of the profiles that declare none.
	t.Setenv("AWS_REGION", "eu-west-1")
	if zshrc != "" {
		os.WriteFile(filepath.Join(home, ".zshrc"), []byte(zshrc), 0o600)
	}
	// A valid token for the acme-sso portal, none for the legacy one.
	tok, err := ssocreds.StandardCachedTokenFilepath("acme-sso")
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Dir(tok), 0o700)
	os.WriteFile(tok, []byte(`{"accessToken":"fictional","expiresAt":"`+time.Now().Add(time.Hour).UTC().Format(time.RFC3339)+`"}`), 0o600)

	dir := t.TempDir()
	cfgPath, credsPath := filepath.Join(dir, "config"), filepath.Join(dir, "credentials")
	os.WriteFile(cfgPath, []byte(doctorConfig), 0o600)
	os.WriteFile(credsPath, []byte(doctorCredentials), 0o600)
	if runtime.GOOS != "windows" {
		// Readable by every user of the machine.
		os.Chmod(credsPath, 0o644)
	}
	files, err := awsconf.LoadFrom(cfgPath, credsPath)
	if err != nil {
		t.Fatal(err)
	}
	web := func(id string) inventory.Instance {
		return inventory.Instance{ID: id, Name: "web", Profile: "acme-prod", Region: "eu-west-1", State: "running"}
	}
	notLogged := &inventory.Target{Profile: "acme-prod", Region: "eu-west-1", FetchedAt: time.Now(), AgentsKnown: true,
		Transcript: inventory.Transcript{Checked: true}, Instances: []inventory.Instance{web("i-01"), web("i-02")}}
	rejected := &inventory.Target{Profile: "acme-billing", Region: "eu-west-1", Err: "fictional", ErrKind: inventory.ErrBadKeys}
	unchecked := &inventory.Target{Profile: "acme-data", Region: "eu-west-1", FetchedAt: time.Now()}
	logged := fresh("acme-temp", "eu-west-1")
	// An SSO profile refused in an opt-in region the account has not enabled, and a
	// throttled pair.
	optIn := &inventory.Target{Profile: "acme-prod", Region: "ap-east-1", Err: "fictional", ErrKind: inventory.ErrBadKeys}
	throttled := &inventory.Target{Profile: "acme-data", Region: "us-west-2", Err: "fictional", ErrKind: inventory.ErrThrottled}
	inv := &inventory.Inventory{Version: 1, Targets: map[string]*inventory.Target{}}
	for _, tg := range []*inventory.Target{notLogged, rejected, unchecked, logged, optIn, throttled} {
		inv.Targets[tg.Key()] = tg
	}
	cfg := &config.Config{ExtraRegions: map[string][]string{"acme-prod": {"ap-east-1"}, "acme-data": {"us-west-2"}}}
	return &app{files: files, cfg: cfg, inv: inv, recents: state.Recents{}}
}

func findingByTitle(fs []finding, prefix string) (finding, bool) {
	for _, f := range fs {
		if strings.HasPrefix(f.title, prefix) {
			return f, true
		}
	}
	return finding{}, false
}

func TestDiagnose(t *testing.T) {
	a := doctorApp(t, "autoload -Uz compinit && compinit\n")
	fs := a.diagnose()
	cases := []struct {
		title  string
		lvl    level
		detail string // a substring of the joined details, if any
		fix    string // a substring of the joined fixes, if any
	}{
		{"session-manager-plugin not found", lvlCrit, "", strings.Join(pluginFix(runtime.GOOS, false), "\n")},
		{"aws CLI not found", lvlInfo, "", ""},
		{"zsh completion not set up", lvlInfo, "", "hop init zsh"},
		{"commented-out keys left in credentials (1 line)", lvlWarn, "", ""},
		{"profile acme-data defined twice: SSO in config, keys in credentials", lvlWarn, "", "[acme-data]"},
		// Only the keys of the credentials file: not acme-prod, refused in ap-east-1.
		{"keys rejected by AWS during the last inventory", lvlWarn, "acme-billing", "delete these sections from credentials"},
		{"temporary keys pasted from the SSO portal: acme-temp", lvlWarn, "", ""},
		{"long-lived plaintext keys in 1 profile", lvlWarn, "acme-billing", ""},
		{"no region set on 2 profiles: using eu-west-1", lvlInfo, "acme-billing, acme-temp", "hop scan"},
		{"SSO portal acme-sso: token valid until", lvlOK, "", ""},
		{"SSO portal acme.awsapps.com: token missing (1 profile)", lvlInfo, "", "hop login acme.awsapps.com"},
		{"SSO portal acme.awsapps.com is declared in two formats", lvlWarn, "legacy format: acme-legacy", "sso_session = acme-sso"},
		{"sessions NOT logged on these accounts", lvlCrit, "acme-prod eu-west-1 (2 instances)", ""},
		{"session logging not checked for 5 profile/region pairs", lvlInfo,
			"acme-legacy eu-west-1, acme-data eu-west-1\n" +
				"keys rejected by AWS (revoked or expired): acme-prod ap-east-1, acme-billing eu-west-1\n" +
				"too many requests, throttled by AWS: acme-data us-west-2", "hop ls --refresh"},
		{"duplicate instance names", lvlInfo, "web: 2 instances in acme-prod", ""},
		{"running instances without the SSM agent", lvlInfo, "web (acme-prod), web (acme-prod)", ""},
	}
	if runtime.GOOS != "windows" {
		cases = append(cases, struct {
			title  string
			lvl    level
			detail string
			fix    string
		}{a.files.CredentialsPath + " is readable by other users (644)", lvlCrit, "", "chmod 600"})
	}
	for _, c := range cases {
		f, ok := findingByTitle(fs, c.title)
		if !ok {
			t.Errorf("missing finding %q", c.title)
			continue
		}
		if f.lvl != c.lvl {
			t.Errorf("%q: level %d, want %d", c.title, f.lvl, c.lvl)
		}
		if d := strings.Join(f.detail, "\n"); !strings.Contains(d, c.detail) {
			t.Errorf("%q: details %q do not mention %q", c.title, d, c.detail)
		}
		if x := strings.Join(f.fix, "\n"); !strings.Contains(x, c.fix) {
			t.Errorf("%q: fixes %q do not mention %q", c.title, x, c.fix)
		}
	}
	if len(fs) != len(cases) {
		var titles []string
		for _, f := range fs {
			titles = append(titles, f.title)
		}
		t.Errorf("%d findings, want %d:\n%s", len(fs), len(cases), strings.Join(titles, "\n"))
	}
	if f, _ := findingByTitle(fs, "keys rejected by AWS"); strings.Join(f.detail, "|") != "acme-billing" {
		t.Errorf("refused keys: %q, want only acme-billing", f.detail)
	}
	// No SSO token expired among the failures: "hop login" would not help.
	if f, _ := findingByTitle(fs, "session logging not checked"); strings.Contains(strings.Join(f.fix, "|"), "hop login") {
		t.Errorf("fix %q", f.fix)
	}
}

func TestDiagnoseRefusedKeys(t *testing.T) {
	a := doctorApp(t, "")
	// Every profile refused in its region: only the keys read from the credentials
	// file are listed. acme-data is defined twice and goes through SSO.
	cfgPath := filepath.Join(t.TempDir(), "config")
	os.WriteFile(cfgPath, []byte(doctorConfig+`
[profile acme-config-keys]
aws_access_key_id = AKIAFAKEFAKEFAKE0004
aws_secret_access_key = fictional-secret
`), 0o600)
	files, err := awsconf.LoadFrom(cfgPath, a.files.CredentialsPath)
	if err != nil {
		t.Fatal(err)
	}
	a.files = files
	a.cfg = &config.Config{}
	a.inv = &inventory.Inventory{Version: 1, Targets: map[string]*inventory.Target{}}
	for _, p := range files.Profiles {
		tg := &inventory.Target{Profile: p.Name, Region: "eu-west-1", Err: "fictional", ErrKind: inventory.ErrBadKeys}
		a.inv.Targets[tg.Key()] = tg
	}
	f, ok := findingByTitle(a.diagnose(), "keys rejected by AWS")
	if !ok {
		t.Fatal("no refused keys reported")
	}
	if got := strings.Join(f.detail, "|"); got != "acme-billing, acme-temp" {
		t.Errorf("refused keys %q, want acme-billing, acme-temp", got)
	}
}

func TestDiagnoseNotCheckedFix(t *testing.T) {
	a := doctorApp(t, "")
	// An expired SSO token among the failures: sign in first.
	tg := &inventory.Target{Profile: "acme-legacy", Region: "eu-west-1", Err: "fictional", ErrKind: inventory.ErrSSOExpired}
	a.inv.Targets[tg.Key()] = tg
	f, ok := findingByTitle(a.diagnose(), "session logging not checked for 5 profile/region pairs")
	if !ok {
		t.Fatal("finding missing")
	}
	if d := strings.Join(f.detail, "\n"); !strings.Contains(d, "SSO token expired: acme-legacy eu-west-1") {
		t.Errorf("details %q", d)
	}
	if strings.Join(f.fix, "|") != "hop login, then hop ls --refresh" {
		t.Errorf("fix %q", f.fix)
	}

	// Many pairs with one cause: wrapped, continuation lines indented.
	a.inv.Targets = map[string]*inventory.Target{}
	a.cfg = &config.Config{ExtraRegions: map[string][]string{"acme-prod": {"us-east-1", "us-east-2", "us-west-1", "us-west-2", "ca-central-1"}}}
	for _, k := range a.keys() {
		profile, region, _ := strings.Cut(k, "|")
		tg := &inventory.Target{Profile: profile, Region: region, Err: "fictional", ErrKind: inventory.ErrNetwork}
		a.inv.Targets[tg.Key()] = tg
	}
	f, _ = findingByTitle(a.diagnose(), "session logging not checked")
	if len(f.detail) < 2 || !strings.HasPrefix(f.detail[0], "network error or timeout: ") || !strings.HasPrefix(f.detail[1], "  ") {
		t.Errorf("details %q", f.detail)
	}
}

func TestPluginFix(t *testing.T) {
	cases := []struct {
		goos    string
		upgrade bool
		want    string
	}{
		{"darwin", false, "brew install --cask session-manager-plugin"},
		{"darwin", true, "brew upgrade --cask session-manager-plugin"},
		{"linux", false, "install session-manager-plugin: https://docs.aws.amazon.com/systems-manager/latest/userguide/session-manager-working-with-install-plugin.html"},
		{"windows", true, "update session-manager-plugin: https://docs.aws.amazon.com/systems-manager/latest/userguide/session-manager-working-with-install-plugin.html"},
	}
	for _, c := range cases {
		if got := strings.Join(pluginFix(c.goos, c.upgrade), "|"); got != c.want {
			t.Errorf("%s, upgrade %v: %q, want %q", c.goos, c.upgrade, got, c.want)
		}
	}
}

func TestDiagnoseZshrc(t *testing.T) {
	for _, c := range []struct {
		zshrc string
		want  bool
	}{
		{"eval \"$(hop init zsh)\"\n", false},
		{"eval \"$(hop init zsh --alias ssm)\"\n", false},
		{"", false}, // no .zshrc: zsh is probably not the shell
		{"export EDITOR=vi\n", true},
	} {
		a := doctorApp(t, c.zshrc)
		if _, got := findingByTitle(a.diagnose(), "zsh completion not set up"); got != c.want {
			t.Errorf("%q: finding %v, want %v", c.zshrc, got, c.want)
		}
	}
}

func TestDiagnosePluginVersion(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake plugin is a shell script")
	}
	for _, c := range []struct {
		version string
		title   string
		lvl     level
	}{
		{"1.2.463.0", "session-manager-plugin 1.2.463.0 is too old", lvlWarn},
		{"1.2.764.0", "session-manager-plugin 1.2.764.0", lvlOK},
	} {
		a := doctorApp(t, "")
		bin := t.TempDir()
		os.WriteFile(filepath.Join(bin, "session-manager-plugin"), []byte("#!/bin/sh\necho "+c.version+"\n"), 0o755)
		t.Setenv("PATH", bin)
		f, ok := findingByTitle(a.diagnose(), c.title)
		if !ok || f.lvl != c.lvl {
			t.Errorf("%s: %+v, found %v", c.version, f, ok)
		}
	}
}

func TestDiagnoseShadowedHop(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake hop is a shell script")
	}
	a := doctorApp(t, "")
	bin := t.TempDir()
	os.WriteFile(filepath.Join(bin, "hop"), []byte("#!/bin/sh\n"), 0o755)
	t.Setenv("PATH", bin)
	shadow, err := filepath.EvalSymlinks(filepath.Join(bin, "hop"))
	if err != nil {
		t.Fatal(err)
	}
	f, ok := findingByTitle(a.diagnose(), "another hop earlier on PATH shadows this one")
	if !ok || f.lvl != lvlWarn || !strings.HasSuffix(f.title, ": "+shadow) {
		t.Errorf("%+v, found %v", f, ok)
	}
}

func TestPrintFinding(t *testing.T) {
	terminal(t, false, false)
	out := capture(t, &os.Stdout, func() {
		printFinding(finding{lvl: lvlCrit, title: "sessions NOT logged", detail: []string{"acme-prod eu-west-1"}, fix: []string{"configure it"}})
		printFinding(finding{lvl: lvlOK, title: "all good"})
	})
	want := "✗ sessions NOT logged\n    acme-prod eu-west-1\n    → configure it\n✓ all good\n"
	if out != want {
		t.Errorf("got %q, want %q", out, want)
	}
}

func TestWrap(t *testing.T) {
	cases := []struct {
		items []string
		n     int
		want  []string
	}{
		{[]string{"a", "b", "c", "d", "e"}, 2, []string{"a, b", "c, d", "e"}},
		{[]string{"a", "b"}, 6, []string{"a, b"}},
		{nil, 3, nil},
	}
	for _, c := range cases {
		if got := wrap(c.items, c.n); strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("wrap(%v, %d) = %q", c.items, c.n, got)
		}
	}
}
