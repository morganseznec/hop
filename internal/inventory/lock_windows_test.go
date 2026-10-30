package inventory

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// On Windows, creating a lock file that another process is deleting fails with
// "access is denied": the lock must count as held, not as absent.
func TestLockBusyOnWindows(t *testing.T) {
	if !lockBusy(&fs.PathError{Op: "open", Path: "save.lock", Err: fs.ErrPermission}) {
		t.Error("a permission error on the lock file must count as a held lock")
	}
	if lockBusy(&fs.PathError{Op: "open", Path: "save.lock", Err: fs.ErrNotExist}) {
		t.Error("only permission errors count as a held lock")
	}
}

// replaceFile waits for a reader to close the destination instead of failing.
func TestReplaceFileWhileOpen(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "inventory.json")
	src := filepath.Join(dir, "new.json")
	if err := os.WriteFile(dst, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- replaceFile(src, dst) }()
	f.Close()
	if err := <-done; err != nil {
		t.Fatalf("replaceFile: %v", err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "new" {
		t.Errorf("content %q", b)
	}
}
