package inventory

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeAWS answers the EC2 and SSM calls made by Fetch. It listens on the loopback
// interface only: no request leaves the machine.
type fakeAWS struct {
	mu    sync.Mutex
	calls []string
	// Request bodies and signing regions, by operation.
	bodies  map[string][]string
	regions map[string][]string
	// Responses by operation name. A missing entry answers an empty success.
	status map[string]int
	body   map[string]string
	// Operations that never answer: the client gives up first.
	hang map[string]bool
}

var signingRegion = regexp.MustCompile(`Credential=[^/]+/\d{8}/([^/]+)/`)

func (f *fakeAWS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	r.Body.Close()
	op := strings.TrimPrefix(r.Header.Get("X-Amz-Target"), "AmazonSSM.")
	contentType := "application/x-amz-json-1.1"
	if op == "" {
		for _, kv := range strings.Split(string(b), "&") {
			if v, ok := strings.CutPrefix(kv, "Action="); ok {
				op = v
			}
		}
		contentType = "text/xml"
	}
	region := ""
	if m := signingRegion.FindStringSubmatch(r.Header.Get("Authorization")); m != nil {
		region = m[1]
	}
	f.mu.Lock()
	f.calls = append(f.calls, op)
	if f.bodies == nil {
		f.bodies, f.regions = map[string][]string{}, map[string][]string{}
	}
	f.bodies[op] = append(f.bodies[op], string(b))
	f.regions[op] = append(f.regions[op], region)
	status, body, hang := f.status[op], f.body[op], f.hang[op]
	f.mu.Unlock()
	if hang {
		<-r.Context().Done()
		return
	}
	if status == 0 {
		status = http.StatusOK
	}
	if body == "" {
		body = "{}"
		if contentType == "text/xml" {
			body = `<` + op + `Response xmlns="http://ec2.amazonaws.com/doc/2016-11-15/"><requestId>r</requestId></` + op + `Response>`
		}
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(status)
	io.WriteString(w, body)
}

func (f *fakeAWS) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// requests returns the bodies and signing regions of the calls to op.
func (f *fakeAWS) requests(op string) (bodies, regions []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.bodies[op]...), append([]string(nil), f.regions[op]...)
}

// fakeEnv isolates the SDK: temporary AWS files and home, no instance metadata, every
// endpoint pointed at the fake server.
func fakeEnv(t *testing.T, endpoint string) {
	t.Helper()
	home := t.TempDir()
	cfg := filepath.Join(home, "config")
	creds := filepath.Join(home, "credentials")
	os.WriteFile(cfg, []byte(`[profile acme-prod]
region = eu-west-1

[sso-session acme-sso]
sso_start_url = https://acme.awsapps.com/start
sso_region = eu-west-1

[profile acme-dual]
sso_session = acme-sso
sso_account_id = 111111111111
sso_role_name = ReadOnly
`), 0o600)
	os.WriteFile(creds, []byte(`[acme-prod]
aws_access_key_id = AKIAFAKEFAKEFAKE0001
aws_secret_access_key = fictional-secret

[acme-dual]
aws_access_key_id = AKIAFAKEFAKEFAKE0002
aws_secret_access_key = fictional-secret
`), 0o600)
	for k, v := range map[string]string{
		"HOME": home, "USERPROFILE": home,
		"AWS_CONFIG_FILE": cfg, "AWS_SHARED_CREDENTIALS_FILE": creds,
		"AWS_ENDPOINT_URL": endpoint, "AWS_EC2_METADATA_DISABLED": "true",
		"AWS_PROFILE": "", "AWS_DEFAULT_PROFILE": "", "AWS_REGION": "", "AWS_DEFAULT_REGION": "",
		"AWS_ACCESS_KEY_ID": "", "AWS_SECRET_ACCESS_KEY": "", "AWS_SESSION_TOKEN": "",
		"AWS_ROLE_ARN": "", "AWS_WEB_IDENTITY_TOKEN_FILE": "",
		"AWS_CONTAINER_CREDENTIALS_FULL_URI": "", "AWS_CONTAINER_CREDENTIALS_RELATIVE_URI": "",
		"AWS_ENDPOINT_URL_EC2": "", "AWS_ENDPOINT_URL_SSM": "", "AWS_IGNORE_CONFIGURED_ENDPOINT_URLS": "",
		"AWS_CA_BUNDLE": "", "AWS_USE_FIPS_ENDPOINT": "", "AWS_USE_DUALSTACK_ENDPOINT": "",
	} {
		t.Setenv(k, v)
	}
}

