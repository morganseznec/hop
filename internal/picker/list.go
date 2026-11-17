package picker

import (
	"unicode"

	"github.com/gdamore/tcell/v2"
	"github.com/mattn/go-runewidth"
)

// list is the state of the picker: the lines, what is typed, the lines that fit it and
// where the cursor is. Nothing here draws.
type list struct {
	lines []string
	texts []text
	// start is the line the cursor is on when nothing is typed.
	start int

	input []rune
	// pos is where the next character typed goes in input.
	pos   int
	words []word

	// shown are the lines that hold every word typed, in the order of the list. The
	// first one is drawn nearest to the prompt.
	shown []int
	// cur is the line under the cursor and off the one on the lowest row, both as
	// indexes in shown.
	cur, off int
}

func newList(n int, line func(i int) string, o Options) *list {
	l := &list{lines: make([]string, n), texts: make([]text, n), start: min(max(o.Start, 0), max(n-1, 0))}
	for i := range l.lines {
		l.lines[i] = line(i)
		searched := l.lines[i]
		if o.Searched != nil {
			searched = firstRunes(searched, o.Searched(i))
		}
		l.texts[i] = newText(searched)
	}
	l.filter()
	return l
}

// firstRunes returns the first n runes of s, or all of s when it has fewer.
func firstRunes(s string, n int) string {
	for i := range s {
		if n <= 0 {
			return s[:i]
		}
		n--
	}
	return s
}

// filter keeps the lines that hold every word typed, those that hold them in the order
// typed first, and otherwise in the order of the list. It puts the cursor on the first
// of them, the one nearest to the prompt: that is where the eye is, on what is being
// typed. With nothing typed, the cursor returns to the line it started on.
func (l *list) filter() {
	l.words = words(string(l.input))
	l.shown = l.shown[:0]
	var rest []int
	for i, t := range l.texts {
		switch f, _ := t.locate(l.words); f {
		case inOrder:
			l.shown = append(l.shown, i)
		case anyOrder:
			rest = append(rest, i)
		}
	}
	l.shown = append(l.shown, rest...)
	l.cur, l.off = 0, 0
	if len(l.words) == 0 && len(l.shown) > 0 {
		l.cur = l.start
	}
}

// view scrolls the list so that the cursor is on one of the rows drawn, and leaves no
// row empty that a line could fill.
func (l *list) view(rows int) {
	rows = max(rows, 1)
	if l.cur < l.off {
		l.off = l.cur
	}
	if l.cur >= l.off+rows {
		l.off = l.cur - rows + 1
	}
	l.off = min(l.off, max(len(l.shown)-rows, 0))
}

// move takes the cursor d lines up the screen, or down with a negative d, and around:
// up from the last line leads to the first, down from the first to the last.
func (l *list) move(d int) {
	if n := len(l.shown); n > 0 {
		l.cur = ((l.cur+d)%n + n) % n
	}
}

// page takes the cursor a screen further and stops at the ends: a page is a way to go
// fast, and going around at that speed loses the place.
func (l *list) page(d int) {
	if n := len(l.shown); n > 0 {
		l.cur = min(max(l.cur+d, 0), n-1)
	}
}

// choice is the line under the cursor, as its index in the list given to Pick.
func (l *list) choice() (int, bool) {
	if l.cur >= len(l.shown) {
		return 0, false
	}
	return l.shown[l.cur], true
}

// edit replaces input[from:to] with what is typed there, and filters again.
func (l *list) edit(from, to int, typed ...rune) {
	in := append([]rune{}, l.input[:from]...)
	in = append(in, typed...)
	l.input = append(in, l.input[to:]...)
	l.pos = from + len(typed)
	l.filter()
}

type outcome int

const (
	stay outcome = iota
	chosen
	aborted
)

// key applies a key press. rows is the height of the list on screen, for the page
// keys, and room the width left for what is typed.
func (l *list) key(e *tcell.EventKey, rows, room int) outcome {
	switch e.Key() {
	case tcell.KeyEsc, tcell.KeyCtrlC, tcell.KeyCtrlD:
		return aborted
	case tcell.KeyEnter:
		if _, ok := l.choice(); ok {
			return chosen
		}
	case tcell.KeyUp, tcell.KeyCtrlK, tcell.KeyCtrlP:
		l.move(1)
	case tcell.KeyDown, tcell.KeyCtrlJ, tcell.KeyCtrlN:
		l.move(-1)
	case tcell.KeyPgUp:
		l.page(max(rows, 1))
	case tcell.KeyPgDn:
		l.page(-max(rows, 1))
	case tcell.KeyLeft, tcell.KeyCtrlB:
		l.pos = max(l.pos-1, 0)
	case tcell.KeyRight, tcell.KeyCtrlF:
		l.pos = min(l.pos+1, len(l.input))
	case tcell.KeyHome, tcell.KeyCtrlA:
		l.pos = 0
	case tcell.KeyEnd, tcell.KeyCtrlE:
		l.pos = len(l.input)
	case tcell.KeyBackspace, tcell.KeyBackspace2:
		if l.pos > 0 {
			l.edit(l.pos-1, l.pos)
		}
	case tcell.KeyDelete:
		if l.pos < len(l.input) {
			l.edit(l.pos, l.pos+1)
		}
	case tcell.KeyCtrlW:
		// The word before the cursor, and the spaces that follow it.
		from := l.pos
		for from > 0 && unicode.IsSpace(l.input[from-1]) {
			from--
		}
		for from > 0 && !unicode.IsSpace(l.input[from-1]) {
			from--
		}
		l.edit(from, l.pos)
	case tcell.KeyCtrlU:
		l.edit(0, l.pos)
	case tcell.KeyRune:
		// A letter typed with Ctrl or Alt is a command the picker does not have, not
		// a letter to search for.
		r := e.Rune()
		if e.Modifiers()&(tcell.ModCtrl|tcell.ModAlt|tcell.ModMeta) != 0 || !unicode.IsPrint(r) {
			break
		}
		if runewidth.StringWidth(string(l.input))+runewidth.RuneWidth(r) <= room {
			l.edit(l.pos, l.pos, r)
		}
	}
	return stay
}
