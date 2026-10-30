package inventory

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestLockBusyOnlyOnWindows(t *testing.T) {
	err := &fs.PathError{Op: "open", Path: "save.lock", Err: fs.ErrPermission}
	if got, want := lockBusy(err), runtime.GOOS == "windows"; got != want {
		t.Errorf("lockBusy = %v, want %v on %s", got, want, runtime.GOOS)
	}
}

func TestReplaceFile(t *testing.T) {
	dir := t.TempDir()
	dst, src := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	if err := os.WriteFile(dst, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := replaceFile(src, dst); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "new" {
		t.Errorf("content %q", b)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Error("the source must be gone after the move")
	}
}
