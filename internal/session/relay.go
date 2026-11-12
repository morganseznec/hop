package session

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/morganseznec/hop/internal/bucket"
)

// Relay is where PushVia may leave a file for the time an instance takes to fetch it:
// a bucket that already exists, and that the profile of the instance may write to, read
// from and delete from.
type Relay struct {
	// Config holds the credentials of the instance's profile, set to the region of the
	// bucket.
	Config aws.Config
	Bucket string
	// Prefix starts the key of the temporary object. It is empty, or ends with "/".
	Prefix string
}

// RelayResult is what PushVia did.
type RelayResult struct {
	// Output is the answer of the command that fetched the file on the instance.
	Output *Output
	// Size is the number of bytes sent.
	Size int64
	// Object is the temporary object in the relay bucket.
	Object bucket.Location
	// Left reports that Object is still in the bucket, and why: the instance may
	// still be fetching it, or deleting it failed (LeftErr).
	Left    bool
	LeftErr error
}

// relayExpiry is how long the presigned URL stays valid past the wait: it must still
// be good when the instance starts the download, however late SSM delivers the command.
const relayExpiry = 5 * time.Minute

// RelayWait is how long PushVia waits for the instance to fetch a file of that size:
// two minutes, plus a second per megabyte, 12 hours at most.
func RelayWait(size int64) time.Duration {
	return min(2*time.Minute+time.Duration(size/1_000_000)*time.Second, 12*time.Hour)
}

// relayWait is RelayWait. Tests shorten it.
var relayWait = RelayWait

// CheckRemote refuses a remote path that is not absolute: a relative one would land in
// the working directory the SSM agent creates for each command.
func CheckRemote(remote string) error {
	if !strings.HasPrefix(remote, "/") {
		return fmt.Errorf("remote path must be absolute (start with /): %s", remote)
	}
	return nil
}

// PushVia copies a file of any size to an instance, through a relay bucket: the file is
// uploaded under a random key, the instance downloads it with a presigned URL (it needs
// no S3 permission of its own, only a route to S3 and curl or wget), the SHA-256
// computed there is compared with the one of the file, and the object is deleted.
//
// Unlike Push, the content never goes through SSM: the command history of the account
// keeps the URL, expired by then, of an object that no longer exists.
//
// Every S3 permission is tried before the instance is asked anything: the upload
// (s3:PutObject), then a look at the object (s3:GetObject, which the presigned URL
// relies on). The object stays in the bucket only when it could not be deleted, or when
// hop stopped waiting while the instance was still fetching it: RelayResult says which.
func PushVia(ctx context.Context, cfg aws.Config, relay Relay, instanceID, local, remote string, progress bucket.Progress, uploaded func()) (res RelayResult, err error) {
	if err := CheckRemote(remote); err != nil {
		return res, err
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return res, err
	}
	// A random key: nobody can guess it, and the name of the file stays out of the URL.
	res.Object = bucket.Location{Bucket: relay.Bucket, Key: relay.Prefix + "hop-relay-" + hex.EncodeToString(nonce)}

	sum := sha256.New()
	up, err := bucket.Upload(ctx, relay.Config, local, res.Object, bucket.UploadOptions{Progress: progress, Digest: sum})
	if err != nil {
		return res, fmt.Errorf("relay bucket: %w", err)
	}
	res.Size = up.Size
	want := hex.EncodeToString(sum.Sum(nil))
	// From here on the object exists: it goes whatever happens next, unless the
	// instance may still be reading it. res is the named result: what the deferred
	// call records reaches the caller.
	keep := false
	defer func() {
		if keep {
			res.Left = true
			return
		}
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cancelTimeout)
		defer cancel()
		if err := bucket.Delete(dctx, relay.Config, res.Object); err != nil {
			res.Left, res.LeftErr = true, err
		}
	}()

	if _, err := bucket.Head(ctx, relay.Config, res.Object); err != nil {
		if errors.Is(err, bucket.ErrDenied) {
			return res, fmt.Errorf("your role may write to the relay bucket %s but not read from it (s3:GetObject): the instance could not fetch the file with a URL signed by that role", relay.Bucket)
		}
		return res, fmt.Errorf("relay bucket: %w", err)
	}
	wait := relayWait(up.Size)
	url, err := bucket.Presign(ctx, relay.Config, res.Object, wait+relayExpiry)
	if err != nil {
		return res, fmt.Errorf("relay bucket: signing the download URL: %w", err)
	}
	script, err := fetchScript(url, remote, want)
	if err != nil {
		return res, err
	}
	if uploaded != nil {
		uploaded()
	}
	out, err := Run(ctx, cfg, instanceID, script, wait)
	res.Output = out
	if err != nil {
		keep = mayStillRun(err)
		return res, err
	}
	if out.Status != "Success" {
		msg := fmt.Sprintf("copy failed on the instance (status %s, exit code %d)", out.Status, out.ExitCode)
		if why := explainFetch(out.Stderr); why != "" {
			msg += ": " + why
		}
		return res, errors.New(msg)
	}
	got := strings.TrimSpace(out.Stdout)
	if got == "" {
		return res, errors.New("copied, but the instance returned no SHA-256 to compare (no sha256sum, sha256 or shasum)")
	}
	if got != want {
		return res, fmt.Errorf("checksum mismatch after copy: local %s, remote %s", want, got)
	}
	return res, nil
}

