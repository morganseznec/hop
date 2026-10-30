package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/morganseznec/hop/internal/awsconf"
	"github.com/morganseznec/hop/internal/config"
	"github.com/morganseznec/hop/internal/inventory"
)

func profileNames(ps []*awsconf.Profile) []string {
	var out []string
	for _, p := range ps {
		out = append(out, p.Name)
	}
	return out
}

func TestSelectedProfiles(t *testing.T) {
	cases := []struct {
		name     string
		cfg      config.Config
		profiles []string
		want     []string
	}{
		// SSO profiles first, then the others, each in file order. [default] has no
		// credentials.
		{"everything", config.Config{}, nil, []string{"acme-prod", "acme-data", "globex", "acme-billing"}},
		{"config exclude", config.Config{Exclude: []string{"acme-data"}}, nil, []string{"acme-prod", "globex", "acme-billing"}},
		{"config include", config.Config{Include: []string{"acme-*"}}, nil, []string{"acme-prod", "acme-data", "acme-billing"}},
		{"-p pattern", config.Config{}, []string{"acme-*"}, []string{"acme-prod", "acme-data", "acme-billing"}},
		{"-p wins over the config exclude", config.Config{Exclude: []string{"acme-billing"}}, []string{"acme-billing"}, []string{"acme-billing"}},
		{"-p keeps the SSO-first order", config.Config{}, []string{"acme-billing", "globex"}, []string{"globex", "acme-billing"}},
		{"-p on a profile without credentials", config.Config{}, []string{"default"}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := newTestApp(t, &c.cfg, filters{profiles: c.profiles})
			if got := profileNames(a.selectedProfiles()); !reflect.DeepEqual(got, c.want) {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestPairs(t *testing.T) {
	cfg := &config.Config{ExtraRegions: map[string][]string{"acme-prod": {"us-east-1"}}}
	a := newTestApp(t, cfg, filters{})
	want := []inventory.Pair{
		{Profile: "acme-prod", Region: "eu-west-1"},
		{Profile: "acme-prod", Region: "us-east-1"},
		// Defined with SSO and keys: queried through SSO.
		{Profile: "acme-data", Region: "eu-west-1", PreferSSO: true},
		{Profile: "globex", Region: "us-east-1"},
		// No region on the profile: the one from [default].
		{Profile: "acme-billing", Region: "eu-west-1"},
	}
	if got := a.pairs(); !reflect.DeepEqual(got, want) {
		t.Errorf("pairs:\n got %+v\nwant %+v", got, want)
	}
	if got := a.keys(); len(got) != 5 || got[1] != "acme-prod|us-east-1" {
		t.Errorf("keys %v", got)
	}

	// -r replaces the regions of every profile, extra regions included.
	a = newTestApp(t, cfg, filters{profiles: []string{"acme-prod", "globex"}, regions: []string{"ap-south-1"}})
	want = []inventory.Pair{{Profile: "acme-prod", Region: "ap-south-1"}, {Profile: "globex", Region: "ap-south-1"}}
	if got := a.pairs(); !reflect.DeepEqual(got, want) {
		t.Errorf("pairs with -r:\n got %+v\nwant %+v", got, want)
	}
	if a.mainRegion("acme-prod") != "eu-west-1" || a.mainRegion("unknown") != "" {
		t.Errorf("main region: %q %q", a.mainRegion("acme-prod"), a.mainRegion("unknown"))
	}
	if !a.preferSSO("acme-data") || a.preferSSO("acme-prod") || a.preferSSO("unknown") {
		t.Error("preferSSO")
	}
}

func TestCheckProfiles(t *testing.T) {
	cases := []struct {
		profiles []string
		want     string // empty: no error
	}{
		{[]string{"acme-prod"}, ""},
		{[]string{"acme-*", "globex"}, ""},
		{[]string{"default"}, ""},
		{[]string{"acme-prd"}, "unknown profile: acme-prd (did you mean acme-prod?)"},
		{[]string{"acme-prod", "zzzz-*"}, "unknown profile: zzzz-*"},
		{[]string{"acme-["}, "unknown profile: acme-[ (did you mean acme-billing?)"},
	}
	for _, c := range cases {
		a := newTestApp(t, nil, filters{profiles: c.profiles})
		err := a.checkProfiles()
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%v: %v", c.profiles, err)
		case c.want != "" && (err == nil || err.Error() != c.want):
			t.Errorf("%v: %v, want %q", c.profiles, err, c.want)
		}
	}
}

func TestMatchAny(t *testing.T) {
	cases := []struct {
		patterns []string
		name     string
		want     bool
	}{
		{[]string{"acme-*"}, "acme-prod", true},
		{[]string{"globex"}, "globex", true},
		{[]string{"acme-?"}, "acme-prod", false},
		// An invalid pattern still matches the exact same name.
		{[]string{"acme[old"}, "acme[old", true},
		{nil, "acme-prod", false},
	}
	for _, c := range cases {
		if got := matchAny(c.patterns, c.name); got != c.want {
			t.Errorf("matchAny(%v, %q) = %v", c.patterns, c.name, got)
		}
	}
}

func TestClosest(t *testing.T) {
	names := []string{"acme-prod", "acme-data", "globex"}
	cases := map[string]string{"acme-prd": "acme-prod", "globx": "globex", "zzzzzz": "", "": "acme-prod"}
	for in, want := range cases {
		if got := closest(in, names); got != want {
			t.Errorf("closest(%q) = %q, want %q", in, got, want)
		}
	}
	if got := closest("acme", nil); got != "" {
		t.Errorf("no names: %q", got)
	}
}

func TestInstancesFilter(t *testing.T) {
	a := newTestApp(t, nil, filters{}, fleetTargets()...)
	ids := func(items []inventory.Instance) string {
		var out []string
		for _, in := range items {
			out = append(out, in.ID)
		}
		return strings.Join(out, ",")
	}
	if got := ids(a.instances(false)); got != "i-01,i-03,i-04,i-05" {
		t.Errorf("running: %s", got)
	}
	if got := ids(a.instances(true)); got != "i-02,i-01,i-03,i-04,i-05" {
		t.Errorf("all: %s", got)
	}
	a.f.env = "prod"
	if got := ids(a.instances(true)); got != "i-02,i-01,i-03" {
		t.Errorf("prod: %s", got)
	}
	a.f.env = "test"
	a.f.profiles = []string{"globex"}
	if got := ids(a.instances(false)); got != "i-05" {
		t.Errorf("test on globex: %s", got)
	}
}

func TestResolve(t *testing.T) {
	isolate(t)
	cases := []struct {
		name    string
		query   []string
		wantID  string
		wantErr []string
	}{
		{"exact name", []string{"acme-p-tools"}, "i-01", nil},
		{"single running match", []string{"etl"}, "i-03", nil},
		{"words", []string{"acme", "etl"}, "i-03", nil},
		{"stopped", []string{"acme-p-proxy"}, "", []string{"acme-p-proxy (acme-prod, eu-west-1) is stopped: start it first"}},
		{"same name in two accounts, no terminal", []string{"tools"}, "", []string{"several instances match", "tools (acme-data, eu-west-1)", "tools (globex, us-east-1)"}},
		// A typo must never connect without confirmation; without a terminal, it is
		// only suggested.
		{"typo, no terminal", []string{"acme-p-tols"}, "", []string{`no instance matches "acme-p-tols"`, "did you mean: acme-p-tools"}},
		{"nothing", []string{"zzzz"}, "", []string{`no instance matches "zzzz"`}},
		{"no query, no terminal", nil, "", []string{"several instances match"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			terminal(t, false, false)
			a := newTestApp(t, nil, filters{}, fleetTargets()...)
			fetchedNow(a)
			var in inventory.Instance
			var err error
			capture(t, &os.Stderr, func() { in, _, err = a.resolve(context.Background(), c.query, forShell) })
			if c.wantErr == nil {
				if err != nil || in.ID != c.wantID {
					t.Errorf("got %s, %v; want %s", in.ID, err, c.wantID)
				}
				return
			}
			if err == nil {
				t.Fatalf("got %s, want an error", in.ID)
			}
			for _, w := range c.wantErr {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not mention %q", err, w)
				}
			}
		})
	}
}

func TestResolveNoRunningInstance(t *testing.T) {
	isolate(t)
	terminal(t, false, false)
	a := newTestApp(t, nil, filters{profiles: []string{"acme-billing"}}, fleetTargets()...)
	fetchedNow(a)
	_, _, err := a.resolve(context.Background(), nil, forShell)
	if err == nil || err.Error() != "no running instance in scope" {
		t.Errorf("error %v", err)
	}
}

func TestResolveTypoAsksFirst(t *testing.T) {
	isolate(t)
	cases := []struct {
		answer string
		wantID string
		err    error
	}{
		{"n\n", "", errAborted},
		{"\n", "i-01", nil}, // the default answer is yes
		{"y\n", "i-01", nil},
	}
	for _, c := range cases {
		t.Run(strings.TrimSpace(c.answer), func(t *testing.T) {
			terminal(t, true, true)
			stdin(t, c.answer)
			a := newTestApp(t, nil, filters{}, fleetTargets()...)
			fetchedNow(a)
			var in inventory.Instance
			var err error
			prompt := capture(t, &os.Stderr, func() { in, _, err = a.resolve(context.Background(), []string{"acme-p-tols"}, forShell) })
			if !strings.Contains(prompt, `"acme-p-tols" does not exist. Did you mean acme-p-tools (acme-prod, eu-west-1)?`) {
				t.Errorf("prompt %q", prompt)
			}
			if in.ID != c.wantID || !errors.Is(err, c.err) {
				t.Errorf("got %q, %v; want %q, %v", in.ID, err, c.wantID, c.err)
			}
		})
	}
}

// docServer answers GetDocument with the given status and body, as the account of
// the instance would, and counts the calls. It listens on the loopback interface only.
func docServer(t *testing.T, status int, body string) (aws.Config, *atomic.Int32) {
	t.Helper()
	calls := &atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Amz-Target") != "AmazonSSM.GetDocument" {
			t.Errorf("unexpected call %s", r.Header.Get("X-Amz-Target"))
		}
		calls.Add(1)
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return aws.Config{
		Region:           "eu-west-1",
		BaseEndpoint:     aws.String(srv.URL),
		RetryMaxAttempts: 1,
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "AKIAFAKEFAKEFAKE0001", SecretAccessKey: "fictional-secret"}, nil
		}),
	}, calls
}

