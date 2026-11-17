// Package picker is the full-screen list hop opens to choose from: instances, buckets,
// the keys of a bucket. Typing narrows the list, the arrows move through it, Enter
// chooses and Esc leaves.
//
// It draws like the fuzzy finders it takes after: the prompt on the last row, the list
// above it, its first line nearest to the prompt. What it does not take from them is
// the search by scattered letters, which keeps too much of a list of names that look
// alike: here every word typed must be in the line, whole.
package picker

import (
	"errors"

	"github.com/gdamore/tcell/v2"
)

// ErrAborted is what Pick returns when the picker is left without a choice: Esc,
// Ctrl-C or Ctrl-D.
var ErrAborted = errors.New("canceled")

// Options is what a picker shows besides its lines.
type Options struct {
	// Prompt stands before what is typed, on the last row.
	Prompt string
	// Header is one line above the prompt: what the list is, what the keys do.
	Header string
	// Start is the line the cursor is on when the picker opens.
	Start int
	// Searched returns how many characters of line i, counted from its start, are
	// compared to what is typed: the name of an object, and not its size or its date,
	// where a number typed would find lines by accident. Nil: the whole line.
	Searched func(i int) int
	// Preview returns what a side panel says of line i, on the right half of the
	// screen. Nil: no panel, and the lines get the whole width.
	Preview func(i int) string
}

// newScreen opens the terminal. Tests give a simulated one.
var newScreen = tcell.NewScreen

// Pick opens the picker over n lines and returns the index of the one chosen, or
// ErrAborted.
func Pick(n int, line func(i int) string, o Options) (int, error) {
	s, err := newScreen()
	if err != nil {
		return 0, err
	}
	if err := s.Init(); err != nil {
		return 0, err
	}
	defer s.Fini()
	return run(s, newList(n, line, o), o)
}

func run(s tcell.Screen, l *list, o Options) (int, error) {
	for {
		w, h := s.Size()
		f := layout(w, h, o)
		l.view(f.rows)
		draw(s, l, o, f)
		switch e := s.PollEvent().(type) {
		case nil:
			// The screen is gone.
			return 0, ErrAborted
		case *tcell.EventResize:
			s.Sync()
		case *tcell.EventKey:
			switch l.key(e, f.rows, f.room) {
			case aborted:
				return 0, ErrAborted
			case chosen:
				i, _ := l.choice()
				return i, nil
			}
		}
	}
}
