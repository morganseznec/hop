//go:build windows

package session

import (
	"os"
	"os/signal"
	"path/filepath"
)

// ignoreSignals keeps Ctrl-C and Ctrl-Break from ending hop until restore is called:
// the plugin, attached to the same console, receives them and forwards them to the
// remote shell. signal.Ignore does not do this on Windows: when no channel listens, the
// runtime hands the event to the default handler, which ends the process. A channel
// that nobody reads is enough, since signal delivery never blocks.
func ignoreSignals() (restore func()) {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	return func() { signal.Stop(c) }
}

const pluginInstallHint = "see https://docs.aws.amazon.com/systems-manager/latest/userguide/session-manager-working-with-install-plugin.html"

func pluginLocations() []string {
	dir := os.Getenv("ProgramFiles")
	if dir == "" {
		return nil
	}
	return []string{filepath.Join(dir, "Amazon", "SessionManagerPlugin", "bin", "session-manager-plugin.exe")}
}
