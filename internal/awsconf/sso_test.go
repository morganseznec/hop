package awsconf

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/credentials/ssocreds"
)

// tempHome points the home directory to a temporary folder, so that the SSO token
// cache read by Portal.Token is never the real one.
func tempHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return home
}

// writeToken writes a cached token file for key, as the AWS CLI does after a login.
func writeToken(t *testing.T, key, content string) string {
	t.Helper()
	path, err := ssocreds.StandardCachedTokenFilepath(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestTokenStates(t *testing.T) {
	now := time.Now().UTC()
	tok := func(access string, exp time.Time) string {
		return `{"accessToken":"` + access + `","expiresAt":"` + exp.Format(time.RFC3339) + `","region":"eu-west-1"}`
	}
	cases := []struct {
		name    string
		content string // empty: no file at all
		want    TokenState
	}{
		{"valid for one hour", tok("secret", now.Add(time.Hour)), TokenValid},
		{"valid with fractional seconds", `{"accessToken":"secret","expiresAt":"` + now.Add(time.Hour).Format("2006-01-02T15:04:05.000Z") + `"}`, TokenValid},
		{"expires within the one-minute margin", tok("secret", now.Add(30*time.Second)), TokenExpired},
		{"expired an hour ago", tok("secret", now.Add(-time.Hour)), TokenExpired},
		{"expired, even with a refresh token", `{"accessToken":"secret","refreshToken":"r","expiresAt":"` + now.Add(-time.Hour).Format(time.RFC3339) + `"}`, TokenExpired},
		{"no file", "", TokenMissing},
		{"not JSON", "{not json", TokenMissing},
		{"no access token", tok("", now.Add(time.Hour)), TokenMissing},
		{"no expiry", `{"accessToken":"secret"}`, TokenMissing},
		{"expiry in another format", `{"accessToken":"secret","expiresAt":"2030-01-02 03:04:05"}`, TokenMissing},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tempHome(t)
			pt := Portal{Key: "acme-sso", SessionName: "acme-sso"}
			if c.content != "" {
				writeToken(t, pt.Key, c.content)
			}
			got := pt.Token()
			if got.State != c.want {
				t.Errorf("state %v, want %v (token %+v)", got.State, c.want, got)
			}
			if c.want == TokenMissing && !got.ExpiresAt.IsZero() {
				t.Errorf("a missing token carries no expiry: %v", got.ExpiresAt)
			}
		})
	}
}

func TestTokenKeyedByStartURL(t *testing.T) {
	// The legacy format keys the cache by start URL, not by profile name.
	tempHome(t)
	url := "https://globex.awsapps.com/start"
	writeToken(t, url, `{"accessToken":"secret","expiresAt":"`+time.Now().Add(time.Hour).UTC().Format(time.RFC3339)+`"}`)
	if got := (Portal{Key: url, StartURL: url}).Token(); got.State != TokenValid {
		t.Errorf("legacy portal token: %+v", got)
	}
	if got := (Portal{Key: "globex", SessionName: "globex"}).Token(); got.State != TokenMissing {
		t.Errorf("a different key must not find the token: %+v", got)
	}
}

func TestTokenUnreadable(t *testing.T) {
	t.Run("directory in place of the file", func(t *testing.T) {
		tempHome(t)
		path, err := ssocreds.StandardCachedTokenFilepath("acme-sso")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if got := (Portal{Key: "acme-sso"}).Token(); got.State != TokenMissing {
			t.Errorf("unreadable token: %+v", got)
		}
	})
	t.Run("no read permission", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("Unix permission bits do not apply on Windows")
		}
		if os.Geteuid() == 0 {
			t.Skip("root reads any file")
		}
		tempHome(t)
		path := writeToken(t, "acme-sso", `{"accessToken":"secret","expiresAt":"`+time.Now().Add(time.Hour).UTC().Format(time.RFC3339)+`"}`)
		if err := os.Chmod(path, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(path, 0o600) })
		if got := (Portal{Key: "acme-sso"}).Token(); got.State != TokenMissing {
			t.Errorf("unreadable token: %+v", got)
		}
	})
}

func TestTokenString(t *testing.T) {
	exp := time.Date(2026, 10, 30, 14, 5, 0, 0, time.Local)
	cases := []struct {
		tok  Token
		want string
	}{
		{Token{State: TokenValid, ExpiresAt: exp}, "valid until 14:05"},
		{Token{State: TokenExpired, ExpiresAt: exp}, "expired Oct 30 14:05"},
		{Token{State: TokenMissing}, "missing"},
		{Token{}, "missing"},
	}
	for _, c := range cases {
		if got := c.tok.String(); got != c.want {
			t.Errorf("%+v: %q, want %q", c.tok, got, c.want)
		}
	}
}

func TestTokenFromLoadedPortals(t *testing.T) {
	// End to end: portals built from the files, tokens read from the temporary home.
	home := tempHome(t)
	cfg := filepath.Join(home, "config")
	os.WriteFile(cfg, []byte(`[sso-session acme-sso]
sso_start_url = https://acme.awsapps.com/start
sso_region = eu-west-1

[profile acme-prod]
sso_session = acme-sso
sso_account_id = 111111111111
sso_role_name = ReadOnly

[profile globex]
sso_start_url = https://globex.awsapps.com/start
sso_region = us-east-1
sso_account_id = 222222222222
sso_role_name = ReadOnly
`), 0o600)
	f, err := LoadFrom(cfg, filepath.Join(home, "missing-credentials"))
	if err != nil {
		t.Fatal(err)
	}
	writeToken(t, "acme-sso", `{"accessToken":"secret","expiresAt":"`+time.Now().Add(time.Hour).UTC().Format(time.RFC3339)+`"}`)
	states := map[string]TokenState{}
	for _, pt := range f.Portals(nil) {
		states[pt.Label()] = pt.Token().State
	}
	want := map[string]TokenState{"acme-sso": TokenValid, "globex.awsapps.com": TokenMissing}
	for label, st := range want {
		if states[label] != st {
			t.Errorf("%s: %v, want %v (all: %v)", label, states[label], st, states)
		}
	}
	if !strings.HasPrefix(f.ConfigPath, home) {
		t.Errorf("config path outside the temporary home: %s", f.ConfigPath)
	}
}
