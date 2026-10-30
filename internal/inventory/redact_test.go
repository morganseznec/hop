package inventory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	const (
		keyID  = "ASIAQWERTYUIOPASDFGH"
		secret = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
		token  = "IQoJb3JpZ2luX2VjEJr//////////wEaCXVzLWVhc3QtMSJHMEUCIQ"
	)
	output := fmt.Sprintf(`{"Version": 1, "AccessKeyId": %q, "SecretAccessKey": %q, "SessionToken": %q, "Expiration": "2026-10-30T12:00:00"}`, keyID, secret, token)
	cases := []struct {
		name, in string
		keep     []string
	}{
		{
			name: "credential_process output quoted by the SDK",
			in:   "failed to refresh cached credentials, process provider error: parse failed of process output: " + output,
			keep: []string{"failed to refresh cached credentials", "process output: " + redacted},
		},
		{
			name: "fields outside a process output",
			in:   fmt.Sprintf(`bad response {"SecretAccessKey":%q,"SessionToken":%q}`, secret, token),
			keep: []string{"bad response"},
		},
		{
			name: "ini-style assignment",
			in:   "line 3: aws_secret_access_key = " + secret + " aws_session_token=" + token,
			keep: []string{"line 3:"},
		},
		{
			name: "a bare access key ID",
			in:   "InvalidClientTokenId: The security token included in the request is invalid: " + keyID,
			keep: []string{"InvalidClientTokenId"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Redact(c.in)
			for _, s := range []string{keyID, secret, token} {
				if strings.Contains(got, s) {
					t.Errorf("%q still contains %q", got, s)
				}
			}
			for _, s := range c.keep {
				if !strings.Contains(got, s) {
					t.Errorf("%q lost %q", got, s)
				}
			}
		})
	}
	if got := Redact("operation error EC2: DescribeInstances, https response error StatusCode: 403"); !strings.Contains(got, "StatusCode: 403") {
		t.Errorf("an ordinary error was altered: %q", got)
	}
}

func TestRedactError(t *testing.T) {
	if RedactError(nil) != nil {
		t.Fatal("nil must stay nil")
	}
	err := RedactError(fmt.Errorf("process output: {\"SecretAccessKey\":\"x\"}: %w", context.Canceled))
	if strings.Contains(err.Error(), "SecretAccessKey") {
		t.Errorf("not redacted: %q", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Error("errors.Is no longer sees the wrapped error")
	}
}

func TestFetchStoresRedactedErrors(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake credential_process is a shell script")
	}
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config")
	// A helper whose output the SDK cannot parse: the expiration has no time zone.
	script := filepath.Join(dir, "creds.sh")
	writeFile(t, script, "#!/bin/sh\necho '{\"Version\":1,\"AccessKeyId\":\"ASIAQWERTYUIOPASDFGH\",\"SecretAccessKey\":\"s3cr3t-value\",\"SessionToken\":\"t0k3n-value\",\"Expiration\":\"2026-10-30T12:00:00\"}'\n", 0o755)
	writeFile(t, cfg, "[profile acme-proc]\ncredential_process = "+script+"\nregion = eu-west-1\n", 0o600)
	t.Setenv("AWS_CONFIG_FILE", cfg)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(dir, "none"))
	// Should the SDK accept the output after all, nothing may leave the machine.
	t.Setenv("AWS_ENDPOINT_URL", "http://127.0.0.1:1")
	tg := Fetch(context.Background(), Pair{Profile: "acme-proc", Region: "eu-west-1"})
	if tg.Err == "" {
		t.Skip("the SDK accepted the helper output; nothing to redact")
	}
	for _, s := range []string{"ASIAQWERTYUIOPASDFGH", "s3cr3t-value", "t0k3n-value"} {
		if strings.Contains(tg.Err, s) {
			t.Errorf("cached error leaks %q: %s", s, tg.Err)
		}
	}
}

func writeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}