// sessionDocument is the GetDocument answer for Session Manager preferences that log to
// logGroup, or to nowhere when it is empty.
func sessionDocument(logGroup string) string {
	content, _ := json.Marshal(map[string]any{"inputs": map[string]string{"cloudWatchLogGroupName": logGroup}})
	b, _ := json.Marshal(map[string]string{"Name": "SSM-SessionManagerRunShell", "Content": string(content)})
	return string(b)
}

func TestTranscriptGate(t *testing.T) {
	on := inventory.Transcript{Checked: true, Enabled: true, LogGroup: "/acme/ssm/sessions", Bucket: "acme-logs"}
	off := inventory.Transcript{Checked: true}
	unknown := inventory.Transcript{}
	// What the account says now, read again for transcript: require.
	logged := sessionDocument("/acme/ssm/sessions")
	notLogged := sessionDocument("")
	const denied = `{"__type":"AccessDeniedException","message":"no ssm:GetDocument"}`
	cases := []struct {
		name      string
		policy    string
		tr        *inventory.Transcript // nil: no inventory for the pair
		doc       string                // GetDocument answer, require only
		wantErr   string
		wantAbort bool
		stderr    string
	}{
		{"logged", config.TranscriptWarn, &on, "", "", false, "session LOGGED on acme-p-tools (i-01, acme-prod, eu-west-1) to /acme/ssm/sessions and s3://acme-logs"},
		{"logged, require", config.TranscriptRequire, &on, logged, "", false, "session LOGGED on acme-p-tools (i-01, acme-prod, eu-west-1) to /acme/ssm/sessions."},
		{"off policy skips the check", config.TranscriptOff, &off, "", "", false, "hop: session on acme-p-tools"},
		{"not verified, warn", config.TranscriptWarn, &unknown, "", "", false, "session logging not verified for acme-prod in eu-west-1"},
		{"not verified, require", config.TranscriptRequire, &unknown, denied, "session logging cannot be verified for acme-prod in eu-west-1 (", false, ""},
		{"no inventory, require", config.TranscriptRequire, nil, denied, "cannot be verified", false, ""},
		{"not logged, require", config.TranscriptRequire, &off, notLogged, "session refused (transcript: require)", false, "does NOT log sessions"},
		// The cache is up to the TTL old: what the account says now wins, both ways.
		{"turned off since the cache, require", config.TranscriptRequire, &on, notLogged, "session refused (transcript: require)", false, "does NOT log sessions"},
		{"unknown in the cache, logged now, require", config.TranscriptRequire, &unknown, logged, "", false, "session LOGGED"},
		// Without a terminal, the question gets its default answer: no.
		{"not logged, warn, no terminal", config.TranscriptWarn, &off, "", "", true, "account acme-prod does NOT log sessions in eu-west-1"},
		{"not logged, default policy", "", &off, "", "", true, "does NOT log sessions"},
	}
	in := instance("acme-p-tools", "i-01", "acme-prod", "eu-west-1", "running", "prod")
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			terminal(t, false, false)
			var targets []*inventory.Target
			if c.tr != nil {
				tg := fresh("acme-prod", "eu-west-1", in)
				tg.Transcript = *c.tr
				targets = append(targets, tg)
			}
			status := http.StatusOK
			if c.doc == denied {
				status = http.StatusBadRequest
			}
			cfg, calls := docServer(t, status, c.doc)
			a := newTestApp(t, &config.Config{Transcript: c.policy}, filters{}, targets...)
			var err error
			out := capture(t, &os.Stderr, func() { err = a.transcriptGate(context.Background(), cfg, in) })
			switch {
			case c.wantAbort:
				if !errors.Is(err, errAborted) {
					t.Errorf("want errAborted, got %v", err)
				}
			case c.wantErr != "":
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Errorf("error %v, want %q", err, c.wantErr)
				}
			case err != nil:
				t.Errorf("unexpected error %v", err)
			}
			if !strings.Contains(out, c.stderr) {
				t.Errorf("stderr %q does not mention %q", out, c.stderr)
			}
			// Only require reads the document again.
			want := int32(0)
			if c.policy == config.TranscriptRequire {
				want = 1
			}
			if n := calls.Load(); n != want {
				t.Errorf("%d GetDocument calls, want %d", n, want)
			}
		})
	}
}

func TestTranscriptGateRequireUpdatesTheCache(t *testing.T) {
	terminal(t, false, false)
	in := instance("acme-p-tools", "i-01", "acme-prod", "eu-west-1", "running", "prod")
	tg := fresh("acme-prod", "eu-west-1", in)
	a := newTestApp(t, &config.Config{Transcript: config.TranscriptRequire}, filters{}, tg)
	cfg, _ := docServer(t, http.StatusOK, sessionDocument(""))
	capture(t, &os.Stderr, func() { a.transcriptGate(context.Background(), cfg, in) })
	if got := a.target(in).Transcript; !got.Checked || got.Enabled {
		t.Errorf("transcript after the check: %+v", got)
	}
}

func TestTranscriptGateConfirmed(t *testing.T) {
	terminal(t, true, true)
	stdin(t, "y\n")
	in := instance("acme-p-tools", "i-01", "acme-prod", "eu-west-1", "running", "prod")
	tg := fresh("acme-prod", "eu-west-1", in)
	tg.Transcript = inventory.Transcript{Checked: true}
	a := newTestApp(t, nil, filters{}, tg)
	var err error
	out := capture(t, &os.Stderr, func() { err = a.transcriptGate(context.Background(), aws.Config{}, in) })
	if err != nil || !strings.Contains(out, "Open anyway? [y/N]") || !strings.Contains(out, "session NOT logged on acme-p-tools") {
		t.Errorf("%v\n%s", err, out)
	}
}

