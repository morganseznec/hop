package session

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/morganseznec/hop/internal/bucket"
	"github.com/morganseznec/hop/internal/fakes3"
)

// relayEnv sets up an "instance" that runs what SSM sends it with the local sh (see
// remoteShell), and a fake S3 with the relay bucket acme-relay. The instance fetches
// with the real curl or wget of this machine.
func relayEnv(t *testing.T) (*fakeSSM, aws.Config, *fakes3.Server, Relay) {
	t.Helper()
	_, curl := exec.LookPath("curl")
	_, wget := exec.LookPath("wget")
	if curl != nil && wget != nil {
		t.Skip("no curl or wget")
	}
	fake, cfg, _ := remoteShell(t)
	srv := fakes3.New(t)
	srv.AddBucket("acme-relay", "eu-west-1")
	relay := Relay{Bucket: "acme-relay", Prefix: "hop/", Config: aws.Config{
		Region: "eu-west-1", BaseEndpoint: aws.String(srv.URL), RetryMaxAttempts: 1,
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "AKIAFAKEFAKEFAKE0001", SecretAccessKey: "fictional-secret"}, nil
		}),
	}}
	return fake, cfg, srv, relay
}

func payload(t *testing.T, size int) (path string, data []byte) {
	t.Helper()
	data = make([]byte, size)
	r := rand.New(rand.NewPCG(uint64(size), 3))
	for i := range data {
		data[i] = byte(r.IntN(256))
	}
	path = filepath.Join(t.TempDir(), "payload.bin")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path, data
}

func TestPushViaEndToEnd(t *testing.T) {
	for _, size := range []int{0, 10, PushLimit + 1, 3_000_000, bucket.MultipartThreshold + 1234} {
		t.Run(strconv.Itoa(size)+" bytes", func(t *testing.T) {
			fake, cfg, srv, relay := relayEnv(t)
			local, data := payload(t, size)
			// A quote, a backslash and a newline in the path, as for Push.
			destDir := filepath.Join(t.TempDir(), "it's a \\ dir\nhere")
			os.MkdirAll(destDir, 0o700)
			dest := filepath.Join(destDir, "my file.bin")

			uploaded := false
			res, err := PushVia(context.Background(), cfg, relay, "i-0aaa", local, dest, nil, func() {
				uploaded = true
				// The object is there for the instance, under the prefix and a name
				// that says nothing of the file.
				keys := srv.Keys("acme-relay")
				if len(keys) != 1 || !strings.HasPrefix(keys[0], "hop/hop-relay-") || len(keys[0]) != len("hop/hop-relay-")+32 {
					t.Errorf("relay object: %v", keys)
				}
			})
			if err != nil {
				t.Fatalf("push: %v (output %+v)", err, res.Output)
			}
			if !uploaded || res.Size != int64(size) || res.Left {
				t.Errorf("result %+v, uploaded %v", res, uploaded)
			}
			got, err := os.ReadFile(dest)
			if err != nil || !bytes.Equal(got, data) {
				t.Errorf("remote copy differs: %d bytes, want %d (%v)", len(got), len(data), err)
			}
			// Nothing stays: neither in the bucket, nor next to the file.
			if keys := srv.Keys("acme-relay"); len(keys) != 0 || srv.Count("DeleteObject") != 1 {
				t.Errorf("the relay object was not deleted: %v, %v", keys, srv.Calls())
			}
			if left, _ := filepath.Glob(filepath.Join(destDir, ".hop-push.*")); len(left) != 0 {
				t.Errorf("temporary directory left on the instance: %v", left)
			}

			sent := fake.got("SendCommand")
			if len(sent) != 1 {
				t.Fatalf("%d SendCommand calls", len(sent))
			}
			cmds := commandsOf(sent[0])
			if cmds[0] != "set -e" {
				t.Errorf("the script must stop on the first error: %q", cmds[0])
			}
			// The file does not travel through SSM, whatever its size, and the URL is
			// never an argument of a command, where the process list would show it.
			script := strings.Join(cmds, "\n")
			if len(script) > 4000 {
				t.Errorf("script of %d characters: the content went through SSM?", len(script))
			}
			for _, c := range cmds {
				if strings.Contains(c, "X-Amz-Signature") && !strings.HasPrefix(c, "http") && !strings.HasPrefix(c, `url = "http`) {
					t.Errorf("the URL is on a command line: %q", c)
				}
			}
		})
	}
}

