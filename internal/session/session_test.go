package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/smithy-go"
)

func TestShellQuote(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", "''"},
		{"/opt/acme/app.conf", "'/opt/acme/app.conf'"},
		{"/opt/acme/my file.txt", "'/opt/acme/my file.txt'"},
		{"it's", `'it'\''s'`},
		{"''", `''\'''\'''`},
		{"$HOME/`id`/$(whoami)", "'$HOME/`id`/$(whoami)'"},
		{"line1\nline2", "'line1\nline2'"},
		{`back\slash "double"`, `'back\slash "double"'`},
	}
	for _, c := range cases {
		if got := ShellQuote(c.in); got != c.want {
			t.Errorf("ShellQuote(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestShellQuoteWithRealShell(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no POSIX shell on Windows")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not found")
	}
	inputs := []string{
		"",
		"plain",
		"with space",
		"it's a 'quoted' word",
		"line1\nline2\n",
		"$HOME `id` $(whoami) ${PATH}",
		`back\slash "double" ; rm -rf / && echo pwned | cat > x`,
		"tab\there",
		"東京/ファイル",
		"*?[a-z]",
		"-n",
	}
	for _, in := range inputs {
		out, err := exec.Command(sh, "-c", "printf '%s' "+ShellQuote(in)).Output()
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		if string(out) != in {
			t.Errorf("round trip through sh: %q became %q", in, out)
		}
	}
}

func TestExplainStart(t *testing.T) {
	api := func(code string) error {
		return &smithy.OperationError{ServiceID: "SSM", OperationName: "StartSession",
			Err: &smithy.GenericAPIError{Code: code, Message: "fictional detail"}}
	}
	cases := []struct {
		code string
		want []string
	}{
		{"TargetNotConnected", []string{"i-0aaa", "not connected"}},
		{"AccessDeniedException", []string{"i-0aaa", "IAM denied", "fictional detail"}},
		{"InvalidDocument", []string{"not found in this region", "fictional detail"}},
	}
	for _, c := range cases {
		err := explainStart(api(c.code), "i-0aaa")
		for _, w := range c.want {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("%s: %q does not mention %q", c.code, err, w)
			}
		}
	}
	other := api("ThrottlingException")
	if got := explainStart(other, "i-0aaa"); !errors.Is(got, other) || got.Error() != other.Error() {
		t.Errorf("an unknown error must pass through: %v", got)
	}
	plain := errors.New("boom")
	if got := explainStart(plain, "i-0aaa"); !errors.Is(got, plain) || got.Error() != plain.Error() {
		t.Errorf("a non-API error must pass through: %v", got)
	}
}

func TestExplainSend(t *testing.T) {
	api := func(code string) error {
		return &smithy.OperationError{ServiceID: "SSM", OperationName: "SendCommand",
			Err: &smithy.GenericAPIError{Code: code, Message: "fictional detail"}}
	}
	cases := []struct {
		code string
		want []string
	}{
		{"InvalidInstanceId", []string{"i-0aaa", "stopped or terminated", "SSM agent"}},
		{"TargetNotConnected", []string{"i-0aaa", "SSM agent"}},
		{"UnsupportedPlatformType", []string{"i-0aaa", "Linux or macOS", "AWS-RunShellScript"}},
		{"AccessDeniedException", []string{"i-0aaa", "IAM denied", "fictional detail"}},
		{"InvalidDocument", []string{"AWS-RunShellScript", "fictional detail"}},
	}
	for _, c := range cases {
		err := explainSend(api(c.code), "i-0aaa")
		for _, w := range c.want {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("%s: %q does not mention %q", c.code, err, w)
			}
		}
		// Unlike StartSession, SendCommand opens no session.
		if strings.Contains(err.Error(), "session") {
			t.Errorf("%s: %q talks about a session", c.code, err)
		}
	}
	other := api("ThrottlingException")
	if got := explainSend(other, "i-0aaa"); !errors.Is(got, other) {
		t.Errorf("an unknown error must pass through: %v", got)
	}
}

