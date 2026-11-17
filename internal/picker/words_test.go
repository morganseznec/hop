package picker

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// holds reports whether a text has every word of a query.
func holds(t text, query string) bool {
	f, _ := t.locate(words(query))
	return f != none
}

func TestWordsAreFoundWhole(t *testing.T) {
	const name = "c2b_2026-11-05.csv.gz"
	for query, want := range map[string]bool{
		"":           true,
		"2026-11-05": true,
		// Words, in any order, each one somewhere in the name.
		"2026 11 05": true,
		"05 c2b":     true,
		"  csv   gz": true,
		"C2B CSV":    true,
		// Without separators, or with others than those of the name.
		"20261105":   true,
		"2026_11_05": true,
		"2026/11/05": true,
		"c2b2026":    true,
		// One word missing is enough to leave the line out.
		"2026 11 06": false,
		"b2c 11 05":  false,
		// Letters are never skipped: a word is in the name whole.
		"cbcsv":  false,
		"c2b26":  false,
		"2612":   false,
		"202605": false,
		// Without separators, a word starts where a part of the name does: "611" is not
		// in 2026-11, nor "2611".
		"611":     false,
		"2611":    false,
		"61105":   false,
		"1105":    true,
		"1105csv": true,
		"05.csv":  true,
		"5.c":     true,
	} {
		if got := holds(newText(name), query); got != want {
			t.Errorf("%q in %s: %v, want %v", query, name, got, want)
		}
	}
}

func TestAWordOfSeparatorsIsSearchedAsItIs(t *testing.T) {
	if !holds(newText("app-server"), "-") {
		t.Error(`"-" is in app-server`)
	}
	// Without its separators the word is empty, and an empty word would be everywhere.
	if holds(newText("appserver"), "-") {
		t.Error(`"-" is not in appserver`)
	}
	if holds(newText("app-server"), "_.") {
		t.Error(`"_." is not in app-server`)
	}
}

func TestWordsInTheOrderTypedFitBest(t *testing.T) {
	for _, c := range []struct {
		line, query string
		want        fit
	}{
		{"c2b_2026-11-05.csv.gz", "2026 11 05", inOrder},
		{"c2b_2026-05-11.csv.gz", "2026 11 05", anyOrder},
		{"c2b_2026-05-11.csv.gz", "2026 11 06", none},
		{"c2b_2026-11-05.csv.gz", "05 c2b", anyOrder},
		{"c2b_2026-11-05.csv.gz", "c2b", inOrder},
		{"c2b_2026-11-05.csv.gz", "", inOrder},
		// A word typed twice needs to be there twice to be in order.
		{"c2b_2026-11-11.csv.gz", "11 11", inOrder},
		{"c2b_2026-11-05.csv.gz", "11 11", anyOrder},
		// The way that ends first leaves room for the next word: "20" is the start of
		// 2026, and "26" its end.
		{"c2b_2026-11-05.csv.gz", "20 26 11", inOrder},
		{"acme-p-backups  acme-prod  eu-west-1  prod", "prod backups", anyOrder},
		{"acme-p-backups  acme-prod  eu-west-1  prod", "backups prod", inOrder},
	} {
		if got, _ := newText(c.line).locate(words(c.query)); got != c.want {
			t.Errorf("%q in %s: %d, want %d", c.query, c.line, got, c.want)
		}
	}
}

func TestMarksCoverWhatWasFound(t *testing.T) {
	for _, c := range []struct{ line, query, want string }{
		{"c2b_2026-11-05.csv.gz", "2026 05", "....xxxx....xx......."},
		// Found without its separators: the separators in between are covered too.
		{"c2b_2026-11-05.csv.gz", "20261105", "....xxxxxxxxxx......."},
		{"c2b_2026-11-05.csv.gz", "C2B gz", "xxx................xx"},
		{"c2b_2026-11-05.csv.gz", "zip", "....................."},
		{"c2b_2026-11-05.csv.gz", "c2b zip", "....................."},
		// In the order typed, each word is marked after the one before it: the second
		// "05" is the day, and not the month again.
		{"c2b_2026-05-05.csv.gz", "05 05", ".........xx.xx......."},
		{"c2b_2026-05-11.csv.gz", "11 05", ".........xx.xx......."},
		// Upper case that grows in lower case elsewhere keeps its place here.
		{"İstanbul-logs", "logs", ".........xxxx"},
	} {
		var got strings.Builder
		for _, m := range newText(c.line).marks(words(c.query)) {
			if m {
				got.WriteByte('x')
			} else {
				got.WriteByte('.')
			}
		}
		if got.String() != c.want {
			t.Errorf("%q in %s:\n got %s\nwant %s", c.query, c.line, got.String(), c.want)
		}
	}
}

// FuzzWords checks what holds for any line and any query: nothing panics, there is
// one mark per rune of the line, every word found has its place in the line, words in
// order follow one another, and a line holds its own words.
func FuzzWords(f *testing.F) {
	f.Add("c2b_2026-11-05.csv.gz", "2026 11 05")
	f.Add("acme-p-backups  acme-prod  eu-west-1  prod", "PROD backups")
	f.Add("İstanbul", "i̇")
	f.Add("", "-")
	f.Add("a\x00b\xff", "\xff ?")
	f.Fuzz(func(t *testing.T, line, query string) {
		text, ws := newText(line), words(query)
		marks := text.marks(ws)
		if len(marks) != utf8.RuneCountInString(line) {
			t.Fatalf("%d marks for the %d runes of %q", len(marks), utf8.RuneCountInString(line), line)
		}
		how, spans := text.locate(ws)
		if (how == none) != (len(spans) != len(ws) || spans == nil && len(ws) > 0) {
			t.Fatalf("%q in %q: fit %d with %d spans for %d words", query, line, how, len(spans), len(ws))
		}
		end := 0
		for i, s := range spans {
			if s[0] < 0 || s[1] > len(text.runes) || s[0] >= s[1] {
				t.Fatalf("%q found from %d to %d in the %d runes of %q", string(ws[i].runes), s[0], s[1], len(text.runes), line)
			}
			if how == inOrder && s[0] < end {
				t.Fatalf("%q in %q: in order, and word %d starts at %d, before %d", query, line, i, s[0], end)
			}
			end = s[1]
		}
		if how == none {
			for i, m := range marks {
				if m {
					t.Fatalf("%q in %q: rune %d is marked and a word is missing", query, line, i)
				}
			}
		}
		// A line holds its own words, in their order.
		if own, _ := text.locate(words(line)); own != inOrder {
			t.Fatalf("%q does not hold its own words in order: %d", line, own)
		}
	})
}
