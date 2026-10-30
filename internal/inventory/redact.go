package inventory

import "regexp"

var (
	// When the SDK cannot parse a credential_process's output, it quotes that output in
	// full in the error: keys and session token included.
	processOutput = regexp.MustCompile(`(?is)(process output:?).*`)
	secretField   = regexp.MustCompile(`(?i)("?(?:secret_?access_?key|session_?token)"?\s*[:=]\s*)("[^"]*"|[^\s,}]+)`)
	accessKeyID   = regexp.MustCompile(`\b(?:AKIA|ASIA)[A-Z0-9]{16}\b`)
)

const redacted = "[redacted]"

// Redact removes credentials from an error message before it is shown, logged or
// written to the cache.
func Redact(s string) string {
	s = processOutput.ReplaceAllString(s, "$1 "+redacted)
	s = secretField.ReplaceAllString(s, "${1}"+redacted)
	return accessKeyID.ReplaceAllString(s, redacted)
}

// RedactError wraps err so that its message goes through Redact, while errors.Is and
// errors.As still see the original error.
func RedactError(err error) error {
	if err == nil {
		return nil
	}
	return redactedError{err}
}

type redactedError struct{ err error }

func (e redactedError) Error() string { return Redact(e.err.Error()) }
func (e redactedError) Unwrap() error { return e.err }