func TestReportProblems(t *testing.T) {
	terminal(t, false, false)
	failed := func(profile, region string, kind inventory.ErrKind) *inventory.Target {
		return &inventory.Target{Profile: profile, Region: region, Err: "fictional", ErrKind: kind, ErrAt: time.Now()}
	}
	cfg := &config.Config{ExtraRegions: map[string][]string{"acme-prod": {"us-east-1"}}}
	a := newTestApp(t, cfg, filters{},
		failed("acme-prod", "eu-west-1", inventory.ErrSSOExpired),
		failed("acme-prod", "us-east-1", inventory.ErrSSOExpired),
		failed("acme-data", "eu-west-1", inventory.ErrSSOExpired),
		failed("globex", "us-east-1", inventory.ErrSSOExpired),
		failed("acme-billing", "eu-west-1", inventory.ErrBadKeys),
		// Not in scope: not reported.
		failed("acme-retired", "eu-west-1", inventory.ErrDenied),
	)
	out := capture(t, &os.Stderr, a.reportProblems)
	want := strings.Join([]string{
		"hop: SSO token expired for acme-sso: acme-prod, acme-prod (us-east-1), acme-data not queried",
		"     hop login acme-sso",
		"hop: SSO token expired for globex.awsapps.com: globex not queried",
		"     hop login globex.awsapps.com",
		"hop: keys rejected by AWS (revoked or expired): acme-billing",
		"     hop doctor for details on static-key profiles",
		"",
	}, "\n")
	if out != want {
		t.Errorf("got:\n%s\nwant:\n%s", out, want)
	}

	// No error, no output.
	a = newTestApp(t, nil, filters{}, fleetTargets()...)
	if out := capture(t, &os.Stderr, a.reportProblems); out != "" {
		t.Errorf("unexpected report: %q", out)
	}
}

func TestReportProblemsGroupsOtherCauses(t *testing.T) {
	terminal(t, false, false)
	failed := func(profile, region string, kind inventory.ErrKind, err string) *inventory.Target {
		return &inventory.Target{Profile: profile, Region: region, Err: err, ErrKind: kind}
	}
	const timeout = "operation error EC2: DescribeInstances, https failed: context deadline exceeded"
	a := newTestApp(t, nil, filters{},
		failed("acme-prod", "eu-west-1", inventory.ErrNetwork, timeout),
		failed("globex", "us-east-1", inventory.ErrNetwork, timeout),
		// An expired token on a profile without a portal (temporary keys) cannot be
		// fixed with hop login: it is reported with the other causes, raw error
		// included.
		failed("acme-billing", "eu-west-1", inventory.ErrSSOExpired, "failed to refresh cached credentials,\n  token expired"),
	)
	out := capture(t, &os.Stderr, a.reportProblems)
	want := "hop: network error or timeout: acme-prod, globex\n" +
		"     " + timeout + "\n" +
		"     check the network, VPN or proxy, then hop ls --refresh\n" +
		"hop: SSO token expired: acme-billing\n" +
		"     failed to refresh cached credentials, token expired\n"
	if out != want {
		t.Errorf("got:\n%s\nwant:\n%s", out, want)
	}
}

func TestReportProblemsRawErrors(t *testing.T) {
	terminal(t, false, false)
	failed := func(profile, region string, kind inventory.ErrKind, err string) *inventory.Target {
		return &inventory.Target{Profile: profile, Region: region, Err: err, ErrKind: kind}
	}
	denied := "operation error EC2: DescribeInstances, https response error StatusCode: 403, RequestID: 0f1e2d3c-fictional, " +
		"api error UnauthorizedOperation: You are not authorized to perform this operation."
	cfg := &config.Config{ExtraRegions: map[string][]string{"acme-prod": {"us-east-1"}}}
	a := newTestApp(t, cfg, filters{},
		failed("acme-prod", "eu-west-1", inventory.ErrDenied, denied),
		failed("acme-prod", "us-east-1", inventory.ErrDenied, "api error AccessDenied: no ssm:DescribeInstanceInformation"),
		// A kind this version does not know is still explained, with its raw error.
		failed("globex", "us-east-1", inventory.ErrKind("fictional-kind"), strings.Repeat("x", 300)),
	)
	out := capture(t, &os.Stderr, a.reportProblems)
	want := "hop: insufficient permissions: acme-prod, acme-prod (us-east-1)\n" +
		"     acme-prod: operation error EC2: DescribeInstances, api error UnauthorizedOperation: You are not authorized to perform this operation.\n" +
		"     acme-prod (us-east-1): api error AccessDenied: no ssm:DescribeInstanceInformation\n" +
		"     the permissions hop needs: https://github.com/morganseznec/hop#iam-permissions\n" +
		"hop: error: globex\n" +
		"     " + strings.Repeat("x", 197) + "...\n"
	if out != want {
		t.Errorf("got:\n%s\nwant:\n%s", out, want)
	}

	// Beyond three distinct errors, the rest is counted.
	var fs []failure
	for i := range 5 {
		fs = append(fs, failure{fmt.Sprintf("p%d", i), fmt.Sprintf("error %d", i)})
	}
	out = capture(t, &os.Stderr, func() { printCauses(fs) })
	if !strings.HasSuffix(out, "     p2: error 2\n     and 2 more\n") || strings.Contains(out, "p3") {
		t.Errorf("capped causes:\n%s", out)
	}
}

// A region the account has not enabled: the fix is to enable it, or to stop querying
// it where it was asked for (extra_regions, or -r).
func TestReportProblemsRegionNotEnabled(t *testing.T) {
	isolate(t)
	terminal(t, false, false)
	failed := &inventory.Target{Profile: "acme-prod", Region: "ap-east-1", Err: "api error OptInRequired: fictional", ErrKind: inventory.ErrRegion}
	cfg := &config.Config{ExtraRegions: map[string][]string{"acme-prod": {"ap-east-1"}}}
	a := newTestApp(t, cfg, filters{}, failed)
	out := capture(t, &os.Stderr, a.reportProblems)
	want := "hop: region not enabled for this account: acme-prod (ap-east-1)\n" +
		"     enable the region on the account, or remove it from extra_regions in " + configPath(t) + "\n"
	if out != want {
		t.Errorf("got:\n%s\nwant:\n%s", out, want)
	}

	a = newTestApp(t, nil, filters{regions: []string{"ap-east-1"}}, failed)
	out = capture(t, &os.Stderr, a.reportProblems)
	if !strings.HasSuffix(out, "     enable the region on the account, or leave it out of -r\n") {
		t.Errorf("with -r:\n%s", out)
	}
}

func TestWarnAgent(t *testing.T) {
	terminal(t, false, false)
	cases := []struct {
		name   string
		agent  string
		ping   time.Time
		known  bool
		target bool
		want   string
	}{
		{"online", "Online", time.Time{}, true, true, ""},
		{"connection lost", "ConnectionLost", time.Now().Add(-2 * time.Hour), true, true, "hop: SSM agent ConnectionLost on acme-p-tools (last contact 2h ago): the session may fail\n"},
		{"no agent", "", time.Time{}, true, true, "hop: no SSM agent registered on acme-p-tools: the session may fail\n"},
		{"agents unknown", "", time.Time{}, false, true, ""},
		{"no inventory", "", time.Time{}, false, false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := instance("acme-p-tools", "i-01", "acme-prod", "eu-west-1", "running", "prod")
			in.Agent, in.LastPing = c.agent, c.ping
			var targets []*inventory.Target
			if c.target {
				tg := fresh("acme-prod", "eu-west-1", in)
				tg.AgentsKnown = c.known
				targets = append(targets, tg)
			}
			a := newTestApp(t, nil, filters{}, targets...)
			if out := capture(t, &os.Stderr, func() { a.warnAgent(in) }); out != c.want {
				t.Errorf("got %q, want %q", out, c.want)
			}
		})
	}
}

func TestCells(t *testing.T) {
	known := fresh("acme-prod", "eu-west-1")
	unknownAgents := fresh("globex", "us-east-1")
	unknownAgents.AgentsKnown = false
	unknownAgents.Transcript = inventory.Transcript{}
	notLogged := fresh("acme-data", "eu-west-1")
	notLogged.Transcript = inventory.Transcript{Checked: true}
	a := newTestApp(t, nil, filters{}, known, unknownAgents, notLogged)

	mk := func(profile, region, agent string, ping time.Time) inventory.Instance {
		return inventory.Instance{ID: "i-01", Name: "x", Profile: profile, Region: region, State: "running", Agent: agent, LastPing: ping}
	}
	agentCases := []struct {
		in   inventory.Instance
		want cell
	}{
		{mk("acme-prod", "eu-west-1", "Online", time.Time{}), cell{"online", cGreen}},
		{mk("acme-prod", "eu-west-1", "ConnectionLost", time.Now().Add(-3*time.Hour)), cell{"lost 3h ago", cYellow}},
		{mk("acme-prod", "eu-west-1", "Inactive", time.Time{}), cell{"inactive", cYellow}},
		{mk("acme-prod", "eu-west-1", "", time.Time{}), cell{"none", cDim}},
		{mk("globex", "us-east-1", "", time.Time{}), cell{"?", cDim}},
		{mk("acme-retired", "eu-west-1", "", time.Time{}), cell{"?", cDim}},
	}
	for _, c := range agentCases {
		if got := a.agentCell(c.in); got != c.want {
			t.Errorf("agentCell(%s %q) = %+v, want %+v", c.in.Profile, c.in.Agent, got, c.want)
		}
	}
	transcriptCases := []struct {
		profile, region string
		want            cell
	}{
		{"acme-prod", "eu-west-1", cell{"yes", cGreen}},
		{"acme-data", "eu-west-1", cell{"NO", cRed}},
		{"globex", "us-east-1", cell{"?", cDim}},
		{"acme-retired", "eu-west-1", cell{"?", cDim}},
	}
	for _, c := range transcriptCases {
		if got := a.transcriptCell(mk(c.profile, c.region, "", time.Time{})); got != c.want {
			t.Errorf("transcriptCell(%s) = %+v, want %+v", c.profile, got, c.want)
		}
	}
}