func TestStoppedWaitingMessages(t *testing.T) {
	denied := errors.New("AccessDeniedException: no ssm:CancelCommand")
	cases := []struct {
		name string
		err  error
		want []string
		not  []string
	}{
		{"running", &TimeoutError{CommandID: "c-0abc", InstanceID: "i-0aaa", Waited: 2 * time.Minute, Status: "InProgress"},
			[]string{"still running after 2m0s", "keeps running on i-0aaa", "c-0abc"}, []string{"cancel"}},
		{"never delivered, canceled", &TimeoutError{CommandID: "c-0abc", InstanceID: "i-0aaa", Waited: 2 * time.Minute, Status: "Pending", Canceled: true},
			[]string{"i-0aaa did not pick up the command within 2m0s", "hop canceled it", "c-0abc"}, []string{"keeps running"}},
		{"never delivered, cancel failed", &TimeoutError{CommandID: "c-0abc", InstanceID: "i-0aaa", Waited: 2 * time.Minute, Status: "Delayed", cancelErr: denied},
			[]string{"did not pick up", "canceling it failed", "no ssm:CancelCommand", "delivery timeout", "c-0abc"}, []string{"keeps running"}},
		{"delivery timed out", &TimeoutError{CommandID: "c-0abc", InstanceID: "i-0aaa", Waited: 2 * time.Minute, Status: "TimedOut"},
			[]string{"did not pick up", "expired", "c-0abc"}, []string{"keeps running", "cancel"}},
		{"interrupted while running", &InterruptedError{CommandID: "c-0abc", InstanceID: "i-0aaa", Status: "InProgress", Err: context.Canceled},
			[]string{"interrupted", "keeps running on i-0aaa", "c-0abc"}, []string{"cancel"}},
		{"interrupted before delivery", &InterruptedError{CommandID: "c-0abc", InstanceID: "i-0aaa", Status: "Pending", Canceled: true, Err: context.Canceled},
			[]string{"interrupted before i-0aaa picked up the command", "hop canceled it", "c-0abc"}, []string{"keeps running"}},
	}
	for _, c := range cases {
		msg := c.err.Error()
		for _, w := range c.want {
			if !strings.Contains(msg, w) {
				t.Errorf("%s: %q does not mention %q", c.name, msg, w)
			}
		}
		for _, w := range c.not {
			if strings.Contains(msg, w) {
				t.Errorf("%s: %q mentions %q", c.name, msg, w)
			}
		}
	}
	if err := (&InterruptedError{Err: context.Canceled}); !errors.Is(err, context.Canceled) {
		t.Error("InterruptedError must wrap the context error")
	}
}

func TestDeliveryTimeout(t *testing.T) {
	cases := []struct {
		wait time.Duration
		want int32
	}{
		{-time.Second, 30},
		{0, 30},
		{10 * time.Second, 30},
		{30 * time.Second, 30},
		{90*time.Second + 500*time.Millisecond, 91},
		{2 * time.Minute, 120},
		{40 * 24 * time.Hour, 2_592_000},
	}
	for _, c := range cases {
		if got := deliveryTimeout(c.wait); got != c.want {
			t.Errorf("deliveryTimeout(%s) = %d, want %d", c.wait, got, c.want)
		}
	}
}

func TestPluginPathMissing(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := PluginPath()
	if err == nil || !strings.Contains(err.Error(), "session-manager-plugin not found") || !strings.Contains(err.Error(), pluginInstallHint) {
		t.Errorf("error %v", err)
	}
}

// fakeSSM answers the SSM calls made by Run, Push and Start. It listens on the
// loopback interface only.
type fakeSSM struct {
	mu sync.Mutex
	// requests by operation, decoded from JSON.
	requests map[string][]map[string]any
	// handler returns status and body for an operation; nil answers "{}".
	handler func(op string, req map[string]any) (int, string)
}

func (f *fakeSSM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	op := strings.TrimPrefix(r.Header.Get("X-Amz-Target"), "AmazonSSM.")
	b, _ := io.ReadAll(r.Body)
	var req map[string]any
	json.Unmarshal(b, &req)
	f.mu.Lock()
	if f.requests == nil {
		f.requests = map[string][]map[string]any{}
	}
	f.requests[op] = append(f.requests[op], req)
	h := f.handler
	f.mu.Unlock()
	status, body := http.StatusOK, "{}"
	if h != nil {
		status, body = h(op, req)
	}
	w.Header().Set("Content-Type", "application/x-amz-json-1.1")
	w.WriteHeader(status)
	io.WriteString(w, body)
}

func (f *fakeSSM) got(op string) []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[op]
}

func newFake(t *testing.T, h func(op string, req map[string]any) (int, string)) (*fakeSSM, aws.Config) {
	t.Helper()
	f := &fakeSSM{handler: h}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	cfg := aws.Config{
		Region:           "eu-west-1",
		BaseEndpoint:     aws.String(srv.URL),
		RetryMaxAttempts: 1,
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "AKIAFAKEFAKEFAKE0001", SecretAccessKey: "fictional-secret"}, nil
		}),
	}
	return f, cfg
}