func TestPushViaKeepsOwnerModeAndTheFileOnFailure(t *testing.T) {
	_, cfg, srv, relay := relayEnv(t)
	local, data := payload(t, 5000)
	dest := filepath.Join(t.TempDir(), "app.conf")
	os.WriteFile(dest, []byte("the one in place"), 0o640)
	os.Chmod(dest, 0o640)

	// What the instance downloads is not what was sent: the file in place stays.
	srv.Corrupt = true
	res, err := PushVia(context.Background(), cfg, relay, "i-0aaa", local, dest, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "copy failed on the instance (status Failed, exit code 3): what the instance downloaded differs from the file sent") {
		t.Fatalf("error %v", err)
	}
	if !strings.Contains(res.Output.Stderr, "app.conf is unchanged") {
		t.Errorf("stderr of the instance: %q", res.Output.Stderr)
	}
	if b, _ := os.ReadFile(dest); string(b) != "the one in place" {
		t.Fatalf("the file in place was replaced by a corrupted download")
	}
	if len(srv.Keys("acme-relay")) != 0 {
		t.Error("the relay object stays after a failure")
	}

	srv.Corrupt = false
	if _, err := PushVia(context.Background(), cfg, relay, "i-0aaa", local, dest, nil, nil); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(dest)
	if b, _ := os.ReadFile(dest); !bytes.Equal(b, data) || st.Mode().Perm() != 0o640 {
		t.Errorf("content %d bytes, mode %v (want 0640)", len(b), st.Mode().Perm())
	}
}

func TestPushViaChecksEveryRightBeforeTheInstance(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*fakes3.Server)
		want  string
	}{
		{"may not write", func(s *fakes3.Server) { s.DenyPut = true }, "relay bucket: IAM denied writing s3://acme-relay/hop/hop-relay-"},
		{"may not read", func(s *fakes3.Server) { s.DenyHead = true }, "your role may write to the relay bucket acme-relay but not read from it (s3:GetObject)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fake, cfg, srv, relay := relayEnv(t)
			c.setup(srv)
			local, _ := payload(t, 100_000)
			dest := filepath.Join(t.TempDir(), "f.bin")
			called := false
			_, err := PushVia(context.Background(), cfg, relay, "i-0aaa", local, dest, nil, func() { called = true })
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %v, want %q", err, c.want)
			}
			// The instance was asked nothing, and nothing stays in the bucket.
			if called || len(fake.got("SendCommand")) != 0 {
				t.Error("a command was sent to the instance")
			}
			if len(srv.Keys("acme-relay")) != 0 {
				t.Errorf("an object stays: %v", srv.Keys("acme-relay"))
			}
		})
	}
}

func TestPushViaReportsAnObjectItCouldNotDelete(t *testing.T) {
	_, cfg, srv, relay := relayEnv(t)
	srv.DenyDelete = true
	local, data := payload(t, 1000)
	dest := filepath.Join(t.TempDir(), "f.bin")
	res, err := PushVia(context.Background(), cfg, relay, "i-0aaa", local, dest, nil, nil)
	// The copy itself worked: the caller warns about the object.
	if err != nil {
		t.Fatal(err)
	}
	if !res.Left || !errors.Is(res.LeftErr, bucket.ErrDenied) || res.Object.Bucket != "acme-relay" || len(srv.Keys("acme-relay")) != 1 {
		t.Errorf("result %+v", res)
	}
	if b, _ := os.ReadFile(dest); !bytes.Equal(b, data) {
		t.Error("the file was not copied")
	}
}