func TestTranscriptText(t *testing.T) {
	cases := []struct {
		tr   inventory.Transcript
		want string
	}{
		{inventory.Transcript{}, "not verified"},
		{inventory.Transcript{Checked: true, Enabled: true, Bucket: "acme-logs"}, "yes, to s3://acme-logs"},
		{inventory.Transcript{Checked: true}, "NO (the SSM-SessionManagerRunShell document has no destination)"},
	}
	for _, c := range cases {
		if got := transcriptText(c.tr); got != c.want {
			t.Errorf("%+v: %q, want %q", c.tr, got, c.want)
		}
	}
}

func TestPreview(t *testing.T) {
	in := instance("acme-p-tools", "i-01", "acme-prod", "eu-west-1", "running", "prod")
	in.PrivateIP, in.Platform, in.Agent, in.LastPing = "10.0.0.1", "Amazon Linux", "ConnectionLost", time.Now().Add(-2*time.Hour)
	in.AlsoVia = []string{"acme-data"}
	a := newTestApp(t, &config.Config{Labels: map[string]string{"acme-prod": "ACME"}}, filters{}, fresh("acme-prod", "eu-west-1", in))
	a.recents[in.Key()] = time.Now().Add(-72 * time.Hour)
	row := func(k, v string) string { return fmt.Sprintf("%-13s %s\n", k, v) }
	want := row("name", "acme-p-tools") + row("instance", "i-01") + row("profile", "acme-prod (ACME)") +
		row("region", "eu-west-1") + row("env", "prod") + row("private IP", "10.0.0.1") + row("platform", "Amazon Linux") +
		row("SSM agent", "ConnectionLost, 2h ago") + row("logging", "yes, to /acme/ssm/sessions") + row("inventory", "just now") +
		row("also via", "acme-data") + row("last used", "3d ago")
	if got := a.preview(in); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}

	// Empty fields are left out; an unknown agent is said so.
	bare := inventory.Instance{ID: "i-09", Name: "bastion", Profile: "globex", Region: "us-east-1"}
	got := a.preview(bare)
	if strings.Contains(got, "env") || strings.Contains(got, "private IP") || !strings.Contains(got, row("SSM agent", "unknown")) || strings.Contains(got, "logging") {
		t.Errorf("bare preview:\n%s", got)
	}
	a.inv.Targets[inventory.TargetKey("globex", "us-east-1")] = fresh("globex", "us-east-1")
	if got := a.preview(bare); !strings.Contains(got, row("SSM agent", "none")) {
		t.Errorf("agents known, none registered:\n%s", got)
	}
}

func TestProfileLabel(t *testing.T) {
	a := newTestApp(t, &config.Config{Labels: map[string]string{"acme-prod": "ACME"}}, filters{})
	if a.profileLabel("acme-prod") != "acme-prod (ACME)" || a.profileLabel("globex") != "globex" {
		t.Errorf("%q %q", a.profileLabel("acme-prod"), a.profileLabel("globex"))
	}
}

func TestPickWithoutTerminal(t *testing.T) {
	terminal(t, false, false)
	a := newTestApp(t, nil, filters{})
	items := []inventory.Instance{
		instance("tools", "i-04", "acme-data", "eu-west-1", "running", ""),
		instance("tools", "i-05", "globex", "us-east-1", "running", ""),
	}
	_, err := a.pick(items, "")
	if err == nil || err.Error() != "several instances match, be more specific: tools (acme-data, eu-west-1); tools (globex, us-east-1)" {
		t.Errorf("error %v", err)
	}
}

func TestPrintJSON(t *testing.T) {
	targets := fleetTargets()
	a := newTestApp(t, &config.Config{Labels: map[string]string{"acme-prod": "ACME"}}, filters{}, targets...)
	items := a.instances(false)[:2]
	var err error
	out := capture(t, &os.Stdout, func() { err = a.printJSON(items) })
	if err != nil {
		t.Fatal(err)
	}
	var rows []struct {
		ID         string               `json:"id"`
		Name       string               `json:"name"`
		Profile    string               `json:"profile"`
		Label      string               `json:"label"`
		Transcript inventory.Transcript `json:"transcript"`
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(rows) != 2 || rows[0].ID != "i-01" || rows[0].Label != "ACME" || !rows[0].Transcript.Enabled || rows[1].Label != "" {
		t.Errorf("rows %+v", rows)
	}
	// No instance gives an empty list, not null: scripts can iterate on it.
	out = capture(t, &os.Stdout, func() { a.printJSON(nil) })
	if strings.TrimSpace(out) != "[]" {
		t.Errorf("empty output %q", out)
	}
}

func TestPrintTableAndFooter(t *testing.T) {
	terminal(t, false, false)
	a := newTestApp(t, &config.Config{Labels: map[string]string{"acme-prod": "ACME"}}, filters{}, fleetTargets()...)
	a.inv.Targets[inventory.TargetKey("globex", "us-east-1")].FetchedAt = time.Now().Add(-5 * time.Minute)
	items := a.instances(true)
	table := capture(t, &os.Stdout, func() { a.printTable(items) })
	lines := strings.Split(strings.TrimSuffix(table, "\n"), "\n")
	if len(lines) != 6 || !strings.HasPrefix(lines[0], "  NAME") {
		t.Fatalf("table:\n%s", table)
	}
	// A stopped instance shows its state in place of the agent.
	if !strings.Contains(lines[1], "acme-p-proxy") || !strings.Contains(lines[1], "stopped") || !strings.Contains(lines[1], "acme-prod (ACME)") {
		t.Errorf("stopped row: %q", lines[1])
	}
	if !strings.Contains(lines[2], "online") || !strings.HasSuffix(lines[2], "yes") {
		t.Errorf("running row: %q", lines[2])
	}

	footer := capture(t, &os.Stderr, func() { a.printFooter(items) })
	want := "5 instances, 3 profiles, regions eu-west-1, us-east-1 (cached 5m ago, --refresh to query again)\n"
	if footer != want {
		t.Errorf("footer %q, want %q", footer, want)
	}
	fetchedNow(a)
	footer = capture(t, &os.Stderr, func() { a.printFooter(items[:1]) })
	if footer != "1 instance, 1 profile, regions eu-west-1\n" {
		t.Errorf("footer after a refresh %q", footer)
	}
}

func TestRegionsOf(t *testing.T) {
	items := []inventory.Instance{{Region: "eu-west-1"}, {Region: "us-east-1"}, {Region: "eu-west-1"}, {Region: "af-south-1"}}
	if got := regionsOf(items); !reflect.DeepEqual(got, []string{"eu-west-1", "us-east-1", "af-south-1"}) {
		t.Errorf("%v", got)
	}
	if got := regionsOf(nil); got != nil {
		t.Errorf("%v", got)
	}
}

func TestPluginExit(t *testing.T) {
	if pluginExit(nil) != nil {
		t.Error("nil must stay nil")
	}
	other := errors.New("boom")
	if got := pluginExit(other); !errors.Is(got, other) || got.Error() != "boom" {
		t.Error("other errors pass through")
	}
	// A real exit status, from this test binary run as a helper process.
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "HOP_TEST_EXIT_CODE=3")
	err := pluginExit(cmd.Run())
	var ex exitError
	if !errors.As(err, &ex) || ex.code != 3 || ex.Error() != "exit status 3" {
		t.Errorf("got %v", err)
	}
}

