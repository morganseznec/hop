package session

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// ListLimit is the most names List returns: enough to complete a path from, and a
// bound on what a directory of a million files sends.
const ListLimit = 2000

// listExitWait is how long List leaves the plugin to exit once the names are read.
// The command has ended by then and the agent closes the session: a completion does
// not wait long for that, and ends the session itself past that time.
var listExitWait = 500 * time.Millisecond

// CheckList refuses what List would refuse, without calling AWS: a directory that is
// not an absolute path.
func CheckList(dir string) error {
	if !strings.HasPrefix(dir, "/") {
		return fmt.Errorf("remote directory must be absolute (start with /): %s", dir)
	}
	return nil
}

// List returns the names in dir, an absolute path on the instance, that start with
// prefix, in the order the shell of the instance sorts them. A directory, or a link to
// one, ends with "/". more reports that there were more than ListLimit.
//
// It goes through a non-interactive session, like Pull, and reads the directory as
// the session user, or with sudo -n when that user cannot. It changes nothing on the
// instance. Names that hold a control character, or that are not UTF-8, are left
// out: they could not be typed back.
func List(ctx context.Context, cfg aws.Config, req Request, dir, prefix string) (names []string, more bool, err error) {
	if err := CheckList(dir); err != nil {
		return nil, false, err
	}
	nonce, err := newNonce()
	if err != nil {
		return nil, false, err
	}
	err = command(ctx, cfg, req, listScript(dir, prefix, nonce), listExitWait, func(stdout io.Reader) (err error) {
		names, more, err = readList(stdout, "--hop"+nonce+"-")
		return err
	})
	if err != nil {
		return nil, false, err
	}
	return names, more, nil
}

// lister is the script that prints the names of the directory $1 that start with $2,
// one per line, "/" after a directory or a link to one. It reads the directory with
// the patterns of the shell rather than with ls: every name comes out as it is, a link
// to nothing included, whatever options the ls of the instance has. A name with a line
// break is left out, since it would read as two. Nothing here is a pattern but the two
// "*": the prefix is compared quoted.
const lister = `nl=$(printf '\nx'); nl=${nl%x}; for f in "$1"/.* "$1"/*; do n=${f##*/}; ` +
	`case $n in .|..) continue;; "$2"*) ;; *) continue;; esac; ` +
	`case $n in *"$nl"*) continue;; esac; ` +
	`if [ -d "$f" ]; then printf '%s/\n' "$n"; elif [ -e "$f" ] || [ -L "$f" ]; then printf '%s\n' "$n"; fi; done`

// listScript returns the shell script that lists dir, between lines that start with
// the marker "--hop<nonce>-":
//
//	--hop<nonce>-begin
//	<one name per line, "/" after a directory>
//	--hop<nonce>-more            (only when names were left out, past ListLimit)
//	--hop<nonce>-end
//
// or a single "--hop<nonce>-error <reason>" line when it cannot read the directory.
// The script is a single line: the agent passes the command on as is.
func listScript(dir, prefix, nonce string) string {
	return strings.Join([]string{
		"d=" + ShellQuote(dir),
		"p=" + ShellQuote(prefix),
		// Built in two steps, so that the marker never appears in the command itself.
		"m=--hop; m=${m}" + nonce,
		`say() { printf '%s-%s\n' "$m" "$*"; }`,
		`fail() { say "error $*"; exit 1; }`,
		// The session user (ssm-user by default) may not read the directory, a home
		// for one: sudo -n then, which never asks for a password.
		`r=; if { [ ! -r "$d" ] || [ ! -x "$d" ]; } && sudo -n true 2>/dev/null; then r="sudo -n"; fi`,
		`$r test -d "$d" || fail "no such directory, or no permission to see it"`,
		`$r test -r "$d" || fail "permission denied, and sudo -n is not allowed"`,
		`say begin`,
		`$r sh -c ` + ShellQuote(lister) + ` sh "$d" "$p" 2>/dev/null | { n=0; while IFS= read -r l; do n=$((n+1)); if [ "$n" -gt ` +
			strconv.Itoa(ListLimit) + ` ]; then say more; break; fi; printf '%s\n' "$l"; done; }`,
		`say end`,
	}, "; ")
}

// readList reads what the plugin prints: its own lines, then the lines of listScript.
func readList(r io.Reader, marker string) (names []string, more bool, err error) {
	br := bufio.NewReaderSize(r, 64*1024)
	// before keeps the last lines printed before the listing, which say why the
	// session failed when it never starts.
	var before []string
	begun := false
	for {
		line, err := br.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		rest, marked := strings.CutPrefix(line, marker)
		word, arg, _ := strings.Cut(rest, " ")
		switch {
		case marked && word == "error":
			return nil, false, fmt.Errorf("cannot list the directory on the instance: %s", arg)
		case marked && word == "begin":
			begun = true
		case marked && word == "more":
			more = true
		case marked && word == "end":
			return names, more, nil
		case marked:
		case begun:
			if listable(line) && len(names) < ListLimit {
				names = append(names, line)
			}
		default:
			if s := strings.TrimSpace(line); s != "" && !strings.HasPrefix(s, "Starting session with SessionId") {
				if len(s) > 200 {
					s = s[:200] + "..."
				}
				before = append(before[max(0, len(before)-4):], s)
			}
		}
		if err != nil {
			if begun {
				return nil, false, errors.New("the session ended before the end of the listing")
			}
			msg := "the session ended before the listing started"
			if len(before) > 0 {
				msg += ": " + strings.Join(before, " / ")
			}
			return nil, false, errors.New(msg)
		}
	}
}

// listable reports whether a line of ls is a name List returns: not empty, UTF-8,
// without a control character, and not "/" alone or a path, which no name is.
func listable(name string) bool {
	if name == "" || !utf8.ValidString(name) {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return false
		}
	}
	base := strings.TrimSuffix(name, "/")
	return base != "" && !strings.Contains(base, "/")
}
