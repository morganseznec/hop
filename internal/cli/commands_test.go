package cli

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/credentials/ssocreds"
	"github.com/spf13/cobra"

	"github.com/morganseznec/hop/internal/awsconf"
	"github.com/morganseznec/hop/internal/config"
	"github.com/morganseznec/hop/internal/inventory"
)

// fleetEnv writes the fleet to the isolated paths hop reads: AWS files, config with a
// label, and a cache where every pair is fresh, so that no command needs AWS.
func fleetEnv(t *testing.T) string {
	t.Helper()
	home := isolate(t)
	t.Setenv("PATH", t.TempDir())
	os.WriteFile(awsconf.ConfigPath(), []byte(fleetConfig), 0o600)
	os.WriteFile(awsconf.CredentialsPath(), []byte(fleetCredentials), 0o600)
	if err := (&config.Config{Labels: map[string]string{"acme-prod": "ACME"}}).Save(); err != nil {
		t.Fatal(err)
	}
	inv := &inventory.Inventory{Version: 1, Targets: map[string]*inventory.Target{}}
	for _, tg := range fleetTargets() {
		inv.Targets[tg.Key()] = tg
	}
	if err := inv.Save(); err != nil {
		t.Fatal(err)
	}
	return home
}

func validToken(t *testing.T, key string) {
	t.Helper()
	p, err := ssocreds.StandardCachedTokenFilepath(key)
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Dir(p), 0o700)
	os.WriteFile(p, []byte(`{"accessToken":"fictional","expiresAt":"`+time.Now().Add(time.Hour).UTC().Format(time.RFC3339)+`"}`), 0o600)
}

func TestCommandTree(t *testing.T) {
	root := newRoot()
	var names []string
	for _, c := range root.Commands() {
		names = append(names, c.Name())
		if c.Name() == "__refresh" && !c.Hidden {
			t.Error("__refresh must stay hidden")
		}
	}
	for _, want := range []string{"ls", "run", "tunnel", "push", "login", "doctor", "scan", "init", "__refresh"} {
		if !slices.Contains(names, want) {
			t.Errorf("command %s missing (have %v)", want, names)
		}
	}
	for _, flag := range []string{"profile", "region", "env", "refresh"} {
		if root.PersistentFlags().Lookup(flag) == nil {
			t.Errorf("flag --%s missing", flag)
		}
	}
}

func TestVersionFlag(t *testing.T) {
	root := newRoot()
	var out strings.Builder
	root.SetOut(&out)
	root.SetArgs([]string{"--version"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.String()) != "hop version "+buildVersion() {
		t.Errorf("%q", out.String())
	}
}

func TestVersionOf(t *testing.T) {
	cases := []struct {
		linked, module, want string
	}{
		{"1.4.0", "v1.4.0", "1.4.0"}, // a release build: the linked version
		{"1.4.0", "", "1.4.0"},
		{"dev", "v1.4.0", "1.4.0"}, // go install ...@v1.4.0 or @latest
		{"dev", "v0.0.0-20261030091500-abcdef123456", "0.0.0-20261030091500-abcdef123456"},
		{"dev", "(devel)", "dev"}, // go build in a clone
		{"dev", "", "dev"},
	}
	for _, c := range cases {
		if got := versionOf(c.linked, c.module); got != c.want {
			t.Errorf("versionOf(%q, %q) = %q, want %q", c.linked, c.module, got, c.want)
		}
	}
}

func TestEarlyErrors(t *testing.T) {
	isolate(t)
	terminal(t, false, false)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip(err)
	}
	defer l.Close()
	busy := strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
	dir := t.TempDir()

	cases := []struct {
		name string
		args []string
		want string
	}{
		{"shell without a terminal", []string{"acme-p-tools"}, "no terminal"},
		{"port zero", []string{"tunnel", "tools", "0"}, "invalid port: 0"},
		{"port too high", []string{"tunnel", "tools", "22", "70000"}, "invalid port: 70000"},
		{"port not a number", []string{"tunnel", "tools", "http"}, "invalid port: http"},
		{"explicit local port in use", []string{"tunnel", "tools", "22", busy}, "local port " + busy + " already in use (try "},
		{"push of a missing file", []string{"push", filepath.Join(dir, "missing"), "tools", "/opt/x"}, "file not found"},
		{"push of a directory", []string{"push", dir, "tools", "/opt/x"}, "file not found"},
		{"run without a command", []string{"run", "tools"}, "requires at least 2 arg(s)"},
		{"scan takes no argument", []string{"scan", "extra"}, "unknown command"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := runHop(t, c.args...)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %v, want %q", err, c.want)
			}
		})
	}
}