func TestGroupHits(t *testing.T) {
	hits := []scanHit{
		{"acme-prod", "ap-south-1", []string{"a"}},
		{"acme-prod", "us-east-1", []string{"b"}},
		{"globex", "eu-central-1", []string{"c"}},
	}
	want := []profileRegions{{"acme-prod", []string{"ap-south-1", "us-east-1"}}, {"globex", []string{"eu-central-1"}}}
	if got := groupHits(hits); !reflect.DeepEqual(got, want) {
		t.Errorf("%+v", got)
	}
	if got := groupHits(nil); got != nil {
		t.Errorf("%+v", got)
	}
}

func TestFindPortal(t *testing.T) {
	a := newTestApp(t, nil, filters{})
	portals := a.files.Portals(nil)
	cases := map[string]string{
		"acme-sso":                         "acme-sso",
		"acme-data":                        "acme-sso",
		"globex.awsapps.com":               "globex.awsapps.com",
		"https://globex.awsapps.com/start": "globex.awsapps.com",
		"globex":                           "globex.awsapps.com",
	}
	for arg, want := range cases {
		pt, ok := findPortal(portals, arg)
		if !ok || pt.Label() != want {
			t.Errorf("findPortal(%q) = %q, %v; want %q", arg, pt.Label(), ok, want)
		}
	}
	for _, arg := range []string{"acme-billing", "unknown"} {
		if _, ok := findPortal(portals, arg); ok {
			t.Errorf("findPortal(%q) found a portal", arg)
		}
	}
}

func TestDecide(t *testing.T) {
	run := func(name string) inventory.Instance {
		return instance(name, "i-"+name, "acme-prod", "eu-west-1", "running", "prod")
	}
	stopped := func(name string) inventory.Instance {
		return instance(name, "i-"+name+"-old", "acme-prod", "eu-west-1", "stopped", "prod")
	}
	ip := run("acme-p-tools")
	ip.PrivateIP = "10.0.1.21"
	hybrid := instance("acme-onprem", "mi-0123456789abcdef0", "acme-prod", "eu-west-1", "running", "prod")
	hybrid.PrivateIP = "192.168.1.5"
	cases := []struct {
		name        string
		matches     []inventory.Instance
		query       string
		interactive bool
		want        verdict
	}{
		{"nothing", nil, "tools", true, verdictNone},
		{"exact name", []inventory.Instance{run("acme-p-tools")}, "acme-p-tools", false, verdictAct},
		{"exact IP", []inventory.Instance{ip}, "10.0.1.21", false, verdictAct},
		{"every word a word of the name", []inventory.Instance{run("acme-p-shop-app")}, "shop app", false, verdictAct},
		{"part of a word, terminal", []inventory.Instance{run("acme-p-tools")}, "too", true, verdictConfirm},
		{"part of a word, no terminal", []inventory.Instance{run("acme-p-tools")}, "too", false, verdictRefuse},
		{"profile only, no terminal", []inventory.Instance{run("acme-p-tools")}, "acme-prod", false, verdictRefuse},
		{"several, terminal", []inventory.Instance{run("tools"), run("tools")}, "tools", true, verdictPick},
		{"several, no terminal", []inventory.Instance{run("tools"), run("tools")}, "tools", false, verdictRefuse},
		// A stopped twin counts: the user may have meant it.
		{"running and stopped, terminal", []inventory.Instance{run("tools"), stopped("tools")}, "tools", true, verdictPick},
		{"running and stopped, no terminal", []inventory.Instance{run("tools"), stopped("tools")}, "tools", false, verdictRefuse},
		{"stopped only", []inventory.Instance{stopped("tools")}, "tools", true, verdictNotRunning},
		{"several stopped", []inventory.Instance{stopped("tools"), stopped("tools")}, "tools", false, verdictNotRunning},
		// A hybrid node declares its own name and IP: only its ID acts on its own.
		{"hybrid node by name, terminal", []inventory.Instance{hybrid}, "acme-onprem", true, verdictConfirm},
		{"hybrid node by name, no terminal", []inventory.Instance{hybrid}, "acme-onprem", false, verdictRefuse},
		{"hybrid node by IP", []inventory.Instance{hybrid}, "192.168.1.5", false, verdictRefuse},
		{"hybrid node by ID", []inventory.Instance{hybrid}, "mi-0123456789abcdef0", false, verdictAct},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := decide(c.matches, c.query, c.interactive); got != c.want {
				t.Errorf("decide = %v, want %v", got, c.want)
			}
		})
	}
}

func TestResolveNeverGuessesWithoutTerminal(t *testing.T) {
	isolate(t)
	twin := instance("acme-p-etl", "i-13", "acme-data", "eu-west-1", "stopped", "prod")
	ipHost := instance("acme-p-db", "i-14", "acme-data", "eu-west-1", "running", "prod")
	ipHost.PrivateIP = "10.0.1.21"
	cases := []struct {
		name    string
		query   []string
		twin    bool
		wantErr []string
	}{
		{"part of a name", []string{"et"}, false, []string{`"et" only partly matches acme-p-etl (acme-data, eu-west-1)`, "no terminal to confirm"}},
		{"a profile name", []string{"globex"}, false, []string{`"globex" only partly matches tools (globex, us-east-1)`}},
		{"partial IP", []string{"10.0.1.2"}, false, []string{"no instance with IP address 10.0.1.2", "did you mean: 10.0.1.21 (acme-p-db)"}},
		{"truncated ID", []string{"i-0123456789abcdef"}, false, []string{"no instance with ID i-0123456789abcdef"}},
		{"stopped twin", []string{"etl"}, true, []string{"several instances match", "acme-p-etl (acme-data, eu-west-1)", "acme-p-etl (acme-data, eu-west-1, stopped)"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			terminal(t, false, false)
			targets := fleetTargets()
			targets[1].Instances = append(targets[1].Instances, ipHost)
			if c.twin {
				targets[1].Instances = append(targets[1].Instances, twin)
			}
			a := newTestApp(t, nil, filters{}, targets...)
			fetchedNow(a)
			var in inventory.Instance
			var err error
			capture(t, &os.Stderr, func() { in, _, err = a.resolve(context.Background(), c.query, forRun) })
			if err == nil {
				t.Fatalf("resolved to %s (%s), want an error", in.Name, in.ID)
			}
			for _, w := range c.wantErr {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not mention %q", err, w)
				}
			}
		})
	}
}

func TestResolveLiteralWithTerminal(t *testing.T) {
	isolate(t)
	terminal(t, true, true)
	stdin(t, "y\n")
	targets := fleetTargets()
	targets[0].Instances[0].PrivateIP = "10.0.1.21"
	a := newTestApp(t, nil, filters{}, targets...)
	fetchedNow(a)
	var err error
	out := capture(t, &os.Stderr, func() { _, _, err = a.resolve(context.Background(), []string{"10.0.1.2"}, forShell) })
	// No question and no picker: a partial address is not a candidate.
	if err == nil || !strings.HasPrefix(err.Error(), "no instance with IP address 10.0.1.2") || strings.Contains(out, "?") {
		t.Errorf("%v\n%s", err, out)
	}
}

func TestResolvePrivateIPAcrossAccounts(t *testing.T) {
	isolate(t)
	cases := []struct {
		name     string
		why      purpose
		profiles []string
		tty      bool
		answer   string
		wantID   string
		wantErr  string
	}{
		{"run, no terminal", forRun, nil, false, "", "",
			"10.0.1.21 is a private IP and 4 profiles are in scope: other accounts may use it too. Narrow with -p acme-prod (no terminal to confirm)"},
		{"push, no terminal", forPush, nil, false, "", "", "Narrow with -p acme-prod"},
		{"run, default answer", forRun, nil, true, "\n", "", errAborted.Error()},
		{"run, confirmed", forRun, nil, true, "y\n", "i-01", ""},
		// A single profile in scope: the address is unambiguous.
		{"run, one profile", forRun, []string{"acme-prod"}, false, "", "i-01", ""},
		// A shell shows where it lands before anything is typed.
		{"shell, no terminal", forShell, nil, false, "", "i-01", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			terminal(t, c.tty, true)
			if c.answer != "" {
				stdin(t, c.answer)
			}
			targets := fleetTargets()
			targets[0].Instances[0].PrivateIP = "10.0.1.21"
			a := newTestApp(t, nil, filters{profiles: c.profiles}, targets...)
			fetchedNow(a)
			var in inventory.Instance
			var asked bool
			var err error
			prompt := capture(t, &os.Stderr, func() { in, asked, err = a.resolve(context.Background(), []string{"10.0.1.21"}, c.why) })
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Errorf("error %v, want %q", err, c.wantErr)
				}
			} else if err != nil || in.ID != c.wantID || asked != c.tty {
				t.Errorf("got %q (asked %v), %v; want %q", in.ID, asked, err, c.wantID)
			}
			if c.tty && !strings.Contains(prompt, "10.0.1.21 is acme-p-tools (acme-prod, eu-west-1), but private IPs repeat across accounts. Run the command there? [y/N]") {
				t.Errorf("prompt %q", prompt)
			}
		})
	}
}

