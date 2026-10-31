package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/morganseznec/hop/internal/awsconf"
	"github.com/morganseznec/hop/internal/config"
	"github.com/morganseznec/hop/internal/inventory"
	"github.com/morganseznec/hop/internal/state"
)

// fakeAWS plays EC2 and SSM for the static-key profile acme-billing. It listens on the
// loopback interface only. The SSO profiles of the fleet have no token in the
// temporary home, so they fail before any call.
type fakeAWS struct {
	mu       sync.Mutex
	commands []string
	// calls lists the EC2 and SSM operations, with the instance asked for when the call
	// is narrowed to one ("DescribeInstances i-0b01").
	calls []string
	// appState and appName replace those of acme-b-app (i-0b01) in eu-west-1.
	appState, appName string
	// lookupDenied refuses DescribeInstances narrowed to one instance.
	lookupDenied bool
	// sessions are the StartSession requests.
	sessions []map[string]any
}

// inventoryCalls returns the calls made to list or look up instances, sorted: what
// SendCommand needs is left out.
func (f *fakeAWS) inventoryCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if !strings.HasPrefix(c, "SendCommand") && !strings.HasPrefix(c, "GetCommandInvocation") {
			out = append(out, c)
		}
	}
	sort.Strings(out)
	return out
}

func (f *fakeAWS) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

var credentialRegion = regexp.MustCompile(`Credential=[^/]+/\d{8}/([^/]+)/`)

