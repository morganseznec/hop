package inventory

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws/retry"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/ssocreds"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

type ErrKind string

const (
	ErrSSOExpired   ErrKind = "sso-expired"
	ErrLoginExpired ErrKind = "login-expired"
	ErrBadKeys      ErrKind = "bad-keys"
	ErrDenied       ErrKind = "access-denied"
	ErrRegion       ErrKind = "region-inactive"
	ErrThrottled    ErrKind = "throttled"
	ErrNetwork      ErrKind = "network"
	ErrMFA          ErrKind = "mfa-required"
	ErrOther        ErrKind = "other"
)

// Phrases of the SDK SSO token provider, which returns plain errors ("failed to read
// cached SSO token file", "refresh cached SSO token failed", "cached SSO token is
// expired", "unable to refresh SSO token"), of ssocreds.InvalidTokenError once turned
// into text, and of hop itself ("SSO token expired for acme-prod").
var ssoTokenMessages = []string{
	"cached sso token",
	"unable to refresh sso token",
	"sso session has expired",
	"sso token expired",
}

// Classify maps an SDK error to a cause that can be explained in a few words. The raw
// plugin message ("KMSEncryption failed ... SSOProviderInvalidToken") is exactly what
// a user should not have to decode.
//
// Typed errors are checked before any message: the SDK credentials cache wraps every
// provider failure in "failed to refresh cached credentials", whatever its cause
// (network down while calling SSO, permission set removed, throttling).
func Classify(err error) ErrKind {
	if err == nil {
		return ""
	}
	var tokErr *ssocreds.InvalidTokenError
	if errors.As(err, &tokErr) {
		return ErrSSOExpired
	}
	// A role assumed with mfa_serial: the SDK needs a code that hop does not ask for.
	var mfaErr awsconfig.AssumeRoleTokenProviderNotSetError
	if errors.As(err, &mfaErr) {
		return ErrMFA
	}
	if isNetwork(err) {
		return ErrNetwork
	}
	if code, service, ok := apiError(err); ok {
		if k := classifyCode(code, service); k != "" {
			return k
		}
	}
	msg := strings.ToLower(err.Error())
	for _, m := range ssoTokenMessages {
		if strings.Contains(msg, m) {
			return ErrSSOExpired
		}
	}
	// The `aws login` provider: "login session has expired, please reauthenticate",
	// "token file not found, please reauthenticate".
	if strings.Contains(msg, "login") && strings.Contains(msg, "reauthenticate") {
		return ErrLoginExpired
	}
	return ErrOther
}

func isNetwork(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	// Concrete types only: net.Error is also implemented by syscall.Errno, so it would
	// match a missing token file ("no such file or directory").
	var opErr *net.OpError
	var dnsErr *net.DNSError
	var urlErr *url.Error
	// RequestSendError: the request never reached AWS (proxy, TLS interception...).
	var reqErr *smithyhttp.RequestSendError
	return errors.As(err, &opErr) || errors.As(err, &dnsErr) || errors.As(err, &urlErr) || errors.As(err, &reqErr)
}

func classifyCode(code, service string) ErrKind {
	if _, ok := retry.DefaultThrottleErrorCodes[code]; ok {
		return ErrThrottled
	}
	switch code {
	case "UnauthorizedException", "InvalidGrantException":
		return ErrSSOExpired
	case "ExpiredTokenException":
		// From the SSO services, the portal token expired. From any other service
		// (SSM, STS), the session keys did.
		if service == "SSO" || service == "SSO OIDC" {
			return ErrSSOExpired
		}
		return ErrBadKeys
	case "InvalidClientException":
		// The client registered by `aws sso login` expired: a new login renews it.
		if service == "SSO OIDC" {
			return ErrSSOExpired
		}
	case "AuthFailure", "InvalidClientTokenId", "SignatureDoesNotMatch", "UnrecognizedClientException", "ExpiredToken", "RequestExpired":
		return ErrBadKeys
	case "UnauthorizedOperation", "ForbiddenException":
		// ForbiddenException: the SSO portal no longer grants this role (permission
		// set removed from the account).
		return ErrDenied
	case "OptInRequired", "InvalidRegion":
		return ErrRegion
	}
	if strings.HasPrefix(code, "AccessDenied") {
		return ErrDenied
	}
	return ""
}

// apiError finds the API error in the chain and the service that returned it. The
// service is that of the closest operation error: a credentials failure inside an EC2
// call is wrapped by both the EC2 and the SSO (or STS) operation errors.
func apiError(err error) (code, service string, ok bool) {
	var api smithy.APIError
	if !errors.As(err, &api) {
		return "", "", false
	}
	var op *smithy.OperationError
	for e := err; errors.As(e, &op); e = op.Err {
		service = op.ServiceID
	}
	return api.ErrorCode(), service, true
}

// Explain returns a short, human-readable phrase.
func (k ErrKind) Explain() string {
	switch k {
	case ErrSSOExpired:
		return "SSO token expired"
	case ErrLoginExpired:
		return `"aws login" session expired`
	case ErrBadKeys:
		return "keys rejected by AWS (revoked or expired)"
	case ErrDenied:
		return "insufficient permissions"
	case ErrRegion:
		return "region not enabled for this account"
	case ErrThrottled:
		return "too many requests, throttled by AWS"
	case ErrNetwork:
		return "network error or timeout"
	case ErrMFA:
		return "role assumed with an MFA code, which hop does not prompt for yet"
	}
	return "error"
}
