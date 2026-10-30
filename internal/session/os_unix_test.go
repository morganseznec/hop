//go:build !windows

package session

import (
	"os"
	"os/signal"
	"syscall"
	"testing"
)

func TestIgnoreSignals(t *testing.T) {
	restore := ignoreSignals()
	defer restore()
	for _, s := range []os.Signal{os.Interrupt, syscall.SIGQUIT, syscall.SIGTSTP} {
		if !signal.Ignored(s) {
			t.Errorf("%v not ignored while the plugin runs", s)
		}
	}
	// The disposition is real, not only bookkeeping: a Ctrl-C does not end the test.
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
}