func (f *fakeAWS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	region := ""
	if m := credentialRegion.FindStringSubmatch(r.Header.Get("Authorization")); m != nil {
		region = m[1]
	}
	if target := r.Header.Get("X-Amz-Target"); target != "" {
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		var req map[string]any
		json.Unmarshal(body, &req)
		call := strings.TrimPrefix(target, "AmazonSSM.")
		if filters, ok := req["Filters"].([]any); ok && len(filters) > 0 {
			ids, _ := filters[0].(map[string]any)["Values"].([]any)
			call += " " + fmt.Sprint(ids...)
		}
		f.record(call)
		switch strings.TrimPrefix(target, "AmazonSSM.") {
		case "DescribeInstanceInformation":
			io.WriteString(w, `{"InstanceInformationList":[{"InstanceId":"i-0b01","PingStatus":"Online","PlatformName":"Amazon Linux"}]}`)
		case "GetDocument":
			io.WriteString(w, `{"Content":"{\"inputs\":{\"cloudWatchLogGroupName\":\"/acme/ssm/sessions\"}}"}`)
		case "SendCommand":
			params, _ := req["Parameters"].(map[string]any)
			cmds, _ := params["commands"].([]any)
			cmd, _ := cmds[0].(string)
			f.mu.Lock()
			f.commands = append(f.commands, cmd)
			f.mu.Unlock()
			io.WriteString(w, `{"Command":{"CommandId":"`+url.QueryEscape(cmd)+`"}}`)
		case "StartSession":
			f.mu.Lock()
			f.sessions = append(f.sessions, req)
			f.mu.Unlock()
			io.WriteString(w, `{"SessionId":"acme-user-0abc","TokenValue":"fictional-session-token","StreamUrl":"wss://ssmmessages.eu-west-1.amazonaws.com/v1/data-channel/acme-user-0abc"}`)
		case "GetCommandInvocation":
			id, _ := url.QueryUnescape(req["CommandId"].(string))
			out := map[string]any{"Status": "Success", "ResponseCode": 0, "StandardOutputContent": "up 1 day\n"}
			switch id {
			case "exit 3":
				out = map[string]any{"Status": "Failed", "ResponseCode": 3, "StandardErrorContent": "failed\n"}
			case "set -e": // a push script
				out = map[string]any{"Status": "Failed", "ResponseCode": 1, "StandardErrorContent": "sh: /root/app.conf: Permission denied\n"}
			}
			b, _ := json.Marshal(out)
			w.Write(b)
		default:
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"__type":"InvalidAction"}`)
		}
		return
	}
	form, _ := url.ParseQuery(string(body))
	w.Header().Set("Content-Type", "text/xml")
	const ns = ` xmlns="http://ec2.amazonaws.com/doc/2016-11-15/"`
	call := form.Get("Action")
	if id := form.Get("InstanceId.1"); id != "" {
		call += " " + id
	}
	f.record(call)
	f.mu.Lock()
	appState, appName, lookupDenied := f.appState, f.appName, f.lookupDenied
	f.mu.Unlock()
	if appState == "" {
		appState = "running"
	}
	if appName == "" {
		appName = "acme-b-app"
	}
	if lookupDenied && form.Get("InstanceId.1") != "" {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `<Response><Errors><Error><Code>UnauthorizedOperation</Code><Message>denied</Message></Error></Errors></Response>`)
		return
	}
	switch form.Get("Action") {
	case "DescribeRegions":
		io.WriteString(w, `<DescribeRegionsResponse`+ns+`><regionInfo>`+
			`<item><regionName>eu-west-1</regionName></item>`+
			`<item><regionName>us-east-1</regionName></item>`+
			`<item><regionName>ap-south-1</regionName></item>`+
			`</regionInfo></DescribeRegionsResponse>`)
	case "DescribeInstances":
		items := ""
		switch region {
		case "eu-west-1":
			items = `<item><instanceId>i-0b01</instanceId><instanceState><code>16</code><name>` + appState + `</name></instanceState>` +
				`<privateIpAddress>10.1.0.1</privateIpAddress><tagSet><item><key>Name</key><value>` + appName + `</value></item></tagSet></item>`
		case "us-east-1":
			items = `<item><instanceId>i-0b02</instanceId><instanceState><code>80</code><name>stopped</name></instanceState>` +
				`<tagSet><item><key>Name</key><value>acme-b-legacy</value></item></tagSet></item>`
		case "ap-south-1":
			// A region the caller may not read (an SCP, for instance).
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `<Response><Errors><Error><Code>UnauthorizedOperation</Code><Message>denied</Message></Error></Errors></Response>`)
			return
		}
		io.WriteString(w, `<DescribeInstancesResponse`+ns+`><reservationSet><item><instancesSet>`+items+
			`</instancesSet></item></reservationSet></DescribeInstancesResponse>`)
	default:
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `<Response><Errors><Error><Code>InvalidAction</Code></Error></Errors></Response>`)
	}
}

// fakeAWSEnv sets up the fleet with the fake as every endpoint. The cache is a few
// minutes old: within the TTL, past the freshness window.
func fakeAWSEnv(t *testing.T) *fakeAWS {
	t.Helper()
	fleetEnv(t)
	terminal(t, false, false)
	f := &fakeAWS{}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	t.Setenv("AWS_ENDPOINT_URL", srv.URL)
	ageCache(t, 5*time.Minute)
	return f
}

// rewriteCache replaces the cache with inv as is. Save keeps the pairs saved since with
// more recent data (see inventory.Save): a test that ages the cache starts over.
func rewriteCache(t *testing.T, inv *inventory.Inventory) {
	t.Helper()
	os.Remove(cachePath(t))
	if err := inv.Save(); err != nil {
		t.Fatal(err)
	}
}

// ageCache dates every pair of the cache from d ago.
func ageCache(t *testing.T, d time.Duration) {
	t.Helper()
	inv := inventory.Load()
	for _, tg := range inv.Targets {
		tg.FetchedAt = time.Now().Add(-d)
	}
	rewriteCache(t, inv)
}

// cacheBillingApp adds acme-b-app (i-0b01, as the fake describes it) to the cache, its
// pair dated from age ago. The agent is cached as lost, to see the lookup refresh it.
func cacheBillingApp(t *testing.T, age time.Duration) {
	t.Helper()
	inv := inventory.Load()
	app := instance("acme-b-app", "i-0b01", "acme-billing", "eu-west-1", "running", "")
	app.PrivateIP, app.Agent = "10.1.0.1", "ConnectionLost"
	tg := inv.Targets[inventory.TargetKey("acme-billing", "eu-west-1")]
	tg.Instances, tg.FetchedAt = []inventory.Instance{app}, time.Now().Add(-age)
	rewriteCache(t, inv)
}

func TestLsRefresh(t *testing.T) {
	fakeAWSEnv(t)
	_, stderr, err := runHop(t, "ls", "--refresh")
	if err != nil {
		t.Fatal(err)
	}
	// The SSO portals have no token here: reported once per portal, with the fix.
	for _, want := range []string{
		"SSO token expired for acme-sso: acme-prod, acme-data not queried",
		"hop login acme-sso",
		"SSO token expired for globex.awsapps.com: globex not queried",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr does not mention %q:\n%s", want, stderr)
		}
	}
	inv := inventory.Load()
	billing := inv.Targets[inventory.TargetKey("acme-billing", "eu-west-1")]
	if billing == nil || billing.Err != "" || len(billing.Instances) != 1 || !billing.Transcript.Enabled {
		t.Fatalf("acme-billing not refreshed: %+v", billing)
	}
	// A failed refresh keeps the instances seen before.
	prod := inv.Targets[inventory.TargetKey("acme-prod", "eu-west-1")]
	if prod.ErrKind != inventory.ErrSSOExpired || len(prod.Instances) != 2 {
		t.Errorf("acme-prod after a failed refresh: %+v", prod)
	}
}

func TestRunRefreshesWhenTheCacheMissesTheInstance(t *testing.T) {
	fake := fakeAWSEnv(t)
	// acme-b-app is not in the cache yet: the search fails, the cache is refreshed and
	// the search runs again.
	out, stderr, err := runHop(t, "run", "acme-b-app", "uptime", "-p")
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	if out != "up 1 day\n" {
		t.Errorf("stdout %q", out)
	}
	if !strings.Contains(stderr, "hop: acme-b-app (i-0b01, acme-billing, eu-west-1)") {
		t.Errorf("the target must always be announced:\n%s", stderr)
	}
	fake.mu.Lock()
	cmds := append([]string(nil), fake.commands...)
	fake.mu.Unlock()
	// Flags after the instance name belong to the remote command.
	if len(cmds) != 1 || cmds[0] != "uptime -p" {
		t.Errorf("commands sent: %q", cmds)
	}
	if _, ok := state.LoadRecents()["acme-billing|eu-west-1|i-0b01"]; !ok {
		t.Error("connection not recorded in the recents")
	}
}

func TestRunExitCode(t *testing.T) {
	fakeAWSEnv(t)
	_, stderr, err := runHop(t, "run", "-p", "acme-billing", "--refresh", "acme-b-app", "exit", "3")
	var ex exitError
	if !errors.As(err, &ex) || ex.code != 3 {
		t.Errorf("want the remote exit code 3, got %v", err)
	}
	if !strings.Contains(stderr, "failed\n") {
		t.Errorf("remote stderr not forwarded: %q", stderr)
	}
}

func TestScan(t *testing.T) {
	fakeAWSEnv(t)
	before, _ := os.ReadFile(configPath(t))

	_, stderr, err := runHop(t, "scan", "-p", "acme-billing")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "acme-billing") || !strings.Contains(stderr, "acme-b-legacy") ||
		!strings.Contains(stderr, "extra_regions:\n  acme-billing: [us-east-1]") {
		t.Errorf("suggestion:\n%s", stderr)
	}
	if !strings.Contains(stderr, "acme-billing ap-south-1 not scanned") {
		t.Errorf("a region that could not be read must be reported, not taken as empty:\n%s", stderr)
	}
	if after, _ := os.ReadFile(configPath(t)); string(after) != string(before) {
		t.Error("scan without --write changed the config")
	}

	_, stderr, err = runHop(t, "scan", "-p", "acme-billing", "--write")
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(cfg.ExtraRegions["acme-billing"], ",") != "us-east-1" || cfg.Label("acme-prod") != "ACME" {
		t.Errorf("config after --write: %+v", cfg)
	}
	if bak, _ := os.ReadFile(configPath(t) + ".bak"); string(bak) != string(before) {
		t.Errorf("previous config not kept as .bak: %q", bak)
	}

	// A second --write does not add the region twice.
	if _, _, err := runHop(t, "scan", "-p", "acme-billing", "--write"); err != nil {
		t.Fatal(err)
	}
	cfg, _ = config.Load()
	if strings.Join(cfg.ExtraRegions["acme-billing"], ",") != "us-east-1" {
		t.Errorf("region added twice: %v", cfg.ExtraRegions)
	}
	// Once declared, the region is watched: nothing left to suggest.
	_, stderr, _ = runHop(t, "scan", "-p", "acme-billing")
	if !strings.Contains(stderr, "no instances outside the regions already watched") {
		t.Errorf("after --write:\n%s", stderr)
	}
}

func TestScanReportsUnreachableProfiles(t *testing.T) {
	fakeAWSEnv(t)
	_, stderr, err := runHop(t, "scan", "-p", "acme-prod")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "acme-prod not scanned: SSO token expired") {
		t.Errorf("stderr:\n%s", stderr)
	}
}

func TestRunRefreshesTheOtherPairsWhenOneFails(t *testing.T) {
	fakeAWSEnv(t)
	// acme-prod failed two minutes ago: this call queries it again, and it fails again
	// (no SSO token here), while the other pairs come from the cache. acme-b-app is in
	// none of them: the miss must still refresh the pairs that were not queried.
	inv := inventory.Load()
	prod := inv.Targets[inventory.TargetKey("acme-prod", "eu-west-1")]
	prod.Err, prod.ErrKind, prod.ErrAt = "fictional", inventory.ErrSSOExpired, time.Now().Add(-2*time.Minute)
	if err := inv.Save(); err != nil {
		t.Fatal(err)
	}
	out, stderr, err := runHop(t, "run", "acme-b-app", "uptime")
	if err != nil || out != "up 1 day\n" {
		t.Fatalf("%v %q\n%s", err, out, stderr)
	}
}

func TestRefreshCommandKeepsTheFilters(t *testing.T) {
	fakeAWSEnv(t)
	old := time.Now().Add(-time.Hour)
	inv := inventory.Load()
	for _, tg := range inv.Targets {
		tg.FetchedAt = old
	}
	rewriteCache(t, inv)
	if _, _, err := runHop(t, "__refresh", "--profile=acme-billing", "--region=us-east-1"); err != nil {
		t.Fatal(err)
	}
	inv = inventory.Load()
	legacy := inv.Targets[inventory.TargetKey("acme-billing", "us-east-1")]
	if legacy == nil || legacy.Err != "" || len(legacy.Instances) != 1 || legacy.Instances[0].Name != "acme-b-legacy" {
		t.Fatalf("the pair asked for was not refreshed: %+v", legacy)
	}
	for _, k := range []string{"acme-prod|eu-west-1", "acme-billing|eu-west-1", "globex|us-east-1"} {
		if tg := inv.Targets[k]; !tg.FetchedAt.Equal(old) || tg.Err != "" {
			t.Errorf("%s queried outside the filters: %+v", k, tg)
		}
	}
}

func TestPushShowsTheRemoteError(t *testing.T) {
	fakeAWSEnv(t)
	local := filepath.Join(t.TempDir(), "app.conf")
	os.WriteFile(local, []byte("listen 8080\n"), 0o600)
	_, stderr, err := runHop(t, "push", local, "acme-b-app", "/root/app.conf")
	if err == nil {
		t.Fatal("a failed copy must fail")
	}
	if !strings.Contains(stderr, "sh: /root/app.conf: Permission denied") {
		t.Errorf("remote error not shown:\n%s", stderr)
	}
}

func TestRunWithAFreshCacheQueriesNothing(t *testing.T) {
	fake := fakeAWSEnv(t)
	cacheBillingApp(t, 0)
	// An exact name, and whole words of the name: neither refreshes nor looks up
	// anything when the pair was queried less than a minute ago.
	for _, query := range []string{"acme-b-app", "b app"} {
		out, stderr, err := runHop(t, "run", query, "uptime")
		if err != nil || out != "up 1 day\n" {
			t.Fatalf("%s: %v %q\n%s", query, err, out, stderr)
		}
	}
	if calls := fake.inventoryCalls(); len(calls) != 0 {
		t.Errorf("calls made with a fresh cache: %q", calls)
	}
}

func TestRunLooksTheInstanceUpWhenTheCacheIsOld(t *testing.T) {
	fake := fakeAWSEnv(t)
	cacheBillingApp(t, 5*time.Minute)
	out, stderr, err := runHop(t, "run", "acme-b-app", "uptime")
	if err != nil || out != "up 1 day\n" {
		t.Fatalf("%v %q\n%s", err, out, stderr)
	}
	// That one instance, in its own account: no refresh of any pair.
	want := []string{"DescribeInstanceInformation i-0b01", "DescribeInstances i-0b01"}
	if calls := fake.inventoryCalls(); !reflect.DeepEqual(calls, want) {
		t.Errorf("calls %q, want %q", calls, want)
	}
	// The agent state comes from the lookup, not from the cache.
	if strings.Contains(stderr, "ConnectionLost") {
		t.Errorf("warned about the cached agent state:\n%s", stderr)
	}
}

func TestRunRefusesAnInstanceThatChanged(t *testing.T) {
	cases := []struct {
		name          string
		state, rename string
		warn, err     string
	}{
		{"stopped since", "stopped", "",
			"hop: acme-b-app (i-0b01, acme-billing, eu-west-1) is now stopped: searching again",
			"acme-b-app (acme-billing, eu-west-1) is stopped: start it first"},
		{"renamed since", "", "acme-b-web",
			"hop: acme-b-app (i-0b01, acme-billing, eu-west-1) is now named acme-b-web: searching again",
			`no instance matches "acme-b-app"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fake := fakeAWSEnv(t)
			cacheBillingApp(t, 5*time.Minute)
			fake.appState, fake.appName = c.state, c.rename
			_, stderr, err := runHop(t, "run", "acme-b-app", "uptime")
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("error %v, want %q", err, c.err)
			}
			if !strings.Contains(stderr, c.warn) {
				t.Errorf("what changed is not said:\n%s", stderr)
			}
			fake.mu.Lock()
			defer fake.mu.Unlock()
			if len(fake.commands) != 0 {
				t.Errorf("command sent to an instance that changed: %q", fake.commands)
			}
		})
	}
}

