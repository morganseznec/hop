package inventory

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials/ssocreds"
)

// slowProvider is a credentials provider that takes a while, and counts how many
// retrievals run at the same time.
type slowProvider struct {
	active, peak, calls *atomic.Int32
	err                 error
}

func (p slowProvider) Retrieve(ctx context.Context) (aws.Credentials, error) {
	n := p.active.Add(1)
	defer p.active.Add(-1)
	p.calls.Add(1)
	for {
		if old := p.peak.Load(); n <= old || p.peak.CompareAndSwap(old, n) {
			break
		}
	}
	time.Sleep(30 * time.Millisecond)
	return aws.Credentials{AccessKeyID: "AKIAFAKEFAKEFAKE0001", SecretAccessKey: "fictional-secret"}, p.err
}

func TestTokenGateLetsTheFirstProfileGoAlone(t *testing.T) {
	var gate TokenGate
	var active, peak, calls atomic.Int32
	var firstDone atomic.Bool
	var startedBeforeFirstEnded atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Go(func() {
			cfg := aws.Config{Credentials: aws.CredentialsProviderFunc(func(ctx context.Context) (aws.Credentials, error) {
				if calls.Load() > 0 && !firstDone.Load() {
					startedBeforeFirstEnded.Add(1)
				}
				creds, err := slowProvider{&active, &peak, &calls, nil}.Retrieve(ctx)
				firstDone.Store(true)
				return creds, err
			})}
			if err := gate.Retrieve(context.Background(), "acme-sso", cfg); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if n := startedBeforeFirstEnded.Load(); n != 0 {
		t.Errorf("%d profiles resolved their credentials while the first was still at it", n)
	}
	// Every profile resolves its own credentials, and the others go together.
	if calls.Load() != 8 || peak.Load() < 2 {
		t.Errorf("%d retrievals, %d at once at most", calls.Load(), peak.Load())
	}
}

func TestTokenGateHoldsNobodyElse(t *testing.T) {
	var gate TokenGate
	release := make(chan struct{})
	blocked := aws.Config{Credentials: aws.CredentialsProviderFunc(func(ctx context.Context) (aws.Credentials, error) {
		<-release
		return aws.Credentials{}, nil
	})}
	quick := aws.Config{Credentials: aws.CredentialsProviderFunc(func(ctx context.Context) (aws.Credentials, error) {
		return aws.Credentials{}, nil
	})}
	done := make(chan error, 1)
	go func() { done <- gate.Retrieve(context.Background(), "acme-sso", blocked) }()
	// Another portal, and a profile without a token, do not wait for acme-sso.
	for _, token := range []string{"globex-sso", ""} {
		finished := make(chan error, 1)
		go func() { finished <- gate.Retrieve(context.Background(), token, quick) }()
		select {
		case err := <-finished:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("token %q waited for another portal", token)
		}
	}
	close(release)
	if err := <-done; err != nil {
		t.Error(err)
	}
}

func TestTokenGateReturnsEachProfileItsOwnError(t *testing.T) {
	var gate TokenGate
	boom := errors.New("permission set removed")
	failing := aws.Config{Credentials: aws.CredentialsProviderFunc(func(ctx context.Context) (aws.Credentials, error) {
		return aws.Credentials{}, boom
	})}
	fine := aws.Config{Credentials: aws.CredentialsProviderFunc(func(ctx context.Context) (aws.Credentials, error) {
		return aws.Credentials{}, nil
	})}
	// The first profile fails: the next ones are not failed with it.
	if err := gate.Retrieve(context.Background(), "acme-sso", failing); !errors.Is(err, boom) {
		t.Errorf("first: %v", err)
	}
	if err := gate.Retrieve(context.Background(), "acme-sso", fine); err != nil {
		t.Errorf("second: %v", err)
	}
	if err := gate.Retrieve(context.Background(), "acme-sso", failing); !errors.Is(err, boom) {
		t.Errorf("third: %v", err)
	}
}

// fakeSSO plays the two services a profile signs in through: SSO OIDC, which renews
// the token, and SSO, which hands out the credentials of a role. Any other call goes to
// next. Renewing takes a moment, so that profiles left to themselves all do it at once.
type fakeSSO struct {
	mu       sync.Mutex
	renewals int
	bearers  []string
	next     http.Handler
}

func (f *fakeSSO) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/token":
		io.Copy(io.Discard, r.Body)
		f.mu.Lock()
		f.renewals++
		n := f.renewals
		f.mu.Unlock()
		time.Sleep(100 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"accessToken":"renewed-%d","expiresIn":3600,"refreshToken":"next-%d","tokenType":"Bearer"}`, n, n)
	case r.URL.Path == "/federation/credentials":
		f.mu.Lock()
		f.bearers = append(f.bearers, r.Header.Get("x-amz-sso_bearer_token"))
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"roleCredentials":{"accessKeyId":"ASIAFAKEFAKEFAKE0001","secretAccessKey":"fictional-secret","sessionToken":"fictional-token","expiration":%d}}`,
			time.Now().Add(time.Hour).UnixMilli())
	case r.Method == http.MethodGet && r.URL.Path == "/":
		// S3 ListBuckets: an account without buckets.
		w.Header().Set("Content-Type", "application/xml")
		io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><ListAllMyBucketsResult><Buckets></Buckets></ListAllMyBucketsResult>`)
	default:
		f.next.ServeHTTP(w, r)
	}
}

// ssoFleet sets up n profiles behind the session acme-sso, whose access token has
// expired and can be renewed, with the fake as every endpoint. It returns the fake and
// one pair per profile.
func ssoFleet(t *testing.T, n int) (*fakeSSO, []Pair) {
	t.Helper()
	fake := &fakeSSO{next: &fakeAWS{}}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	fakeEnv(t, srv.URL)
	// The SSO clients are built before the SDK reads AWS_ENDPOINT_URL: without their own
	// variables, they would call AWS.
	t.Setenv("AWS_ENDPOINT_URL_SSO_OIDC", srv.URL)
	t.Setenv("AWS_ENDPOINT_URL_SSO", srv.URL)
	t.Setenv("AWS_ENDPOINT_URL_S3", "")

	var cfg strings.Builder
	cfg.WriteString("[sso-session acme-sso]\nsso_start_url = https://acme.awsapps.com/start\nsso_region = eu-west-1\n")
	var pairs []Pair
	for i := 1; i <= n; i++ {
		name := fmt.Sprintf("acme-%d", i)
		fmt.Fprintf(&cfg, "\n[profile %s]\nsso_session = acme-sso\nsso_account_id = %012d\nsso_role_name = ReadOnly\nregion = eu-west-1\n", name, i)
		pairs = append(pairs, Pair{Profile: name, Region: "eu-west-1", Token: "acme-sso"})
	}
	if err := os.WriteFile(os.Getenv("AWS_CONFIG_FILE"), []byte(cfg.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv("AWS_SHARED_CREDENTIALS_FILE"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	tok, err := ssocreds.StandardCachedTokenFilepath("acme-sso")
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Dir(tok), 0o700)
	expired := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	token := `{"accessToken":"expired","expiresAt":"` + expired + `","refreshToken":"fictional-refresh","clientId":"fictional-client","clientSecret":"fictional-secret"}`
	if err := os.WriteFile(tok, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	return fake, pairs
}

func (f *fakeSSO) check(t *testing.T, profiles int) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	// One renewal for the portal, whatever the number of profiles behind it: left to
	// themselves, they each renew the token, and fight over its cache file.
	if f.renewals != 1 {
		t.Errorf("the token was renewed %d times for %d profiles, want once", f.renewals, profiles)
	}
	if len(f.bearers) != profiles {
		t.Errorf("%d profiles asked for credentials, want %d", len(f.bearers), profiles)
	}
	for _, b := range f.bearers {
		if b != "renewed-1" {
			t.Errorf("credentials asked with the token %q, want the renewed one", b)
		}
	}
}

func TestFetchAllRenewsASharedSSOTokenOnce(t *testing.T) {
	fake, pairs := ssoFleet(t, 8)
	targets := FetchAll(context.Background(), pairs, 20*time.Second, nil)
	for _, tg := range targets {
		if tg.Err != "" {
			t.Errorf("%s: %s (%s)", tg.Profile, tg.Err, tg.ErrKind)
		}
	}
	fake.check(t, len(pairs))
}

func TestFetchBucketsRenewsASharedSSOTokenOnce(t *testing.T) {
	fake, pairs := ssoFleet(t, 8)
	lists := FetchBuckets(context.Background(), pairs, 20*time.Second, nil)
	for _, l := range lists {
		if l.Err != "" {
			t.Errorf("%s: %s (%s)", l.Profile, l.Err, l.ErrKind)
		}
	}
	fake.check(t, len(pairs))
}

func TestClassifyATokenCacheLostToAnotherProcess(t *testing.T) {
	// What the SDK says when another process rewrote the token file first: the token is
	// good, and signing in again would fix nothing.
	err := errors.New("failed to refresh cached credentials, refresh cached SSO token failed, unable to cache refreshed SSO token, failed to replace old cached SSO token file, rename /home/acme/.aws/sso/cache/af37.json.tmp-1 /home/acme/.aws/sso/cache/af37.json: no such file or directory")
	if k := Classify(err); k != ErrOther {
		t.Errorf("classified as %q", k)
	}
	// A token that really cannot be renewed is still an expired token.
	err = errors.New("failed to refresh cached credentials, refresh cached SSO token failed, unable to refresh SSO token, operation error SSO OIDC: CreateToken")
	if k := Classify(err); k != ErrSSOExpired {
		t.Errorf("classified as %q", k)
	}
}
