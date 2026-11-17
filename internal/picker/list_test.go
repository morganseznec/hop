package picker

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
)

// backups is a listing as hop s3 shows one: the way back, then objects, each with its
// size, its date and its storage class after its name.
var backups = []struct{ name, rest string }{
	{"..", "back to the prefix above"},
	{"b2c_2025-06-24.csv.gz", "    611 B  2025-06-25 04:00  STANDARD"},
	{"b2c_2026-05-11.csv.gz", "   1.0 KB  2026-05-12 04:00  STANDARD"},
	{"b2c_2026-11-05.csv.gz", "    469 B  2026-11-06 03:00  STANDARD"},
	{"c2b_2026-05-12.csv.gz", "  10.9 KB  2026-05-13 04:00  STANDARD"},
	{"c2b_2026-10-14.csv.gz", " 102.7 KB  2026-10-15 04:00  STANDARD"},
	{"c2b_2026-11-04.csv.gz", "  50.3 KB  2026-11-05 03:00  STANDARD"},
	{"c2b_2026-11-05.csv.gz", "  44.7 KB  2026-11-06 03:00  STANDARD"},
}

func backupLine(i int) string { return backups[i].name + "  " + backups[i].rest }

// names is the part of the lines that is searched: the name each one starts with.
var names = Options{Prompt: "hop> ", Start: 1, Searched: func(i int) int { return len(backups[i].name) }}

func key(k tcell.Key) *tcell.EventKey { return tcell.NewEventKey(k, 0, tcell.ModNone) }

// typed are the key presses that write s.
func typed(s string) []*tcell.EventKey {
	var keys []*tcell.EventKey
	for _, r := range s {
		keys = append(keys, tcell.NewEventKey(tcell.KeyRune, r, tcell.ModNone))
	}
	return keys
}

// press applies key presses to a list shown on rows rows, as run does.
func press(l *list, rows int, keys ...*tcell.EventKey) outcome {
	for _, k := range keys {
		if out := l.key(k, rows, 60); out != stay {
			return out
		}
		l.view(rows)
	}
	return stay
}

// found are the names of the lines the list shows, from the one nearest to the prompt.
func found(l *list) string {
	var out []string
	for _, i := range l.shown {
		out = append(out, strings.Fields(l.lines[i])[0])
	}
	return strings.Join(out, " ")
}

func TestTypingWordsNarrowsTheList(t *testing.T) {
	l := newList(len(backups), backupLine, names)
	if len(l.shown) != len(backups) || l.cur != 1 {
		t.Fatalf("opened on %d lines, cursor on %d", len(l.shown), l.cur)
	}
	// A date typed with spaces finds what it finds with dashes.
	press(l, 20, typed("2026 11 05")...)
	// The two objects of that day first, then the one of 11 May, which holds the three
	// words in another order.
	if got := found(l); got != "b2c_2026-11-05.csv.gz c2b_2026-11-05.csv.gz b2c_2026-05-11.csv.gz" {
		t.Errorf(`"2026 11 05" shows %s`, got)
	}
	for _, exact := range []string{"2026-11-05", "20261105", "2026_11_05 ", "2026-11 05"} {
		other := newList(len(backups), backupLine, names)
		press(other, 20, typed(exact)...)
		if got := found(other); got != "b2c_2026-11-05.csv.gz c2b_2026-11-05.csv.gz" {
			t.Errorf("%q shows %s", exact, got)
		}
	}
	// One more word, one line left.
	press(l, 20, typed(" c2b")...)
	if got := found(l); got != "c2b_2026-11-05.csv.gz" {
		t.Errorf(`with "c2b": %s`, got)
	}
	if i, ok := l.choice(); !ok || i != 7 {
		t.Errorf("choice %d, %v", i, ok)
	}
}

