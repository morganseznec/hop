package session

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/smithy-go"
)

// SSM truncates the output returned by GetCommandInvocation to 24,000 characters.
const outputLimit = 24000

// pollInterval is the pause between two GetCommandInvocation calls. Tests shorten it.
var pollInterval = 1500 * time.Millisecond

// SendCommand accepts a delivery timeout between 30 seconds and 30 days.
const (
	minDeliveryTimeout = 30 * time.Second
	maxDeliveryTimeout = 30 * 24 * time.Hour
)

// cancelTimeout bounds the CancelCommand call made once hop stops waiting, with a
// context of its own since the caller's may already be canceled.
const cancelTimeout = 10 * time.Second

type Output struct {
	CommandID string
	Status    string
	Stdout    string
	Stderr    string
	ExitCode  int32
	Truncated bool
}

// TimeoutError reports that hop stopped waiting for a command. Status is the last
// invocation status seen: Pending, Delayed or empty means that the command never
// reached the instance, and Canceled whether hop canceled it; TimedOut, that SSM gave
// up delivering it. Otherwise the command keeps running on the instance.
type TimeoutError struct {
	CommandID  string
	InstanceID string
	Waited     time.Duration
	Status     string
	Canceled   bool
	cancelErr  error
}

func (e *TimeoutError) Error() string {
	if e.Status != "TimedOut" && !undelivered(e.Status) {
		return fmt.Sprintf("still running after %s: the command keeps running on %s (id %s)", e.Waited, e.InstanceID, e.CommandID)
	}
	what := "the command expired"
	if e.Status != "TimedOut" {
		what = dropped(e.Canceled, e.cancelErr)
	}
	return fmt.Sprintf("%s did not pick up the command within %s (instance stopped or SSM agent offline): %s (id %s)", e.InstanceID, e.Waited, what, e.CommandID)
}

// InterruptedError reports that the context ended (Ctrl-C) while hop waited for a
// command. The fields read as in TimeoutError; Err is the context error.
type InterruptedError struct {
	CommandID  string
	InstanceID string
	Status     string
	Canceled   bool
	Err        error
	cancelErr  error
}

func (e *InterruptedError) Error() string {
	if !undelivered(e.Status) {
		return fmt.Sprintf("interrupted: the command keeps running on %s (id %s)", e.InstanceID, e.CommandID)
	}
	return fmt.Sprintf("interrupted before %s picked up the command: %s (id %s)", e.InstanceID, dropped(e.Canceled, e.cancelErr), e.CommandID)
}

func (e *InterruptedError) Unwrap() error { return e.Err }

// undelivered reports whether a command whose invocation has this status has not
// reached the instance yet. Right after SendCommand, the invocation does not exist:
// its status is empty.
func undelivered(status string) bool {
	return status == "" || status == "Pending" || status == "Delayed"
}

// dropped says what became of a command that has not reached the instance.
func dropped(canceled bool, cancelErr error) string {
	if canceled {
		return "hop canceled it"
	}
	msg := "it runs if the instance picks it up before its delivery timeout"
	if cancelErr != nil {
		msg = fmt.Sprintf("canceling it failed (%v): %s", cancelErr, msg)
	}
	return msg
}

// deliveryTimeout is the TimeoutSeconds of SendCommand: as long as hop waits, within
// the bounds SSM accepts.
func deliveryTimeout(wait time.Duration) int32 {
	d := min(max(wait, minDeliveryTimeout), maxDeliveryTimeout)
	return int32((d + time.Second - 1) / time.Second)
}

// Run sends shell commands through SendCommand and waits for them to finish. The SDK
// serializes the parameters as JSON, so quotes and slashes need no manual escaping.
//
// The delivery timeout of the command is the wait (30 seconds at least), and a command
// that has not reached the instance when hop stops waiting (wait over or ctx canceled)
// is canceled: it never runs hours later, when a stopped instance comes back. A command
// already running keeps running. The TimeoutError or InterruptedError says which.
func Run(ctx context.Context, cfg aws.Config, instanceID string, commands []string, wait time.Duration) (*Output, error) {
	c := ssm.NewFromConfig(cfg)
	sent, err := c.SendCommand(ctx, &ssm.SendCommandInput{
		InstanceIds:    []string{instanceID},
		DocumentName:   aws.String("AWS-RunShellScript"),
		Parameters:     map[string][]string{"commands": commands},
		Comment:        aws.String("hop"),
		TimeoutSeconds: aws.Int32(deliveryTimeout(wait)),
	})
	if err != nil {
		return nil, explainSend(err, instanceID)
	}
	id := aws.ToString(sent.Command.CommandId)
	status := ""
	stop := func(cause error) error {
		canceled, cancelErr := cancelUndelivered(ctx, c, id, instanceID, status)
		if cause != nil {
			return &InterruptedError{CommandID: id, InstanceID: instanceID, Status: status, Canceled: canceled, Err: cause, cancelErr: cancelErr}
		}
		return &TimeoutError{CommandID: id, InstanceID: instanceID, Waited: wait, Status: status, Canceled: canceled, cancelErr: cancelErr}
	}
	deadline := time.Now().Add(wait)
	for {
		inv, err := c.GetCommandInvocation(ctx, &ssm.GetCommandInvocationInput{CommandId: aws.String(id), InstanceId: aws.String(instanceID)})
		if err == nil {
			status = string(inv.Status)
			switch status {
			case "Success", "Failed", "Cancelled", "TimedOut":
				if aws.ToString(inv.StatusDetails) == "Delivery Timed Out" {
					return nil, &TimeoutError{CommandID: id, InstanceID: instanceID, Waited: wait, Status: status}
				}
				out := &Output{
					CommandID: id, Status: status, ExitCode: inv.ResponseCode,
					Stdout: aws.ToString(inv.StandardOutputContent),
					Stderr: aws.ToString(inv.StandardErrorContent),
				}
				out.Truncated = len(out.Stdout) >= outputLimit || len(out.Stderr) >= outputLimit
				return out, nil
			}
		} else if ctx.Err() == nil {
			// Right after SendCommand, the invocation does not exist yet.
			var api smithy.APIError
			if !errors.As(err, &api) || api.ErrorCode() != "InvocationDoesNotExist" {
				return nil, err
			}
		}
		if err := ctx.Err(); err != nil {
			return nil, stop(err)
		}
		if time.Now().After(deadline) {
			return nil, stop(nil)
		}
		select {
		case <-ctx.Done():
			return nil, stop(ctx.Err())
		case <-time.After(pollInterval):
		}
	}
}