func jsonBody(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func invocation(status string, code int, stdout, stderr string) string {
	return jsonBody(map[string]any{"Status": status, "ResponseCode": code, "StandardOutputContent": stdout, "StandardErrorContent": stderr})
}

func commandsOf(req map[string]any) []string {
	params, _ := req["Parameters"].(map[string]any)
	raw, _ := params["commands"].([]any)
	var out []string
	for _, c := range raw {
		s, _ := c.(string)
		out = append(out, s)
	}
	return out
}

// fastPolls shortens the pause between two GetCommandInvocation calls for one test.
func fastPolls(t *testing.T) {
	t.Helper()
	saved := pollInterval
	pollInterval = 5 * time.Millisecond
	t.Cleanup(func() { pollInterval = saved })
}

func TestRun(t *testing.T) {
	cases := []struct {
		name   string
		inv    string
		status string
		code   int32
		trunc  bool
	}{
		{"success", invocation("Success", 0, "up 3 days\n", ""), "Success", 0, false},
		{"remote failure keeps the exit code", invocation("Failed", 3, "", "no such file\n"), "Failed", 3, false},
		{"output at the SSM limit is flagged", invocation("Success", 0, strings.Repeat("x", outputLimit), ""), "Success", 0, true},
		{"stderr at the SSM limit is flagged", invocation("Failed", 1, "", strings.Repeat("e", outputLimit)), "Failed", 1, true},
		{"just under the limit", invocation("Success", 0, strings.Repeat("x", outputLimit-1), ""), "Success", 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fake, cfg := newFake(t, func(op string, req map[string]any) (int, string) {
				switch op {
				case "SendCommand":
					return 200, `{"Command":{"CommandId":"c-0abc"}}`
				case "GetCommandInvocation":
					return 200, c.inv
				}
				return 400, `{"__type":"InvalidAction"}`
			})
			out, err := Run(context.Background(), cfg, "i-0aaa", []string{"uptime", "echo 'quoted \"text\"'"}, 5*time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if out.CommandID != "c-0abc" || out.Status != c.status || out.ExitCode != c.code || out.Truncated != c.trunc {
				t.Errorf("output %+v", out)
			}
			sent := fake.got("SendCommand")
			if len(sent) != 1 {
				t.Fatalf("SendCommand calls: %d", len(sent))
			}
			if sent[0]["DocumentName"] != "AWS-RunShellScript" || sent[0]["Comment"] != "hop" {
				t.Errorf("request %v", sent[0])
			}
			// The delivery timeout matches the wait: a command the instance has not
			// picked up by then must not run later.
			if sent[0]["TimeoutSeconds"] != float64(300) {
				t.Errorf("TimeoutSeconds %v, want 300", sent[0]["TimeoutSeconds"])
			}
			// Within the document's default hour, its default applies.
			if params, _ := sent[0]["Parameters"].(map[string]any); params["executionTimeout"] != nil {
				t.Errorf("executionTimeout %v for a 5-minute wait", params["executionTimeout"])
			}
			if ids, _ := sent[0]["InstanceIds"].([]any); len(ids) != 1 || ids[0] != "i-0aaa" {
				t.Errorf("instance ids %v", sent[0]["InstanceIds"])
			}
			if got := commandsOf(sent[0]); strings.Join(got, "\n") != "uptime\necho 'quoted \"text\"'" {
				t.Errorf("commands %q", got)
			}
			if n := len(fake.got("CancelCommand")); n != 0 {
				t.Errorf("%d CancelCommand calls after a finished command", n)
			}
		})
	}
}

func TestRunWaitsForTheInvocation(t *testing.T) {
	fastPolls(t)
	// Right after SendCommand, the invocation does not exist yet: Run polls again.
	polls := 0
	var mu sync.Mutex
	_, cfg := newFake(t, func(op string, req map[string]any) (int, string) {
		if op == "SendCommand" {
			return 200, `{"Command":{"CommandId":"c-0abc"}}`
		}
		mu.Lock()
		defer mu.Unlock()
		polls++
		switch polls {
		case 1:
			return 400, `{"__type":"InvocationDoesNotExist","message":"not yet"}`
		case 2:
			return 200, invocation("Pending", -1, "", "")
		case 3:
			return 200, invocation("InProgress", -1, "", "")
		}
		return 200, invocation("Success", 0, "done\n", "")
	})
	out, err := Run(context.Background(), cfg, "i-0aaa", []string{"true"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if out.Stdout != "done\n" || polls != 4 {
		t.Errorf("output %+v after %d polls", out, polls)
	}
}

// stalled answers GetCommandInvocation with the same status forever: an empty status
// stands for an invocation that does not exist yet. cancel is the CancelCommand answer.
func stalled(status, details string, cancel func() (int, string)) func(op string, req map[string]any) (int, string) {
	return func(op string, req map[string]any) (int, string) {
		switch op {
		case "SendCommand":
			return 200, `{"Command":{"CommandId":"c-0abc"}}`
		case "GetCommandInvocation":
			if status == "" {
				return 400, `{"__type":"InvocationDoesNotExist","message":"not yet"}`
			}
			return 200, jsonBody(map[string]any{"Status": status, "StatusDetails": details, "ResponseCode": -1})
		case "CancelCommand":
			return cancel()
		}
		return 400, `{"__type":"InvalidAction"}`
	}
}

func cancelOK() (int, string) { return 200, `{}` }

func cancelDenied() (int, string) {
	return 400, `{"__type":"AccessDeniedException","message":"no ssm:CancelCommand"}`
}

func TestRunStopsWaiting(t *testing.T) {
	cases := []struct {
		name      string
		status    string
		details   string
		cancel    func() (int, string)
		interrupt bool
		// cancels is the number of CancelCommand calls expected.
		cancels  int
		canceled bool
		want     string
	}{
		{"timeout while running", "InProgress", "In Progress", cancelOK, false, 0, false, "keeps running on i-0aaa"},
		{"timeout before the invocation exists", "", "", cancelOK, false, 1, true, "hop canceled it"},
		{"timeout while pending", "Pending", "Pending", cancelOK, false, 1, true, "hop canceled it"},
		{"timeout while delayed", "Delayed", "Delayed", cancelOK, false, 1, true, "hop canceled it"},
		{"timeout, cancel denied", "Pending", "Pending", cancelDenied, false, 1, false, "no ssm:CancelCommand"},
		{"delivery timed out on the SSM side", "TimedOut", "Delivery Timed Out", cancelOK, false, 0, false, "expired"},
		{"interrupted while running", "InProgress", "In Progress", cancelOK, true, 0, false, "keeps running on i-0aaa"},
		{"interrupted while pending", "Pending", "Pending", cancelOK, true, 1, true, "hop canceled it"},
		{"interrupted before the invocation exists", "", "", cancelOK, true, 1, true, "hop canceled it"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fastPolls(t)
			fake, cfg := newFake(t, stalled(c.status, c.details, c.cancel))
			ctx, wait := context.Background(), time.Duration(0)
			if c.interrupt {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 50*time.Millisecond)
				defer cancel()
				wait = time.Hour
			}
			start := time.Now()
			out, err := Run(ctx, cfg, "i-0aaa", []string{"sleep 600"}, wait)
			if out != nil {
				t.Errorf("output %+v", out)
			}
			if time.Since(start) > 5*time.Second {
				t.Errorf("stopping took %s", time.Since(start))
			}
			if err == nil || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "c-0abc") {
				t.Errorf("error %v, want it to mention %q and the command id", err, c.want)
			}
			var te *TimeoutError
			var ie *InterruptedError
			switch {
			case c.interrupt:
				if !errors.As(err, &ie) || !errors.Is(err, context.DeadlineExceeded) || ie.Canceled != c.canceled {
					t.Errorf("want an InterruptedError wrapping the context error, canceled=%v: %#v", c.canceled, err)
				}
			case !errors.As(err, &te) || te.Canceled != c.canceled || te.InstanceID != "i-0aaa":
				t.Errorf("want a TimeoutError, canceled=%v: %#v", c.canceled, err)
			}
			calls := fake.got("CancelCommand")
			if len(calls) != c.cancels {
				t.Fatalf("%d CancelCommand calls, want %d", len(calls), c.cancels)
			}
			// The cancellation goes out even when the context is already canceled.
			for _, call := range calls {
				if ids, _ := call["InstanceIds"].([]any); call["CommandId"] != "c-0abc" || len(ids) != 1 || ids[0] != "i-0aaa" {
					t.Errorf("CancelCommand request %v", call)
				}
			}
		})
	}
}

