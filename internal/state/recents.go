// Package state remembers recent connections, so that the machines actually in use
// are offered first.
package state

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// dir returns the folder of the state files. Without a home directory (HOME unset)
// and without XDG_STATE_HOME, it fails rather than use a folder relative to the
// working directory.
func dir() (string, error) {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "hop"), nil
	}
	if runtime.GOOS == "windows" {
		if d, err := os.UserCacheDir(); err == nil {
			return filepath.Join(d, "hop"), nil
		}
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", errors.New("no folder for the recent instances: the home directory is not set (set HOME, or XDG_STATE_HOME)")
	}
	return filepath.Join(home, ".local", "state", "hop"), nil
}

// Recents maps Instance.Key() to the time of the last connection.
type Recents map[string]time.Time

func LoadRecents() Recents {
	r := Recents{}
	d, err := dir()
	if err != nil {
		return r
	}
	if b, err := os.ReadFile(filepath.Join(d, "recents.json")); err == nil {
		if json.Unmarshal(b, &r) != nil {
			return Recents{}
		}
	}
	return r
}

// Touch records a connection and forgets anything unused for 90 days. The file is
// written to a temporary file then renamed, so that two hops connecting at the same
// time never leave a truncated file behind.
func (r Recents) Touch(key string) error {
	r[key] = time.Now()
	for k, t := range r {
		if time.Since(t) > 90*24*time.Hour {
			delete(r, k)
		}
	}
	d, err := dir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(d, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(d, "recents-*.json")
	if err != nil {
		return err
	}
	_, err = tmp.Write(b)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), filepath.Join(d, "recents.json"))
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
	return err
}
