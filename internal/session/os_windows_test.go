//go:build windows

package session

import (
	"os"
	"os/signal"
	"testing"
)

func TestIgnoreSignals(t *testing.T) {
	// signal.Ignore would let Ctrl-C end hop on Windows: a listening channel must absorb
	// the event instead, and go away with restore.
	restore := ignoreSignals()
	if signal.Ignored(os.Interrupt) {
		t.Error("Ctrl-C handled with signal.Ignore, which does not keep hop alive on Windows")
	}
	restore()
	if signal.Ignored(os.Interrupt) {
		t.Error("Ctrl-C ignored after restore")
	}
}
