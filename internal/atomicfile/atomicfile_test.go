package atomicfile

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func leftovers(t *testing.T, dir string) []string {
	t.Helper()
	m, _ := filepath.Glob(filepath.Join(dir, ".part-*"))
	return m
}

func TestCommitRenamesIntoPlace(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "app.log")
	f, err := Create(dst, ".part-*")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Discard()
	if _, err := os.Stat(dst); err == nil {
		t.Fatal("the destination exists before Commit")
	}
	f.WriteString("hello")
	if err := f.Commit(); err != nil {
		t.Fatal(err)
	}
	// Discard after Commit, as a deferred call does, must leave the file alone.
	f.Discard()
	b, err := os.ReadFile(dst)
	if err != nil || string(b) != "hello" {
		t.Fatalf("content %q, %v", b, err)
	}
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(dst); st.Mode().Perm() != 0o600 {
			t.Errorf("a new file must be readable by its owner only, got %v", st.Mode().Perm())
		}
	}
	if l := leftovers(t, dir); len(l) != 0 {
		t.Errorf("temporary files left: %v", l)
	}
}

func TestCommitKeepsTheModeOfAnExistingFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no permission bits to keep")
	}
	dst := filepath.Join(t.TempDir(), "app.conf")
	if err := os.WriteFile(dst, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := Create(dst, ".part-*")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Discard()
	f.WriteString("new")
	if err := f.Commit(); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(dst)
	b, _ := os.ReadFile(dst)
	if st.Mode().Perm() != 0o644 || string(b) != "new" {
		t.Errorf("mode %v, content %q", st.Mode().Perm(), b)
	}
}

func TestDiscardLeavesNothingBehind(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "app.log")
	if err := os.WriteFile(dst, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := Create(dst, ".part-*")
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("half a file")
	f.Discard()
	if l := leftovers(t, dir); len(l) != 0 {
		t.Errorf("temporary files left: %v", l)
	}
	if b, _ := os.ReadFile(dst); string(b) != "old" {
		t.Errorf("the destination changed: %q", b)
	}
}

func TestCreateFailsWithoutTheDirectory(t *testing.T) {
	if _, err := Create(filepath.Join(t.TempDir(), "missing", "f"), ".part-*"); err == nil {
		t.Error("expected an error")
	}
}