func TestRunErrors(t *testing.T) {
	t.Run("SendCommand rejected", func(t *testing.T) {
		fake, cfg := newFake(t, func(op string, req map[string]any) (int, string) {
			return 400, `{"__type":"AccessDeniedException","message":"no ssm:SendCommand"}`
		})
		_, err := Run(context.Background(), cfg, "i-0aaa", []string{"true"}, time.Minute)
		if err == nil || !strings.Contains(err.Error(), "IAM denied") || !strings.Contains(err.Error(), "i-0aaa") {
			t.Errorf("error %v", err)
		}
		if len(fake.got("GetCommandInvocation")) != 0 {
			t.Error("polled after a failed SendCommand")
		}
	})
	t.Run("instance not managed", func(t *testing.T) {
		_, cfg := newFake(t, func(op string, req map[string]any) (int, string) {
			return 400, `{"__type":"InvalidInstanceId","message":"Instances [[i-0aaa]] not in a valid state"}`
		})
		_, err := Run(context.Background(), cfg, "i-0aaa", []string{"true"}, time.Minute)
		if err == nil || !strings.Contains(err.Error(), "SSM agent") {
			t.Errorf("error %v", err)
		}
	})
	t.Run("polling fails", func(t *testing.T) {
		// An error that waiting does not fix: hop stops following the command, cancels
		// it since it was never seen on the instance, and says which command it was.
		fake, cfg := newFake(t, func(op string, req map[string]any) (int, string) {
			switch op {
			case "SendCommand":
				return 200, `{"Command":{"CommandId":"c-0abc"}}`
			case "CancelCommand":
				return 200, `{}`
			}
			return 400, `{"__type":"InvalidCommandId","message":"gone"}`
		})
		_, err := Run(context.Background(), cfg, "i-0aaa", []string{"true"}, time.Minute)
		var api smithy.APIError
		if !errors.As(err, &api) || api.ErrorCode() != "InvalidCommandId" {
			t.Errorf("error %v", err)
		}
		var pe *PollError
		if !errors.As(err, &pe) || !pe.Canceled || !strings.Contains(err.Error(), "c-0abc") || !strings.Contains(err.Error(), "hop canceled it") {
			t.Errorf("want a PollError naming the command, canceled: %v", err)
		}
		if n := len(fake.got("CancelCommand")); n != 1 {
			t.Errorf("%d CancelCommand calls", n)
		}
		if n := len(fake.got("GetCommandInvocation")); n != 1 {
			t.Errorf("polled %d times after a permanent error", n)
		}
	})
	t.Run("polling fails while the command runs", func(t *testing.T) {
		polls := 0
		var mu sync.Mutex
		fake, cfg := newFake(t, func(op string, req map[string]any) (int, string) {
			switch op {
			case "SendCommand":
				return 200, `{"Command":{"CommandId":"c-0abc"}}`
			case "GetCommandInvocation":
				mu.Lock()
				defer mu.Unlock()
				if polls++; polls == 1 {
					return 200, invocation("InProgress", -1, "", "")
				}
				return 400, `{"__type":"AccessDeniedException","message":"no ssm:GetCommandInvocation"}`
			}
			return 200, `{}`
		})
		fastPolls(t)
		_, err := Run(context.Background(), cfg, "i-0aaa", []string{"sleep 60"}, time.Minute)
		var pe *PollError
		if !errors.As(err, &pe) || pe.Canceled || !strings.Contains(err.Error(), "may still run") || !strings.Contains(err.Error(), "c-0abc") {
			t.Errorf("error %v", err)
		}
		if n := len(fake.got("CancelCommand")); n != 0 {
			t.Errorf("a running command was canceled (%d calls)", n)
		}
	})
}

