package session

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
)

// fakePlugin puts a session-manager-plugin script first on PATH. It records its
// arguments and the variables hop hands over, then exits with code 7.
func fakePlugin(t *testing.T, shebang string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake plugin is a shell script")
	}
	bin := t.TempDir()
	out := t.TempDir()
	script := shebang + `
printf '%s\n' "$@" > "$HOP_TEST_OUT/args"
printf '%s' "$AWS_SSM_START_SESSION_RESPONSE" > "$HOP_TEST_OUT/response"
printf '%s' "${AWS_SHARED_CREDENTIALS_FILE-unset}" > "$HOP_TEST_OUT/creds"
exit 7
`
	if err := os.WriteFile(filepath.Join(bin, "session-manager-plugin"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("HOP_TEST_OUT", out)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(out, "user-credentials"))
	return out
}

func startSessionOK(op string, req map[string]any) (int, string) {
	switch op {
	case "StartSession":
		return 200, `{"SessionId":"acme-user-0abc","TokenValue":"fictional-session-token","StreamUrl":"wss://ssmmessages.eu-west-1.amazonaws.com/v1/data-channel/acme-user-0abc"}`
	case "TerminateSession":
		return 200, `{"SessionId":"acme-user-0abc"}`
	}
	return 400, `{"__type":"InvalidAction"}`
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestPluginPathFound(t *testing.T) {
	fakePlugin(t, "#!/bin/sh")
	p, err := PluginPath()
	if err != nil || filepath.Base(p) != "session-manager-plugin" {
		t.Errorf("%s, %v", p, err)
	}
}

func TestStartShell(t *testing.T) {
	out := fakePlugin(t, "#!/bin/sh")
	fake, cfg := newFake(t, startSessionOK)
	err := Start(context.Background(), cfg, Request{Profile: "acme-prod", Region: "eu-west-1", InstanceID: "i-0aaa"})

	// The plugin's exit code comes back as is.
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 7 {
		t.Fatalf("want the plugin exit code 7, got %v", err)
	}
	reqs := fake.got("StartSession")
	if len(reqs) != 1 || reqs[0]["Target"] != "i-0aaa" || reqs[0]["DocumentName"] != nil {
		t.Errorf("StartSession request: %v", reqs)
	}

	args := strings.Split(strings.TrimSuffix(readFile(t, filepath.Join(out, "args")), "\n"), "\n")
	if len(args) != 6 {
		t.Fatalf("plugin arguments: %q", args)
	}
	// The plugin calls the endpoint hop's own calls went to: here, the fake.
	if args[0] != "AWS_SSM_START_SESSION_RESPONSE" || args[1] != "eu-west-1" || args[2] != "StartSession" ||
		args[3] != "acme-prod" || args[5] != aws.ToString(cfg.BaseEndpoint) {
		t.Errorf("plugin arguments: %q", args)
	}
	var params map[string]any
	if err := json.Unmarshal([]byte(args[4]), &params); err != nil || params["Target"] != "i-0aaa" || params["DocumentName"] != nil {
		t.Errorf("request parameters %q: %v", args[4], err)
	}
	// The session token travels through the environment, never on the command line
	// where `ps` would show it.
	for _, a := range args {
		if strings.Contains(a, "fictional-session-token") {
			t.Errorf("session token in the plugin arguments: %q", a)
		}
	}
	var resp map[string]string
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(out, "response"))), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["SessionId"] != "acme-user-0abc" || resp["TokenValue"] != "fictional-session-token" || !strings.HasPrefix(resp["StreamUrl"], "wss://") {
		t.Errorf("session response: %v", resp)
	}
	// Without PreferSSO, the user's credentials file is left alone.
	if got := readFile(t, filepath.Join(out, "creds")); got != filepath.Join(out, "user-credentials") {
		t.Errorf("credentials file changed: %q", got)
	}
	if n := len(fake.got("TerminateSession")); n != 0 {
		t.Errorf("session terminated %d times after a normal run", n)
	}
}

func TestStartPortForwardingPreferSSO(t *testing.T) {
	out := fakePlugin(t, "#!/bin/sh")
	fake, cfg := newFake(t, startSessionOK)
	req := Request{
		Profile: "acme-dual", Region: "us-east-1", InstanceID: "i-0bbb", PreferSSO: true,
		Document:   "AWS-StartPortForwardingSessionToRemoteHost",
		Parameters: map[string][]string{"portNumber": {"5432"}, "localPortNumber": {"15432"}, "host": {"db.acme.internal"}},
	}
	Start(context.Background(), cfg, req)

	sent := fake.got("StartSession")
	if len(sent) != 1 || sent[0]["DocumentName"] != req.Document {
		t.Fatalf("StartSession request: %v", sent)
	}
	if p, _ := sent[0]["Parameters"].(map[string]any); p == nil || len(p) != 3 {
		t.Errorf("parameters sent: %v", sent[0]["Parameters"])
	}
	args := strings.Split(strings.TrimSuffix(readFile(t, filepath.Join(out, "args")), "\n"), "\n")
	var params struct {
		Target       string
		DocumentName string
		Parameters   map[string][]string
	}
	if err := json.Unmarshal([]byte(args[4]), &params); err != nil {
		t.Fatal(err)
	}
	if params.Target != "i-0bbb" || params.DocumentName != req.Document || params.Parameters["host"][0] != "db.acme.internal" {
		t.Errorf("plugin parameters: %+v", params)
	}
	if args[5] != aws.ToString(cfg.BaseEndpoint) {
		t.Errorf("endpoint %s", args[5])
	}
	// With PreferSSO, the plugin must not read the keys of the credentials file.
	if got := readFile(t, filepath.Join(out, "creds")); got != os.DevNull {
		t.Errorf("AWS_SHARED_CREDENTIALS_FILE=%q, want %s", got, os.DevNull)
	}
}

