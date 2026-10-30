package inventory

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/ssocreds"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// apiErr wraps an API error code the way the SDK returns it from an operation.
func apiErr(service, op, code string) error {
	return &smithy.OperationError{
		ServiceID:     service,
		OperationName: op,
		Err:           &smithy.GenericAPIError{Code: code, Message: "fictional message for " + code},
	}
}

// sendErr is what the SDK returns when the request never reaches AWS.
func sendErr(service, op, host string, cause error) error {
	return &smithy.OperationError{
		ServiceID:     service,
		OperationName: op,
		Err: &smithyhttp.RequestSendError{Err: &url.Error{
			Op:  "Post",
			URL: "https://" + host + "/",
			Err: &net.OpError{Op: "dial", Net: "tcp", Err: cause},
		}},
	}
}

// cached runs err through the SDK credentials cache, which wraps every provider error
// before Fetch sees it.
func cached(err error) error {
	c := aws.NewCredentialsCache(aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		return aws.Credentials{}, err
	}))
	_, got := c.Retrieve(context.Background())
	return got
}

func TestClassifyAPIErrors(t *testing.T) {
	cases := []struct {
		code string
		want ErrKind
	}{
		{"UnauthorizedException", ErrSSOExpired},
		{"InvalidGrantException", ErrSSOExpired},
		// Outside the SSO services, an expired token is an expired session key.
		{"ExpiredTokenException", ErrBadKeys},
		{"InvalidClientException", ErrOther},
		{"AuthFailure", ErrBadKeys},
		{"InvalidClientTokenId", ErrBadKeys},
		{"SignatureDoesNotMatch", ErrBadKeys},
		{"UnrecognizedClientException", ErrBadKeys},
		{"ExpiredToken", ErrBadKeys},
		{"RequestExpired", ErrBadKeys},
		{"UnauthorizedOperation", ErrDenied},
		{"AccessDenied", ErrDenied},
		{"AccessDeniedException", ErrDenied},
		{"AccessDeniedForDependencyException", ErrDenied},
		{"ForbiddenException", ErrDenied},
		{"OptInRequired", ErrRegion},
		{"InvalidRegion", ErrRegion},
		{"ThrottlingException", ErrThrottled},
		{"Throttling", ErrThrottled},
		{"RequestLimitExceeded", ErrThrottled},
		{"TooManyRequestsException", ErrThrottled},
		{"InternalError", ErrOther},
	}
	for _, c := range cases {
		t.Run(c.code, func(t *testing.T) {
			err := apiErr("EC2", "DescribeInstances", c.code)
			if got := Classify(err); got != c.want {
				t.Errorf("Classify(%s) = %q, want %q", c.code, got, c.want)
			}
			// Wrapped once more by the caller: same result.
			if got := Classify(fmt.Errorf("acme-prod eu-west-1: %w", err)); got != c.want {
				t.Errorf("wrapped %s: %q, want %q", c.code, got, c.want)
			}
		})
	}
}

func TestClassifyThroughCredentialsCache(t *testing.T) {
	// API errors raised while getting credentials (STS, SSO) keep their meaning once
	// wrapped by the credentials cache.
	cases := []struct {
		name string
		err  error
		want ErrKind
	}{
		{"SSO portal rejects the token", apiErr("SSO", "GetRoleCredentials", "UnauthorizedException"), ErrSSOExpired},
		{"SSO token refresh refused", fmt.Errorf("refresh cached SSO token failed, unable to refresh SSO token, %w",
			apiErr("SSO OIDC", "CreateToken", "InvalidGrantException")), ErrSSOExpired},
		{"SSO token expired at refresh", fmt.Errorf("refresh cached SSO token failed, unable to refresh SSO token, %w",
			apiErr("SSO OIDC", "CreateToken", "ExpiredTokenException")), ErrSSOExpired},
		{"SSO client registration expired", apiErr("SSO OIDC", "CreateToken", "InvalidClientException"), ErrSSOExpired},
		{"permission set removed from the account", apiErr("SSO", "GetRoleCredentials", "ForbiddenException"), ErrDenied},
		{"SSO portal throttles", apiErr("SSO", "GetRoleCredentials", "TooManyRequestsException"), ErrThrottled},
		{"STS denies the role", apiErr("STS", "AssumeRole", "AccessDenied"), ErrDenied},
		{"STS rejects the source keys", apiErr("STS", "AssumeRole", "InvalidClientTokenId"), ErrBadKeys},
		{"STS: expired web identity token", apiErr("STS", "AssumeRoleWithWebIdentity", "ExpiredTokenException"), ErrBadKeys},
		{"STS throttles", apiErr("STS", "AssumeRole", "Throttling"), ErrThrottled},
		{"invalid token error", &ssocreds.InvalidTokenError{}, ErrSSOExpired},
		{"credential_process fails", errors.New("error in credential_process, exit status 1"), ErrOther},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Classify(cached(c.err)); got != c.want {
				t.Errorf("got %q, want %q (%v)", got, c.want, cached(c.err))
			}
		})
	}
}