func TestOnlyTheNameIsSearched(t *testing.T) {
	l := newList(len(backups), backupLine, names)
	// "611" is the size of the first object, "04:00" an hour, "standard" a class, and
	// "back" what the first line says of "..": none of them is a name.
	for _, q := range []string{"611", "04:00", "standard", "back", "2025-06-25"} {
		l.edit(0, len(l.input), []rune(q)...)
		if len(l.shown) != 0 {
			t.Errorf("%q found %s", q, found(l))
		}
	}
	// 2026-11-05 is the date c2b_2026-11-04 was written on: only the two objects named
	// after that day are found, and not the 31 lines that hold those digits somewhere.
	l.edit(0, len(l.input), []rune("2026-11-05")...)
	if got := found(l); got != "b2c_2026-11-05.csv.gz c2b_2026-11-05.csv.gz" {
		t.Errorf("by date: %s", got)
	}
	// Without Searched, the whole line is: a bucket is found by its profile too.
	whole := newList(len(backups), backupLine, Options{})
	whole.edit(0, 0, []rune("611 standard")...)
	if got := found(whole); got != "b2c_2025-06-24.csv.gz" {
		t.Errorf("whole line: %s", got)
	}
}

func TestTheCursorGoesToTheFirstLineFound(t *testing.T) {
	l := newList(len(backups), backupLine, names)
	// Up the list first, as when looking around before typing.
	press(l, 20, key(tcell.KeyUp), key(tcell.KeyUp), key(tcell.KeyUp))
	if l.cur != 4 {
		t.Fatalf("cursor on %d", l.cur)
	}
	press(l, 20, typed("11")...)
	if l.cur != 0 || l.off != 0 {
		t.Errorf("after typing, cursor on %d, list from %d", l.cur, l.off)
	}
	if i, _ := l.choice(); backups[i].name != "b2c_2026-05-11.csv.gz" {
		t.Errorf("under the cursor: %s", backups[i].name)
	}
	// Every change of what is typed puts it back there.
	press(l, 20, key(tcell.KeyUp))
	press(l, 20, typed("-05")...)
	if i, _ := l.choice(); l.cur != 0 || backups[i].name != "b2c_2026-11-05.csv.gz" {
		t.Errorf("after one more word: cursor on %d, %s", l.cur, backups[i].name)
	}
	// Moving in what is typed changes nothing to the list.
	press(l, 20, key(tcell.KeyUp), key(tcell.KeyLeft), key(tcell.KeyHome), key(tcell.KeyEnd))
	if l.cur != 1 {
		t.Errorf("the cursor moved with the arrows of the input: %d", l.cur)
	}
	// Everything erased: back where the picker opened.
	press(l, 20, key(tcell.KeyCtrlU))
	if len(l.shown) != len(backups) || l.cur != 1 {
		t.Errorf("erased: %d lines, cursor on %d", len(l.shown), l.cur)
	}
	// Nothing found: nothing to choose, and Enter does nothing.
	if out := press(l, 20, append(typed("zip"), key(tcell.KeyEnter))...); out != stay || len(l.shown) != 0 {
		t.Errorf("Enter on an empty list: %v, %d lines", out, len(l.shown))
	}
	if _, ok := l.choice(); ok {
		t.Error("a choice in an empty list")
	}
}