func TestStartTerminatesWhenPluginCannotRun(t *testing.T) {
	// The plugin is on PATH but cannot be executed (missing interpreter): the session
	// already opened on the AWS side must be closed.
	fakePlugin(t, "#!/nonexistent/hop-test-interpreter")
	fake, cfg := newFake(t, startSessionOK)
	err := Start(context.Background(), cfg, Request{Profile: "acme-prod", Region: "eu-west-1", InstanceID: "i-0aaa"})
	if err == nil {
		t.Fatal("want an error")
	}
	term := fake.got("TerminateSession")
	if len(term) != 1 || term[0]["SessionId"] != "acme-user-0abc" {
		t.Errorf("TerminateSession calls: %v", term)
	}
}

func TestTerminateWithACanceledContext(t *testing.T) {
	// Ctrl-C may have canceled the caller's context: the session is closed anyway.
	fake, cfg := newFake(t, startSessionOK)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	terminate(ctx, ssm.NewFromConfig(cfg), "acme-user-0abc")
	term := fake.got("TerminateSession")
	if len(term) != 1 || term[0]["SessionId"] != "acme-user-0abc" {
		t.Errorf("TerminateSession calls: %v", term)
	}
}

func TestStartErrors(t *testing.T) {
	t.Run("no plugin", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		fake, cfg := newFake(t, startSessionOK)
		if err := Start(context.Background(), cfg, Request{InstanceID: "i-0aaa", Region: "eu-west-1"}); err == nil || !strings.Contains(err.Error(), "not found") {
			t.Errorf("error %v", err)
		}
		if n := len(fake.got("StartSession")); n != 0 {
			t.Errorf("a session was opened without a plugin to use it (%d calls)", n)
		}
	})
	t.Run("agent not connected", func(t *testing.T) {
		out := fakePlugin(t, "#!/bin/sh")
		_, cfg := newFake(t, func(op string, req map[string]any) (int, string) {
			return 400, `{"__type":"TargetNotConnected","message":"i-0aaa is not connected."}`
		})
		err := Start(context.Background(), cfg, Request{InstanceID: "i-0aaa", Region: "eu-west-1"})
		if err == nil || !strings.Contains(err.Error(), "not connected") {
			t.Errorf("error %v", err)
		}
		if _, err := os.Stat(filepath.Join(out, "args")); err == nil {
			t.Error("the plugin ran after a failed StartSession")
		}
	})
}

// isolateAWSConfig hides the AWS configuration of the machine running the tests.
func isolateAWSConfig(t *testing.T) {
	t.Helper()
	empty := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_CONFIG_FILE", empty)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", empty)
	for _, k := range []string{
		"AWS_PROFILE", "AWS_DEFAULT_PROFILE", "AWS_REGION", "AWS_DEFAULT_REGION",
		"AWS_ENDPOINT_URL", "AWS_ENDPOINT_URL_SSM", "AWS_IGNORE_CONFIGURED_ENDPOINT_URLS",
		"AWS_USE_FIPS_ENDPOINT", "AWS_USE_DUALSTACK_ENDPOINT",
	} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
}

func TestPluginEndpoint(t *testing.T) {
	cases := []struct {
		name   string
		region string
		env    map[string]string
		opts   []func(*config.LoadOptions) error
		want   string
	}{
		{name: "commercial", region: "eu-west-1", want: "https://ssm.eu-west-1.amazonaws.com"},
		{name: "China", region: "cn-north-1", want: "https://ssm.cn-north-1.amazonaws.com.cn"},
		{name: "FIPS", region: "us-east-1", env: map[string]string{"AWS_USE_FIPS_ENDPOINT": "true"}, want: "https://ssm-fips.us-east-1.amazonaws.com"},
		{name: "dual-stack", region: "eu-west-1", env: map[string]string{"AWS_USE_DUALSTACK_ENDPOINT": "true"}, want: "https://ssm.eu-west-1.api.aws"},
		{name: "AWS_ENDPOINT_URL_SSM", region: "eu-west-1", env: map[string]string{"AWS_ENDPOINT_URL_SSM": "https://vpce-0abc.ssm.eu-west-1.vpce.amazonaws.com"},
			want: "https://vpce-0abc.ssm.eu-west-1.vpce.amazonaws.com"},
		{name: "BaseEndpoint", region: "eu-west-1", opts: []func(*config.LoadOptions) error{config.WithBaseEndpoint("http://127.0.0.1:4566")},
			want: "http://127.0.0.1:4566"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			isolateAWSConfig(t)
			for k, v := range c.env {
				t.Setenv(k, v)
			}
			cfg, err := config.LoadDefaultConfig(context.Background(), append(c.opts, config.WithRegion(c.region))...)
			if err != nil {
				t.Fatal(err)
			}
			if got := pluginEndpoint(context.Background(), ssm.NewFromConfig(cfg).Options()); got != c.want {
				t.Errorf("endpoint %s, want %s", got, c.want)
			}
		})
	}
}
