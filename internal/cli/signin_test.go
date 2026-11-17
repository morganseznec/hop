package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/credentials/ssocreds"

	"github.com/morganseznec/hop/internal/inventory"
	"github.com/morganseznec/hop/internal/picker"
)

// signingAWSCLI puts on PATH an aws command that signs in for real, as far as hop can
// tell: it leaves a valid token for the portal in the SSO cache. log receives a line
// each time it runs.
func signingAWSCLI(t *testing.T, key string) (log string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake aws command is a shell script")
	}
	token, err := ssocreds.StandardCachedTokenFilepath(key)
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Dir(token), 0o700)
	dir := t.TempDir()
	log = filepath.Join(dir, "ran")
	script := fmt.Sprintf("#!/bin/sh\necho \"$*\" >> '%s'\nprintf '%%s' '{\"accessToken\":\"fictional\",\"expiresAt\":\"%s\"}' > '%s'\n",
		log, time.Now().Add(time.Hour).UTC().Format(time.RFC3339), token)
	if err := os.WriteFile(filepath.Join(dir, "aws"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	return log
}

// renamedSince puts in the cache what hop knew of acme-prod three days ago, when it
// could last sign in: i-0p01 was then named acme-b-app, as the instance of acme-billing
// is. It has been renamed since. erredAgo is how long ago the last query failed on the
// expired token.
func renamedSince(t *testing.T, erredAgo time.Duration) {
	t.Helper()
	inv := inventory.Load()
	tg := inv.Targets[inventory.TargetKey("acme-prod", "eu-west-1")]
	tg.Instances = []inventory.Instance{instance("acme-b-app", "i-0p01", "acme-prod", "eu-west-1", "running", "prod")}
	tg.FetchedAt = time.Now().Add(-72 * time.Hour)
	tg.Err, tg.ErrKind, tg.ErrAt = "SSO token expired", inventory.ErrSSOExpired, time.Now().Add(-erredAgo)
	rewriteCache(t, inv)
}

// choosing stands in for the instance picker: it records the lines shown and chooses
// the one of that profile.
func choosing(t *testing.T, profile string) *[][]string {
	t.Helper()
	var shown [][]string
	saved := pickInstance
	t.Cleanup(func() { pickInstance = saved })
	pickInstance = func(n int, line func(int) string, o picker.Options) (int, error) {
		var lines []string
		choice := -1
		for i := 0; i < n; i++ {
			l := strings.Join(strings.Fields(line(i)), " ")
			lines = append(lines, l)
			if strings.Contains(l, " "+profile+" ") {
				choice = i
			}
		}
		shown = append(shown, append(lines, o.Header))
		if choice < 0 {
			return 0, picker.ErrAborted
		}
		return choice, nil
	}
	return &shown
}

// ranLog is what the fake aws command wrote, or nothing when it never ran.
func ranLog(path string) string {
	b, _ := os.ReadFile(path)
	return string(b)
}

const signInQuestion = "SSO token expired for acme-sso: hop cannot check the instances it found there. Sign in again now? [Y/n]"

// The case this is for: the sign-in has expired, the cache still holds an instance
// under a name it no longer has, and that name is also the one of the instance meant.
// hop used to ask which of the two.
func TestSignInBeforeAskingToChoose(t *testing.T) {
	f := fakeAWSEnv(t)
	cacheBillingApp(t, 5*time.Minute)
	renamedSince(t, 2*time.Minute)
	terminal(t, true, true)
	stdin(t, "y\n")
	ran := signingAWSCLI(t, "acme-sso")
	shown := choosing(t, "acme-billing")
	f.mu.Lock()
	f.prodName = "acme-p-tools"
	f.mu.Unlock()

	stdout, stderr, err := runHop(t, "run", "acme-b-app", "uptime")
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	// Asked to sign in first, once, to the portal of the candidate it could not check.
	if strings.Count(stderr, signInQuestion) != 1 || strings.Contains(stderr, "globex") {
		t.Errorf("question:\n%s", stderr)
	}
	if got := ranLog(ran); strings.Count(got, "\n") != 1 || !strings.Contains(got, "sso login") {
		t.Errorf("aws ran: %q", got)
	}
	// Signed in, the account says the instance has another name now: one instance is
	// left under the name typed, and there is nothing to choose.
	if len(*shown) != 0 {
		t.Errorf("the picker opened: %q", *shown)
	}
	if stdout != "up 1 day\n" {
		t.Errorf("stdout %q", stdout)
	}
	f.mu.Lock()
	if len(f.commands) != 1 || f.commands[0] != "uptime" {
		t.Errorf("commands sent: %q", f.commands)
	}
	f.mu.Unlock()
	if !strings.Contains(stderr, "acme-b-app (i-0b01, acme-billing") {
		t.Errorf("the command did not go to the instance of acme-billing:\n%s", stderr)
	}
	// The cache holds what the accounts behind the portal say now, both of them.
	inv := inventory.Load()
	prod := inv.Targets[inventory.TargetKey("acme-prod", "eu-west-1")]
	if prod.Err != "" || len(prod.Instances) != 1 || prod.Instances[0].Name != "acme-p-tools" || time.Since(prod.FetchedAt) > time.Minute {
		t.Errorf("acme-prod in the cache: %+v", prod)
	}
	if data := inv.Targets[inventory.TargetKey("acme-data", "eu-west-1")]; data.Err != "" || time.Since(data.FetchedAt) > time.Minute {
		t.Errorf("acme-data in the cache: %+v", data)
	}
}

func TestSignInDeclinedIsNotAskedTwice(t *testing.T) {
	fakeAWSEnv(t)
	cacheBillingApp(t, 5*time.Minute)
	renamedSince(t, 2*time.Minute)
	terminal(t, true, true)
	ran := signingAWSCLI(t, "acme-sso")

	// A no: the choice is offered as the cache has it, and the one that can be
	// reached still works.
	stdin(t, "n\n")
	shown := choosing(t, "acme-billing")
	stdout, stderr, err := runHop(t, "run", "acme-b-app", "uptime")
	if err != nil || stdout != "up 1 day\n" {
		t.Fatalf("%v, stdout %q\n%s", err, stdout, stderr)
	}
	if len(*shown) != 1 || len((*shown)[0]) != 3 || (*shown)[0][2] != `"acme-b-app" exists 2 times` {
		t.Fatalf("picker: %q", *shown)
	}
	if strings.Count(stderr, signInQuestion) != 1 {
		t.Errorf("question:\n%s", stderr)
	}

	// The other one cannot be reached without the sign-in that was declined: hop says
	// so, and does not ask the same question again.
	stdin(t, "n\n")
	choosing(t, "acme-prod")
	_, stderr, err = runHop(t, "run", "acme-b-app", "uptime")
	if err == nil || err.Error() != "SSO token expired for acme-sso: hop login acme-sso" {
		t.Fatalf("%v\n%s", err, stderr)
	}
	if strings.Count(stderr, "Sign in again now?") != 1 {
		t.Errorf("asked twice:\n%s", stderr)
	}
	if _, err := os.Stat(ran); err == nil {
		t.Errorf("aws ran after a no: %s", ranLog(ran))
	}
}

func TestSignInIsNotOfferedWithoutATerminal(t *testing.T) {
	f := fakeAWSEnv(t)
	cacheBillingApp(t, 5*time.Minute)
	renamedSince(t, 2*time.Minute)
	ran := signingAWSCLI(t, "acme-sso")
	shown := choosing(t, "acme-billing")

	_, stderr, err := runHop(t, "run", "acme-b-app", "uptime")
	if err == nil || !strings.HasPrefix(err.Error(), "several instances match, be more specific: ") {
		t.Fatalf("%v\n%s", err, stderr)
	}
	// Nobody to ask: say which account could not be looked at, and how to fix it.
	if strings.Contains(stderr, "Sign in again now?") || !strings.Contains(stderr, "SSO token expired for acme-sso") || !strings.Contains(stderr, "hop login acme-sso") {
		t.Errorf("stderr:\n%s", stderr)
	}
	if _, err := os.Stat(ran); err == nil || len(*shown) != 0 {
		t.Errorf("aws ran, or the picker opened: %q", *shown)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.commands) != 0 {
		t.Errorf("commands sent: %q", f.commands)
	}
}

// Signed in from another terminal since the last query failed: there is nothing to
// ask, only accounts to look at again.
func TestSignedInMeanwhileIsQueriedWithoutAQuestion(t *testing.T) {
	fakeAWSEnv(t)
	cacheBillingApp(t, 5*time.Minute)
	// The failure is too recent for the pair to be queried again on its own.
	renamedSince(t, 10*time.Second)
	validToken(t, "acme-sso")
	terminal(t, true, true)
	stdin(t, "")
	shown := choosing(t, "acme-billing")

	stdout, stderr, err := runHop(t, "run", "acme-b-app", "uptime")
	if err != nil || stdout != "up 1 day\n" {
		t.Fatalf("%v, stdout %q\n%s", err, stdout, stderr)
	}
	if strings.Contains(stderr, "Sign in again now?") || len(*shown) != 0 {
		t.Errorf("asked, or the picker opened: %q\n%s", *shown, stderr)
	}
}

// A single strong match needs no question before it: the sign-in is offered when its
// credentials are loaded, as before.
func TestSignInIsNotOfferedBeforeASureInstance(t *testing.T) {
	fakeAWSEnv(t)
	cacheBillingApp(t, 5*time.Minute)
	renamedSince(t, 2*time.Minute)
	terminal(t, true, true)
	stdin(t, "n\n")
	_, stderr, err := runHop(t, "run", "-p", "acme-prod", "acme-b-app", "uptime")
	if err == nil || err.Error() != "SSO token expired for acme-sso: hop login acme-sso" {
		t.Fatalf("%v\n%s", err, stderr)
	}
	if strings.Contains(stderr, "cannot check the instances") || strings.Count(stderr, "SSO token expired for acme-sso. Sign in again now?") != 1 {
		t.Errorf("stderr:\n%s", stderr)
	}
}