func TestRunPollsThroughTransientErrors(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"throttled", 400, `{"__type":"ThrottlingException","message":"Rate exceeded"}`},
		{"too many requests", 429, `{"__type":"TooManyRequestsException","message":"slow down"}`},
		{"server error", 500, `{"__type":"InternalServerError","message":"try again"}`},
		{"unavailable", 503, `{"message":"unavailable"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fastPolls(t)
			polls := 0
			var mu sync.Mutex
			fake, cfg := newFake(t, func(op string, req map[string]any) (int, string) {
				if op == "SendCommand" {
					return 200, `{"Command":{"CommandId":"c-0abc"}}`
				}
				mu.Lock()
				defer mu.Unlock()
				switch polls++; polls {
				case 1:
					return 200, invocation("InProgress", -1, "", "")
				case 2, 3:
					return c.status, c.body
				}
				return 200, invocation("Success", 0, "done\n", "")
			})
			out, err := Run(context.Background(), cfg, "i-0aaa", []string{"true"}, time.Minute)
			if err != nil || out.Stdout != "done\n" {
				t.Fatalf("output %+v, error %v", out, err)
			}
			if n := len(fake.got("CancelCommand")); n != 0 {
				t.Errorf("%d CancelCommand calls", n)
			}
		})
	}
	t.Run("network down until the wait is over", func(t *testing.T) {
		fastPolls(t)
		fake, cfg := newFake(t, func(op string, req map[string]any) (int, string) {
			if op == "SendCommand" {
				return 200, `{"Command":{"CommandId":"c-0abc"}}`
			}
			return 200, `{}`
		})
		// The commands go through, the status checks do not: a server that is gone.
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.Header.Get("X-Amz-Target"), "GetCommandInvocation") {
				hj, ok := w.(http.Hijacker)
				if !ok {
					t.Error("no hijacker")
					return
				}
				conn, _, _ := hj.Hijack()
				conn.Close()
				return
			}
			fake.ServeHTTP(w, r)
		}))
		defer srv.Close()
		cfg.BaseEndpoint = aws.String(srv.URL)
		_, err := Run(context.Background(), cfg, "i-0aaa", []string{"true"}, 100*time.Millisecond)
		var te *TimeoutError
		if !errors.As(err, &te) || !te.Canceled || !strings.Contains(err.Error(), "the last status check failed") || !strings.Contains(err.Error(), "c-0abc") {
			t.Errorf("error %v", err)
		}
		if n := len(fake.got("CancelCommand")); n != 1 {
			t.Errorf("%d CancelCommand calls", n)
		}
	})
}

func TestExecutionTimeout(t *testing.T) {
	cases := []struct {
		wait time.Duration
		want string // empty: the document's default
	}{
		{0, ""},
		{2 * time.Minute, ""},
		{time.Hour, ""},
		{time.Hour + time.Second, "3601"},
		{90*time.Minute + 500*time.Millisecond, "5401"},
		{3 * time.Hour, "10800"},
		{48 * time.Hour, "172800"},
		{100 * time.Hour, "172800"},
	}
	for _, c := range cases {
		got, ok := executionTimeout(c.wait)
		if got != c.want || ok != (c.want != "") {
			t.Errorf("executionTimeout(%s) = %q, %v; want %q", c.wait, got, ok, c.want)
		}
	}
}

func TestRunLongerThanAnHour(t *testing.T) {
	fake, cfg := newFake(t, func(op string, req map[string]any) (int, string) {
		if op == "SendCommand" {
			return 200, `{"Command":{"CommandId":"c-0abc"}}`
		}
		return 200, invocation("Success", 0, "", "")
	})
	if _, err := Run(context.Background(), cfg, "i-0aaa", []string{"make all"}, 3*time.Hour); err != nil {
		t.Fatal(err)
	}
	sent := fake.got("SendCommand")
	params, _ := sent[0]["Parameters"].(map[string]any)
	// A string, as every parameter of an SSM document.
	if got, _ := params["executionTimeout"].([]any); len(got) != 1 || got[0] != "10800" {
		t.Errorf("executionTimeout %#v, want [\"10800\"]", params["executionTimeout"])
	}
	if sent[0]["TimeoutSeconds"] != float64(10800) {
		t.Errorf("TimeoutSeconds %v", sent[0]["TimeoutSeconds"])
	}
}

func TestCheckPush(t *testing.T) {
	dir := t.TempDir()
	small := filepath.Join(dir, "app.conf")
	os.WriteFile(small, []byte("listen 8080\n"), 0o600)
	limit := filepath.Join(dir, "limit.bin")
	os.WriteFile(limit, make([]byte, PushLimit), 0o600)
	big := filepath.Join(dir, "big.bin")
	os.WriteFile(big, make([]byte, PushLimit+1), 0o600)
	cases := []struct {
		local, remote string
		want          string // empty: accepted
	}{
		{small, "/opt/acme/app.conf", ""},
		{limit, "/opt/acme/limit.bin", ""},
		{big, "/opt/acme/big.bin", "too large"},
		{small, "app.conf", "absolute"},
		// The remote path is checked first: it needs no file system access.
		{filepath.Join(dir, "missing"), "relative", "absolute"},
		{filepath.Join(dir, "missing"), "/opt/acme/x", "missing"},
	}
	for _, c := range cases {
		err := CheckPush(c.local, c.remote)
		if (c.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.want)) {
			t.Errorf("CheckPush(%s, %s) = %v, want %q", filepath.Base(c.local), c.remote, err, c.want)
		}
	}
}

func TestPushRejectsBeforeAnyCall(t *testing.T) {
	fake, cfg := newFake(t, nil)
	dir := t.TempDir()
	big := filepath.Join(dir, "big.bin")
	os.WriteFile(big, make([]byte, PushLimit+1), 0o600)
	if _, err := Push(context.Background(), cfg, "i-0aaa", big, "/opt/acme/big.bin"); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Errorf("file over the limit: %v", err)
	}
	if _, err := Push(context.Background(), cfg, "i-0aaa", filepath.Join(dir, "missing"), "/opt/acme/x"); err == nil {
		t.Error("missing local file: want an error")
	}
	small := filepath.Join(dir, "app.conf")
	os.WriteFile(small, []byte("listen 8080\n"), 0o600)
	// A relative path would land in the agent's working directory for the command.
	for _, remote := range []string{"", "app.conf", "./app.conf", "../etc/app.conf", "~/app.conf", "-rf", `C:\acme\app.conf`} {
		if _, err := Push(context.Background(), cfg, "i-0aaa", small, remote); err == nil || !strings.Contains(err.Error(), "absolute") {
			t.Errorf("remote path %q: %v", remote, err)
		}
	}
	if n := len(fake.got("SendCommand")); n != 0 {
		t.Errorf("%d SendCommand calls", n)
	}
}

// posixTools skips the test when the local machine cannot play a Linux or macOS
// instance.
func posixTools(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("no POSIX shell on Windows")
	}
	for _, tool := range []string{"sh", "base64", "cut", "cat"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " not found")
		}
	}
	found := false
	for _, tool := range []string{"sha256sum", "sha256", "shasum"} {
		if _, err := exec.LookPath(tool); err == nil {
			found = true
		}
	}
	if !found {
		t.Skip("no sha256sum, sha256 or shasum")
	}
}

// remoteShell plays the instance: it runs the commands received by SendCommand with
// the local sh, as AWS-RunShellScript would, with TMPDIR pointing to a temporary folder
// so that the files Push leaves behind can be checked.
func remoteShell(t *testing.T) (*fakeSSM, aws.Config, string) {
	t.Helper()
	posixTools(t)
	remoteTmp := t.TempDir()
	var mu sync.Mutex
	var result string
	fake, cfg := newFake(t, func(op string, req map[string]any) (int, string) {
		switch op {
		case "SendCommand":
			script := append([]string{"export TMPDIR=" + ShellQuote(remoteTmp)}, commandsOf(req)...)
			stdout, stderr, code := runScript(script)
			status := "Success"
			if code != 0 {
				status = "Failed"
			}
			mu.Lock()
			result = invocation(status, code, stdout, stderr)
			mu.Unlock()
			return 200, `{"Command":{"CommandId":"c-0abc"}}`
		case "GetCommandInvocation":
			mu.Lock()
			defer mu.Unlock()
			return 200, result
		}
		return 400, `{"__type":"InvalidAction"}`
	})
	return fake, cfg, remoteTmp
}

// runScript runs commands with the local sh and returns their output and exit code.
// The script goes through stdin, as the SSM agent runs it from a file: as a single
// argument (sh -c), Linux would refuse anything over 128 KiB.
func runScript(script []string) (stdout, stderr string, code int) {
	cmd := exec.Command("sh", "-s")
	cmd.Stdin = strings.NewReader(strings.Join(script, "\n") + "\n")
	var out, errOut strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		code = 1
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		}
	}
	return out.String(), errOut.String(), code
}

func TestPushEndToEnd(t *testing.T) {
	sizes := []int{
		1,
		2100,   // exactly one 2800-character base64 chunk
		2101,   // one byte into the second chunk
		10_000, // several chunks
		PushLimit,
	}
	for _, size := range sizes {
		t.Run(strconv.Itoa(size)+" bytes", func(t *testing.T) {
			fake, cfg, remoteTmp := remoteShell(t)
			local := filepath.Join(t.TempDir(), "payload.bin")
			data := make([]byte, size)
			for i := range data {
				data[i] = byte(i*7 + i/251)
			}
			os.WriteFile(local, data, 0o600)
			// A quote, a backslash and a newline: GNU sha256sum escapes the last two
			// when it prints a file name.
			dest := filepath.Join(t.TempDir(), "it's a \\ dir\nhere", "my file.bin")
			os.MkdirAll(filepath.Dir(dest), 0o700)

			out, err := Push(context.Background(), cfg, "i-0aaa", local, dest)
			if err != nil {
				t.Fatalf("push: %v (output %+v)", err, out)
			}
			got, err := os.ReadFile(dest)
			if err != nil || string(got) != string(data) {
				t.Errorf("remote copy differs: %d bytes, want %d (%v)", len(got), len(data), err)
			}
			if left, _ := filepath.Glob(filepath.Join(remoteTmp, ".hop-push.*")); len(left) != 0 {
				t.Errorf("temporary file left on the instance: %v", left)
			}
			cmds := commandsOf(fake.got("SendCommand")[0])
			if cmds[0] != "set -e" {
				t.Errorf("the script must stop on the first error: %q", cmds[0])
			}
			for _, c := range cmds {
				if len(c) > 3000 {
					t.Errorf("command of %d characters", len(c))
				}
			}
		})
	}
}

func TestPushEmptyFile(t *testing.T) {
	_, cfg, remoteTmp := remoteShell(t)
	local := filepath.Join(t.TempDir(), "empty")
	os.WriteFile(local, nil, 0o600)
	dest := filepath.Join(t.TempDir(), "empty")
	if out, err := Push(context.Background(), cfg, "i-0aaa", local, dest); err != nil {
		t.Fatalf("push: %v (output %+v)", err, out)
	}
	if st, err := os.Stat(dest); err != nil || st.Size() != 0 {
		t.Errorf("remote file: %v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(remoteTmp, ".hop-push.*")); len(left) != 0 {
		t.Errorf("temporary file left on the instance: %v", left)
	}
}

func TestPushWithBSDBase64(t *testing.T) {
	// Meaningful on a macOS host, where the local base64 is the one of an EC2 Mac
	// instance: it reads only stdin or -i.
	_, cfg, _ := remoteShell(t)
	local := filepath.Join(t.TempDir(), "app.conf")
	os.WriteFile(local, []byte("listen 8080\n"), 0o600)
	if out, err := Push(context.Background(), cfg, "i-0aaa", local, filepath.Join(t.TempDir(), "app.conf")); err != nil {
		t.Errorf("push: %v (output %+v)", err, out)
	}
}

func TestPushScript(t *testing.T) {
	data := []byte("listen 8080\n")
	cmds := pushScript(data, "/opt/acme/app.conf")
	if cmds[0] != "set -e" {
		t.Errorf("the script must stop on the first error: %q", cmds[0])
	}
	mkdir := slices.IndexFunc(cmds, func(c string) bool { return strings.Contains(c, "mktemp -d") })
	trap := slices.IndexFunc(cmds, func(c string) bool { return strings.HasPrefix(c, "trap ") })
	create := slices.IndexFunc(cmds, func(c string) bool { return strings.HasPrefix(c, `: > "$d/`) })
	if mkdir < 0 || trap < 0 || create < 0 || mkdir > trap || trap > create {
		t.Errorf("private directory, then the trap, then the files: %q", cmds)
	}
	for _, c := range cmds {
		// umask 077 in the script itself would make a new destination root-only.
		if strings.Contains(c, "umask") && !strings.HasPrefix(c, "d=$(umask 077 && mktemp -d ") {
			t.Errorf("umask outside a subshell: %q", c)
		}
		// A predictable name in /tmp lets another local user swap the file.
		if strings.Contains(c, "/tmp/") && !strings.Contains(c, "mktemp") {
			t.Errorf("fixed path in /tmp: %q", c)
		}
		if strings.Contains(c, "/opt/acme") && c != "dst='/opt/acme/app.conf'" {
			t.Errorf("the destination must go through $dst: %q", c)
		}
	}
	if again := pushScript(data, "/opt/acme/app.conf"); !slices.Equal(again, cmds) {
		t.Error("pushScript is not deterministic")
	}
}

