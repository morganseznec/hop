package session

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// PullDocument is the Session Manager document Pull goes through. It runs one command
// without a terminal and streams its output, stdout and stderr together, as the SSM
// agent produces it: no size limit, unlike Run Command.
const PullDocument = "AWS-StartNonInteractiveCommand"

// pluginExitWait bounds the wait for the plugin once the file is received: it exits by
// itself when the agent closes the session.
var pluginExitWait = 10 * time.Second

// CheckPull refuses what Pull would refuse, without calling AWS: a remote path that is
// not absolute, or that names a directory.
func CheckPull(remote string) error {
	if !strings.HasPrefix(remote, "/") {
		// The command runs in a working directory of the SSM agent's choosing.
		return fmt.Errorf("remote path must be absolute (start with /): %s", remote)
	}
	if strings.HasSuffix(remote, "/") {
		return fmt.Errorf("remote path names a directory: %s", remote)
	}
	return nil
}

// Progress receives the number of bytes written so far, and the size of the remote
// file (-1 when unknown).
type Progress func(done, total int64)

// Pull downloads the file remote, an absolute path on the instance, to local, through
// a non-interactive session (PullDocument). The command reads the file as the session
// user, or with sudo -n when that user cannot, and sends it in base64 with its SHA-256.
// The file is written next to local, then renamed over it once the SHA-256 computed
// here matches: an interrupted transfer never leaves a partial file under that name.
// A new file is readable by its owner only; an existing one keeps its mode.
func Pull(ctx context.Context, cfg aws.Config, req Request, remote, local string, progress Progress) (int64, error) {
	if err := CheckPull(remote); err != nil {
		return 0, err
	}
	nonce, err := newNonce()
	if err != nil {
		return 0, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(local), ".hop-pull-*")
	if err != nil {
		return 0, err
	}
	defer func() {
		if tmp != nil {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()

	req.Document = PullDocument
	// The agent splits the command into words like a shell would, then runs it without
	// a shell: the script needs one of its own.
	req.Parameters = map[string][]string{"command": {"sh -c " + ShellQuote(pullScript(remote, nonce))}}
	s, err := open(ctx, cfg, req)
	if err != nil {
		return 0, err
	}
	// The plugin ends the session as soon as its stdin closes, before the output arrives:
	// stdin stays open until the plugin exits.
	inR, inW, err := os.Pipe()
	if err != nil {
		s.terminate(ctx)
		return 0, err
	}
	defer inW.Close()
	s.cmd.Stdin = inR
	stdout, err := s.cmd.StdoutPipe()
	if err != nil {
		inR.Close()
		s.terminate(ctx)
		return 0, err
	}
	var stderr tail
	s.cmd.Stderr = &stderr
	err = s.cmd.Start()
	inR.Close()
	if err != nil {
		s.terminate(ctx)
		return 0, err
	}

	// Ctrl-C cancels ctx: the plugin, which ignores it, is stopped here.
	stopped := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			s.cmd.Process.Kill()
		case <-stopped:
		}
	}()
	h := sha256.New()
	n, sum, recvErr := receive(stdout, "--hop"+nonce+"-", io.MultiWriter(tmp, h), progress)
	if recvErr != nil {
		s.cmd.Process.Kill()
	}
	exited := make(chan error, 1)
	go func() { exited <- s.cmd.Wait() }()
	select {
	case <-exited:
	case <-time.After(pluginExitWait):
		// The file is complete; the session may not be closed yet.
		s.cmd.Process.Kill()
		<-exited
		s.terminate(ctx)
	}
	close(stopped)

	switch {
	case ctx.Err() != nil:
		s.terminate(ctx)
		return n, ctx.Err()
	case recvErr != nil:
		s.terminate(ctx)
		if msg := stderr.String(); msg != "" {
			return n, fmt.Errorf("%w (session-manager-plugin: %s)", recvErr, msg)
		}
		return n, recvErr
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != sum {
		return n, fmt.Errorf("checksum mismatch after download: local %s, remote %s", got, sum)
	}
	if st, err := os.Stat(local); err == nil {
		if err := tmp.Chmod(st.Mode().Perm()); err != nil {
			return n, err
		}
	}
	if err := tmp.Close(); err != nil {
		return n, err
	}
	if err := os.Rename(tmp.Name(), local); err != nil {
		return n, err
	}
	tmp = nil
	return n, nil
}

// pullScript returns the shell script that sends path, between lines that start with
// the marker "--hop<nonce>-":
//
//	--hop<nonce>-size <bytes>
//	--hop<nonce>-begin
//	<base64>
//	--hop<nonce>-stderr <what cat wrote to stderr>   (only when it wrote something)
//	--hop<nonce>-end <exit code of cat> <SHA-256>
//
// or a single "--hop<nonce>-error <reason>" line when it cannot read the file. "-" is
// not a base64 character, so the end of the data is the first line that starts with
// one. The script is a single line: the agent passes the command on as is.
func pullScript(path, nonce string) string {
	return strings.Join([]string{
		// set -f: no globbing when the output of wc is split below.
		"set -f",
		"f=" + ShellQuote(path),
		// Built in two steps, so that the marker never appears in the command itself.
		"m=--hop; m=${m}" + nonce,
		`say() { printf '%s-%s\n' "$m" "$*"; }`,
		`fail() { say "error $*"; exit 1; }`,
		// The session user (ssm-user by default) may not read the file: sudo -n then,
		// which never asks for a password.
		`r=; if [ ! -r "$f" ] && sudo -n true 2>/dev/null; then r="sudo -n"; fi`,
		`$r test -e "$f" || fail "no such file, or no permission to see it"`,
		`$r test -d "$f" && fail "is a directory"`,
		`$r test -r "$f" || fail "permission denied, and sudo -n is not allowed"`,
		// sha256sum on Linux, sha256 on FreeBSD, shasum on older macOS.
		`if command -v sha256sum >/dev/null 2>&1; then h=sha256sum; elif command -v sha256 >/dev/null 2>&1; then h="sha256 -q"; elif command -v shasum >/dev/null 2>&1; then h="shasum -a 256"; else fail "no sha256sum, sha256 or shasum"; fi`,
		`for c in base64 mkfifo mktemp tee; do command -v $c >/dev/null 2>&1 || fail "no $c"; done`,
		// mktemp creates the directory 0700 under a random name: no other local user can
		// read or swap the pipe.
		`d=$(umask 077 && mktemp -d "${TMPDIR:-/tmp}/.hop-pull.XXXXXXXX") || fail "cannot create a temporary directory"`,
		`trap 'rm -rf "$d"' EXIT`,
		`mkfifo "$d/p" || fail "mkfifo failed"`,
		`set -- $($r wc -c "$f")`,
		`say "size $1"`,
		// The SHA-256 is computed on the bytes sent, through the pipe, rather than on the
		// file read again afterwards: a log written to meanwhile still matches.
		// "&" ends a command like ";" does, and cannot be followed by one.
		`$h < "$d/p" > "$d/sum" 2>/dev/null & say begin`,
		`{ $r cat "$f" 2>"$d/err"; echo $? > "$d/rc"; } | tee "$d/p" 2>/dev/null | base64 2>/dev/null`,
		`wait`,
		`echo`,
		`if [ -s "$d/err" ]; then say "stderr $(head -c 500 "$d/err" | tr '\n' ' ')"; fi`,
		`say "end $(cat "$d/rc") $(cut -d' ' -f1 < "$d/sum")"`,
	}, "; ")
}

// receive reads what the plugin prints: its own lines, then the lines of pullScript.
// It writes the decoded file to w and returns its size and the remote SHA-256. It
// stops at the end line: the plugin's closing lines are not read.
func receive(r io.Reader, marker string, w io.Writer, progress Progress) (int64, string, error) {
	br := bufio.NewReaderSize(r, 64*1024)
	total := int64(-1)
	// before keeps the last lines printed before the transfer, which say why the
	// session failed when it never starts.
	var before []string
	for {
		line, err := br.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		if rest, ok := strings.CutPrefix(line, marker); ok {
			word, arg, _ := strings.Cut(rest, " ")
			if word == "begin" {
				break
			}
			switch word {
			case "error":
				return 0, "", fmt.Errorf("cannot read the file on the instance: %s", arg)
			case "size":
				if n, err := strconv.ParseInt(arg, 10, 64); err == nil {
					total = n
				}
			}
		} else if s := strings.TrimSpace(line); s != "" && !strings.HasPrefix(s, "Starting session with SessionId") {
			if len(s) > 200 {
				s = s[:200] + "..."
			}
			before = append(before[max(0, len(before)-4):], s)
		}
		if err != nil {
			msg := "the session ended before the transfer started"
			if len(before) > 0 {
				msg += ": " + strings.Join(before, " / ")
			}
			return 0, "", errors.New(msg)
		}
	}

	cw := &counter{w: w, total: total, progress: progress}
	run := &base64Run{r: br}
	if _, err := io.Copy(cw, base64.NewDecoder(base64.StdEncoding, run)); err != nil {
		switch {
		case cw.err != nil:
			return cw.n, "", cw.err
		case run.ended:
			return cw.n, "", endedEarly(cw.n)
		}
		return cw.n, "", fmt.Errorf("transfer corrupted after %d bytes: %v", cw.n, err)
	}
	var stderr string
	for {
		line, err := br.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		if rest, ok := strings.CutPrefix(line, marker); ok {
			word, arg, _ := strings.Cut(rest, " ")
			switch word {
			case "stderr":
				stderr = strings.TrimSpace(arg)
			case "end":
				code, sum, _ := strings.Cut(arg, " ")
				if code != "0" {
					msg := fmt.Sprintf("reading the file failed on the instance (exit code %s)", code)
					if stderr != "" {
						msg += ": " + stderr
					}
					return cw.n, "", errors.New(msg)
				}
				return cw.n, strings.TrimSpace(sum), nil
			}
		}
		if err != nil {
			return cw.n, "", endedEarly(cw.n)
		}
	}
}

func endedEarly(n int64) error {
	return fmt.Errorf("the session ended after %d bytes, before the end of the file", n)
}

// base64Run reads the base64 text of the data, line breaks left out, and ends at the
// first other character, which it leaves in r. ended says that the stream ended first.
type base64Run struct {
	r     *bufio.Reader
	ended bool
}

func (b *base64Run) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		c, err := b.r.ReadByte()
		if err != nil {
			b.ended = true
			if n > 0 {
				return n, nil
			}
			return 0, io.EOF
		}
		switch {
		case c == '\n' || c == '\r':
		case 'A' <= c && c <= 'Z', 'a' <= c && c <= 'z', '0' <= c && c <= '9', c == '+', c == '/', c == '=':
			p[n] = c
			n++
		default:
			b.r.UnreadByte()
			if n > 0 {
				return n, nil
			}
			return 0, io.EOF
		}
	}
	return n, nil
}

// counter counts the bytes written and reports them to progress. err keeps the write
// error, to tell it from a decoding error.
type counter struct {
	w        io.Writer
	n, total int64
	progress Progress
	err      error
}

func (c *counter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	if err != nil {
		c.err = err
	}
	if c.progress != nil {
		c.progress(c.n, c.total)
	}
	return n, err
}

// tail keeps the last 2 KB written to it, on one line: what the plugin says on stderr.
type tail struct {
	mu  sync.Mutex
	buf []byte
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > 2048 {
		t.buf = t.buf[len(t.buf)-2048:]
	}
	return len(p), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.Join(strings.Fields(string(t.buf)), " ")
}

// newNonce returns 16 random hex characters, which make the markers of a transfer
// impossible to find in the file itself.
func newNonce() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