func TestPushViaExplainsWhatTheInstanceCouldNotDo(t *testing.T) {
	_, cfg, srv, relay := relayEnv(t)
	// The role may read (the look before the command passes), the instance may not: a
	// bucket policy, or the policy of its VPC endpoint.
	srv.DenyGet = true
	local, _ := payload(t, 1000)
	dest := filepath.Join(t.TempDir(), "f.bin")
	res, err := PushVia(context.Background(), cfg, relay, "i-0aaa", local, dest, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "S3 refused the download (403)") {
		t.Fatalf("error %v (stderr %q)", err, res.Output.Stderr)
	}
	if _, err := os.Stat(dest); err == nil {
		t.Error("a file was written")
	}
	if len(srv.Keys("acme-relay")) != 0 {
		t.Error("the relay object stays after a failure")
	}
}

func TestPushViaLeavesTheObjectToAnInstanceStillFetching(t *testing.T) {
	fastPolls(t)
	saved := relayWait
	relayWait = func(int64) time.Duration { return 50 * time.Millisecond }
	t.Cleanup(func() { relayWait = saved })
	local, _ := payload(t, 1000)

	for _, c := range []struct {
		status string
		left   bool
	}{
		// The command reached the instance: it may be in the middle of the download.
		{"InProgress", true},
		// It never did: hop cancels it, and nothing will ever fetch the object.
		{"Pending", false},
	} {
		t.Run(c.status, func(t *testing.T) {
			srv := fakes3.New(t)
			srv.AddBucket("acme-relay", "eu-west-1")
			relay := Relay{Bucket: "acme-relay", Config: aws.Config{
				Region: "eu-west-1", BaseEndpoint: aws.String(srv.URL), RetryMaxAttempts: 1,
				Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
					return aws.Credentials{AccessKeyID: "AKIAFAKEFAKEFAKE0001", SecretAccessKey: "fictional-secret"}, nil
				}),
			}}
			_, cfg := newFake(t, func(op string, req map[string]any) (int, string) {
				switch op {
				case "SendCommand":
					return 200, `{"Command":{"CommandId":"c-0abc"}}`
				case "GetCommandInvocation":
					return 200, invocation(c.status, -1, "", "")
				}
				return 200, `{}`
			})
			res, err := PushVia(context.Background(), cfg, relay, "i-0aaa", local, "/opt/acme/f.bin", nil, nil)
			var te *TimeoutError
			if !errors.As(err, &te) {
				t.Fatalf("error %v", err)
			}
			if res.Left != c.left || res.LeftErr != nil || (len(srv.Keys("acme-relay")) == 1) != c.left {
				t.Errorf("left %v (want %v), keys %v", res.Left, c.left, srv.Keys("acme-relay"))
			}
		})
	}
}

func TestPushViaRefusesBeforeAnyCall(t *testing.T) {
	_, cfg, srv, relay := relayEnv(t)
	local, _ := payload(t, 10)
	if _, err := PushVia(context.Background(), cfg, relay, "i-0aaa", local, "relative/path", nil, nil); err == nil || !strings.Contains(err.Error(), "must be absolute") {
		t.Errorf("relative path: %v", err)
	}
	if _, err := PushVia(context.Background(), cfg, relay, "i-0aaa", filepath.Join(t.TempDir(), "missing"), "/opt/f", nil, nil); err == nil {
		t.Error("missing file accepted")
	}
	if len(srv.Calls()) != 0 {
		t.Errorf("S3 was called: %v", srv.Calls())
	}
}