func TestTheArrowsGoAround(t *testing.T) {
	l := newList(len(backups), backupLine, Options{})
	last := len(backups) - 1
	// Down from the first line, the one nearest to the prompt: the last one.
	press(l, 20, key(tcell.KeyDown))
	if l.cur != last {
		t.Errorf("down from the first line: %d", l.cur)
	}
	// And up from the last: the first.
	press(l, 20, key(tcell.KeyUp))
	if l.cur != 0 {
		t.Errorf("up from the last line: %d", l.cur)
	}
	// Ctrl-P and Ctrl-K go up, Ctrl-N and Ctrl-J down, around as well.
	press(l, 20, key(tcell.KeyCtrlN), key(tcell.KeyCtrlJ))
	if l.cur != last-1 {
		t.Errorf("Ctrl-N, Ctrl-J: %d", l.cur)
	}
	press(l, 20, key(tcell.KeyCtrlP), key(tcell.KeyCtrlK), key(tcell.KeyCtrlP))
	if l.cur != 1 {
		t.Errorf("Ctrl-P, Ctrl-K, Ctrl-P: %d", l.cur)
	}
	// Around a list narrowed by what is typed, too.
	press(l, 20, typed("c2b")...)
	press(l, 20, key(tcell.KeyDown))
	if i, _ := l.choice(); l.cur != 3 || backups[i].name != "c2b_2026-11-05.csv.gz" {
		t.Errorf("down from the first of 4: %d, %s", l.cur, backups[i].name)
	}
	// One line: the arrows stay on it.
	press(l, 20, typed(" 10-14")...)
	press(l, 20, key(tcell.KeyUp), key(tcell.KeyDown), key(tcell.KeyDown))
	if l.cur != 0 || len(l.shown) != 1 {
		t.Errorf("one line: cursor on %d of %d", l.cur, len(l.shown))
	}
	// No line: nothing to move, and nothing breaks.
	press(l, 20, typed(" zip")...)
	press(l, 20, key(tcell.KeyUp), key(tcell.KeyDown), key(tcell.KeyPgUp), key(tcell.KeyPgDn))
	if l.cur != 0 || len(l.shown) != 0 {
		t.Errorf("no line: cursor on %d of %d", l.cur, len(l.shown))
	}
}

func TestTheListFollowsTheCursor(t *testing.T) {
	// Three rows for eight lines.
	const rows = 3
	l := newList(len(backups), backupLine, Options{})
	l.view(rows)
	press(l, rows, key(tcell.KeyUp), key(tcell.KeyUp), key(tcell.KeyUp))
	if l.cur != 3 || l.off != 1 {
		t.Errorf("up three times: cursor on %d, list from %d", l.cur, l.off)
	}
	// Around from the first line: the last one, on the top row.
	press(l, rows, key(tcell.KeyDown), key(tcell.KeyDown), key(tcell.KeyDown), key(tcell.KeyDown))
	if l.cur != 7 || l.off != 5 {
		t.Errorf("around to the last line: cursor on %d, list from %d", l.cur, l.off)
	}
	// And around the other way: the first one, on the lowest row.
	press(l, rows, key(tcell.KeyUp))
	if l.cur != 0 || l.off != 0 {
		t.Errorf("around to the first line: cursor on %d, list from %d", l.cur, l.off)
	}
	// A page is as many lines as the screen shows, and stops at the ends.
	press(l, rows, key(tcell.KeyPgUp))
	if l.cur != 3 {
		t.Errorf("one page up: %d", l.cur)
	}
	press(l, rows, key(tcell.KeyPgUp), key(tcell.KeyPgUp), key(tcell.KeyPgUp))
	if l.cur != 7 || l.off != 5 {
		t.Errorf("pages up stop on the last line: cursor on %d, list from %d", l.cur, l.off)
	}
	press(l, rows, key(tcell.KeyPgDn), key(tcell.KeyPgDn), key(tcell.KeyPgDn), key(tcell.KeyPgDn))
	if l.cur != 0 || l.off != 0 {
		t.Errorf("pages down stop on the first line: cursor on %d, list from %d", l.cur, l.off)
	}
	// A taller screen leaves no row empty that a line could fill.
	press(l, rows, key(tcell.KeyDown))
	l.view(6)
	if l.cur != 7 || l.off != 2 {
		t.Errorf("taller screen: cursor on %d, list from %d", l.cur, l.off)
	}
}