const ec2Instances = `<DescribeInstancesResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/">
  <requestId>r</requestId>
  <reservationSet>
    <item>
      <reservationId>r-1</reservationId>
      <instancesSet>
        <item>
          <instanceId>i-0aaa</instanceId>
          <instanceState><code>16</code><name>running</name></instanceState>
          <privateIpAddress>10.0.0.1</privateIpAddress>
          <platformDetails>Linux/UNIX</platformDetails>
          <tagSet><item><key>Name</key><value>acme-p-tools</value></item></tagSet>
        </item>
        <item>
          <instanceId>i-0bbb</instanceId>
          <instanceState><code>80</code><name>stopped</name></instanceState>
          <tagSet><item><key>env</key><value>uat</value></item></tagSet>
        </item>
      </instancesSet>
    </item>
    <item>
      <reservationId>r-2</reservationId>
      <instancesSet>
        <item>
          <instanceId>i-0ccc</instanceId>
          <instanceState><code>16</code><name>running</name></instanceState>
          <privateIpAddress>10.0.0.3</privateIpAddress>
          <tagSet><item><key>Name</key><value>acme-t-app</value></item></tagSet>
        </item>
      </instancesSet>
    </item>
  </reservationSet>
</DescribeInstancesResponse>`

const lastPing = 1793350800 // 2026-10-30, epoch seconds as SSM returns them

var ssmAgents = fmt.Sprintf(`{"InstanceInformationList":[
  {"InstanceId":"i-0aaa","PingStatus":"Online","LastPingDateTime":%d,"PlatformName":"Amazon Linux"},
  {"InstanceId":"mi-0ddd","Name":"acme-onprem","ComputerName":"host1","IPAddress":"192.168.1.5","PingStatus":"ConnectionLost","PlatformName":"Ubuntu"},
  {"InstanceId":"mi-0eee","ComputerName":"acme-nas","PingStatus":"Online"},
  {"InstanceId":"mi-0fff","PingStatus":"Inactive"}
]}`, lastPing)

func ssmDocument(content string) string {
	b, _ := json.Marshal(map[string]string{"Name": "SSM-SessionManagerRunShell", "Content": content})
	return string(b)
}

func TestFetch(t *testing.T) {
	fake := &fakeAWS{body: map[string]string{
		"DescribeInstances":           ec2Instances,
		"DescribeInstanceInformation": ssmAgents,
		"GetDocument":                 ssmDocument(`{"inputs":{"cloudWatchLogGroupName":"/acme/ssm/sessions","cloudWatchEncryptionEnabled":true}}`),
	}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	fakeEnv(t, srv.URL)

	before := time.Now()
	tg := Fetch(context.Background(), Pair{Profile: "acme-prod", Region: "eu-west-1"})
	if tg.Err != "" {
		t.Fatalf("fetch failed: %s (calls %v)", tg.Err, fake.called())
	}
	if tg.Profile != "acme-prod" || tg.Region != "eu-west-1" || tg.FetchedAt.Before(before) || !tg.AgentsKnown {
		t.Errorf("target: %+v", tg)
	}
	if !tg.Transcript.Enabled || !tg.Transcript.Encrypted || tg.Transcript.LogGroup != "/acme/ssm/sessions" {
		t.Errorf("transcript: %+v", tg.Transcript)
	}

	byID := map[string]Instance{}
	for _, in := range tg.Instances {
		byID[in.ID] = in
		if in.Profile != "acme-prod" || in.Region != "eu-west-1" {
			t.Errorf("%s: profile/region %s/%s", in.ID, in.Profile, in.Region)
		}
	}
	if len(byID) != 6 {
		t.Fatalf("%d instances: %+v", len(byID), tg.Instances)
	}
	checks := []struct {
		id, name, state, ip, env, platform, agent string
	}{
		// The SSM platform name is more precise than the EC2 one.
		{"i-0aaa", "acme-p-tools", "running", "10.0.0.1", "prod", "Amazon Linux", "Online"},
		// No Name tag: the ID stands in. The env tag wins over the profile name.
		{"i-0bbb", "i-0bbb", "stopped", "", "test", "", ""},
		// No SSM agent.
		{"i-0ccc", "acme-t-app", "running", "10.0.0.3", "test", "", ""},
		// Hybrid servers exist only on the SSM side and are listed as running.
		{"mi-0ddd", "acme-onprem", "running", "192.168.1.5", "prod", "Ubuntu", "ConnectionLost"},
		{"mi-0eee", "acme-nas", "running", "", "prod", "", "Online"},
		{"mi-0fff", "mi-0fff", "running", "", "prod", "", "Inactive"},
	}
	for _, c := range checks {
		in := byID[c.id]
		got := []string{in.Name, in.State, in.PrivateIP, in.Env, in.Platform, in.Agent}
		want := []string{c.name, c.state, c.ip, c.env, c.platform, c.agent}
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("%s:\n got %q\nwant %q", c.id, got, want)
		}
	}
	if !byID["i-0aaa"].LastPing.Equal(time.Unix(lastPing, 0)) {
		t.Errorf("last ping %v", byID["i-0aaa"].LastPing)
	}
}

