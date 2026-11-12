package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/morganseznec/hop/internal/config"
	"github.com/morganseznec/hop/internal/fakes3"
)

// relayPushEnv sets up the fleet with fake EC2 and SSM (see fakeAWSEnv), a fake S3
// holding the bucket acme-relay, in another region than the instances, and
// relay_buckets in hop's configuration. A push script succeeds on the instance and
// prints the SHA-256 of file, when given.
func relayPushEnv(t *testing.T, relays map[string]string, file string) (*fakeAWS, *fakes3.Server) {
	t.Helper()
	fake := fakeAWSEnv(t)
	srv := fakes3.New(t)
	t.Setenv("AWS_ENDPOINT_URL_S3", srv.URL)
	srv.AddBucket("acme-relay", "us-east-1")
	if err := (&config.Config{RelayBuckets: relays}).Save(); err != nil {
		t.Fatal(err)
	}
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(b)
		fake.pushStdout = hex.EncodeToString(sum[:])
	}
	return fake, srv
}

func bigFile(t *testing.T, size int) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "release.tar.gz")
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i * 31)
	}
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPushThroughTheRelayBucket(t *testing.T) {
	big := bigFile(t, 64_001) // one byte too many for SSM alone
	fake, srv := relayPushEnv(t, map[string]string{"acme-*": "acme-relay/tmp/hop"}, big)

	stdout, stderr, err := runHop(t, "push", big, "acme-b-app", "/opt/acme/release.tar.gz")
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	if stdout != "" {
		t.Errorf("stdout %q", stdout)
	}
	for _, want := range []string{
		"hop: " + big + " -> acme-b-app:/opt/acme/release.tar.gz, 64.0 KB through s3://acme-relay\n",
		"hop: uploaded: acme-b-app is fetching it (up to 2m0s)\n",
		"hop: copied, SHA-256 matches on both sides\n",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
	// The object went under the prefix, and is gone.
	if n := srv.Count("PUT /acme-relay/tmp/hop/hop-relay-"); n != 1 {
		t.Errorf("upload: %v", srv.Calls())
	}
	if len(srv.Keys("acme-relay")) != 0 || srv.Count("DeleteObject") != 1 {
		t.Errorf("the relay object stays: %v", srv.Calls())
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.scripts) != 1 {
		t.Fatalf("%d commands sent", len(fake.scripts))
	}
	script := strings.Join(fake.scripts[0], "\n")
	// The instance gets a URL, signed for the region of the bucket, not the file.
	for _, want := range []string{"dst='/opt/acme/release.tar.gz'", "/acme-relay/tmp/hop/hop-relay-", "X-Amz-Signature=", "%2Fus-east-1%2Fs3%2Faws4_request", fake.pushStdout} {
		if !strings.Contains(script, want) {
			t.Errorf("script lacks %q:\n%s", want, script)
		}
	}
	if len(script) > 4000 {
		t.Errorf("script of %d characters: did the file go through SSM?", len(script))
	}
}

func TestPushRelayOnDemand(t *testing.T) {
	small := filepath.Join(t.TempDir(), "api.key")
	os.WriteFile(small, []byte("fictional-key\n"), 0o600)
	fake, srv := relayPushEnv(t, map[string]string{"*": "acme-relay"}, small)

	// A small file goes through SSM, and its content into the command history...
	if _, stderr, err := runHop(t, "push", small, "acme-b-app", "/etc/acme/api.key"); err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	if len(srv.Calls()) != 0 {
		t.Errorf("a small push went to S3: %v", srv.Calls())
	}
	// ... unless --relay keeps it out.
	_, stderr, err := runHop(t, "push", "--relay", small, "acme-b-app", "/etc/acme/api.key")
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	if srv.Count("PutObject") != 1 || !strings.Contains(stderr, "14 B through s3://acme-relay") {
		t.Errorf("--relay: %v\n%s", srv.Calls(), stderr)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.scripts) != 2 {
		t.Fatalf("%d commands sent", len(fake.scripts))
	}
	// base64 of "fictional-key\n" is in the first script only.
	if !strings.Contains(strings.Join(fake.scripts[0], "\n"), "ZmljdGlvbmFsLWtleQo=") || strings.Contains(strings.Join(fake.scripts[1], "\n"), "ZmljdGlvbmFsLWtleQo=") {
		t.Error("with --relay, the content must stay out of the command")
	}
}

func TestPushRelayRefusals(t *testing.T) {
	big := bigFile(t, 100_000)
	cases := []struct {
		name   string
		relays map[string]string
		args   []string
		want   string
	}{
		{"no relay bucket at all", nil, []string{"push", big, "acme-b-app", "/opt/x"},
			"100000 bytes, too large for SSM alone (limit 64000): with a relay bucket in hop's configuration (relay_buckets), hop push sends it through S3"},
		{"none for this profile", map[string]string{"acme-prod": "acme-relay", "globex": "acme-relay"}, []string{"push", big, "acme-b-app", "/opt/x"},
			"names no bucket for the profile acme-billing"},
		{"--relay without a bucket", nil, []string{"push", "--relay", big, "acme-b-app", "/opt/x"}, "--relay needs a bucket: name one under relay_buckets in "},
		{"--relay to a bucket", map[string]string{"*": "acme-relay"}, []string{"push", "--relay", big, "s3://acme-relay/x"}, "--relay is for a push to an instance"},
		{"a bucket that does not exist", map[string]string{"*": "acme-gone"}, []string{"push", big, "acme-b-app", "/opt/x"},
			"relay bucket acme-gone (relay_buckets in "},
		{"a relative path", map[string]string{"*": "acme-relay"}, []string{"push", big, "acme-b-app", "opt/x"}, "remote path must be absolute"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fake, srv := relayPushEnv(t, c.relays, big)
			_, _, err := runHop(t, c.args...)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %v, want %q", err, c.want)
			}
			// Nothing was uploaded, and the instance was asked nothing.
			if srv.Count("PutObject")+srv.Count("CreateMultipartUpload") != 0 {
				t.Errorf("S3 was written to: %v", srv.Calls())
			}
			fake.mu.Lock()
			defer fake.mu.Unlock()
			if len(fake.scripts) != 0 {
				t.Error("a command was sent")
			}
		})
	}
}

