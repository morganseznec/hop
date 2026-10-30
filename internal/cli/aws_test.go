package cli

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

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
			items = `<item><instanceId>i-0b01</instanceId><instanceState><code>16</code><name>running</name></instanceState>` +
				`<privateIpAddress>10.1.0.1</privateIpAddress><tagSet><item><key>Name</key><value>acme-b-app</value></item></tagSet></item>`
		case "us-east-1":
			items = `<item><instanceId>i-0b02</instanceId><instanceState><code>80</code><name>stopped</name></instanceState>` +
				`<tagSet><item><key>Name</key><value>acme-b-legacy</value></item></tagSet></item>`
		}
		io.WriteString(w, `<DescribeInstancesResponse`+ns+`><reservationSet><item><instancesSet>`+items+
			`</instancesSet></item></reservationSet></DescribeInstancesResponse>`)
	default:
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `<Response><Errors><Error><Code>InvalidAction</Code></Error></Errors></Response>`)
	}
}

func fakeAWSEnv(t *testing.T) *fakeAWS {
	t.Helper()
	fleetEnv(t)
	terminal(t, false, false)
	f := &fakeAWS{}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	t.Setenv("AWS_ENDPOINT_URL", srv.URL)
	return f
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
	before, _ := os.ReadFile(config.Path())

	_, stderr, err := runHop(t, "scan", "-p", "acme-billing")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "acme-billing") || !strings.Contains(stderr, "acme-b-legacy") ||
		!strings.Contains(stderr, "extra_regions:\n  acme-billing: [us-east-1]") {
		t.Errorf("suggestion:\n%s", stderr)
	}
	if after, _ := os.ReadFile(config.Path()); string(after) != string(before) {
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
	if bak, _ := os.ReadFile(config.Path() + ".bak"); string(bak) != string(before) {
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
	if err := inv.Save(); err != nil {
		t.Fatal(err)
	}
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