// mayStillRun reports whether the command hop stopped following had reached the
// instance: it may then still be downloading the file.
func mayStillRun(err error) bool {
	var te *TimeoutError
	var ie *InterruptedError
	var pe *PollError
	switch {
	case errors.As(err, &te):
		return te.Status != "TimedOut" && !undelivered(te.Status)
	case errors.As(err, &ie):
		return !undelivered(ie.Status)
	case errors.As(err, &pe):
		return !undelivered(pe.Status)
	}
	return false
}

// hashOf returns the shell pipeline that prints the SHA-256 of a file, given as the
// shell word that names it: sha256sum on Linux, sha256 on FreeBSD, shasum on older
// macOS (see pushScript). Each tool reads the file itself: a redirection after the
// pipeline would feed cut.
func hashOf(file string) string {
	return `(sha256sum < ` + file + ` 2>/dev/null || sha256 -q < ` + file + ` 2>/dev/null || shasum -a 256 < ` + file + `) | cut -d' ' -f1`
}

// fetchScript returns the shell commands that download url to dst and print the
// SHA-256 of dst. The file is downloaded into a private directory next to dst, on the
// same file system (/tmp is often too small for what a relay carries), and compared
// with want, the SHA-256 of the file sent, before anything touches dst: a download that
// fails, or that is not the file, leaves dst as it was. It is then copied over dst as
// pushScript does, so an existing file keeps its owner and mode. The instance needs room
// for the file twice while it is copied.
func fetchScript(url, dst, want string) ([]string, error) {
	// The URL goes into a quoted here-document, and for curl between double quotes: it
	// must not be able to end either.
	if strings.ContainsAny(url, "\"\\\n\r") || !strings.HasPrefix(url, "http") {
		return nil, errors.New("unexpected characters in the presigned URL")
	}
	if _, err := hex.DecodeString(want); err != nil || len(want) != 64 {
		return nil, errors.New("unexpected SHA-256 for the file sent")
	}
	return []string{
		"set -e",
		"dst=" + ShellQuote(dst),
		`d=$(umask 077 && mktemp -d "$(dirname "$dst")/.hop-push.XXXXXXXX")`,
		`trap 'rm -rf "$d"' EXIT`,
		// Created up front: an old curl writes no file at all for an empty one.
		`: > "$d/data"`,
		// The URL is read from stdin, not from the command line, where any user of the
		// instance could read it in the process list while the download runs.
		`if command -v curl >/dev/null 2>&1; then curl -fsS --retry 2 -o "$d/data" -K - <<'HOP_URL'`,
		`url = "` + url + `"`,
		`HOP_URL`,
		`elif command -v wget >/dev/null 2>&1; then wget -q -O "$d/data" -i - <<'HOP_URL'`,
		url,
		`HOP_URL`,
		`else echo "no curl or wget on the instance" >&2; exit 127; fi`,
		// "$( (" with a space: dash, the sh of Debian and Ubuntu, reads "$((" as the
		// start of an arithmetic expansion.
		`got=$( ` + hashOf(`"$d/data"`) + ` )`,
		`[ "$got" = ` + want + ` ] || { echo "the file downloaded is not the file sent (SHA-256 $got): $dst is unchanged" >&2; exit 3; }`,
		`cat "$d/data" > "$dst"`,
		hashOf(`"$dst"`),
	}, nil
}

// explainFetch reads what curl or wget said on the instance, and says what to check.
func explainFetch(stderr string) string {
	s := strings.ToLower(stderr)
	switch {
	case strings.Contains(s, "no curl or wget"):
		return "the instance has neither curl nor wget to fetch the file"
	case strings.Contains(s, "is not the file sent"):
		return "what the instance downloaded differs from the file sent"
	case strings.Contains(s, "wget") && (strings.Contains(s, "unrecognized option") || strings.Contains(s, "invalid option")):
		// The wget of BusyBox (Alpine) has no -i: it would need the URL as an argument,
		// where the process list shows it.
		return "the wget of the instance cannot read the URL from stdin (BusyBox), and there is no curl: install curl on the instance"
	case strings.Contains(s, "403"):
		return "S3 refused the download (403): a policy of the bucket, or of the VPC endpoint the instance goes through, does not allow it, or the credentials that signed the URL expired"
	case strings.Contains(s, "could not resolve") || strings.Contains(s, "unable to resolve") || strings.Contains(s, "timed out") ||
		strings.Contains(s, "failed to connect") || strings.Contains(s, "couldn't connect") || strings.Contains(s, "connection refused"):
		return "the instance cannot reach S3: it needs a VPC endpoint for S3, or a route out"
	case strings.Contains(s, "no space left"):
		return "no space left on the instance: the file needs twice its size there while it is copied"
	}
	return ""
}
