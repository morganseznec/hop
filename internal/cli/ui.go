package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/mattn/go-runewidth"
	"golang.org/x/term"
)

var (
	stderrTTY = term.IsTerminal(int(os.Stderr.Fd()))
	stdoutTTY = term.IsTerminal(int(os.Stdout.Fd()))
	stdinTTY  = term.IsTerminal(int(os.Stdin.Fd()))
	noColor   = os.Getenv("NO_COLOR") != ""
)

const (
	cReset  = "\033[0m"
	cBold   = "\033[1m"
	cDim    = "\033[2m"
	cRed    = "\033[31m"
	cGreen  = "\033[32m"
	cYellow = "\033[33m"
	cCyan   = "\033[36m"
)

func paint(tty bool, color, s string) string {
	if !tty || noColor || s == "" {
		return s
	}
	return color + s + cReset
}

// Tool messages go to stderr, prefixed with "hop: ". stdout is kept for data (ls, run
// output).
func info(format string, a ...any) { fmt.Fprintf(os.Stderr, "hop: "+format+"\n", a...) }

func warn(format string, a ...any) {
	fmt.Fprintln(os.Stderr, paint(stderrTTY, cYellow, "hop: "+fmt.Sprintf(format, a...)))
}

func hint(format string, a ...any) {
	fmt.Fprintln(os.Stderr, paint(stderrTTY, cDim, "     "+fmt.Sprintf(format, a...)))
}

// confirm asks a yes/no question; def is the answer to Enter. Without a terminal,
// nobody can answer: it returns false without waiting, so a script never signs in,
// connects or opens anything on a default.
func confirm(question string, def bool) bool {
	if !stdinTTY || !stderrTTY {
		return false
	}
	suffix := " [y/N] "
	if def {
		suffix = " [Y/n] "
	}
	fmt.Fprint(os.Stderr, paint(true, cBold, "hop: "+question)+suffix)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "":
		return def
	case "y", "yes":
		return true
	}
	return false
}

// table aligns columns where some cells are colored: tabwriter counts ANSI sequences
// as text and shifts everything.
type cell struct {
	text  string
	color string
}

type table struct {
	head []string
	rows [][]cell
}

func (t *table) add(cells ...cell) { t.rows = append(t.rows, cells) }

func (t *table) print(w io.Writer, tty bool) {
	widths := make([]int, len(t.head))
	for i, h := range t.head {
		widths[i] = runewidth.StringWidth(h)
	}
	for _, r := range t.rows {
		for i, c := range r {
			// A row may have more cells than the header: its extra columns get
			// their own width.
			if i == len(widths) {
				widths = append(widths, 0)
			}
			widths[i] = max(widths[i], runewidth.StringWidth(c.text))
		}
	}
	line := func(cells []cell) {
		var b strings.Builder
		b.WriteString("  ")
		for i, c := range cells {
			pad := ""
			if i < len(cells)-1 {
				pad = strings.Repeat(" ", widths[i]-runewidth.StringWidth(c.text)+2)
			}
			b.WriteString(paint(tty, c.color, c.text) + pad)
		}
		fmt.Fprintln(w, strings.TrimRight(b.String(), " "))
	}
	var head []cell
	for _, h := range t.head {
		head = append(head, cell{h, cBold})
	}
	line(head)
	for _, r := range t.rows {
		line(r)
	}
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}