func TestResolveHybridNode(t *testing.T) {
	isolate(t)
	hybrid := instance("acme-onprem", "mi-0123456789abcdef0", "acme-prod", "eu-west-1", "running", "prod")
	hybrid.PrivateIP = "192.168.1.5"
	cases := []struct {
		name    string
		query   string
		tty     bool
		wantErr string
		prompt  string
	}{
		{"name, no terminal", "acme-onprem", false,
			"acme-onprem (acme-prod, eu-west-1) is a hybrid node: its name and IP are reported by the node itself, give its ID mi-0123456789abcdef0 (no terminal to confirm)", ""},
		{"IP, no terminal", "192.168.1.5", false, "is a hybrid node", ""},
		{"ID, no terminal", "mi-0123456789abcdef0", false, "", ""},
		{"name, terminal", "acme-onprem", true, "",
			"acme-onprem (mi-0123456789abcdef0, acme-prod, eu-west-1) is a hybrid node: its name and IP are reported by the node itself. Connect? [Y/n]"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			terminal(t, c.tty, true)
			if c.tty {
				stdin(t, "\n")
			}
			targets := fleetTargets()
			targets[0].Instances = append(targets[0].Instances, hybrid)
			a := newTestApp(t, nil, filters{}, targets...)
			fetchedNow(a)
			var in inventory.Instance
			var asked bool
			var err error
			prompt := capture(t, &os.Stderr, func() { in, asked, err = a.resolve(context.Background(), []string{c.query}, forShell) })
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Errorf("error %v, want %q", err, c.wantErr)
				}
				return
			}
			if err != nil || in.ID != hybrid.ID || asked != c.tty {
				t.Errorf("got %q (asked %v), %v", in.ID, asked, err)
			}
			if !strings.Contains(prompt, c.prompt) {
				t.Errorf("prompt %q", prompt)
			}
		})
	}
}

func TestResolveConfirmDefaults(t *testing.T) {
	isolate(t)
	cases := []struct {
		why    purpose
		answer string
		wantID string
		suffix string
	}{
		{forShell, "\n", "i-01", "Connect? [Y/n]"},
		{forTunnel, "\n", "i-01", "Open the tunnel? [Y/n]"},
		{forRun, "\n", "", "Run the command there? [y/N]"},
		{forPush, "\n", "", "Copy the file there? [y/N]"},
		{forRun, "y\n", "i-01", "Run the command there? [y/N]"},
		{forShell, "n\n", "", "Connect? [Y/n]"},
	}
	for _, c := range cases {
		t.Run(c.suffix+strings.TrimSpace(c.answer), func(t *testing.T) {
			terminal(t, true, true)
			stdin(t, c.answer)
			a := newTestApp(t, nil, filters{}, fleetTargets()...)
			fetchedNow(a)
			var in inventory.Instance
			var err error
			prompt := capture(t, &os.Stderr, func() { in, _, err = a.resolve(context.Background(), []string{"acme-p-too"}, c.why) })
			if !strings.Contains(prompt, `"acme-p-too" only partly matches acme-p-tools (acme-prod, eu-west-1). `+c.suffix) {
				t.Errorf("prompt %q", prompt)
			}
			if in.ID != c.wantID || (c.wantID == "" && !errors.Is(err, errAborted)) {
				t.Errorf("got %q, %v; want %q", in.ID, err, c.wantID)
			}
		})
	}

	// A typo on run defaults to no as well.
	terminal(t, true, true)
	stdin(t, "\n")
	a := newTestApp(t, nil, filters{}, fleetTargets()...)
	fetchedNow(a)
	var err error
	prompt := capture(t, &os.Stderr, func() { _, _, err = a.resolve(context.Background(), []string{"acme-p-tols"}, forRun) })
	if !errors.Is(err, errAborted) || !strings.Contains(prompt, "Did you mean acme-p-tools (acme-prod, eu-west-1)? [y/N]") {
		t.Errorf("%v\n%s", err, prompt)
	}
}

func TestResolveRefreshesOnAMissOnly(t *testing.T) {
	isolate(t)
	terminal(t, false, false)
	// The cache is a few minutes old: within the TTL, past the freshness window. AWS is
	// out of reach here, so a refresh fails and the cached instances stay.
	aged := func() []*inventory.Target {
		targets := fleetTargets()
		for _, tg := range targets {
			tg.FetchedAt = time.Now().Add(-5 * time.Minute)
		}
		return targets
	}

	// Every word a whole word of the name: hop acts on it without refreshing anything
	// (reach looks that one instance up instead, see TestReach).
	a := newTestApp(t, nil, filters{}, aged()...)
	var in inventory.Instance
	var err error
	capture(t, &os.Stderr, func() { in, _, err = a.resolve(context.Background(), []string{"etl"}, forRun) })
	if err != nil || in.ID != "i-03" || len(a.fetched) != 0 {
		t.Errorf("strong match: %s, %v, fetched %v", in.ID, err, a.fetched)
	}

	// An exact name neither.
	a = newTestApp(t, nil, filters{}, aged()...)
	capture(t, &os.Stderr, func() { in, _, err = a.resolve(context.Background(), []string{"acme-p-etl"}, forRun) })
	if err != nil || in.ID != "i-03" || len(a.fetched) != 0 {
		t.Errorf("exact name: %s, %v, fetched %v", in.ID, err, a.fetched)
	}

	// A partial match is a miss: the pairs past the window are queried again before
	// hop answers, the others are not.
	targets := aged()
	targets[0].FetchedAt = time.Now().Add(-10 * time.Second)
	a = newTestApp(t, nil, filters{}, targets...)
	capture(t, &os.Stderr, func() { _, _, err = a.resolve(context.Background(), []string{"et"}, forRun) })
	if err == nil || !strings.Contains(err.Error(), "only partly matches") {
		t.Errorf("partial match: %v", err)
	}
	want := map[string]bool{"acme-data|eu-west-1": true, "globex|us-east-1": true, "acme-billing|eu-west-1": true}
	if !reflect.DeepEqual(a.fetched, want) {
		t.Errorf("pairs queried on a miss: %v, want %v", a.fetched, want)
	}
}

func TestEnsureCanceled(t *testing.T) {
	isolate(t)
	terminal(t, false, false)
	targets := fleetTargets()
	for _, tg := range targets {
		tg.FetchedAt = time.Now().Add(-time.Hour)
	}
	a := newTestApp(t, nil, filters{}, targets...)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.ensure(ctx, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("ensure: %v", err)
	}
	for _, tg := range a.inv.Targets {
		if tg.Err != "" {
			t.Errorf("%s recorded as an error after Ctrl-C: %s", tg.Key(), tg.Err)
		}
	}
	if len(a.fetched) != 0 {
		t.Errorf("pairs marked as queried: %v", a.fetched)
	}
	if _, err := os.Stat(cachePath(t)); !os.IsNotExist(err) {
		t.Errorf("cache written after Ctrl-C: %v", err)
	}
	// resolve stops there too, instead of opening the picker.
	if _, _, err := a.resolve(ctx, nil, forShell); !errors.Is(err, context.Canceled) {
		t.Errorf("resolve: %v", err)
	}
}

// chainConfig adds assumed roles on top of an SSO profile, of keys and of a
// credential_process, and a role that loops on itself.
const chainConfig = fleetConfig + `
[profile acme-admin]
role_arn = arn:aws:iam::111111111111:role/Admin
source_profile = acme-prod
region = eu-west-1

[profile acme-admin-2]
role_arn = arn:aws:iam::111111111111:role/Admin2
source_profile = acme-admin

[profile billing-admin]
role_arn = arn:aws:iam::444444444444:role/Admin
source_profile = acme-billing

[profile vault]
credential_process = hop-test-missing-helper
region = eu-west-1

[profile vault-admin]
role_arn = arn:aws:iam::555555555555:role/Admin
source_profile = vault

[profile loop]
role_arn = arn:aws:iam::666666666666:role/Admin
source_profile = loop
`

