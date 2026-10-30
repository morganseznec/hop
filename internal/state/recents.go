// Package state remembers recent connections, so that the machines actually in use
// are offered first.
package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

func dir() string {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "hop")
	}
	if runtime.GOOS == "windows" {
		if d, err := os.UserCacheDir(); err == nil {
			return filepath.Join(d, "hop")
		}
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "hop")
}

func path() string { return filepath.Join(dir(), "recents.json") }

// Recents maps Instance.Key() to the time of the last connection.
type Recents map[string]time.Time

func LoadRecents() Recents {
	r := Recents{}
	if b, err := os.ReadFile(path()); err == nil {
		if json.Unmarshal(b, &r) != nil {
			return Recents{}
		}
	}
	return r
}

// Touch records a connection and forgets anything unused for 90 days.
func (r Recents) Touch(key string) {
	r[key] = time.Now()
	for k, t := range r {
		if time.Since(t) > 90*24*time.Hour {
			delete(r, k)
		}
	}
	if os.MkdirAll(dir(), 0o700) != nil {
		return
	}
	if b, err := json.Marshal(r); err == nil {
		_ = os.WriteFile(path(), b, 0o600) // losing the order of recents is harmless
	}
}
