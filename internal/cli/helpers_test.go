package cli

import (
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/morganseznec/hop/internal/awsconf"
	"github.com/morganseznec/hop/internal/config"
	"github.com/morganseznec/hop/internal/inventory"
	"github.com/morganseznec/hop/internal/state"
)

func TestMain(m *testing.M) {
	// Completion may start this binary again with "__refresh" to refresh the cache in
	// the background: in a test binary, that must never run the suite again.
	for _, a := range os.Args[1:] {
		if a == "__refresh" {
			os.Exit(0)
		}
	}
	// Helper process for tests that need a real exit code (see TestPluginExit).
	if code := os.Getenv("HOP_TEST_EXIT_CODE"); code != "" {
		n, _ := strconv.Atoi(code)
		os.Exit(n)
	}
	os.Exit(m.Run())
}

// isolate points every path hop reads (AWS files, config, cache, state, home) to a
// temporary folder, and every AWS endpoint to a closed local port, so that a test can
// neither read the machine's files nor reach AWS.
func isolate(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	for k, v := range map[string]string{
		"HOME":                        home,
		"USERPROFILE":                 home,
		"APPDATA":                     filepath.Join(home, "AppData", "Roaming"),
		"LOCALAPPDATA":                filepath.Join(home, "AppData", "Local"),
		"XDG_CONFIG_HOME":             filepath.Join(home, "config"),
		"XDG_CACHE_HOME":              filepath.Join(home, "cache"),
		"XDG_STATE_HOME":              filepath.Join(home, "state"),
		"AWS_CONFIG_FILE":             filepath.Join(home, "aws", "config"),
		"AWS_SHARED_CREDENTIALS_FILE": filepath.Join(home, "aws", "credentials"),
		"AWS_ENDPOINT_URL":            "http://127.0.0.1:1",
		"AWS_EC2_METADATA_DISABLED":   "true",
		"AWS_PROFILE":                 "",
		"AWS_REGION":                  "",
		"AWS_DEFAULT_REGION":          "",
		"AWS_ACCESS_KEY_ID":           "",
		"AWS_SECRET_ACCESS_KEY":       "",
		"AWS_SESSION_TOKEN":           "",
		"AWS_USE_FIPS_ENDPOINT":       "",
		"AWS_USE_DUALSTACK_ENDPOINT":  "",
	} {
		t.Setenv(k, v)
	}
	os.MkdirAll(filepath.Join(home, "aws"), 0o700)
	return home
}

// terminal sets the terminal flags and the color switch for the duration of a test.
func terminal(t *testing.T, tty, colorOff bool) {
	t.Helper()
	in, out, errTTY, nc := stdinTTY, stdoutTTY, stderrTTY, noColor
	stdinTTY, stdoutTTY, stderrTTY, noColor = tty, tty, tty, colorOff
	t.Cleanup(func() { stdinTTY, stdoutTTY, stderrTTY, noColor = in, out, errTTY, nc })
}

// capture returns what fn writes to *f (os.Stdout or os.Stderr).
func capture(t *testing.T, f **os.File, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := *f
	*f = w
	t.Cleanup(func() { *f = old })
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		r.Close()
		done <- string(b)
	}()
	fn()
	*f = old
	w.Close()
	return <-done
}

// stdin feeds input to os.Stdin for the duration of a test.
func stdin(t *testing.T, input string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		io.WriteString(w, input)
		w.Close()
	}()
	old := os.Stdin
	os.Stdin = r
	t.Cleanup(func() {
		os.Stdin = old
		r.Close()
	})
}

const fleetConfig = `[sso-session acme-sso]
sso_start_url = https://acme.awsapps.com/start
sso_region = eu-west-1

[profile acme-prod]
sso_session = acme-sso
sso_account_id = 111111111111
sso_role_name = ReadOnly
region = eu-west-1

[profile acme-data]
sso_session = acme-sso
sso_account_id = 222222222222
sso_role_name = ReadOnly
region = eu-west-1

[profile globex]
sso_start_url = https://globex.awsapps.com/start
sso_region = us-east-1
sso_account_id = 333333333333
sso_role_name = ReadOnly
region = us-east-1

[default]
region = eu-west-1
`

const fleetCredentials = `[acme-billing]
aws_access_key_id = AKIAFAKEFAKEFAKE0001
aws_secret_access_key = fictional-secret

[acme-data]
aws_access_key_id = AKIAFAKEFAKEFAKE0002
aws_secret_access_key = fictional-secret
`

func loadFiles(t *testing.T, cfg, creds string) *awsconf.Files {
	t.Helper()
	dir := t.TempDir()
	cp, kp := filepath.Join(dir, "config"), filepath.Join(dir, "credentials")
	os.WriteFile(cp, []byte(cfg), 0o600)
	os.WriteFile(kp, []byte(creds), 0o600)
	files, err := awsconf.LoadFrom(cp, kp)
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func instance(name, id, profile, region, state, env string) inventory.Instance {
	return inventory.Instance{ID: id, Name: name, Profile: profile, Region: region, State: state, Env: env, Agent: "Online"}
}

// fresh is a target refreshed just now, with session logging on.
func fresh(profile, region string, instances ...inventory.Instance) *inventory.Target {
	return &inventory.Target{
		Profile: profile, Region: region, FetchedAt: time.Now(), AgentsKnown: true,
		Transcript: inventory.Transcript{Checked: true, Enabled: true, LogGroup: "/acme/ssm/sessions"},
		Instances:  instances,
	}
}

// newTestApp builds an app from the fleet files and the given targets, without
// reading anything from the machine.
func newTestApp(t *testing.T, cfg *config.Config, f filters, targets ...*inventory.Target) *app {
	t.Helper()
	if cfg == nil {
		cfg = &config.Config{}
	}
	inv := &inventory.Inventory{Version: 1, Targets: map[string]*inventory.Target{}}
	for _, tg := range targets {
		inv.Targets[tg.Key()] = tg
	}
	return &app{files: loadFiles(t, fleetConfig, fleetCredentials), cfg: cfg, inv: inv, recents: state.Recents{}, f: f}
}

// fleetTargets covers every pair of the fleet, all fresh, so that nothing triggers a
// refresh.
func fleetTargets() []*inventory.Target {
	return []*inventory.Target{
		fresh("acme-prod", "eu-west-1",
			instance("acme-p-tools", "i-01", "acme-prod", "eu-west-1", "running", "prod"),
			instance("acme-p-proxy", "i-02", "acme-prod", "eu-west-1", "stopped", "prod")),
		fresh("acme-data", "eu-west-1",
			instance("acme-p-etl", "i-03", "acme-data", "eu-west-1", "running", "prod"),
			instance("tools", "i-04", "acme-data", "eu-west-1", "running", "")),
		fresh("globex", "us-east-1",
			instance("tools", "i-05", "globex", "us-east-1", "running", "test")),
		fresh("acme-billing", "eu-west-1"),
	}
}

// fetchedNow marks every pair in scope as queried during this call, so that resolve
// never refreshes.
func fetchedNow(a *app) {
	a.fetched = map[string]bool{}
	for _, k := range a.keys() {
		a.fetched[k] = true
	}
}