func TestPushScriptWithRealShell(t *testing.T) {
	posixTools(t)
	sum := func(b []byte) string {
		s := sha256.Sum256(b)
		return hex.EncodeToString(s[:])
	}
	// The script runs with umask 022, as AWS-RunShellScript does as root. The probe,
	// right after the temporary files are created, checks that they are private.
	run := func(t *testing.T, data []byte, dst string) (string, string, int, string) {
		t.Helper()
		tmpDir := t.TempDir()
		cmds := pushScript(data, dst)
		create := slices.IndexFunc(cmds, func(c string) bool { return strings.HasPrefix(c, `: > "$d/`) })
		probe := `[ "$(ls -ld "$d" | cut -c1-10)" = "drwx------" ] || { echo "$d is not private" >&2; exit 9; }`
		cmds = slices.Insert(cmds, create+1, probe)
		stdout, stderr, code := runScript(append([]string{"umask 022", "export TMPDIR=" + ShellQuote(tmpDir)}, cmds...))
		left, _ := filepath.Glob(filepath.Join(tmpDir, ".hop-push.*"))
		if len(left) != 0 {
			t.Errorf("temporary files left behind: %v", left)
		}
		return strings.TrimSpace(stdout), stderr, code, tmpDir
	}

	t.Run("new file gets the default mode", func(t *testing.T) {
		dst := filepath.Join(t.TempDir(), "app.conf")
		data := []byte("listen 8080\n")
		got, stderr, code, _ := run(t, data, dst)
		if code != 0 || got != sum(data) {
			t.Fatalf("exit %d, output %q, stderr %q", code, got, stderr)
		}
		if st, err := os.Stat(dst); err != nil || st.Mode().Perm() != 0o644 {
			t.Errorf("new file: %v, %v (want 0644)", st.Mode(), err)
		}
	})
	t.Run("existing file keeps its mode", func(t *testing.T) {
		dst := filepath.Join(t.TempDir(), "app.conf")
		os.WriteFile(dst, []byte("old content, longer than the new one\n"), 0o640)
		os.Chmod(dst, 0o640)
		data := []byte("new\n")
		got, stderr, code, _ := run(t, data, dst)
		if code != 0 || got != sum(data) {
			t.Fatalf("exit %d, output %q, stderr %q", code, got, stderr)
		}
		b, _ := os.ReadFile(dst)
		st, err := os.Stat(dst)
		if string(b) != "new\n" || err != nil || st.Mode().Perm() != 0o640 {
			t.Errorf("content %q, mode %v, %v (want 0640)", b, st.Mode(), err)
		}
	})
	t.Run("empty file", func(t *testing.T) {
		dst := filepath.Join(t.TempDir(), "empty")
		got, stderr, code, _ := run(t, nil, dst)
		if code != 0 || got != sum(nil) {
			t.Fatalf("exit %d, output %q, stderr %q", code, got, stderr)
		}
		if st, err := os.Stat(dst); err != nil || st.Size() != 0 {
			t.Errorf("remote file: %v", err)
		}
	})
	t.Run("failure cleans up", func(t *testing.T) {
		dst := filepath.Join(t.TempDir(), "missing-dir", "app.conf")
		got, _, code, _ := run(t, []byte("listen 8080\n"), dst)
		if code == 0 || got != "" {
			t.Errorf("exit %d, output %q: the script must stop before hashing", code, got)
		}
	})
}

