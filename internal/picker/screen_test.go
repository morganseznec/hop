package picker

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
)

// played is a simulated terminal that reads keys as a person would type them, once
// the picker has opened it.
type played struct {
	tcell.SimulationScreen
	width, height int
	keys          []*tcell.EventKey
}

func (p *played) Init() error {
	if err := p.SimulationScreen.Init(); err != nil {
		return err
	}
	p.SetSize(p.width, p.height)
	go func() {
		for _, k := range p.keys {
			// The screen holds a few events at a time.
			for p.PostEvent(k) != nil {
				time.Sleep(time.Millisecond)
			}
		}
	}()
	return nil
}

// Fini keeps what the picker drew last, for the test to read.
func (p *played) Fini() {}

// play opens the picker on a simulated terminal of that size and presses the keys.
// It returns what Pick returned, and the screen as it was before the last key.
func play(t *testing.T, width, height int, n int, line func(int) string, o Options, keys ...*tcell.EventKey) (int, *played, error) {
	t.Helper()
	p := &played{SimulationScreen: tcell.NewSimulationScreen("UTF-8"), width: width, height: height, keys: keys}
	saved := newScreen
	newScreen = func() (tcell.Screen, error) { return p, nil }
	t.Cleanup(func() { newScreen = saved; p.SimulationScreen.Fini() })
	i, err := Pick(n, line, o)
	return i, p, err
}

// rows is the text of the screen, row by row, without the spaces that end a row.
func (p *played) rows() []string {
	cells, width, height := p.GetContents()
	out := make([]string, height)
	for y := range out {
		var b strings.Builder
		for x := 0; x < width; x++ {
			if r := cells[y*width+x].Runes; len(r) > 0 {
				b.WriteString(string(r))
			} else {
				b.WriteByte(' ')
			}
		}
		out[y] = strings.TrimRight(b.String(), " ")
	}
	return out
}

// styles gives, for one row, a letter per column: the style it is drawn in.
func (p *played) styles(y int) string {
	cells, width, _ := p.GetContents()
	var b strings.Builder
	for x := 0; x < width; x++ {
		switch cells[y*width+x].Style {
		case styleFound:
			b.WriteByte('f')
		case styleCursor:
			b.WriteByte('c')
		case styleCursorFound:
			b.WriteByte('F')
		case stylePointer:
			b.WriteByte('>')
		default:
			b.WriteByte('.')
		}
	}
	return strings.TrimRight(b.String(), ".")
}