func TestUnknownProfile(t *testing.T) {
	fleetEnv(t)
	terminal(t, false, false)
	_, _, err := runHop(t, "ls", "-p", "acme-prd")
	if err == nil || err.Error() != "unknown profile: acme-prd (did you mean acme-prod?)" {
		t.Errorf("error %v", err)
	}
}

func TestLsFromCache(t *testing.T) {
	fleetEnv(t)
	terminal(t, false, false)

	out, stderr, err := runHop(t, "ls", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var rows []struct {
		ID    string `json:"id"`
		Label string `json:"label"`
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	var ids []string
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	if strings.Join(ids, ",") != "i-01,i-03,i-04,i-05" || rows[0].Label != "ACME" {
		t.Errorf("ls --json: %+v", rows)
	}
	if stderr != "" {
		t.Errorf("unexpected messages: %q", stderr)
	}

	// Words filter like the search, but keep the per-account order.
	out, _, err = runHop(t, "ls", "--all", "-e", "prod", "acme")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != 4 || !strings.Contains(lines[1], "acme-p-proxy") || !strings.Contains(lines[3], "acme-p-etl") {
		t.Errorf("ls --all -e prod acme:\n%s", out)
	}

	_, stderr, err = runHop(t, "ls", "zzzz")
	if err != nil || !strings.Contains(stderr, "hop: no instances") {
		t.Errorf("no match: %v %q", err, stderr)
	}
}

func TestLogin(t *testing.T) {
	fleetEnv(t)
	terminal(t, false, false)

	_, stderr, err := runHop(t, "login", "--status")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "acme-sso") || !strings.Contains(stderr, "globex.awsapps.com") || !strings.Contains(stderr, "missing") {
		t.Errorf("status:\n%s", stderr)
	}

	_, _, err = runHop(t, "login", "acme-billing")
	if err == nil || err.Error() != "not an SSO portal or profile: acme-billing (hop login --status lists them)" {
		t.Errorf("static-key profile: %v", err)
	}

	// No aws CLI on PATH: the command to run is given instead.
	_, _, err = runHop(t, "login", "acme-prod")
	if err == nil || !strings.Contains(err.Error(), "aws sso login --sso-session acme-sso") {
		t.Errorf("without the aws CLI: %v", err)
	}

	validToken(t, "acme-sso")
	validToken(t, "https://globex.awsapps.com/start")
	_, stderr, err = runHop(t, "login")
	if err != nil || !strings.Contains(stderr, "all SSO tokens are valid") {
		t.Errorf("valid tokens: %v\n%s", err, stderr)
	}
}

func TestDoctorCommand(t *testing.T) {
	fleetEnv(t)
	terminal(t, false, false)
	out, _, err := runHop(t, "doctor")
	if err != nil {
		t.Fatal(err)
	}
	// Most severe first.
	first := strings.SplitN(out, "\n", 2)[0]
	if !strings.HasPrefix(first, "✗ ") {
		t.Errorf("first finding %q is not critical\n%s", first, out)
	}
	if !strings.Contains(out, "profile acme-data defined twice") {
		t.Errorf("doctor output:\n%s", out)
	}
}

func TestRefreshCommandWithFreshCache(t *testing.T) {
	fleetEnv(t)
	terminal(t, false, false)
	before, _ := os.ReadFile(cachePath(t))
	if _, _, err := runHop(t, "__refresh"); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(cachePath(t))
	if string(before) != string(after) {
		t.Error("a fresh cache was rewritten")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(cachePath(t)), "refresh.lock")); !os.IsNotExist(err) {
		t.Errorf("lock left behind: %v", err)
	}
}