func TestReachWhenTheLookupFails(t *testing.T) {
	cases := []struct {
		name   string
		why    purpose
		tty    bool
		answer string
		want   string // empty: hop goes ahead
	}{
		// The session targets the ID: it fails cleanly if the instance is gone.
		{"shell", forShell, false, "", ""},
		{"tunnel", forTunnel, false, "", ""},
		{"run without a terminal", forRun, false, "",
			"acme-b-app (i-0b01, acme-billing, eu-west-1) could not be checked again (insufficient permissions; inventory from 5m ago): not acting on it without a terminal to confirm"},
		{"push without a terminal", forPush, false, "", "not acting on it without a terminal to confirm"},
		{"run, default answer", forRun, true, "\n", errAborted.Error()},
		{"run, confirmed", forRun, true, "y\n", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fake := fakeAWSEnv(t)
			cacheBillingApp(t, 5*time.Minute)
			fake.lookupDenied = true
			terminal(t, c.tty, true)
			if c.answer != "" {
				stdin(t, c.answer)
			}
			a, err := newApp(filters{})
			if err != nil {
				t.Fatal(err)
			}
			var in inventory.Instance
			prompt := capture(t, &os.Stderr, func() { in, _, err = a.reach(context.Background(), []string{"acme-b-app"}, c.why) })
			if c.want == "" {
				if err != nil || in.ID != "i-0b01" {
					t.Errorf("got %s, %v", in.ID, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %v, want %q", err, c.want)
			}
			if c.tty && !strings.Contains(prompt, "could not be checked again (insufficient permissions; inventory from 5m ago). Run the command there? [y/N]") {
				t.Errorf("prompt %q", prompt)
			}
		})
	}
}

func TestPushChecksBeforeResolving(t *testing.T) {
	fake := fakeAWSEnv(t)
	dir := t.TempDir()
	big := filepath.Join(dir, "big.bin")
	os.WriteFile(big, make([]byte, 400_001), 0o600)
	small := filepath.Join(dir, "app.conf")
	os.WriteFile(small, []byte("listen 8080\n"), 0o600)
	cases := []struct {
		args []string
		want string
	}{
		// "nowhere" matches no instance: resolving it would fail differently, after a
		// refresh.
		{[]string{"push", big, "nowhere", "/opt/acme/big.bin"}, big + ": 400001 bytes, too large for this method (limit 400000)"},
		{[]string{"push", small, "nowhere", "opt/acme/app.conf"}, "remote path must be absolute"},
	}
	for _, c := range cases {
		_, _, err := runHop(t, c.args...)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: error %v, want %q", c.args[1:], err, c.want)
		}
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.calls) != 0 {
		t.Errorf("AWS called before the checks: %q", fake.calls)
	}
	if short := newPush(&filters{}).Short; !strings.Contains(short, "400 KB") {
		t.Errorf("help %q", short)
	}
}