func TestPickDrawsTheListAboveThePrompt(t *testing.T) {
	o := names
	o.Header = "s3://acme-backups/telcos/ · 7 objects · Enter opens, Esc quits"
	keys := append(typed("2026 11 05"), key(tcell.KeyEnter))
	i, screen, err := play(t, 80, 10, len(backups), backupLine, o, keys...)
	if err != nil || backups[i].name != "b2c_2026-11-05.csv.gz" {
		t.Fatalf("chose %d, %v", i, err)
	}
	want := []string{
		"",
		"",
		"",
		"",
		"  b2c_2026-05-11.csv.gz     1.0 KB  2026-05-12 04:00  STANDARD",
		"  c2b_2026-11-05.csv.gz    44.7 KB  2026-11-06 03:00  STANDARD",
		"> b2c_2026-11-05.csv.gz      469 B  2026-11-06 03:00  STANDARD",
		"  3/8",
		"  s3://acme-backups/telcos/ · 7 objects · Enter opens, Esc quits",
		"hop> 2026 11 05",
	}
	got := screen.rows()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("screen:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// The line under the cursor is a bar, what the words cover is in another color,
	// in the name and nowhere else: the date after it holds the same digits.
	if got, want := screen.styles(6), ">>ccccFFFFcFFcFF"+strings.Repeat("c", len(backupLine(3))-14); got != want {
		t.Errorf("styles under the cursor:\n got %s\nwant %s", got, want)
	}
	if got, want := screen.styles(5), "......ffff.ff.ff"; got != want {
		t.Errorf("styles of the line above:\n got %s\nwant %s", got, want)
	}
	// The terminal's cursor is where the next character goes.
	if x, y, visible := screen.GetCursor(); x != 15 || y != 9 || !visible {
		t.Errorf("cursor at %d,%d, visible %v", x, y, visible)
	}
}

func TestPickOpensOnTheStartLine(t *testing.T) {
	i, screen, err := play(t, 60, 8, len(backups), backupLine, names, key(tcell.KeyEnter))
	if err != nil || i != 1 {
		t.Fatalf("chose %d, %v", i, err)
	}
	got := screen.rows()
	// Six rows for eight lines, the first one nearest to the prompt, the cursor on
	// the second.
	if !strings.HasPrefix(got[5], "  ..") || !strings.HasPrefix(got[4], "> b2c_2025-06-24.csv.gz") || !strings.HasPrefix(got[0], "  c2b_2026-10-14.csv.gz") {
		t.Errorf("screen:\n%s", strings.Join(got, "\n"))
	}
	if got[6] != "  8/8" || got[7] != "hop>" {
		t.Errorf("count and prompt: %q, %q", got[6], got[7])
	}
}

func TestPickGoesAroundOnScreen(t *testing.T) {
	// Down from the first line: the last one, and the list scrolls to show it on the
	// top row.
	i, screen, err := play(t, 60, 6, len(backups), backupLine, Options{Prompt: "hop> "}, key(tcell.KeyDown), key(tcell.KeyEnter))
	if err != nil || i != len(backups)-1 {
		t.Fatalf("chose %d, %v", i, err)
	}
	got := screen.rows()
	if !strings.HasPrefix(got[0], "> c2b_2026-11-05.csv.gz") || !strings.HasPrefix(got[3], "  c2b_2026-05-12.csv.gz") {
		t.Errorf("screen:\n%s", strings.Join(got, "\n"))
	}
	// And up from there: the first one again.
	i, screen, err = play(t, 60, 6, len(backups), backupLine, Options{Prompt: "hop> "}, key(tcell.KeyDown), key(tcell.KeyUp), key(tcell.KeyEnter))
	if err != nil || i != 0 || !strings.HasPrefix(screen.rows()[3], "> ..") {
		t.Errorf("chose %d, %v\n%s", i, err, strings.Join(screen.rows(), "\n"))
	}
}

func TestPickLeavesWithoutAChoice(t *testing.T) {
	for _, k := range []tcell.Key{tcell.KeyEsc, tcell.KeyCtrlC} {
		if _, _, err := play(t, 60, 8, len(backups), backupLine, names, append(typed("c2b"), key(k))...); !errors.Is(err, ErrAborted) {
			t.Errorf("%s: %v", tcell.KeyNames[k], err)
		}
	}
	// Nothing found: the count says so, Enter does nothing, Esc leaves.
	_, screen, err := play(t, 60, 8, len(backups), backupLine, names, append(typed("zip"), key(tcell.KeyEnter), key(tcell.KeyEsc))...)
	if !errors.Is(err, ErrAborted) || screen.rows()[6] != "  0/8" || screen.rows()[5] != "" {
		t.Errorf("%v\n%s", err, strings.Join(screen.rows(), "\n"))
	}
}

func TestPickShowsASidePanel(t *testing.T) {
	o := Options{Prompt: "hop> ", Header: "8 lines · Enter to connect, Esc to cancel", Preview: func(i int) string {
		return "name          " + backups[i].name + "\nsaid          a line much too long for the half of the screen it is given\n"
	}}
	i, screen, err := play(t, 60, 7, len(backups), backupLine, o, key(tcell.KeyUp), key(tcell.KeyUp), key(tcell.KeyEnter))
	if err != nil || i != 2 {
		t.Fatalf("chose %d, %v", i, err)
	}
	// Half the screen for the list, which gets four rows of the seven.
	want := []string{
		"  b2c_2026-11-05.csv.gz    .. ┌────────────────────────────┐",
		"> b2c_2026-05-11.csv.gz    .. │ name          b2c_2026-0.. │",
		"  b2c_2025-06-24.csv.gz    .. │ said          a line muc.. │",
		"  ..  back to the prefix ab.. │                            │",
		"  8/8                         │                            │",
		"  8 lines · Enter to connec.. │                            │",
		"hop>                          └────────────────────────────┘",
	}
	if got := screen.rows(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("screen:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// Nothing found: the panel is empty, and is not asked about a line that is not
	// there.
	_, screen, err = play(t, 60, 7, len(backups), backupLine, o, append(typed("zip"), key(tcell.KeyEsc))...)
	if !errors.Is(err, ErrAborted) || screen.rows()[1] != strings.Repeat(" ", 30)+"│                            │" {
		t.Errorf("%v\n%s", err, strings.Join(screen.rows(), "\n"))
	}
}

func TestPickFollowsTheSizeOfTheTerminal(t *testing.T) {
	p := &played{SimulationScreen: tcell.NewSimulationScreen("UTF-8"), width: 60, height: 12}
	if err := p.SimulationScreen.Init(); err != nil {
		t.Fatal(err)
	}
	defer p.SimulationScreen.Fini()
	p.SetSize(60, 12)
	l := newList(len(backups), backupLine, names)
	done := make(chan int)
	go func() {
		i, _ := run(p, l, names)
		done <- i
	}()
	send := func(e tcell.Event) {
		for p.PostEvent(e) != nil {
			time.Sleep(time.Millisecond)
		}
	}
	// Up to the last line, then a terminal of five rows: three for the list, which
	// still shows the line under the cursor.
	for range 6 {
		send(key(tcell.KeyUp))
	}
	p.SetSize(40, 5)
	send(tcell.NewEventResize(40, 5))
	send(key(tcell.KeyEnter))
	if i := <-done; i != 7 {
		t.Fatalf("chose %d", i)
	}
	got := p.rows()
	if len(got) != 5 || !strings.HasPrefix(got[0], "> c2b_2026-11-05.csv.gz    44.7 KB  20..") || got[3] != "  8/8" {
		t.Errorf("screen:\n%s", strings.Join(got, "\n"))
	}
}

// FuzzKeys presses any keys on terminals of any size, small ones included: nothing
// panics, the cursor stays on a line that is shown and on a row that is drawn, and
// what is typed stays within its row.
func FuzzKeys(f *testing.F) {
	f.Add([]byte("2026 11\x01\x02zip\x03\x04"), uint8(60), uint8(8), true)
	f.Add([]byte("\x05\x05\x05\x06\x06c2b\x07\x08\x09"), uint8(3), uint8(2), false)
	f.Add([]byte("\x0a\x0b\x0c\x0d\x0e\x0f"), uint8(0), uint8(0), true)
	command := []tcell.Key{tcell.KeyUp, tcell.KeyDown, tcell.KeyPgUp, tcell.KeyPgDn, tcell.KeyLeft, tcell.KeyRight,
		tcell.KeyHome, tcell.KeyEnd, tcell.KeyBackspace, tcell.KeyDelete, tcell.KeyCtrlW, tcell.KeyCtrlU, tcell.KeyTab,
		tcell.KeyCtrlN, tcell.KeyCtrlP, tcell.KeyCtrlT}
	f.Fuzz(func(t *testing.T, pressed []byte, width, height uint8, panel bool) {
		s := tcell.NewSimulationScreen("UTF-8")
		if err := s.Init(); err != nil {
			t.Fatal(err)
		}
		defer s.Fini()
		s.SetSize(int(width), int(height))
		o := Options{Prompt: "hop> ", Header: "a header", Start: 3, Searched: names.Searched}
		if panel {
			o.Preview = func(i int) string { return backupLine(i) + "\n" + backups[i].rest }
		}
		l := newList(len(backups), backupLine, o)
		for _, b := range pressed {
			k := tcell.NewEventKey(tcell.KeyRune, rune(b), tcell.ModNone)
			if int(b) < len(command) {
				k = key(command[b])
			}
			fr := layout(int(width), int(height), o)
			l.view(fr.rows)
			draw(s, l, o, fr)
			if out := l.key(k, fr.rows, fr.room); out != stay {
				// A byte may be Esc, Enter or Ctrl-C: the picker ends there.
				if _, ok := l.choice(); out == chosen && !ok {
					t.Fatalf("%s chose in a list that shows nothing", k.Name())
				}
				return
			}
			l.view(fr.rows)
			if l.pos < 0 || l.pos > len(l.input) || len(l.input) > max(fr.room, 0) {
				t.Fatalf("typed %q, at %d, for a room of %d", string(l.input), l.pos, fr.room)
			}
			if len(l.shown) == 0 {
				if l.cur != 0 || l.off != 0 {
					t.Fatalf("no line shown, cursor on %d, list from %d", l.cur, l.off)
				}
				continue
			}
			if l.cur < 0 || l.cur >= len(l.shown) || l.off < 0 || l.cur < l.off || l.cur >= l.off+max(fr.rows, 1) {
				t.Fatalf("cursor on %d of %d, list from %d on %d rows", l.cur, len(l.shown), l.off, fr.rows)
			}
		}
	})
}