func TestCompletion(t *testing.T) {
	fleetEnv(t)
	cmd := &cobra.Command{}
	f := &filters{}
	names := func(list []string) []string {
		var out []string
		for _, s := range list {
			n, _, _ := strings.Cut(s, "\t")
			out = append(out, n)
		}
		return out
	}

	got, dir := completeQuery(f)(cmd, nil, "")
	// Running instances only, each name once.
	if want := []string{"acme-p-tools", "acme-p-etl", "tools"}; !reflect.DeepEqual(names(got), want) || dir != cobra.ShellCompDirectiveNoFileComp {
		t.Errorf("instances %q, want names %v", got, want)
	}
	if got[0] != "acme-p-tools\tacme-prod (ACME), eu-west-1 prod" {
		t.Errorf("description %q", got[0])
	}
	// No environment: no trailing space.
	if got[2] != "tools\tacme-data, eu-west-1" {
		t.Errorf("description without env %q", got[2])
	}
	if got, _ := completeQuery(f)(cmd, []string{"acme-p-tools"}, ""); got != nil {
		t.Errorf("a second argument is not an instance: %v", got)
	}

	// -p and -e are applied to completion as well.
	scoped := &filters{profiles: []string{"globex"}, env: "test"}
	if got, _ := completeQuery(scoped)(cmd, nil, ""); !reflect.DeepEqual(names(got), []string{"tools"}) {
		t.Errorf("scoped: %q", got)
	}

	if got, _ := completeNth(f, 1)(cmd, []string{"local.txt"}, ""); len(got) != 3 {
		t.Errorf("position 1: %q", got)
	}
	if got, _ := completeNth(f, 1)(cmd, nil, ""); got != nil {
		t.Errorf("position 0: %q", got)
	}

	push := newPush(f).ValidArgsFunction
	if got, d := push(cmd, nil, ""); got != nil || d != cobra.ShellCompDirectiveDefault {
		t.Errorf("push, local file: %q %v", got, d)
	}
	if got, _ := push(cmd, []string{"local.txt"}, ""); len(got) != 3 {
		t.Errorf("push, instance: %q", got)
	}
	if got, d := push(cmd, []string{"local.txt", "tools"}, ""); got != nil || d != cobra.ShellCompDirectiveNoFileComp {
		t.Errorf("push, remote path: %q %v", got, d)
	}

	profiles, _ := completeProfiles(cmd, nil, "")
	if want := []string{"acme-prod", "acme-data", "globex", "acme-billing"}; !reflect.DeepEqual(names(profiles), want) {
		t.Errorf("profiles %q", profiles)
	}
	if profiles[2] != "globex\tSSO (legacy), us-east-1" {
		t.Errorf("profile description %q", profiles[2])
	}
	// No region: no trailing ", ".
	if profiles[3] != "acme-billing\tstatic keys" {
		t.Errorf("profile description without region %q", profiles[3])
	}

	portals, _ := newLogin(f).ValidArgsFunction(cmd, nil, "")
	if !reflect.DeepEqual(portals, []string{"acme-sso\tmissing", "globex.awsapps.com\tmissing"}) {
		t.Errorf("portals %q", portals)
	}
}

func TestCompletionWithBrokenConfig(t *testing.T) {
	fleetEnv(t)
	os.WriteFile(configPath(t), []byte("transcript: always\n"), 0o600)
	if got, _ := completeQuery(&filters{})(&cobra.Command{}, nil, ""); got != nil {
		t.Errorf("an invalid config must give no completion, not a crash: %v", got)
	}
	os.Remove(awsconf.ConfigPath())
	os.MkdirAll(awsconf.ConfigPath(), 0o700)
	if got, _ := completeProfiles(&cobra.Command{}, nil, ""); got != nil {
		t.Errorf("unreadable AWS config: %v", got)
	}
}