func TestCredentialChains(t *testing.T) {
	a := newTestApp(t, nil, filters{})
	a.files = loadFiles(t, chainConfig, fleetCredentials)
	cases := []struct {
		profile string
		portal  string // empty: not SSO
		process bool
	}{
		{"acme-prod", "acme-sso", false},
		{"globex", "globex.awsapps.com", false},
		{"acme-admin", "acme-sso", false},
		{"acme-admin-2", "acme-sso", false},
		{"acme-billing", "", false},
		{"billing-admin", "", false},
		{"vault", "", true},
		{"vault-admin", "", true},
		{"loop", "", false},
		{"unknown", "", false},
	}
	for _, c := range cases {
		pt, ok := a.ssoPortal(c.profile)
		if ok != (c.portal != "") || pt.Label() != c.portal {
			t.Errorf("ssoPortal(%s) = %q, %v; want %q", c.profile, pt.Label(), ok, c.portal)
		}
		if got := a.usesProcess(c.profile); got != c.process {
			t.Errorf("usesProcess(%s) = %v", c.profile, got)
		}
	}
}

func TestReportProblemsAssumedRoleOnSSO(t *testing.T) {
	terminal(t, false, false)
	a := newTestApp(t, nil, filters{profiles: []string{"acme-admin"}},
		&inventory.Target{Profile: "acme-admin", Region: "eu-west-1", Err: "fictional", ErrKind: inventory.ErrSSOExpired})
	a.files = loadFiles(t, chainConfig, fleetCredentials)
	out := capture(t, &os.Stderr, a.reportProblems)
	if out != "hop: SSO token expired for acme-sso: acme-admin not queried\n     hop login acme-sso\n" {
		t.Errorf("got %q", out)
	}
}

func TestAWSConfigOffersLoginOnlyForSSO(t *testing.T) {
	home := isolate(t)
	terminal(t, true, true)
	stdin(t, "y\ny\n")
	t.Setenv("PATH", t.TempDir()) // no aws CLI: a login attempt would say so
	os.WriteFile(filepath.Join(home, "aws", "config"), []byte(chainConfig), 0o600)
	a := newTestApp(t, nil, filters{})
	a.files = loadFiles(t, chainConfig, fleetCredentials)

	// The helper fails and the SDK says "failed to refresh cached credentials", which
	// reads like an expired SSO token. The profile has no portal: the real error is
	// returned and nothing is offered.
	in := instance("vault-app", "i-21", "vault", "eu-west-1", "running", "")
	var err error
	out := capture(t, &os.Stderr, func() { _, err = a.awsConfig(context.Background(), in) })
	if err == nil || !strings.HasPrefix(err.Error(), "credentials for profile vault: ") || !strings.Contains(err.Error(), "credential_process") {
		t.Errorf("error %v", err)
	}
	if strings.Contains(out, "Sign in again") {
		t.Errorf("SSO login offered for a credential_process profile:\n%s", out)
	}

	// An assumed role on an SSO profile gets the offer, for the right portal.
	in = instance("admin-app", "i-22", "acme-admin", "eu-west-1", "running", "")
	out = capture(t, &os.Stderr, func() { _, err = a.awsConfig(context.Background(), in) })
	if !strings.Contains(out, "SSO token expired for acme-sso. Sign in again now?") || err == nil || !strings.Contains(err.Error(), "aws CLI not found") {
		t.Errorf("assumed role on SSO: %v\n%s", err, out)
	}
}

