package bucket

import (
	"fmt"
	"path"
	"strings"
)

// HasMeta reports whether s holds a wildcard: *, ? or [.
func HasMeta(s string) bool { return strings.ContainsAny(s, "*?[") }

// SplitPattern cuts the key of a place where its first wildcard stands: what comes
// before, up to the last "/", is a prefix S3 can list; the rest is a pattern for what
// that listing returns. "2026/11/app-*.log" gives "2026/11/" and "app-*.log". A key
// without a wildcard is all prefix.
func SplitPattern(key string) (prefix, pattern string) {
	i := strings.IndexAny(key, "*?[")
	if i < 0 {
		return key, ""
	}
	cut := strings.LastIndex(key[:i], "/") + 1
	return key[:cut], key[cut:]
}

// Filter narrows a listing to the names that answer a pattern and some words. The zero
// Filter keeps everything.
type Filter struct {
	// Pattern is the wildcard part of the place listed (see SplitPattern). It is part
	// of a key: it is matched as written, in the case it was typed.
	Pattern string
	// Words are what was typed after the place, to search with. A plain word must be
	// part of the name, in any case. A word with a wildcard is a pattern too, compared
	// in any case as well.
	Words []string
}

// NewFilter builds a Filter, and refuses a pattern that cannot be one: an opening
// bracket never closed.
func NewFilter(pattern string, words []string) (Filter, error) {
	for _, p := range append([]string{pattern}, words...) {
		if _, err := path.Match(p, ""); err != nil {
			return Filter{}, fmt.Errorf("invalid pattern %q: a [ needs its ]", p)
		}
	}
	f := Filter{Pattern: pattern}
	for _, w := range words {
		f.Words = append(f.Words, strings.ToLower(w))
	}
	return f, nil
}

// Empty reports whether the filter keeps everything.
func (f Filter) Empty() bool { return f.Pattern == "" && len(f.Words) == 0 }

// String writes the filter as it was typed, for messages.
func (f Filter) String() string {
	return strings.Join(append(nonEmpty(f.Pattern), f.Words...), " ")
}

func nonEmpty(s string) []string {
	if s == "" {
		return nil
	}
	return []string{s}
}

// Match reports whether a name passes the filter. name is a key relative to the prefix
// listed, with its final "/" when it stands for a group of keys.
func (f Filter) Match(name string) bool {
	if f.Pattern != "" && !glob(f.Pattern, name) {
		return false
	}
	lower := strings.ToLower(name)
	for _, w := range f.Words {
		if HasMeta(w) {
			if !glob(w, lower) {
				return false
			}
		} else if !strings.Contains(lower, w) {
			return false
		}
	}
	return true
}

// glob reports whether name fits pattern, as a whole or by its last part, what follows
// its last "/": "*.log" finds 2026/11/app.log. As in a shell, * and ? stop at "/". The
// final "/" of a group of keys is not part of its name.
func glob(pattern, name string) bool {
	name = strings.TrimSuffix(name, "/")
	if ok, _ := path.Match(pattern, name); ok {
		return true
	}
	ok, _ := path.Match(pattern, path.Base(name))
	return ok
}