func TestEditingWhatIsTyped(t *testing.T) {
	l := newList(len(backups), backupLine, names)
	in := func() string { return string(l.input) }

	press(l, 20, typed("c2b 11")...)
	// In the middle of what is typed.
	press(l, 20, key(tcell.KeyLeft), key(tcell.KeyLeft), key(tcell.KeyLeft))
	press(l, 20, typed(" 2026")...)
	if in() != "c2b 2026 11" || l.pos != 8 {
		t.Errorf("typed in the middle: %q, at %d", in(), l.pos)
	}
	press(l, 20, key(tcell.KeyBackspace), key(tcell.KeyBackspace2))
	if in() != "c2b 20 11" {
		t.Errorf("Backspace: %q", in())
	}
	press(l, 20, key(tcell.KeyDelete), key(tcell.KeyDelete))
	if in() != "c2b 201" || l.pos != 6 {
		t.Errorf("Delete: %q, at %d", in(), l.pos)
	}
	// Past the ends, nothing.
	press(l, 20, key(tcell.KeyEnd), key(tcell.KeyDelete), key(tcell.KeyRight))
	press(l, 20, key(tcell.KeyHome), key(tcell.KeyBackspace), key(tcell.KeyLeft), key(tcell.KeyCtrlW))
	if in() != "c2b 201" || l.pos != 0 {
		t.Errorf("past the ends: %q, at %d", in(), l.pos)
	}
	// Ctrl-W takes the word before the cursor, and the spaces after it.
	press(l, 20, key(tcell.KeyCtrlE))
	press(l, 20, typed("  ")...)
	press(l, 20, key(tcell.KeyCtrlW))
	if in() != "c2b " {
		t.Errorf("Ctrl-W: %q", in())
	}
	// Ctrl-U takes what is before the cursor.
	press(l, 20, typed("05")...)
	press(l, 20, key(tcell.KeyCtrlB), key(tcell.KeyCtrlB), key(tcell.KeyCtrlU))
	if in() != "05" || l.pos != 0 {
		t.Errorf("Ctrl-U: %q, at %d", in(), l.pos)
	}
	press(l, 20, key(tcell.KeyCtrlF), key(tcell.KeyCtrlA), key(tcell.KeyCtrlE), key(tcell.KeyCtrlU))
	if in() != "" || len(l.shown) != len(backups) {
		t.Errorf("all erased: %q, %d lines", in(), len(l.shown))
	}

	// A letter with Ctrl or Alt is not a letter to search for, nor is a control
	// character.
	press(l, 20, tcell.NewEventKey(tcell.KeyRune, 'x', tcell.ModAlt), key(tcell.KeyCtrlT), key(tcell.KeyTab),
		tcell.NewEventKey(tcell.KeyRune, '\u0085', tcell.ModNone))
	if in() != "" {
		t.Errorf("typed by a command key: %q", in())
	}
	// What is typed stops where the row does.
	for _, k := range typed(strings.Repeat("ab", 40)) {
		l.key(k, 20, 10)
	}
	if in() != "ababababab" {
		t.Errorf("past the width of the row: %q", in())
	}
}

func TestLeavingAndChoosing(t *testing.T) {
	for _, k := range []tcell.Key{tcell.KeyEsc, tcell.KeyCtrlC, tcell.KeyCtrlD} {
		l := newList(len(backups), backupLine, names)
		if out := press(l, 20, key(k)); out != aborted {
			t.Errorf("%s: %v", tcell.KeyNames[k], out)
		}
	}
	l := newList(len(backups), backupLine, names)
	if out := press(l, 20, key(tcell.KeyEnter)); out != chosen {
		t.Errorf("Enter: %v", out)
	}
	// The cursor starts where it was asked to, within the list.
	for start, want := range map[int]int{-3: 0, 0: 0, 5: 5, 99: 7} {
		l := newList(len(backups), backupLine, Options{Start: start})
		if i, _ := l.choice(); i != want {
			t.Errorf("start %d: cursor on %d", start, i)
		}
	}
	// An empty list opens, and has nothing to choose.
	empty := newList(0, backupLine, Options{Start: 2})
	if out := press(empty, 20, key(tcell.KeyUp), key(tcell.KeyEnter)); out != stay {
		t.Errorf("Enter in an empty list: %v", out)
	}
}