// fakeAWSCLI puts on PATH an aws command that succeeds without doing anything: hop
// believes the sign-in worked.
func fakeAWSCLI(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake aws command is a shell script")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "aws"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

// chainTargets has a fresh pair for every profile of chainConfig, the role looping on
// itself aside.
func chainTargets() []*inventory.Target {
	var out []*inventory.Target
	for _, p := range []string{"acme-prod", "acme-data", "globex", "acme-billing", "acme-admin", "acme-admin-2", "billing-admin", "vault", "vault-admin"} {
		out = append(out, fresh(p, "eu-west-1"))
	}
	return out
}

func checkInvalidated(t *testing.T, inv *inventory.Inventory) {
	t.Helper()
	// The acme-sso profiles, and the roles assumed on top of them.
	for _, p := range []string{"acme-prod", "acme-data", "acme-admin", "acme-admin-2"} {
		if tg := inv.Targets[inventory.TargetKey(p, "eu-west-1")]; tg.Fresh(time.Hour, time.Now()) || tg.InvalidatedAt.IsZero() {
			t.Errorf("%s not invalidated: %+v", p, tg)
		}
	}
	for _, p := range []string{"globex", "acme-billing", "billing-admin", "vault", "vault-admin"} {
		if tg := inv.Targets[inventory.TargetKey(p, "eu-west-1")]; !tg.Fresh(time.Hour, time.Now()) {
			t.Errorf("%s invalidated by a login to another portal: %+v", p, tg)
		}
	}
}

func TestInvalidatePortal(t *testing.T) {
	a := newTestApp(t, nil, filters{}, chainTargets()...)
	a.files = loadFiles(t, chainConfig, fleetCredentials)
	pt, ok := a.ssoPortal("acme-admin-2")
	if !ok {
		t.Fatal("no portal for acme-admin-2")
	}
	a.invalidatePortal(pt)
	checkInvalidated(t, a.inv)
}

func TestLoginInvalidatesChainedProfiles(t *testing.T) {
	fleetEnv(t)
	terminal(t, false, false)
	fakeAWSCLI(t)
	os.WriteFile(awsconf.ConfigPath(), []byte(chainConfig), 0o600)
	inv := &inventory.Inventory{Version: 1, Targets: map[string]*inventory.Target{}}
	for _, tg := range chainTargets() {
		inv.Targets[tg.Key()] = tg
	}
	rewriteCache(t, inv)

	_, stderr, err := runHop(t, "login", "acme-sso")
	if err != nil || !strings.Contains(stderr, "signing in to SSO portal acme-sso (acme-prod, acme-data)") {
		t.Fatalf("%v\n%s", err, stderr)
	}
	checkInvalidated(t, inventory.Load())
}

func TestAWSConfigLoginInvalidatesChainedProfiles(t *testing.T) {
	isolate(t)
	terminal(t, true, true)
	stdin(t, "y\n")
	fakeAWSCLI(t)
	os.WriteFile(awsconf.ConfigPath(), []byte(chainConfig), 0o600)
	a := newTestApp(t, nil, filters{}, chainTargets()...)
	a.files = loadFiles(t, chainConfig, fleetCredentials)
	in := instance("admin-app", "i-22", "acme-admin-2", "eu-west-1", "running", "")
	var err error
	out := capture(t, &os.Stderr, func() { _, err = a.awsConfig(context.Background(), in) })
	// The fake sign-in leaves no token behind: the credentials still fail, after the
	// cache was invalidated.
	if !strings.Contains(out, "Sign in again now?") || err == nil || !strings.Contains(err.Error(), "still no credentials for acme-admin-2") {
		t.Fatalf("%v\n%s", err, out)
	}
	checkInvalidated(t, a.inv)
	checkInvalidated(t, inventory.Load())
}

func TestAWSConfigWithoutTerminalNeverSignsIn(t *testing.T) {
	fleetEnv(t)
	terminal(t, false, false)
	a := newTestApp(t, nil, filters{})
	in := instance("acme-p-tools", "i-01", "acme-prod", "eu-west-1", "running", "prod")
	var err error
	out := capture(t, &os.Stderr, func() { _, err = a.awsConfig(context.Background(), in) })
	if err == nil || err.Error() != "SSO token expired for acme-sso: hop login acme-sso" || out != "" {
		t.Errorf("%v\n%q", err, out)
	}
}

func TestBackgroundRefresh(t *testing.T) {
	isolate(t)
	a := newTestApp(t, nil, filters{profiles: []string{"shop-*", "acme-prod"}, regions: []string{"us-east-1"}, env: "prod"})
	want := []string{"__refresh", "--profile=shop-*", "--profile=acme-prod", "--region=us-east-1"}
	if got := a.refreshArgs(); !reflect.DeepEqual(got, want) {
		t.Errorf("args %q, want %q", got, want)
	}
	if got := newTestApp(t, nil, filters{}).refreshArgs(); !reflect.DeepEqual(got, []string{"__refresh"}) {
		t.Errorf("no filter: %q", got)
	}

	// A stale credential_process profile is left to the commands typed by hand: the
	// background refresh neither queries it nor starts because of it.
	a = newTestApp(t, nil, filters{profiles: []string{"vault", "vault-admin"}})
	a.files = loadFiles(t, chainConfig, fleetCredentials)
	a.background = true
	if todo := a.stale(false); len(todo) != 0 {
		t.Errorf("background pairs: %+v", todo)
	}
	if err := a.ensure(context.Background(), false); err != nil || len(a.inv.Targets) != 0 || len(a.fetched) != 0 {
		t.Errorf("background ensure: %v, targets %v, fetched %v", err, a.inv.Targets, a.fetched)
	}
	a.background = false
	if todo := a.stale(false); len(todo) != 2 {
		t.Errorf("by hand, credential_process profiles are queried: %+v", todo)
	}
}

func TestLsJSONStaleAfterTheTTL(t *testing.T) {
	a := newTestApp(t, &config.Config{CacheTTL: "15m"}, filters{}, fleetTargets()...)
	a.inv.Targets[inventory.TargetKey("globex", "us-east-1")].FetchedAt = time.Now().Add(-20 * time.Minute)
	out := capture(t, &os.Stdout, func() { a.printJSON(a.instances(false)) })
	var rows []struct {
		ID    string `json:"id"`
		Stale bool   `json:"stale"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Stale != (r.ID == "i-05") || r.Error != "" {
			t.Errorf("row %+v", r)
		}
	}
}

func TestEmptyScopeSaysWhichFilter(t *testing.T) {
	t.Run("include and exclude", func(t *testing.T) {
		fleetEnv(t)
		terminal(t, false, false)
		os.WriteFile(configPath(t), []byte("include: [shop-*]\n"), 0o600)
		_, _, err := runHop(t, "ls")
		if err == nil || err.Error() != "include and exclude in "+configPath(t)+" leave none of the 4 profiles with credentials in scope" {
			t.Errorf("%v", err)
		}
	})
	t.Run("-p without credentials", func(t *testing.T) {
		fleetEnv(t)
		terminal(t, false, false)
		_, _, err := runHop(t, "run", "-p", "default", "tools", "uptime")
		if err == nil || err.Error() != "-p default matches only profiles without credentials (default)" {
			t.Errorf("%v", err)
		}
	})
	t.Run("-e on hop ls", func(t *testing.T) {
		fleetEnv(t)
		terminal(t, false, false)
		_, stderr, err := runHop(t, "ls", "-p", "acme-prod", "-e", "staging")
		if err != nil || !strings.Contains(stderr, "hop: no instances: -e test filters out 2 instances\n") {
			t.Errorf("%v\n%s", err, stderr)
		}
	})

	stoppedOnly := fresh("acme-prod", "eu-west-1", instance("acme-p-proxy", "i-02", "acme-prod", "eu-west-1", "stopped", "prod"))
	cases := []struct {
		name    string
		f       filters
		targets []*inventory.Target
		all     bool
		want    string
	}{
		{"stopped only", filters{profiles: []string{"acme-prod"}}, []*inventory.Target{stoppedOnly}, false, "1 instance not running, see hop ls --all"},
		{"stopped only, --all", filters{profiles: []string{"acme-prod"}}, []*inventory.Target{stoppedOnly}, true, ""},
		{"-e", filters{profiles: []string{"acme-prod"}, env: "test"}, fleetTargets(), false, "-e test filters out 2 instances"},
		{"-r", filters{profiles: []string{"acme-prod", "acme-data"}, regions: []string{"us-east-1"}}, fleetTargets(), false,
			"-r us-east-1 filters out 3 instances in the profiles' own regions"},
		{"-r, nothing elsewhere either", filters{profiles: []string{"acme-billing"}, regions: []string{"us-east-1"}}, fleetTargets(), false, ""},
		{"no instances at all", filters{profiles: []string{"acme-billing"}}, fleetTargets(), false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := newTestApp(t, nil, c.f, c.targets...)
			if got := a.emptyReason(c.all); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}

	t.Run("a search with nothing left to search", func(t *testing.T) {
		isolate(t)
		terminal(t, false, false)
		a := newTestApp(t, nil, filters{profiles: []string{"globex"}, env: "prod"}, fleetTargets()...)
		fetchedNow(a)
		var err error
		capture(t, &os.Stderr, func() { _, _, err = a.resolve(context.Background(), []string{"tools"}, forShell) })
		if err == nil || err.Error() != `no instance matches "tools" (-e prod filters out 1 instance)` {
			t.Errorf("%v", err)
		}
	})

	t.Run("picker without a running instance", func(t *testing.T) {
		isolate(t)
		terminal(t, false, false)
		a := newTestApp(t, nil, filters{profiles: []string{"acme-prod"}}, stoppedOnly)
		fetchedNow(a)
		var err error
		capture(t, &os.Stderr, func() { _, _, err = a.resolve(context.Background(), nil, forShell) })
		if err == nil || err.Error() != "no running instance in scope: 1 instance not running, see hop ls --all" {
			t.Errorf("%v", err)
		}
	})
}

// mfaConfig adds roles assumed with an MFA code: one on the static keys of
// acme-billing, one chained on top of it.
const mfaConfig = fleetConfig + `
[profile billing-mfa]
role_arn = arn:aws:iam::444444444444:role/Admin
source_profile = acme-billing
mfa_serial = arn:aws:iam::444444444444:mfa/alice
region = eu-west-1

[profile billing-mfa-2]
role_arn = arn:aws:iam::444444444444:role/ReadOnly
source_profile = billing-mfa
region = eu-west-1
`

func TestAWSConfigRefusesMFAProfiles(t *testing.T) {
	isolate(t)
	terminal(t, false, false)
	a := newTestApp(t, nil, filters{})
	a.files = loadFiles(t, mfaConfig, fleetCredentials)
	in := instance("billing-app", "i-31", "billing-mfa-2", "eu-west-1", "running", "")
	_, err := a.awsConfig(context.Background(), in)
	want := "profile billing-mfa-2 needs an MFA code (mfa_serial in billing-mfa), and hop does not prompt for MFA codes yet: use a profile without MFA, or one whose credentials come from aws-vault or another credential_process"
	if err == nil || err.Error() != want {
		t.Errorf("error %v", err)
	}
}

func TestDescribeAllIsBounded(t *testing.T) {
	var items []inventory.Instance
	for i := range 8 {
		items = append(items, instance(fmt.Sprintf("web-%d", i), fmt.Sprintf("i-%02d", i), "acme-prod", "eu-west-1", "running", "prod"))
	}
	got := describeAll(items)
	if !strings.HasPrefix(got, "web-0 (acme-prod, eu-west-1); ") || !strings.HasSuffix(got, "web-4 (acme-prod, eu-west-1); and 3 more") {
		t.Errorf("%q", got)
	}
	if got := describeAll(items[:5]); strings.Contains(got, "more") || strings.Count(got, ";") != 4 {
		t.Errorf("five: %q", got)
	}
	if err := severalMatch(items); !strings.Contains(err.Error(), "and 3 more") {
		t.Errorf("severalMatch: %v", err)
	}
}

func TestAWSConfigCanceledAtThePrompt(t *testing.T) {
	// Ctrl-C at "Sign in again now? [Y/n]": no sign-in, the context error (exit 130),
	// even though Enter would answer yes.
	fleetEnv(t) // no aws CLI on PATH: a sign-in would say so
	terminal(t, true, true)
	silentStdin(t)
	a := newTestApp(t, nil, filters{})
	in := instance("acme-p-tools", "i-01", "acme-prod", "eu-west-1", "running", "prod")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	t.Cleanup(func() { os.Stderr = old })
	seen := make(chan string)
	go func() {
		// Cancel as soon as the question shows up, as Ctrl-C would.
		var got strings.Builder
		br := bufio.NewReader(r)
		for {
			b, err := br.ReadByte()
			if err != nil {
				break
			}
			got.WriteByte(b)
			if strings.HasSuffix(got.String(), "[Y/n] ") {
				cancel()
			}
		}
		io.Copy(io.Discard, r)
		seen <- got.String()
	}()
	_, err = a.awsConfig(ctx, in)
	os.Stderr = old
	w.Close()
	out := <-seen
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error %v, want context.Canceled", err)
	}
	if !strings.Contains(out, "Sign in again now?") || strings.Contains(out, "aws CLI not found") {
		t.Errorf("stderr %q", out)
	}
}
