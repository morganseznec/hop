//go:build windows

package cli

import (
	"os"

	"golang.org/x/sys/windows"
)

// Windows consoles only interpret ANSI colors once virtual terminal processing is
// on. Where it cannot be turned on, print plain text.
func init() {
	for _, f := range []*os.File{os.Stdout, os.Stderr} {
		h := windows.Handle(f.Fd())
		var mode uint32
		if windows.GetConsoleMode(h, &mode) != nil {
			continue
		}
		if windows.SetConsoleMode(h, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING) != nil {
			noColor = true
		}
	}
}
