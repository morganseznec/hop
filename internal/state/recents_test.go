package state

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func tempStateDir(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	t.Setenv("XDG_STATE_HOME", base)
	return filepath.Join(base, "hop")
}

func TestDir(t *testing.T) {
	stateDir := tempStateDir(t)
	if got := path(); got != filepath.Join(stateDir, "recents.json") {
		t.Errorf("with XDG_STATE_HOME: %s", got)
	}

	home := t.TempDir()
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("LOCALAPPDATA", filepath.Join(home, "AppData", "Local"))
	want := filepath.Join(home, ".local", "state", "hop")
	if runtime.GOOS == "windows" {
		want = filepath.Join(home, "AppData", "Local", "hop")
	}
	if got := dir(); got != want {
		t.Errorf("without XDG_STATE_HOME: %s, want %s", got, want)
	}
}

func TestLoadRecentsWithoutFile(t *testing.T) {
	tempStateDir(t)
	r := LoadRecents()
	if r == nil || len(r) != 0 {
		t.Errorf("no file: %v", r)
	}
}

func TestLoadRecentsCorrupt(t *testing.T) {
	dir := tempStateDir(t)
	os.MkdirAll(dir, 0o700)
	for _, content := range []string{"{not json", `["a list"]`, `{"acme-prod|eu-west-1|i-1": "yesterday"}`, ""} {
		os.WriteFile(filepath.Join(dir, "recents.json"), []byte(content), 0o600)
		r := LoadRecents()
		if r == nil {
			t.Fatalf("%q: nil map, Touch would panic", content)
		}
		r.Touch("acme-prod|eu-west-1|i-2")
	}
}

func TestTouchRoundTrip(t *testing.T) {
	tempStateDir(t)
	before := time.Now()
	r := LoadRecents()
	r.Touch("acme-prod|eu-west-1|i-1")
	r.Touch("globex|us-east-1|i-2")

	got := LoadRecents()
	if len(got) != 2 {
		t.Fatalf("round trip: %v", got)
	}
	for k, ts := range r {
		if !got[k].Equal(ts) {
			t.Errorf("%s: %v, want %v", k, got[k], ts)
		}
		if ts.Before(before) || time.Since(ts) > time.Minute {
			t.Errorf("%s: touched at %v", k, ts)
		}
	}
}

func TestTouchPrunes(t *testing.T) {
	tempStateDir(t)
	now := time.Now()
	r := Recents{
		"acme-prod|eu-west-1|i-old":    now.Add(-91 * 24 * time.Hour),
		"acme-prod|eu-west-1|i-recent": now.Add(-89 * 24 * time.Hour),
		"globex|us-east-1|i-again":     now.Add(-200 * 24 * time.Hour),
	}
	// Touching an old key refreshes it instead of pruning it.
	r.Touch("globex|us-east-1|i-again")
	if _, ok := r["acme-prod|eu-west-1|i-old"]; ok {
		t.Error("an entry unused for 91 days must be pruned")
	}
	if _, ok := r["acme-prod|eu-west-1|i-recent"]; !ok {
		t.Error("an entry used 89 days ago must be kept")
	}
	if time.Since(r["globex|us-east-1|i-again"]) > time.Minute {
		t.Error("the touched entry must be refreshed")
	}
	if got := LoadRecents(); len(got) != 2 {
		t.Errorf("pruning not saved: %v", got)
	}
}

func TestTouchFilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits do not apply on Windows")
	}
	dir := tempStateDir(t)
	Recents{}.Touch("acme-prod|eu-west-1|i-1")
	st, err := os.Stat(filepath.Join(dir, "recents.json"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm()&0o077 != 0 {
		t.Errorf("recents readable by others: %o", st.Mode().Perm())
	}
	dst, _ := os.Stat(dir)
	if dst.Mode().Perm()&0o077 != 0 {
		t.Errorf("state folder open to others: %o", dst.Mode().Perm())
	}
}

func TestTouchUnwritableFolder(t *testing.T) {
	// The state is a convenience: a folder that cannot be created is not an error,
	// and the in-memory map is still updated.
	blocker := filepath.Join(t.TempDir(), "file")
	os.WriteFile(blocker, nil, 0o600)
	t.Setenv("XDG_STATE_HOME", blocker)
	r := Recents{}
	r.Touch("acme-prod|eu-west-1|i-1")
	if _, ok := r["acme-prod|eu-west-1|i-1"]; !ok {
		t.Error("in-memory entry lost")
	}
}
