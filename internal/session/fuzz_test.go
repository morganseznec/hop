package session

import (
	"strings"
	"testing"
)

// unquote reads s the way a POSIX shell reads a word made of single-quoted strings and
// \-escaped characters. ok is false if anything else appears, since the shell would
// then interpret it.
func unquote(s string) (out string, ok bool) {
	var b strings.Builder
	quoted := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quoted && c == '\'':
			quoted = false
		case quoted:
			b.WriteByte(c)
		case c == '\'':
			quoted = true
		case c == '\\' && i+1 < len(s):
			i++
			b.WriteByte(s[i])
		default:
			return "", false
		}
	}
	return b.String(), !quoted
}

// FuzzShellQuote checks that any string, once quoted, reaches the remote shell as one
// word with exactly its original bytes: remote paths and commands cannot break out.
func FuzzShellQuote(f *testing.F) {
	for _, seed := range []string{"", "/etc/app.conf", "it's", "'", "''", `\`, "$(id)", "`id`", "a b\nc", "-rf", "é"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		q := ShellQuote(s)
		got, ok := unquote(q)
		if !ok || got != s {
			t.Fatalf("ShellQuote(%q) = %s reads back as %q (ok=%v)", s, q, got, ok)
		}
	})
}
