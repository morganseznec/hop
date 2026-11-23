package cli

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/morganseznec/hop/internal/session"
)

// What hop pull downloads: one or more paths on the instance, each of which may hold
// a pattern in its file name (*, ?, [...]), which the instance expands. The shell
// sends a path with braces, {17,18,19}, as several paths.

// splitPull tells the paths on the instance from the local path, among the paths
// given after the instance. One path is on the instance. With two, the second is the
// local file or directory, as it always was, except when it reads as the brace of
// the first: an absolute path in the same directory, which does not exist here. With
// three or more, the last is local only when it is a directory here (or ends with
// /): otherwise every path is on the instance.
func splitPull(paths []string) (remotes []string, local string) {
	n := len(paths)
	if n < 2 {
		return paths, ""
	}
	last := paths[n-1]
	if isDir(last) || strings.HasSuffix(last, "/") || strings.HasSuffix(last, string(filepath.Separator)) {
		return paths[:n-1], last
	}
	if hasPattern(last) {
		return paths, ""
	}
	if n == 2 {
		first := paths[0]
		if strings.HasPrefix(last, "/") && path.Dir(last) == path.Dir(first) && !isDir(filepath.Dir(last)) {
			return paths, ""
		}
		return paths[:1], last
	}
	return paths, ""
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// hasPattern reports whether a path holds a character the shell reads as a pattern.
func hasPattern(p string) bool { return strings.ContainsAny(p, "*?[") }

// checkRemotePattern refuses, without calling AWS, a path on the instance that
// Pull would refuse, or a pattern hop cannot expand: one outside the file name, or
// not well formed.
func checkRemotePattern(remote string) error {
	if err := session.CheckPull(remote); err != nil {
		return err
	}
	if !hasPattern(remote) {
		return nil
	}
	dir, base := path.Split(remote)
	if hasPattern(dir) {
		return fmt.Errorf("a pattern on the instance goes in the file name only, not in the directory: %s", remote)
	}
	if _, err := path.Match(base, ""); err != nil {
		return fmt.Errorf("invalid pattern: %s", remote)
	}
	return nil
}

// patternPrefix is what every name a pattern matches starts with: the part before
// its first special character. It narrows the listing on the instance.
func patternPrefix(base string) string {
	if i := strings.IndexAny(base, `*?[\`); i >= 0 {
		return base[:i]
	}
	return base
}

// lister lists the names of a directory of the instance that start with a prefix
// (see session.List). Tests replace it.
type lister func(ctx context.Context, dir, prefix string) (names []string, more bool, err error)

// expandPulls returns the files to download: each path as given, and in place of a
// pattern, the files of its directory it matches, in the order the instance sorts
// them. Directories are left out, and names that start with a dot unless the pattern
// does, as a shell does. A pattern that matches no file is an error, as a path that
// does not exist would be. A file named twice is downloaded once.
func expandPulls(ctx context.Context, remotes []string, list lister) ([]string, error) {
	var files []string
	seen := map[string]bool{}
	add := func(f string) {
		if !seen[f] {
			seen[f] = true
			files = append(files, f)
		}
	}
	for _, r := range remotes {
		if !hasPattern(r) {
			add(r)
			continue
		}
		dir, base := path.Split(r)
		names, more, err := list(ctx, dir, patternPrefix(base))
		if err != nil {
			return nil, err
		}
		if more {
			return nil, fmt.Errorf("more than %d names in %s start with %q: make the pattern %s narrower", session.ListLimit, dir, patternPrefix(base), base)
		}
		found := 0
		for _, name := range names {
			if strings.HasSuffix(name, "/") || (strings.HasPrefix(name, ".") && !strings.HasPrefix(base, ".")) {
				continue
			}
			if ok, _ := path.Match(base, name); ok {
				add(dir + name)
				found++
			}
		}
		if found == 0 {
			return nil, fmt.Errorf("no file on the instance matches %s", r)
		}
	}
	return files, nil
}

// pullTargets returns where each file lands, in the directory local (the current
// directory when empty), under its own name. Two files of the same name, from two
// directories, would land on each other: that is refused.
func pullTargets(files []string, local string) ([]string, error) {
	targets := make([]string, len(files))
	from := map[string]string{}
	for i, f := range files {
		t, err := pullTarget(f, local)
		if err != nil {
			return nil, err
		}
		if other, ok := from[t]; ok {
			return nil, fmt.Errorf("%s and %s would both land in %s: download them one at a time", other, f, t)
		}
		from[t] = f
		targets[i] = t
	}
	return targets, nil
}

// checkPullDir refuses, before anything is asked, a local path that cannot take
// several files: it must be a directory, or nothing (the current directory).
func checkPullDir(local string) error {
	if local == "" || isDir(local) {
		return nil
	}
	if strings.HasSuffix(local, "/") || strings.HasSuffix(local, string(filepath.Separator)) {
		return fmt.Errorf("directory not found: %s", local)
	}
	return fmt.Errorf("several files land in a directory, and %s is not one", local)
}

// pullMany downloads files one after the other, each through its own session, and
// goes on past a file that fails: it says which failed at the end.
func pullMany(ctx context.Context, cfg aws.Config, req session.Request, name string, files, targets []string) error {
	var total int64
	var failed []string
	for i, f := range files {
		info("%s:%s -> %s", name, f, targets[i])
		p := &progress{tty: stderrTTY}
		n, err := session.Pull(ctx, cfg, req, f, targets[i], p.update)
		p.clear()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			warn("%s: %s", f, err)
			failed = append(failed, f)
			continue
		}
		total += n
		info("%s received, SHA-256 matches on both sides", byteSize(n))
	}
	info("%d of %d files received, %s in all", len(files)-len(failed), len(files), byteSize(total))
	if len(failed) > 0 {
		return fmt.Errorf("%d of %d files failed: %s", len(failed), len(files), strings.Join(failed, ", "))
	}
	return nil
}
