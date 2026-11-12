package bucket

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

var (
	// ErrNotFound reports an object that does not exist, or that the caller may not
	// see: without s3:ListBucket, S3 answers 403 for a missing key.
	ErrNotFound = errors.New("no such object")
	// ErrExists reports an upload refused because the object is already there.
	ErrExists = errors.New("already exists")
	// ErrDenied reports a call that IAM, or the policy of the bucket, refused.
	ErrDenied = errors.New("IAM denied")
)

// codeAndStatus returns the S3 error code and the HTTP status of an SDK error. A HEAD
// request has no body to carry a code: the status is then all there is.
func codeAndStatus(err error) (code, message string, status int) {
	var api smithy.APIError
	if errors.As(err, &api) {
		code, message = api.ErrorCode(), api.ErrorMessage()
	}
	var re *smithyhttp.ResponseError
	if errors.As(err, &re) {
		status = re.HTTPStatusCode()
	}
	return code, message, status
}

// explain turns the usual S3 errors into a message that names the bucket or the object
// and says what to check. action is what hop was doing: "listing", "reading",
// "writing".
func explain(err error, action string, loc Location) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	code, message, status := codeAndStatus(err)
	switch {
	case code == "NoSuchBucket":
		return fmt.Errorf("no bucket named %s", loc.Bucket)
	case code == "NoSuchKey" || code == "NotFound" || status == 404:
		return fmt.Errorf("%w: %s", ErrNotFound, loc)
	case code == "InvalidObjectState":
		return fmt.Errorf("%s is archived: restore it before downloading it", loc)
	case status == 301 || code == "PermanentRedirect" || code == "AuthorizationHeaderMalformed" || code == "IllegalLocationConstraintException":
		return fmt.Errorf("bucket %s is not in the region hop has for it: hop s3 ls --refresh reads it again", loc.Bucket)
	case status == 403 || strings.HasPrefix(code, "AccessDenied"):
		// A HEAD request has no message of its own: the SDK repeats the status.
		detail := ""
		if message != "" && !strings.EqualFold(message, "Forbidden") && !strings.EqualFold(message, "Access Denied") {
			detail = ": " + message
		}
		return fmt.Errorf("%w %s %s%s", ErrDenied, action, loc, detail)
	}
	return err
}
