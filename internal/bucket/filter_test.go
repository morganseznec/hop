package bucket

import (
	"strings"
	"testing"
)

func TestSplitPattern(t *testing.T) {
	for key, want := range map[string][2]string{
		"2026/11/app.log":       {"2026/11/app.log", ""},
		"2026/11/":              {"2026/11/", ""},
		"":                      {"", ""},
		"2026/11/app-*.log":     {"2026/11/", "app-*.log"},
		"2026/11/*":             {"2026/11/", "*"},
		"*.log":                 {"", "*.log"},
		"2026/*/app.log":        {"2026/", "*/app.log"},
		"logs/app-[0-9]?.gz":    {"logs/", "app-[0-9]?.gz"},
		"a/b?c/d*":              {"a/", "b?c/d*"},
		"2026/11/app.log/extra": {"2026/11/app.log/extra", ""},
	} {
		prefix, pattern := SplitPattern(key)
		if prefix != want[0] || pattern != want[1] {
			t.Errorf("SplitPattern(%q) = %q, %q; want %q, %q", key, prefix, pattern, want[0], want[1])
		}
	}
}

func TestFilterMatch(t *testing.T) {
	cases := []struct {
		pattern string
		words   []string
		name    string
		want    bool
	}{
		// No filter keeps everything.
		{"", nil, "app.log", true},
		// A plain word is part of the name, in any case.
		{"", []string{"error"}, "2026/11/Error-report.log", true},
		{"", []string{"ERROR"}, "2026/11/error-report.log", true},
		{"", []string{"warn"}, "2026/11/error-report.log", false},
		// Every word must be there, in any order.
		{"", []string{"log", "2026"}, "2026/11/app.log", true},
		{"", []string{"log", "2025"}, "2026/11/app.log", false},
		// A word with a wildcard fits the whole name, or its last part.
		{"", []string{"*.log"}, "app.log", true},
		{"", []string{"*.log"}, "2026/11/app.log", true},
		{"", []string{"*.LOG"}, "2026/11/app.log", true},
		{"", []string{"*.log"}, "2026/11/app.log.gz", false},
		{"", []string{"app*"}, "2026/11/app.log", true},
		{"", []string{"2026/*/app.log"}, "2026/11/app.log", true},
		// * and ? stop at "/", as in a shell.
		{"", []string{"2026*app.log"}, "2026/11/app.log", false},
		{"", []string{"app-?.log"}, "app-1.log", true},
		{"", []string{"app-?.log"}, "app-12.log", false},
		{"", []string{"app-[0-9][0-9].log"}, "app-12.log", true},
		// A group of keys is named without its final "/".
		{"", []string{"20*"}, "2026/", true},
		{"", []string{"*6"}, "2026/", true},
		{"", []string{"2026"}, "2026/", true},
		// The pattern of the place is part of a key: its case counts.
		{"*.log", nil, "app.log", true},
		{"*.LOG", nil, "app.log", false},
		{"*/app.log", nil, "11/app.log", true},
		// Neither the whole name (two levels for one *), nor its last part (app.log
		// has no "/").
		{"*/app.log", nil, "11/12/app.log", false},
		// Pattern and words together.
		{"*.log", []string{"app"}, "app.log", true},
		{"*.log", []string{"db"}, "app.log", false},
	}
	for _, c := range cases {
		f, err := NewFilter(c.pattern, c.words)
		if err != nil {
			t.Fatalf("NewFilter(%q, %q): %v", c.pattern, c.words, err)
		}
		if got := f.Match(c.name); got != c.want {
			t.Errorf("Filter(%q, %q).Match(%q) = %v, want %v", c.pattern, c.words, c.name, got, c.want)
		}
	}
}

func TestNewFilter(t *testing.T) {
	if f, err := NewFilter("", nil); err != nil || !f.Empty() || f.String() != "" {
		t.Errorf("empty filter: %+v, %v", f, err)
	}
	f, err := NewFilter("*.log", []string{"Error", "2026"})
	if err != nil || f.Empty() || f.String() != "*.log error 2026" {
		t.Errorf("filter: %+v, %q, %v", f, f.String(), err)
	}
	// A bracket never closed is not a pattern: say so rather than match nothing.
	for _, bad := range [][2]string{{"app-[0-9", ""}, {"", "[abc"}} {
		words := []string{}
		if bad[1] != "" {
			words = append(words, bad[1])
		}
		if _, err := NewFilter(bad[0], words); err == nil || !strings.Contains(err.Error(), "invalid pattern") {
			t.Errorf("NewFilter(%q, %q): %v", bad[0], words, err)
		}
	}
	if !HasMeta("a*") || !HasMeta("a?") || !HasMeta("[a]") || HasMeta("app.log") || HasMeta("") {
		t.Error("HasMeta")
	}
}

func FuzzFilter(f *testing.F) {
	for _, s := range [][3]string{{"2026/11/app-*.log", "error", "2026/11/app-1.log"}, {"*", "", ""}, {"[", "[a", "x"}, {"a/b?c/d*", "*.GZ", "a/bxc/d.gz"}, {"", "é", "É/"}} {
		f.Add(s[0], s[1], s[2])
	}
	f.Fuzz(func(t *testing.T, key, word, name string) {
		prefix, pattern := SplitPattern(key)
		// The key is cut, not changed, and the prefix is what S3 can list as it is.
		if prefix+pattern != key || HasMeta(prefix) || (prefix != "" && !strings.HasSuffix(prefix, "/") && pattern != "") {
			t.Errorf("SplitPattern(%q) = %q, %q", key, prefix, pattern)
		}
		if pattern != "" && !HasMeta(pattern) {
			t.Errorf("SplitPattern(%q): a pattern without a wildcard: %q", key, pattern)
		}
		filter, err := NewFilter(pattern, []string{word})
		if err != nil {
			return
		}
		filter.Match(name) // must not panic, whatever the name
		// A plain word found in the name, with no pattern, always matches.
		if plain, err := NewFilter("", []string{word}); err == nil && !HasMeta(word) {
			if got, want := plain.Match(name), strings.Contains(strings.ToLower(name), strings.ToLower(word)); got != want {
				t.Errorf("Match(%q) with the word %q = %v, want %v", name, word, got, want)
			}
		}
	})
}