func TestClassifyCredentialsFailureInsideACall(t *testing.T) {
	// A credentials failure during an EC2 call is wrapped by the EC2 operation error,
	// then by the SSO OIDC one: the service that returned the code is the closest.
	inner := fmt.Errorf("failed to retrieve credentials: %w",
		cached(fmt.Errorf("refresh cached SSO token failed, unable to refresh SSO token, %w",
			apiErr("SSO OIDC", "CreateToken", "ExpiredTokenException"))))
	err := &smithy.OperationError{ServiceID: "EC2", OperationName: "DescribeRegions", Err: inner}
	if got := Classify(err); got != ErrSSOExpired {
		t.Errorf("got %q, want %q (%v)", got, ErrSSOExpired, err)
	}
	// errors.Join is followed too.
	joined := errors.Join(errors.New("first provider failed"), apiErr("SSO", "GetRoleCredentials", "ForbiddenException"))
	if got := Classify(joined); got != ErrDenied {
		t.Errorf("joined: got %q, want %q", got, ErrDenied)
	}
}

func TestClassifySSOProviders(t *testing.T) {
	// Real SDK SSO providers, reading a token cache in a temporary home: no network
	// call happens, because the token is missing or expired.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	cacheFile := func(key string) string {
		p, err := ssocreds.StandardCachedTokenFilepath(key)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	writeToken := func(key, content string) {
		p := cacheFile(key)
		os.MkdirAll(filepath.Dir(p), 0o700)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeToken("https://globex.awsapps.com/start", `{"accessToken":"secret","expiresAt":"2020-01-01T00:00:00Z"}`)

	retrieve := func(p aws.CredentialsProvider) error {
		_, err := aws.NewCredentialsCache(p).Retrieve(context.Background())
		if err == nil {
			t.Fatal("want an error")
		}
		return err
	}
	cases := []struct {
		name string
		p    aws.CredentialsProvider
	}{
		{"legacy format, no token", ssocreds.New(nil, "111111111111", "ReadOnly", "https://acme.awsapps.com/start")},
		{"legacy format, expired token", ssocreds.New(nil, "111111111111", "ReadOnly", "https://globex.awsapps.com/start")},
		{"sso_session format, no token", ssocreds.New(nil, "111111111111", "ReadOnly", "https://acme.awsapps.com/start", func(o *ssocreds.Options) {
			o.SSOTokenProvider = ssocreds.NewSSOTokenProvider(nil, cacheFile("acme-sso"))
		})},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := retrieve(c.p)
			if got := Classify(err); got != ErrSSOExpired {
				t.Errorf("got %q, want %q (%v)", got, ErrSSOExpired, err)
			}
		})
	}
}

func TestClassifyNetwork(t *testing.T) {
	dns := &net.DNSError{Err: "no such host", Name: "ec2.eu-west-1.amazonaws.com", IsNotFound: true}
	cases := []struct {
		name string
		err  error
	}{
		{"DNS failure on EC2", sendErr("EC2", "DescribeInstances", "ec2.eu-west-1.amazonaws.com", dns)},
		{"connection refused", sendErr("SSM", "DescribeInstanceInformation", "ssm.eu-west-1.amazonaws.com", errors.New("connect: connection refused"))},
		{"timeout", fmt.Errorf("operation error EC2: DescribeInstances, %w", context.DeadlineExceeded)},
		{"bare net error", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("i/o timeout")}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Classify(c.err); got != ErrNetwork {
				t.Errorf("got %q, want %q (%v)", got, ErrNetwork, c.err)
			}
		})
	}
}