func TestClean(t *testing.T) {
	cases := map[string]string{
		"acme-p-tools":                      "acme-p-tools",
		"acme\x1b[2J\x1b]0;pwned\x07-tools": "acme[2J]0;pwned-tools",
		"acme\r\nfake line\tx":              "acmefake linex",
		"acme\u009b31m":                     "acme31m", // C1 CSI
		"acme\u202eslof":                    "acmeslof",
		"acme été 中文":                       "acme été 中文", // printable text is kept
		"":                                  "",
	}
	for in, want := range cases {
		if got := clean(in); got != want {
			t.Errorf("clean(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFetchStripsControlCharacters(t *testing.T) {
	// XML 1.0 carries line breaks, C1 controls and bidirectional overrides; JSON
	// carries everything, ESC included.
	ec2Body := `<DescribeInstancesResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/">
  <requestId>r</requestId>
  <reservationSet><item><reservationId>r-1</reservationId><instancesSet>
    <item>
      <instanceId>i-0aaa</instanceId>
      <instanceState><code>16</code><name>running</name></instanceState>
      <tagSet>
        <item><key>Name</key><value>acme&#x9B;2J&#xA;fake line&#x202E;</value></item>
        <item><key>env&#xD;</key><value>d&#x85;ev</value></item>
      </tagSet>
    </item>
    <item>
      <instanceId>i-0bbb</instanceId>
      <instanceState><code>16</code><name>running</name></instanceState>
      <tagSet><item><key>Name</key><value>&#xA;&#x9B;&#x9;</value></item></tagSet>
    </item>
  </instancesSet></item></reservationSet>
</DescribeInstancesResponse>`
	fake := &fakeAWS{body: map[string]string{
		"DescribeInstances": ec2Body,
		"DescribeInstanceInformation": `{"InstanceInformationList":[
  {"InstanceId":"mi-0ccc","Name":"acme\u001b[31monprem","IPAddress":"10.0.0.9\u001b[0m","PlatformName":"Ubuntu\u0007","PingStatus":"Online"}
]}`,
		"GetDocument": ssmDocument(`{"inputs":{"cloudWatchLogGroupName":"/acme/ssm\u001b[8m","s3BucketName":"acme-logs\u000a"}}`),
	}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	fakeEnv(t, srv.URL)

	tg := Fetch(context.Background(), Pair{Profile: "acme-prod", Region: "eu-west-1"})
	if tg.Err != "" {
		t.Fatalf("fetch failed: %s", tg.Err)
	}
	got := map[string]Instance{}
	for _, in := range tg.Instances {
		got[in.ID] = in
	}
	checks := []struct{ id, name, env, ip, platform string }{
		// The env tag is read once cleaned: "env\r" = "d\u0085ev".
		{"i-0aaa", "acme2Jfake line", "test", "", ""},
		// Nothing printable left: the ID stands in, as for a missing Name tag.
		{"i-0bbb", "i-0bbb", "prod", "", ""},
		{"mi-0ccc", "acme[31monprem", "prod", "10.0.0.9[0m", "Ubuntu"},
	}
	for _, c := range checks {
		in := got[c.id]
		if in.Name != c.name || in.Env != c.env || in.PrivateIP != c.ip || in.Platform != c.platform {
			t.Errorf("%s: name %q env %q ip %q platform %q, want %q %q %q %q",
				c.id, in.Name, in.Env, in.PrivateIP, in.Platform, c.name, c.env, c.ip, c.platform)
		}
	}
	if tg.Transcript.LogGroup != "/acme/ssm[8m" || tg.Transcript.Bucket != "acme-logs" {
		t.Errorf("transcript %+v", tg.Transcript)
	}
}

func TestFetchPartialFailures(t *testing.T) {
	cases := []struct {
		name   string
		status map[string]int
		body   map[string]string
		check  func(t *testing.T, tg *Target)
	}{
		{
			name:   "SSM agents denied: EC2 instances kept, agents unknown",
			status: map[string]int{"DescribeInstanceInformation": 400},
			body: map[string]string{
				"DescribeInstances":           ec2Instances,
				"DescribeInstanceInformation": `{"__type":"AccessDeniedException","message":"denied"}`,
			},
			check: func(t *testing.T, tg *Target) {
				if tg.Err != "" || tg.AgentsKnown || len(tg.Instances) != 3 {
					t.Errorf("%+v", tg)
				}
				for _, in := range tg.Instances {
					if in.Agent != "" {
						t.Errorf("%s: agent %q without agent data", in.ID, in.Agent)
					}
				}
			},
		},
		{
			name:   "no session document: checked and not logged",
			status: map[string]int{"GetDocument": 400},
			body:   map[string]string{"GetDocument": `{"__type":"InvalidDocument","message":"missing"}`},
			check: func(t *testing.T, tg *Target) {
				if tg.Err != "" || tg.Transcript != (Transcript{Checked: true}) {
					t.Errorf("transcript %+v (err %s)", tg.Transcript, tg.Err)
				}
			},
		},
		{
			name:   "session document denied: not checked",
			status: map[string]int{"GetDocument": 400},
			body:   map[string]string{"GetDocument": `{"__type":"AccessDeniedException","message":"denied"}`},
			check: func(t *testing.T, tg *Target) {
				if tg.Err != "" || tg.Transcript.Checked || tg.Transcript.Enabled || tg.Transcript.Err == "" {
					t.Errorf("transcript %+v", tg.Transcript)
				}
			},
		},
		{
			name:   "EC2 denied: the whole pair fails",
			status: map[string]int{"DescribeInstances": 403},
			body: map[string]string{"DescribeInstances": `<Response><Errors><Error><Code>UnauthorizedOperation</Code>` +
				`<Message>not authorized</Message></Error></Errors><RequestID>r</RequestID></Response>`},
			check: func(t *testing.T, tg *Target) {
				if tg.Err == "" || tg.ErrKind != ErrDenied || tg.ErrAt.IsZero() || !tg.FetchedAt.IsZero() {
					t.Errorf("%+v", tg)
				}
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fake := &fakeAWS{status: c.status, body: c.body}
			srv := httptest.NewServer(fake)
			defer srv.Close()
			fakeEnv(t, srv.URL)
			c.check(t, Fetch(context.Background(), Pair{Profile: "acme-prod", Region: "eu-west-1"}))
		})
	}
}

func TestFetchPreferSSO(t *testing.T) {
	fake := &fakeAWS{}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	fakeEnv(t, srv.URL)

	// Without PreferSSO, the SDK picks the keys of the credentials file.
	tg := Fetch(context.Background(), Pair{Profile: "acme-dual", Region: "eu-west-1"})
	if tg.Err != "" || len(fake.called()) == 0 {
		t.Fatalf("keys not used: %+v, calls %v", tg, fake.called())
	}

	// With PreferSSO, the credentials file is ignored: no token in the temporary home,
	// so the pair fails as an expired SSO token, before any AWS call.
	n := len(fake.called())
	tg = Fetch(context.Background(), Pair{Profile: "acme-dual", Region: "eu-west-1", PreferSSO: true})
	if tg.ErrKind != ErrSSOExpired {
		t.Errorf("PreferSSO: %+v", tg)
	}
	if len(fake.called()) != n {
		t.Errorf("calls made with the keys despite PreferSSO: %v", fake.called()[n:])
	}
}

func TestFetchAll(t *testing.T) {
	fake := &fakeAWS{body: map[string]string{"DescribeInstances": ec2Instances}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	fakeEnv(t, srv.URL)

	pairs := []Pair{
		{Profile: "acme-prod", Region: "eu-west-1"},
		{Profile: "ghost", Region: "eu-west-1"},
		{Profile: "acme-prod", Region: "us-east-1"},
	}
	var mu sync.Mutex
	var progress []int
	got := FetchAll(context.Background(), pairs, 10*time.Second, func(done, total int) {
		mu.Lock()
		defer mu.Unlock()
		if total != len(pairs) {
			t.Errorf("total %d", total)
		}
		progress = append(progress, done)
	})
	if len(got) != len(pairs) {
		t.Fatalf("%d results", len(got))
	}
	for i, p := range pairs {
		if got[i].Profile != p.Profile || got[i].Region != p.Region {
			t.Errorf("result %d is %s/%s, want %s/%s", i, got[i].Profile, got[i].Region, p.Profile, p.Region)
		}
	}
	if got[0].Err != "" || got[2].Err != "" || len(got[0].Instances) != 3 {
		t.Errorf("successful pairs: %+v / %+v", got[0], got[2])
	}
	if got[1].Err == "" || got[1].ErrKind != ErrOther {
		t.Errorf("unknown profile: %+v", got[1])
	}
	if strings.Trim(fmt.Sprint(progress), "[]") != "1 2 3" {
		t.Errorf("progress %v", progress)
	}
	if out := FetchAll(context.Background(), nil, time.Second, nil); len(out) != 0 {
		t.Errorf("no pairs: %v", out)
	}
}

func TestFetchAgentPageSize(t *testing.T) {
	fake := &fakeAWS{body: map[string]string{"DescribeInstanceInformation": ssmAgents}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	fakeEnv(t, srv.URL)

	if tg := Fetch(context.Background(), Pair{Profile: "acme-prod", Region: "eu-west-1"}); tg.Err != "" || !tg.AgentsKnown {
		t.Fatalf("%+v", tg)
	}
	bodies, _ := fake.requests("DescribeInstanceInformation")
	if len(bodies) != 1 {
		t.Fatalf("%d DescribeInstanceInformation calls", len(bodies))
	}
	var req struct{ MaxResults int }
	if err := json.Unmarshal([]byte(bodies[0]), &req); err != nil || req.MaxResults != 50 {
		t.Errorf("DescribeInstanceInformation asks for pages of %d (%v): %s", req.MaxResults, err, bodies[0])
	}
}

func TestFetchKeepsTheTranscriptWhenTheAgentsTimeOut(t *testing.T) {
	// An account with thousands of nodes: the agent pages take longer than the time
	// allowed for the pair.
	fake := &fakeAWS{
		body: map[string]string{
			"DescribeInstances": ec2Instances,
			"GetDocument":       ssmDocument(`{"inputs":{"cloudWatchLogGroupName":"/acme/ssm/sessions"}}`),
		},
		hang: map[string]bool{"DescribeInstanceInformation": true},
	}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	fakeEnv(t, srv.URL)

	tg := FetchAll(context.Background(), []Pair{{Profile: "acme-prod", Region: "eu-west-1"}}, time.Second, nil)[0]
	if tg.Err != "" || tg.AgentsKnown || len(tg.Instances) != 3 {
		t.Fatalf("EC2 instances must be kept, agents unknown: %+v", tg)
	}
	// GetDocument does not wait for the agents: the transcript is checked.
	if !tg.Transcript.Checked || !tg.Transcript.Enabled || tg.Transcript.LogGroup != "/acme/ssm/sessions" {
		t.Errorf("transcript lost to the agents timeout: %+v", tg.Transcript)
	}

	// Recorded over a previous inventory, the agents seen before stay.
	ping := time.Now().Add(-time.Hour)
	inv := &Inventory{Targets: map[string]*Target{}}
	inv.Put(&Target{Profile: "acme-prod", Region: "eu-west-1", FetchedAt: ping, AgentsKnown: true,
		Transcript: Transcript{Checked: true, Enabled: true, LogGroup: "/acme/ssm/sessions"},
		Instances:  []Instance{{ID: "i-0aaa", Name: "acme-p-tools", Agent: "Online", LastPing: ping, Platform: "Amazon Linux"}}})
	inv.Put(tg)
	got := inv.Targets[TargetKey("acme-prod", "eu-west-1")]
	if got.Instances[0].ID != "i-0aaa" || got.Instances[0].Agent != "Online" || got.Instances[0].Platform != "Amazon Linux" {
		t.Errorf("agent of i-0aaa lost: %+v", got.Instances[0])
	}
}

func TestFetchAllResolvesCredentialsOncePerProfile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the credential_process helper is a shell script")
	}
	fake := &fakeAWS{body: map[string]string{"DescribeInstances": ec2Instances}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	fakeEnv(t, srv.URL)

	// A credential_process that counts its runs, as GetRoleCredentials or AssumeRole
	// would count calls.
	dir := t.TempDir()
	count := filepath.Join(dir, "runs")
	helper := filepath.Join(dir, "creds.sh")
	os.WriteFile(helper, []byte("#!/bin/sh\necho run >> '"+count+"'\n"+
		`echo '{"Version":1,"AccessKeyId":"AKIAFAKEFAKEFAKE0009","SecretAccessKey":"fictional","SessionToken":"fictional"}'`+"\n"), 0o700)
	f, err := os.OpenFile(os.Getenv("AWS_CONFIG_FILE"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(f, "\n[profile acme-vault]\ncredential_process = %s\n", helper)
	f.Close()

	regions := []string{"eu-west-1", "us-east-1", "ap-south-1", "eu-central-1"}
	var pairs []Pair
	for _, r := range regions {
		pairs = append(pairs, Pair{Profile: "acme-vault", Region: r})
	}
	pairs = append(pairs, Pair{Profile: "acme-prod", Region: "eu-west-1"})
	for _, tg := range FetchAll(context.Background(), pairs, 10*time.Second, nil) {
		if tg.Err != "" || len(tg.Instances) != 3 {
			t.Errorf("%s: %+v", tg.Key(), tg)
		}
	}
	b, _ := os.ReadFile(count)
	if runs := strings.Count(string(b), "run"); runs != 1 {
		t.Errorf("credentials resolved %d times for one profile in %d regions", runs, len(regions))
	}
	// Each pair is still queried in its own region.
	_, signed := fake.requests("DescribeInstances")
	slices.Sort(signed)
	if want := []string{"ap-south-1", "eu-central-1", "eu-west-1", "eu-west-1", "us-east-1"}; !slices.Equal(signed, want) {
		t.Errorf("regions queried %v, want %v", signed, want)
	}
}

func TestLookup(t *testing.T) {
	notFound := `<Response><Errors><Error><Code>InvalidInstanceID.NotFound</Code><Message>gone</Message></Error></Errors><RequestID>r</RequestID></Response>`
	denied := `<Response><Errors><Error><Code>UnauthorizedOperation</Code><Message>denied</Message></Error></Errors><RequestID>r</RequestID></Response>`
	ping := time.Now().Add(-time.Hour)
	cached := Instance{ID: "i-0aaa", Name: "acme-p-tools", Profile: "acme-prod", Region: "eu-west-1", PrivateIP: "10.0.0.1",
		State: "running", Platform: "Amazon Linux", Agent: "ConnectionLost", LastPing: ping, AlsoVia: []string{"acme-dual"}}
	hybrid := Instance{ID: "mi-0ddd", Name: "acme-onprem", Profile: "acme-prod", Region: "eu-west-1", State: "running", Agent: "Online"}
	cases := []struct {
		name      string
		cached    Instance
		status    map[string]int
		body      map[string]string
		wantFound bool
		wantErr   bool
		check     func(t *testing.T, in Instance)
	}{
		{
			name: "running, agent read again", cached: cached, wantFound: true,
			body: map[string]string{"DescribeInstances": ec2Instances, "DescribeInstanceInformation": ssmAgents},
			check: func(t *testing.T, in Instance) {
				if in.Name != "acme-p-tools" || !in.Running() || in.Agent != "Online" || !in.LastPing.Equal(time.Unix(lastPing, 0)) ||
					in.Profile != "acme-prod" || in.Region != "eu-west-1" || len(in.AlsoVia) != 1 {
					t.Errorf("%+v", in)
				}
			},
		},
		{
			name: "no agent registered any more", cached: cached, wantFound: true,
			body: map[string]string{"DescribeInstances": ec2Instances, "DescribeInstanceInformation": `{"InstanceInformationList":[]}`},
			check: func(t *testing.T, in Instance) {
				if in.Agent != "" || !in.LastPing.IsZero() || in.Platform != "Linux/UNIX" {
					t.Errorf("%+v", in)
				}
			},
		},
		{
			name: "agents denied: the cached agent stays", cached: cached, wantFound: true,
			status: map[string]int{"DescribeInstanceInformation": 400},
			body:   map[string]string{"DescribeInstances": ec2Instances, "DescribeInstanceInformation": `{"__type":"AccessDeniedException","message":"denied"}`},
			check: func(t *testing.T, in Instance) {
				if in.Agent != "ConnectionLost" || !in.LastPing.Equal(ping) || in.Platform != "Amazon Linux" {
					t.Errorf("%+v", in)
				}
			},
		},
		{
			name: "terminated", cached: cached, wantFound: true,
			body: map[string]string{"DescribeInstances": strings.Replace(ec2Instances, "<code>16</code><name>running</name>", "<code>48</code><name>terminated</name>", 1)},
			check: func(t *testing.T, in Instance) {
				if in.State != "terminated" || in.Running() {
					t.Errorf("%+v", in)
				}
			},
		},
		{
			name: "gone", cached: cached,
			status: map[string]int{"DescribeInstances": 400},
			body:   map[string]string{"DescribeInstances": notFound},
		},
		{
			name: "EC2 denied", cached: cached, wantErr: true,
			status: map[string]int{"DescribeInstances": 403},
			body:   map[string]string{"DescribeInstances": denied},
		},
		{
			name: "hybrid node", cached: hybrid, wantFound: true,
			body: map[string]string{"DescribeInstanceInformation": ssmAgents},
			check: func(t *testing.T, in Instance) {
				if in.Name != "acme-onprem" || in.Agent != "ConnectionLost" || in.PrivateIP != "192.168.1.5" {
					t.Errorf("%+v", in)
				}
			},
		},
		{
			name: "hybrid node deregistered", cached: hybrid,
			body: map[string]string{"DescribeInstanceInformation": `{"InstanceInformationList":[]}`},
		},
		{
			name: "hybrid node, SSM denied", cached: hybrid, wantErr: true,
			status: map[string]int{"DescribeInstanceInformation": 400},
			body:   map[string]string{"DescribeInstanceInformation": `{"__type":"AccessDeniedException","message":"denied"}`},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fake := &fakeAWS{status: c.status, body: c.body}
			srv := httptest.NewServer(fake)
			defer srv.Close()
			fakeEnv(t, srv.URL)
			cfg, err := AWSConfig(context.Background(), "acme-prod", "eu-west-1", false)
			if err != nil {
				t.Fatal(err)
			}
			in, found, err := Lookup(context.Background(), cfg, c.cached)
			if found != c.wantFound || (err != nil) != c.wantErr {
				t.Fatalf("found %v, err %v; want %v, error %v", found, err, c.wantFound, c.wantErr)
			}
			if c.check != nil {
				c.check(t, in)
			}
			// One instance asked for, never the whole account.
			ec2Bodies, _ := fake.requests("DescribeInstances")
			ssmBodies, _ := fake.requests("DescribeInstanceInformation")
			if c.cached.Hybrid() != (len(ec2Bodies) == 0) || len(ssmBodies) != 1 {
				t.Fatalf("calls %v", fake.called())
			}
			for _, b := range ec2Bodies {
				if !strings.Contains(b, "InstanceId.1="+c.cached.ID) || strings.Contains(b, "Filter") {
					t.Errorf("DescribeInstances not narrowed to the instance: %s", b)
				}
			}
			if want := `"Filters":[{"Key":"InstanceIds","Values":["` + c.cached.ID + `"]}]`; !strings.Contains(ssmBodies[0], want) {
				t.Errorf("DescribeInstanceInformation not narrowed to the instance: %s", ssmBodies[0])
			}
			if slices.Contains(fake.called(), "GetDocument") {
				t.Error("a lookup read the transcript settings")
			}
		})
	}
}