func TestPushChecksum(t *testing.T) {
	cases := []struct {
		stdout string
		want   string
	}{
		{strings.Repeat("0", 64) + "\n", "checksum mismatch"},
		{"", "no SHA-256"},
	}
	for _, c := range cases {
		_, cfg := newFake(t, func(op string, req map[string]any) (int, string) {
			if op == "SendCommand" {
				return 200, `{"Command":{"CommandId":"c-0abc"}}`
			}
			return 200, invocation("Success", 0, c.stdout, "")
		})
		local := filepath.Join(t.TempDir(), "app.conf")
		os.WriteFile(local, []byte("listen 8080\n"), 0o600)
		out, err := Push(context.Background(), cfg, "i-0aaa", local, "/opt/acme/app.conf")
		if err == nil || !strings.Contains(err.Error(), c.want) || out == nil {
			t.Errorf("stdout %q: error %v, output %+v", c.stdout, err, out)
		}
	}
}

func TestPushRemoteFailure(t *testing.T) {
	_, cfg := newFake(t, func(op string, req map[string]any) (int, string) {
		if op == "SendCommand" {
			return 200, `{"Command":{"CommandId":"c-0abc"}}`
		}
		return 200, invocation("Failed", 1, "", "Permission denied\n")
	})
	local := filepath.Join(t.TempDir(), "app.conf")
	os.WriteFile(local, []byte("listen 8080\n"), 0o600)
	// The output comes back with the error, for the caller to print stderr.
	out, err := Push(context.Background(), cfg, "i-0aaa", local, "/opt/acme/app.conf")
	if err == nil || !strings.Contains(err.Error(), "Failed") || out == nil || out.Stderr != "Permission denied\n" {
		t.Errorf("error %v, output %+v", err, out)
	}
}