func TestClassifyFileErrorsAreNotNetwork(t *testing.T) {
	// syscall.Errno implements net.Error: a missing file must not look like a network
	// failure.
	_, err := os.Open(filepath.Join(t.TempDir(), "missing.json"))
	if err == nil {
		t.Fatal("want an error")
	}
	if got := Classify(err); got != ErrOther {
		t.Errorf("missing file: %q, want %q", got, ErrOther)
	}
	if got := Classify(cached(fmt.Errorf("failed to read cached SSO token file, %w", err))); got != ErrSSOExpired {
		t.Errorf("missing SSO token file: %q, want %q", got, ErrSSOExpired)
	}
}

func TestClassifyNetworkWhileGettingCredentials(t *testing.T) {
	// Offline, a valid SSO token or a role must not be reported as an expired token.
	cases := []struct {
		name string
		err  error
	}{
		{"offline while the SSO token is valid", sendErr("SSO", "GetRoleCredentials", "portal.sso.eu-west-1.amazonaws.com",
			&net.DNSError{Err: "no such host", Name: "portal.sso.eu-west-1.amazonaws.com", IsNotFound: true})},
		{"offline while assuming a role", sendErr("STS", "AssumeRole", "sts.eu-west-1.amazonaws.com", errors.New("connect: network is unreachable"))},
		{"timeout while getting credentials", fmt.Errorf("operation error SSO: GetRoleCredentials, %w", context.DeadlineExceeded)},
		{"offline while refreshing the SSO token", fmt.Errorf("refresh cached SSO token failed, unable to refresh SSO token, %w",
			sendErr("SSO OIDC", "CreateToken", "oidc.eu-west-1.amazonaws.com", errors.New("connect: network is unreachable")))},
		{"interrupted", fmt.Errorf("operation error STS: AssumeRole, %w", context.Canceled)},
		{"proxy refuses the request", &smithy.OperationError{ServiceID: "SSO", OperationName: "GetRoleCredentials",
			Err: &smithyhttp.RequestSendError{Err: errors.New("proxyconnect tcp: tls: handshake failure")}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := cached(c.err)
			if got := Classify(err); got != ErrNetwork {
				t.Errorf("got %q, want %q (%v)", got, ErrNetwork, err)
			}
		})
	}
}

// redirect sends every SDK request to a local server, whatever its host, so that no
// request can reach AWS. fail, when set, stands for a network that is down.
type redirect struct {
	host string
	fail error
}

func (r redirect) RoundTrip(req *http.Request) (*http.Response, error) {
	if r.fail != nil {
		return nil, r.fail
	}
	req = req.Clone(req.Context())
	req.URL.Scheme, req.URL.Host = "http", r.host
	return http.DefaultTransport.RoundTrip(req)
}