// cancelUndelivered cancels the command if its last known status says that it has not
// reached the instance. It reports whether it did, or the CancelCommand error.
func cancelUndelivered(ctx context.Context, c *ssm.Client, commandID, instanceID, status string) (bool, error) {
	if !undelivered(status) {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cancelTimeout)
	defer cancel()
	_, err := c.CancelCommand(ctx, &ssm.CancelCommandInput{CommandId: aws.String(commandID), InstanceIds: []string{instanceID}})
	return err == nil, err
}

// explainSend turns the usual SendCommand errors into a message that says what to
// check on the instance or in IAM.
func explainSend(err error, id string) error {
	var api smithy.APIError
	if errors.As(err, &api) {
		switch api.ErrorCode() {
		case "InvalidInstanceId", "TargetNotConnected":
			return fmt.Errorf("SSM cannot send commands to %s: the instance is stopped or terminated, or its SSM agent is not running or not registered", id)
		case "UnsupportedPlatformType":
			return fmt.Errorf("%s does not run Linux or macOS: hop runs commands with AWS-RunShellScript", id)
		case "AccessDeniedException":
			return fmt.Errorf("IAM denied sending a command to %s: %s", id, api.ErrorMessage())
		case "InvalidDocument":
			return fmt.Errorf("AWS-RunShellScript is not available in this region: %s", api.ErrorMessage())
		}
	}
	return err
}

// PushLimit is the largest file Push accepts: beyond it, the SendCommand parameters
// get too large.
const PushLimit = 400_000

// pushChunk is the number of base64 characters written by one command of the push
// script, to keep each command short.
const pushChunk = 2800

// Push uploads a file through SendCommand, in base64 chunks, then compares the SHA-256
// computed on the instance with the one of the local file. remote must be absolute.
// When the copy fails on the instance, Push returns the Output (and its Stderr) along
// with the error.
func Push(ctx context.Context, cfg aws.Config, instanceID, local, remote string) (*Output, error) {
	if !strings.HasPrefix(remote, "/") {
		// A relative path would land in the working directory the SSM agent creates for
		// each command.
		return nil, fmt.Errorf("remote path must be absolute (start with /): %s", remote)
	}
	b, err := os.ReadFile(local)
	if err != nil {
		return nil, err
	}
	if len(b) > PushLimit {
		return nil, fmt.Errorf("%d bytes, too large for this method (limit %d): go through S3 instead", len(b), PushLimit)
	}
	sum := sha256.Sum256(b)
	want := hex.EncodeToString(sum[:])

	tmp := "/tmp/.hop-push." + rand.Text()
	out, err := Run(ctx, cfg, instanceID, pushScript(b, tmp, remote), 2*time.Minute)
	if err != nil {
		return nil, err
	}
	if out.Status != "Success" {
		return out, fmt.Errorf("copy failed on the instance (status %s, exit code %d)", out.Status, out.ExitCode)
	}
	got := strings.TrimSpace(out.Stdout)
	if got == "" {
		return out, errors.New("copied, but the instance returned no SHA-256 to compare (no sha256sum, sha256 or shasum)")
	}
	if got != want {
		return out, fmt.Errorf("checksum mismatch after copy: local %s, remote %s", want, got)
	}
	return out, nil
}

// pushScript returns the shell commands that write data to dst and print its SHA-256.
// The base64 text goes, in chunks, to the private temporary file tmp.b64, and is decoded
// to tmp, which is then copied over dst. Writing dst in place keeps the owner and mode
// of an existing file, and a new file gets the instance's default umask; the trap
// removes the temporary files whether the script succeeds or not.
func pushScript(data []byte, tmp, dst string) []string {
	enc := base64.StdEncoding.EncodeToString(data)
	cmds := []string{
		"set -e",
		"tmp=" + ShellQuote(tmp),
		"dst=" + ShellQuote(dst),
		`trap 'rm -f "$tmp.b64" "$tmp"' EXIT`,
		// Created up front, so that an empty file has a temporary file too.
		`(umask 077 && : > "$tmp.b64" && : > "$tmp")`,
	}
	for i := 0; i < len(enc); i += pushChunk {
		cmds = append(cmds, fmt.Sprintf(`printf '%%s' '%s' >> "$tmp.b64"`, enc[i:min(i+pushChunk, len(enc))]))
	}
	return append(cmds,
		// The macOS base64 reads only stdin or -i.
		`base64 -d < "$tmp.b64" > "$tmp"`,
		`cat "$tmp" > "$dst"`,
		// Hashing stdin keeps the file name out of the output, which GNU sha256sum escapes
		// when it contains a backslash or a newline. sha256sum on Linux, sha256 on FreeBSD
		// (pfSense), shasum on older macOS.
		`(sha256sum < "$dst" 2>/dev/null || sha256 -q < "$dst" 2>/dev/null || shasum -a 256 < "$dst") | cut -d' ' -f1`,
	)
}

// ShellQuote wraps s in single quotes, escaping the ones it contains.
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