func TestFetchScript(t *testing.T) {
	sum := sha256.Sum256([]byte("x"))
	want := hex.EncodeToString(sum[:])
	cmds, err := fetchScript("https://acme-relay.s3.eu-west-1.amazonaws.com/hop/hop-relay-0abc?X-Amz-Signature=abc&X-Amz-Expires=420", "/opt/acme/it's here.bin", want)
	if err != nil {
		t.Fatal(err)
	}
	script := strings.Join(cmds, "\n")
	for _, w := range []string{
		`dst='/opt/acme/it'\''s here.bin'`,
		// Next to the destination, private, and removed whatever happens.
		`d=$(umask 077 && mktemp -d "$(dirname "$dst")/.hop-push.XXXXXXXX")`,
		`trap 'rm -rf "$d"' EXIT`,
		"curl -fsS --retry 2 -o \"$d/data\" -K - <<'HOP_URL'\nurl = \"https://acme-relay",
		"wget -q -O \"$d/data\" -i - <<'HOP_URL'\nhttps://acme-relay",
		`[ "$got" = ` + want + ` ]`,
	} {
		if !strings.Contains(script, w) {
			t.Errorf("script lacks %q:\n%s", w, script)
		}
	}
	// dash, the sh of Debian and Ubuntu, takes "$((" for arithmetic: a command
	// substitution that starts with a subshell needs a space in between.
	if strings.Contains(script, "$((") {
		t.Errorf("the script has a \"$((\", a syntax error in dash:\n%s", script)
	}
	// The comparison comes before the copy.
	if strings.Index(script, `[ "$got" =`) > strings.Index(script, `cat "$d/data" > "$dst"`) {
		t.Error("the file is copied before it is compared")
	}
	// A URL that could end the here-document or the quotes is refused, and so is
	// anything that is not a SHA-256 where the script compares one.
	for _, u := range []string{"https://x/\"; rm -rf /", "https://x/a\nHOP_URL", "https://x/a\\b", "file:///etc/passwd", ""} {
		if _, err := fetchScript(u, "/opt/f", want); err == nil {
			t.Errorf("URL %q accepted", u)
		}
	}
	for _, h := range []string{"", "abc", strings.Repeat("z", 64), want + " ] || true; [ 1 = 1"} {
		if _, err := fetchScript("https://x/a", "/opt/f", h); err == nil {
			t.Errorf("SHA-256 %q accepted", h)
		}
	}
}

func TestExplainFetch(t *testing.T) {
	for stderr, want := range map[string]string{
		"curl: (22) The requested URL returned error: 403":                          "S3 refused the download (403)",
		"curl: (6) Could not resolve host: acme-relay.s3.amazonaws":                 "the instance cannot reach S3",
		"curl: (28) Connection timed out after 300000 milliseconds":                 "the instance cannot reach S3",
		"wget: unable to resolve host address 'acme-relay.s3'":                      "the instance cannot reach S3",
		"no curl or wget on the instance":                                           "neither curl nor wget",
		"wget: unrecognized option: i\nBusyBox v1.37.0 multi-call binary":           "cannot read the URL from stdin (BusyBox)",
		"curl: (23) Failure writing output to destination: No space left on device": "no space left on the instance",
		"sh: 1: cannot create /root/x: Permission denied":                           "",
	} {
		if got := explainFetch(stderr); (want == "") != (got == "") || !strings.Contains(got, want) {
			t.Errorf("explainFetch(%q) = %q, want %q", stderr, got, want)
		}
	}
}

func TestMayStillRun(t *testing.T) {
	for _, c := range []struct {
		err  error
		want bool
	}{
		{&TimeoutError{Status: "InProgress"}, true},
		{&TimeoutError{Status: "Pending", Canceled: true}, false},
		{&TimeoutError{Status: "TimedOut"}, false},
		{&TimeoutError{}, false},
		{&InterruptedError{Status: "InProgress"}, true},
		{&InterruptedError{Status: ""}, false},
		{&PollError{Status: "InProgress", Err: errors.New("x")}, true},
		{&PollError{Status: "Delayed", Err: errors.New("x")}, false},
		{errors.New("anything else"), false},
	} {
		if got := mayStillRun(c.err); got != c.want {
			t.Errorf("mayStillRun(%v) = %v", c.err, got)
		}
	}
}

func TestRelayWait(t *testing.T) {
	for size, want := range map[int64]time.Duration{
		0:                 2 * time.Minute,
		64_001:            2 * time.Minute,
		600_000_000:       12 * time.Minute,
		1_000_000_000_000: 12 * time.Hour,
	} {
		if got := RelayWait(size); got != want {
			t.Errorf("RelayWait(%d) = %v, want %v", size, got, want)
		}
	}
}
