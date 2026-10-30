//go:build !windows

package session

import (
	"os"
	"os/signal"
	"runtime"
	"syscall"
)

// ignoreSignals makes hop ignore Ctrl-C, Ctrl-\ and Ctrl-Z, as `aws ssm start-session`
// does: the plugin inherits the ignored dispositions, then receives the signals from
// the terminal and forwards them to the remote shell.
//
// restore does nothing: os/signal has no way to undo Ignore (Reset only undoes Notify),
// so the signals stay ignored after the session, which is when hop exits.
func ignoreSignals() (restore func()) {
	signal.Ignore(os.Interrupt, syscall.SIGQUIT, syscall.SIGTSTP)
	return func() {}
}

var pluginInstallHint = func() string {
	if runtime.GOOS == "darwin" {
		return "brew install --cask session-manager-plugin"
	}
	return "see https://docs.aws.amazon.com/systems-manager/latest/userguide/session-manager-working-with-install-plugin.html"
}()
