package picker

import (
	"strings"
	"unicode"
)

// text is the part of a line that what is typed is compared to.
type text struct {
	// runes is that part in lower case, rune for rune: runes[i] is the i-th rune of
	// the line.
	runes []rune
	// bare is runes without what is neither a letter nor a digit, and at tells where
	// each of its runes stands in runes: "2026-11-05" also reads "20261105".
	bare []rune
	at   []int
}

func newText(s string) text {
	t := text{runes: lower(s)}
	for i, r := range t.runes {
		if isAlnum(r) {
			t.bare = append(t.bare, r)
			t.at = append(t.at, i)
		}
	}
	return t
}

// lower puts s in lower case one rune at a time, so that the result has as many runes
// as s: strings.ToLower may change that count, and the positions to highlight with it.
func lower(s string) []rune {
	return []rune(strings.Map(unicode.ToLower, s))
}

// word is one of the words typed.
type word struct {
	runes []rune
	// bare is the word without its separators. It is empty when the word holds nothing
	// else: "-" is then searched as it is, and only so.
	bare []rune
}

// words cuts what is typed at its spaces. Every word must be found for a line to stay,
// in any order: "2026 11 05" finds c2b_2026-11-05.csv.gz, and so does "05 c2b".
// The lines that hold them in the order typed come first (see locate).
func words(input string) []word {
	var ws []word
	for _, f := range strings.Fields(input) {
		w := word{runes: lower(f)}
		for _, r := range w.runes {
			if isAlnum(r) {
				w.bare = append(w.bare, r)
			}
		}
		ws = append(ws, w)
	}
	return ws
}

// find looks for a word in a text, from the rune start on, and returns the runes it
// covers there, from and to (to is not one of them).
//
// The word is searched as typed, anywhere. It is also searched without separators on
// either side, so that "20261105" and "2026_11_05" find 2026-11-05, but then only from
// the start of a part of the text: read anywhere, "611" would be found in 2026-11.
// Letters are never skipped: a word is found whole or not at all. Of two ways to find
// it, the one that ends first is kept, which leaves the most room for the next word.
func (t text) find(w word, start int) (from, to int, ok bool) {
	if i := index(t.runes[start:], w.runes); i >= 0 {
		from, to, ok = start+i, start+i+len(w.runes), true
	}
	if len(w.bare) == 0 {
		return from, to, ok
	}
	// The first rune of bare that stands at start or after it.
	k := 0
	for k < len(t.at) && t.at[k] < start {
		k++
	}
	for k < len(t.bare) {
		i := index(t.bare[k:], w.bare)
		if i < 0 {
			break
		}
		k += i
		if first := t.at[k]; first == 0 || !isAlnum(t.runes[first-1]) {
			if end := t.at[k+len(w.bare)-1] + 1; !ok || end < to {
				from, to, ok = first, end, true
			}
			break
		}
		k++
	}
	return from, to, ok
}

func isAlnum(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

// Fit says how well a text answers the words typed.
type fit int

const (
	// none: a word is missing.
	none fit = iota
	// anyOrder: every word is there.
	anyOrder
	// inOrder: every word is there, one after the other as typed. Those lines come
	// first: of 2026-11-05 and 2026-05-11, "2026 11 05" means the first.
	inOrder
)

// locate finds the words in the text: how well it fits, and the runes each word
// covers, as pairs from, to. It looks for the words one after the other first, then
// for each one anywhere.
func (t text) locate(ws []word) (fit, [][2]int) {
	spans := make([][2]int, 0, len(ws))
	next := 0
	for _, w := range ws {
		from, to, ok := t.find(w, next)
		if !ok {
			break
		}
		spans = append(spans, [2]int{from, to})
		next = to
	}
	if len(spans) == len(ws) {
		return inOrder, spans
	}
	spans = spans[:0]
	for _, w := range ws {
		from, to, ok := t.find(w, 0)
		if !ok {
			return none, nil
		}
		spans = append(spans, [2]int{from, to})
	}
	return anyOrder, spans
}

// marks tells which runes of the text the words cover, to highlight them.
func (t text) marks(ws []word) []bool {
	m := make([]bool, len(t.runes))
	_, spans := t.locate(ws)
	for _, s := range spans {
		for i := s[0]; i < s[1]; i++ {
			m[i] = true
		}
	}
	return m
}

// index is strings.Index over runes: where needle starts in hay, or -1.
func index(hay, needle []rune) int {
	if len(needle) == 0 {
		return 0
	}
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i] != needle[0] {
			continue
		}
		j := 1
		for j < len(needle) && hay[i+j] == needle[j] {
			j++
		}
		if j == len(needle) {
			return i
		}
	}
	return -1
}