func TestClassifyRealCredentialChains(t *testing.T) {
	// The SDK itself builds the error chains: an SSO profile with a valid token, and a
	// role assumed with static keys, each failing in a different way.
	fakeEnv(t, "http://127.0.0.1:1")
	cfgPath := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(cfgPath, []byte(`[sso-session acme-sso]
sso_start_url = https://acme.awsapps.com/start
sso_region = eu-west-1

[profile acme-sso-prod]
sso_session = acme-sso
sso_account_id = 111111111111
sso_role_name = ReadOnly

[profile acme-role]
role_arn = arn:aws:iam::111111111111:role/Admin
source_profile = acme-prod
`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_CONFIG_FILE", cfgPath)
	tok, err := ssocreds.StandardCachedTokenFilepath("acme-sso")
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Dir(tok), 0o700)
	valid := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	if err := os.WriteFile(tok, []byte(`{"accessToken":"fictional","expiresAt":"`+valid+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	offline := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: network is unreachable")}
	cases := []struct {
		name    string
		profile string
		offline bool
		status  int
		code    string
		want    ErrKind
	}{
		{"SSO, offline", "acme-sso-prod", true, 0, "", ErrNetwork},
		{"SSO, permission set removed", "acme-sso-prod", false, 403, "ForbiddenException", ErrDenied},
		{"SSO, throttled", "acme-sso-prod", false, 429, "TooManyRequestsException", ErrThrottled},
		{"SSO, token refused", "acme-sso-prod", false, 401, "UnauthorizedException", ErrSSOExpired},
		{"assume role, offline", "acme-role", true, 0, "", ErrNetwork},
		{"assume role, denied", "acme-role", false, 403, "AccessDenied", ErrDenied},
		{"assume role, throttled", "acme-role", false, 400, "Throttling", ErrThrottled},
		{"assume role, revoked source keys", "acme-role", false, 403, "InvalidClientTokenId", ErrBadKeys},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// SSO (GET /federation/credentials) answers in JSON, STS in XML.
				if r.Method == http.MethodGet {
					w.Header().Set("X-Amzn-Errortype", c.code)
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(c.status)
					fmt.Fprint(w, `{"message":"fictional"}`)
					return
				}
				w.Header().Set("Content-Type", "text/xml")
				w.WriteHeader(c.status)
				fmt.Fprintf(w, `<ErrorResponse><Error><Type>Sender</Type><Code>%s</Code><Message>fictional</Message></Error><RequestId>r</RequestId></ErrorResponse>`, c.code)
			}))
			defer srv.Close()
			rt := redirect{host: srv.Listener.Addr().String()}
			if c.offline {
				rt.fail = offline
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cfg, err := awsconfig.LoadDefaultConfig(ctx,
				awsconfig.WithSharedConfigProfile(c.profile),
				awsconfig.WithRegion("eu-west-1"),
				awsconfig.WithRetryMaxAttempts(1),
				awsconfig.WithHTTPClient(&http.Client{Transport: rt}),
			)
			if err != nil {
				t.Fatal(err)
			}
			_, err = cfg.Credentials.Retrieve(ctx)
			if err == nil {
				t.Fatal("want an error")
			}
			if got := Classify(err); got != c.want {
				t.Errorf("got %q, want %q (%v)", got, c.want, err)
			}
		})
	}
}

func TestClassifyMessages(t *testing.T) {
	cases := []struct {
		msg  string
		want ErrKind
	}{
		{"failed to refresh cached credentials, the SSO session has expired or is invalid", ErrSSOExpired},
		{"refresh cached SSO token failed, operation error SSO OIDC: CreateToken", ErrSSOExpired},
		{"failed to read cached SSO token file, open /x: no such file or directory", ErrSSOExpired},
		{"refresh cached SSO token failed, cached SSO token is expired, or not present, and cannot be refreshed", ErrSSOExpired},
		{"profile acme-prod: SSO token expired", ErrSSOExpired},
		{"SSO token expired for acme-sso", ErrSSOExpired},
		{"failed to refresh cached credentials, load login token: token file not found, please reauthenticate", ErrLoginExpired},
		{"failed to refresh cached credentials, create oauth2 token: login session has expired, please reauthenticate", ErrLoginExpired},
		// The credentials cache prefix alone says nothing about the cause.
		{"failed to refresh cached credentials, error in credential_process, exit status 1", ErrOther},
		{"failed to refresh cached credentials, operation error STS: AssumeRole, https response error", ErrOther},
		{"failed to refresh cached credentials, no EC2 IMDS role found", ErrOther},
		{"failed to load shared config profile, acme-prod", ErrOther},
		{"something unexpected", ErrOther},
	}
	for _, c := range cases {
		if got := Classify(errors.New(c.msg)); got != c.want {
			t.Errorf("%q: %q, want %q", c.msg, got, c.want)
		}
	}
	if got := Classify(nil); got != "" {
		t.Errorf("Classify(nil) = %q", got)
	}
}

func TestExplain(t *testing.T) {
	kinds := []ErrKind{ErrSSOExpired, ErrLoginExpired, ErrBadKeys, ErrDenied, ErrRegion, ErrThrottled, ErrNetwork}
	seen := map[string]ErrKind{}
	for _, k := range kinds {
		e := k.Explain()
		if e == "" || e == "error" {
			t.Errorf("%s: no explanation (%q)", k, e)
		}
		if other, dup := seen[e]; dup {
			t.Errorf("%s and %s share the explanation %q", k, other, e)
		}
		seen[e] = k
	}
	for _, k := range []ErrKind{ErrOther, "", "unknown"} {
		if k.Explain() != "error" {
			t.Errorf("%q: %q, want the generic text", k, k.Explain())
		}
	}
}