func TestCompleteRegions(t *testing.T) {
	got, dir := completeRegions(nil, nil, "")
	if dir != cobra.ShellCompDirectiveNoFileComp || len(got) < 20 {
		t.Fatalf("%d regions, directive %v", len(got), dir)
	}
	seen := map[string]bool{}
	for _, r := range got {
		parts := strings.Split(r, "-")
		if len(parts) != 3 || len(parts[0]) != 2 || parts[1] == "" {
			t.Errorf("not a region name: %q", r)
		}
		if _, err := strconv.Atoi(parts[2]); err != nil {
			t.Errorf("not a region name: %q", r)
		}
		if seen[r] {
			t.Errorf("duplicate region %q", r)
		}
		seen[r] = true
	}
	for _, must := range []string{"eu-west-1", "us-east-1", "af-south-1"} {
		if !seen[must] {
			t.Errorf("%s missing", must)
		}
	}
}

func TestRunAndPushRefuseWindows(t *testing.T) {
	fleetEnv(t)
	terminal(t, false, false)
	inv := inventory.Load()
	win := instance("acme-b-win", "i-0b09", "acme-billing", "eu-west-1", "running", "prod")
	win.Platform = "Windows"
	inv.Targets[inventory.TargetKey("acme-billing", "eu-west-1")].Instances = []inventory.Instance{win}
	if err := inv.Save(); err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(t.TempDir(), "app.conf")
	os.WriteFile(local, []byte("x"), 0o600)
	// Refused before any AWS call: the endpoint is closed here.
	for _, args := range [][]string{{"run", "acme-b-win", "dir"}, {"push", local, "acme-b-win", "/c/app.conf"}} {
		_, _, err := runHop(t, args...)
		want := "acme-b-win runs Windows: hop run and hop push support Linux and macOS instances; use a shell session (hop acme-b-win)"
		if err == nil || err.Error() != want {
			t.Errorf("%s: %v", args[0], err)
		}
	}
}

func TestPosixOnly(t *testing.T) {
	cases := map[string]bool{
		"":                                 true,
		"Linux/UNIX":                       true,
		"Amazon Linux":                     true,
		"Ubuntu":                           true,
		"macOS":                            true,
		"Windows":                          false,
		"Windows with SQL Server Standard": false,
		"Microsoft Windows Server 2022 Datacenter": false,
	}
	for platform, ok := range cases {
		in := instance("x", "i-1", "acme-prod", "eu-west-1", "running", "")
		in.Platform = platform
		if err := posixOnly(in); (err == nil) != ok {
			t.Errorf("%q: %v", platform, err)
		}
	}
}

func TestLocalPort(t *testing.T) {
	cases := []struct {
		name             string
		remote, explicit string
		busy             []string
		want, wantErr    string
		stderr           string
	}{
		// Below 1024, listening needs administrator rights: 8000 above.
		{"https", "443", "", nil, "8443", "", ""},
		{"http", "80", "", nil, "8080", "", ""},
		{"privileged default in use", "22", "", []string{"8022"}, "8023", "", ""},
		{"unprivileged default", "3000", "", nil, "3000", "", ""},
		{"default in use", "3000", "", []string{"3000"}, "3001", "", "hop: local port 3000 already in use, using 3001\n"},
		{"explicit", "443", "8443", nil, "8443", "", ""},
		{"explicit privileged", "443", "443", nil, "", "local port 443 cannot be opened: it may be reserved to administrators (below 1024) or in use (try 8443)", ""},
		{"explicit in use", "5432", "15432", []string{"15432"}, "", "local port 15432 already in use (try 15433)", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			terminal(t, false, false)
			free := func(p string) bool {
				n, _ := strconv.Atoi(p)
				return n >= 1024 && !slices.Contains(c.busy, p)
			}
			var got string
			var err error
			stderr := capture(t, &os.Stderr, func() { got, err = localPort(c.remote, c.explicit, free) })
			if c.wantErr != "" {
				if err == nil || err.Error() != c.wantErr {
					t.Errorf("error %v, want %q", err, c.wantErr)
				}
				return
			}
			if err != nil || got != c.want || stderr != c.stderr {
				t.Errorf("got %q, %v, stderr %q; want %q, stderr %q", got, err, stderr, c.want, c.stderr)
			}
		})
	}

	// Nothing free: no empty "(try )".
	none := func(string) bool { return false }
	if _, err := localPort("3000", "3000", none); err == nil || err.Error() != "local port 3000 already in use" {
		t.Errorf("explicit, nothing free: %v", err)
	}
	if _, err := localPort("443", "", none); err == nil || err.Error() != "no free local port found for 443: give one as the third argument" {
		t.Errorf("default, nothing free: %v", err)
	}
}

