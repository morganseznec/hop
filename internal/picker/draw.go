package picker

import (
	"fmt"
	"strings"

	"github.com/gdamore/tcell/v2"
	"github.com/mattn/go-runewidth"
)

// The colors are those of the terminal's own palette, so that the picker follows its
// theme, light or dark.
var (
	stylePrompt = tcell.StyleDefault.Foreground(tcell.ColorBlue)
	styleInput  = tcell.StyleDefault.Bold(true)
	styleHeader = tcell.StyleDefault.Foreground(tcell.ColorGreen)
	styleCount  = tcell.StyleDefault.Foreground(tcell.ColorYellow)
	// styleFound is for the characters that a word typed covers.
	styleFound = tcell.StyleDefault.Foreground(tcell.ColorGreen)
	// The line under the cursor is a dark bar.
	styleCursor      = tcell.StyleDefault.Foreground(tcell.ColorYellow).Background(tcell.ColorBlack).Bold(true)
	styleCursorFound = tcell.StyleDefault.Foreground(tcell.ColorDarkCyan).Background(tcell.ColorBlack).Bold(true)
	stylePointer     = tcell.StyleDefault.Foreground(tcell.ColorRed).Background(tcell.ColorBlack)
	styleBox         = tcell.StyleDefault.Foreground(tcell.ColorGray)
)

// frame says where things go on a screen of a given size. From the bottom up: the
// prompt, the header when there is one, the count, then the list.
type frame struct {
	width, height int
	// lineWidth is the room for a line of the list, and for the header: the screen,
	// or its left half next to a side panel.
	lineWidth int
	// rows is how many lines of the list fit, and bottom the row of the one nearest
	// to the prompt.
	rows, bottom int
	// room is the width left for what is typed.
	room int
}

func layout(width, height int, o Options) frame {
	f := frame{width: width, height: height, lineWidth: width}
	if o.Preview != nil {
		f.lineWidth = width/2 - 1
	}
	taken := 2 // the prompt and the count
	if o.Header != "" {
		taken++
	}
	f.rows = max(height-taken, 0)
	f.bottom = height - taken - 1
	f.room = max(f.lineWidth-runewidth.StringWidth(o.Prompt)-1, 0)
	return f
}

func draw(s tcell.Screen, l *list, o Options, f frame) {
	s.Clear()
	y := f.height - 1

	// What is typed, with the terminal's cursor where the next character goes.
	x := put(s, 0, y, o.Prompt, stylePrompt, f.width)
	put(s, x, y, string(l.input), styleInput, f.width)
	s.ShowCursor(x+runewidth.StringWidth(string(l.input[:l.pos])), y)
	y--

	if o.Header != "" {
		put(s, 2, y, runewidth.Truncate(o.Header, f.lineWidth-2, ".."), styleHeader, f.lineWidth)
		y--
	}
	put(s, 2, y, fmt.Sprintf("%d/%d", len(l.shown), len(l.lines)), styleCount, f.lineWidth)

	for row := 0; row < f.rows && l.off+row < len(l.shown); row++ {
		i := l.shown[l.off+row]
		drawLine(s, f.bottom-row, l.lines[i], l.texts[i].marks(l.words), l.off+row == l.cur, f.lineWidth)
	}
	if o.Preview != nil {
		said := ""
		if i, ok := l.choice(); ok {
			said = o.Preview(i)
		}
		drawPreview(s, said, f)
	}
	s.Show()
}

// drawLine writes one line of the list on row y, the characters found in another
// color, and the whole line as a bar under the cursor. A line too long for the width
// ends with "..".
func drawLine(s tcell.Screen, y int, line string, found []bool, cursor bool, width int) {
	plain, marked := tcell.StyleDefault, styleFound
	if cursor {
		plain, marked = styleCursor, styleCursorFound
		s.SetContent(0, y, '>', nil, stylePointer)
		s.SetContent(1, y, ' ', nil, stylePointer)
	}
	x := 2
	for j, r := range []rune(line) {
		style := plain
		if j < len(found) && found[j] {
			style = marked
		}
		rw := runewidth.RuneWidth(r)
		if x+rw+2 > width {
			put(s, x, y, "..", style, width)
			return
		}
		s.SetContent(x, y, shown(r), nil, style)
		x += rw
	}
}

// drawPreview frames the right half of the screen and writes there what the panel
// says, a line too long ending with "..".
func drawPreview(s tcell.Screen, said string, f frame) {
	left, right := f.width/2, f.width-1
	if right-left < 4 || f.height < 3 {
		return
	}
	for x := left + 1; x < right; x++ {
		s.SetContent(x, 0, '─', nil, styleBox)
		s.SetContent(x, f.height-1, '─', nil, styleBox)
	}
	for y := 1; y < f.height-1; y++ {
		s.SetContent(left, y, '│', nil, styleBox)
		s.SetContent(right, y, '│', nil, styleBox)
	}
	s.SetContent(left, 0, '┌', nil, styleBox)
	s.SetContent(right, 0, '┐', nil, styleBox)
	s.SetContent(left, f.height-1, '└', nil, styleBox)
	s.SetContent(right, f.height-1, '┘', nil, styleBox)

	// One column of air on each side of the text.
	width := right - left - 3
	for i, line := range strings.Split(strings.TrimRight(said, "\n"), "\n") {
		if i+1 >= f.height-1 {
			break
		}
		put(s, left+2, i+1, runewidth.Truncate(line, width, ".."), tcell.StyleDefault, right-1)
	}
}

// put writes str from column x of row y, up to the column limit, which it does not
// reach. It returns the column that follows what it wrote.
func put(s tcell.Screen, x, y int, str string, style tcell.Style, limit int) int {
	for _, r := range str {
		rw := runewidth.RuneWidth(r)
		if x+rw > limit {
			break
		}
		s.SetContent(x, y, shown(r), nil, style)
		x += rw
	}
	return x
}

// shown is the rune drawn for r: a control character would move the terminal's cursor
// rather than show, so it is drawn as a space.
func shown(r rune) rune {
	if r < ' ' || r == 0x7f {
		return ' '
	}
	return r
}