func TestPushRelayFailures(t *testing.T) {
	big := bigFile(t, 100_000)

	t.Run("the instance cannot write the file", func(t *testing.T) {
		// Without a SHA-256 to print, the fake instance fails on a permission.
		_, srv := relayPushEnv(t, map[string]string{"*": "acme-relay"}, "")
		_, stderr, err := runHop(t, "push", big, "acme-b-app", "/root/app.conf")
		if err == nil || !strings.Contains(err.Error(), "copy failed on the instance (status Failed, exit code 1)") {
			t.Fatalf("error %v", err)
		}
		if !strings.Contains(stderr, "sh: /root/app.conf: Permission denied") {
			t.Errorf("remote error not shown:\n%s", stderr)
		}
		if len(srv.Keys("acme-relay")) != 0 {
			t.Error("the relay object stays after a failure")
		}
	})
	t.Run("the role may not write to the bucket", func(t *testing.T) {
		fake, srv := relayPushEnv(t, map[string]string{"*": "acme-relay"}, big)
		srv.DenyPut = true
		_, stderr, err := runHop(t, "push", big, "acme-b-app", "/opt/x")
		if err == nil || !strings.Contains(err.Error(), "relay bucket: IAM denied writing s3://acme-relay/hop-relay-") {
			t.Fatalf("error %v", err)
		}
		if !strings.Contains(stderr, "your role needs s3:PutObject, s3:GetObject, s3:DeleteObject and s3:AbortMultipartUpload on the relay bucket") {
			t.Errorf("no hint:\n%s", stderr)
		}
		fake.mu.Lock()
		defer fake.mu.Unlock()
		if len(fake.scripts) != 0 {
			t.Error("a command was sent")
		}
	})
	t.Run("the object cannot be deleted", func(t *testing.T) {
		_, srv := relayPushEnv(t, map[string]string{"*": "acme-relay"}, big)
		srv.DenyDelete = true
		_, stderr, err := runHop(t, "push", big, "acme-b-app", "/opt/x")
		// The file is in place: a warning, not a failure.
		if err != nil {
			t.Fatalf("%v\n%s", err, stderr)
		}
		if !strings.Contains(stderr, "the temporary object s3://acme-relay/hop-relay-") || !strings.Contains(stderr, "could not be deleted") || !strings.Contains(stderr, "copied, SHA-256 matches on both sides") {
			t.Errorf("stderr:\n%s", stderr)
		}
	})
	t.Run("the instance got another file", func(t *testing.T) {
		fake, _ := relayPushEnv(t, map[string]string{"*": "acme-relay"}, big)
		fake.pushStdout = strings.Repeat("0", 64)
		_, _, err := runHop(t, "push", big, "acme-b-app", "/opt/x")
		if err == nil || !strings.Contains(err.Error(), "checksum mismatch after copy") {
			t.Fatalf("error %v", err)
		}
	})
}