func TestLsJSONReportsStalePairs(t *testing.T) {
	fleetEnv(t)
	terminal(t, false, false)
	// acme-prod failed a few seconds ago (no SSO token): it is not queried again, and
	// its last known instances are still listed.
	inv := inventory.Load()
	prod := inv.Targets[inventory.TargetKey("acme-prod", "eu-west-1")]
	prod.FetchedAt = time.Now().Add(-26 * time.Hour)
	prod.Err, prod.ErrKind, prod.ErrAt = "failed to refresh cached credentials, cached SSO token is expired", inventory.ErrSSOExpired, time.Now()
	rewriteCache(t, inv)

	out, stderr, err := runHop(t, "ls", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "SSO token expired for acme-sso: acme-prod not queried") || !strings.Contains(stderr, "hop login acme-sso") {
		t.Errorf("the failing pair must be reported on stderr:\n%s", stderr)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	byID := map[string]map[string]any{}
	for _, r := range rows {
		byID[r["id"].(string)] = r
	}
	tools, etl := byID["i-01"], byID["i-03"]
	if tools == nil || etl == nil {
		t.Fatalf("rows %v", rows)
	}
	if tools["stale"] != true || !strings.HasPrefix(fmt.Sprint(tools["error"]), "SSO token expired: failed to refresh cached credentials") {
		t.Errorf("row of the failing pair: %v", tools)
	}
	at, err := time.Parse(time.RFC3339, fmt.Sprint(tools["fetched_at"]))
	if err != nil || time.Since(at) < 25*time.Hour || !strings.HasSuffix(fmt.Sprint(tools["fetched_at"]), "Z") {
		t.Errorf("fetched_at %v: %v", tools["fetched_at"], err)
	}
	if _, ok := etl["error"]; ok || etl["stale"] != false {
		t.Errorf("row of a fresh pair: %v", etl)
	}
	if at, err := time.Parse(time.RFC3339, fmt.Sprint(etl["fetched_at"])); err != nil || time.Since(at) > time.Minute {
		t.Errorf("fetched_at %v: %v", etl["fetched_at"], err)
	}
	// The existing fields keep their names.
	for _, k := range []string{"id", "name", "profile", "region", "state", "env", "agent", "transcript"} {
		if _, ok := tools[k]; !ok {
			t.Errorf("field %s missing: %v", k, tools)
		}
	}
}

func TestEnvSynonyms(t *testing.T) {
	fleetEnv(t)
	terminal(t, false, false)
	for _, env := range []string{"test", "staging", "DEV", "qa", "uat"} {
		out, _, err := runHop(t, "ls", "--json", "-e", env)
		if err != nil {
			t.Fatalf("-e %s: %v", env, err)
		}
		var rows []struct {
			ID string `json:"id"`
		}
		json.Unmarshal([]byte(out), &rows)
		if len(rows) != 1 || rows[0].ID != "i-05" {
			t.Errorf("-e %s: %s", env, out)
		}
	}
	out, _, err := runHop(t, "ls", "--json", "-e", "production")
	if err != nil || !strings.Contains(out, `"i-01"`) || strings.Contains(out, `"i-05"`) {
		t.Errorf("-e production: %v\n%s", err, out)
	}

	_, _, err = runHop(t, "ls", "-e", "sandbox")
	if err == nil {
		t.Fatal("-e sandbox: want an error")
	}
	for _, want := range []string{`unknown environment "sandbox"`, "use prod (or p, prd, production) or test (or ", "staging", "dev", "uat"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestCheckRegion(t *testing.T) {
	valid := append([]string{"us-gov-west-1", "us-gov-east-1", "cn-north-1", "cn-northwest-1", "us-iso-east-1",
		"us-isob-east-1", "us-isof-south-1", "eu-isoe-west-1", "eusc-de-east-1"}, knownRegions...)
	for _, r := range valid {
		if err := checkRegion(r); err != nil {
			t.Errorf("%s: %v", r, err)
		}
	}
	cases := map[string]string{
		"eu-west-1a": "availability zone, its region is eu-west-1",
		"EU-WEST-1":  "lower case (eu-west-1)",
		"eu-west1":   "expected a region name",
		"ireland":    "expected a region name",
		"eu_west_1":  "expected a region name",
		"":           "expected a region name",
		"eu-west-1 ": "expected a region name",
	}
	for r, want := range cases {
		if err := checkRegion(r); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", r, err, want)
		}
	}

	// Refused before anything is read or queried.
	isolate(t)
	terminal(t, false, false)
	_, _, err := runHop(t, "ls", "-r", "eu-west-1,eu-west-3b")
	if err == nil || !strings.Contains(err.Error(), `invalid region "eu-west-3b"`) {
		t.Errorf("hop ls -r eu-west-3b: %v", err)
	}
}

func TestCompleteRecentRegions(t *testing.T) {
	got, _ := completeRegions(nil, nil, "")
	for _, r := range []string{"ap-east-1", "ap-east-2", "ap-south-2", "ap-southeast-3", "ap-southeast-4", "ap-southeast-5",
		"ap-southeast-6", "ap-southeast-7", "ca-west-1", "mx-central-1", "il-central-1", "me-central-1"} {
		if !slices.Contains(got, r) {
			t.Errorf("%s missing", r)
		}
	}
}

func TestFirstRun(t *testing.T) {
	cases := []struct {
		name          string
		config, creds string // empty: no file
	}{
		{"no AWS files", "", ""},
		{"a default with only a region", "[default]\nregion = eu-west-1\n", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			isolate(t)
			t.Setenv("PATH", t.TempDir()) // doctor runs the tools it finds
			terminal(t, false, false)
			if c.config != "" {
				os.WriteFile(awsconf.ConfigPath(), []byte(c.config), 0o600)
			}
			want := fmt.Sprintf(`no AWS profile with credentials in %s or %s: create one with "aws configure sso", then run hop ls`,
				awsconf.ConfigPath(), awsconf.CredentialsPath())
			for _, args := range [][]string{{"ls"}, {"ls", "--json"}, {"run", "tools", "uptime"}, {"ls", "-p", "acme-prod"}} {
				_, _, err := runHop(t, args...)
				if err == nil || err.Error() != want {
					t.Errorf("%v: %v\nwant %s", args, err, want)
				}
			}
			out, _, _ := runHop(t, "doctor")
			if !strings.Contains(out, "no AWS profile with credentials") || !strings.Contains(out, "aws configure sso") {
				t.Errorf("doctor:\n%s", out)
			}
		})
	}
}

func TestWithoutHome(t *testing.T) {
	isolate(t)
	terminal(t, false, false)
	for _, k := range []string{"HOME", "USERPROFILE", "home", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_STATE_HOME", "APPDATA", "LOCALAPPDATA"} {
		t.Setenv(k, "")
	}
	wd := t.TempDir()
	t.Chdir(wd)
	_, _, err := runHop(t, "ls")
	if err == nil || !strings.Contains(err.Error(), "home directory is not set") {
		t.Errorf("error %v", err)
	}
	if left, _ := os.ReadDir(wd); len(left) != 0 {
		t.Errorf("written relative to the working directory: %v", left)
	}
}