func TestMFAProfilesAreSkipped(t *testing.T) {
	fakeAWSEnv(t)
	os.WriteFile(awsconf.ConfigPath(), []byte(mfaConfig), 0o600)
	_, stderr, err := runHop(t, "ls", "--refresh", "-p", "billing-mfa*")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"hop: role assumed with an MFA code, which hop does not prompt for yet: billing-mfa, billing-mfa-2",
		"use a profile without mfa_serial, or get the credentials from aws-vault or another credential_process",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr does not mention %q:\n%s", want, stderr)
		}
	}
	inv := inventory.Load()
	tg := inv.Targets[inventory.TargetKey("billing-mfa-2", "eu-west-1")]
	if tg == nil || tg.ErrKind != inventory.ErrMFA || !strings.Contains(tg.Err, "profile billing-mfa-2 needs an MFA code (mfa_serial in billing-mfa)") {
		t.Fatalf("billing-mfa-2: %+v", tg)
	}

	// Recorded once: not queried again every minute.
	tg.ErrAt = time.Now().Add(-time.Hour)
	rewriteCache(t, inv)
	if _, _, err := runHop(t, "ls", "-p", "billing-mfa*"); err != nil {
		t.Fatal(err)
	}
	if again := inventory.Load().Targets[tg.Key()]; !again.ErrAt.Equal(tg.ErrAt) {
		t.Errorf("recorded again at %v", again.ErrAt)
	}

	// Once mfa_serial is gone, the profile is queried right away.
	os.WriteFile(awsconf.ConfigPath(), []byte(strings.ReplaceAll(mfaConfig, "mfa_serial", "# mfa_serial")), 0o600)
	if _, _, err := runHop(t, "ls", "-p", "billing-mfa*"); err != nil {
		t.Fatal(err)
	}
	if again := inventory.Load().Targets[tg.Key()]; again.ErrKind == inventory.ErrMFA {
		t.Errorf("still recorded as needing MFA: %+v", again)
	}
}

func TestCacheDropsRemovedProfiles(t *testing.T) {
	fakeAWSEnv(t)
	inv := inventory.Load()
	gone := fresh("acme-old", "eu-west-1", instance("old-app", "i-0f01", "acme-old", "eu-west-1", "running", ""))
	inv.Targets[gone.Key()] = gone
	rewriteCache(t, inv)
	if _, _, err := runHop(t, "ls", "--refresh", "-p", "acme-billing"); err != nil {
		t.Fatal(err)
	}
	after := inventory.Load()
	if _, ok := after.Targets[gone.Key()]; ok {
		t.Error("the pair of a profile gone from the AWS files is still cached")
	}
	if _, ok := after.Targets[inventory.TargetKey("acme-prod", "eu-west-1")]; !ok {
		t.Error("a profile outside -p was dropped")
	}
}
