package main

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// TestGenerateSocketPathUnique checks that per-session socket paths carry the
// expected suffix and differ from one another (the whole point: concurrent
// sessions must not collide).
func TestGenerateSocketPathUnique(t *testing.T) {
	a, b := generateSocketPath(), generateSocketPath()
	if filepath.Ext(a) != ".sock" || filepath.Base(a)[:4] != "flue" {
		t.Errorf("path %q: want flue-<rand>.sock", a)
	}
	if a == b {
		t.Errorf("two generated paths are identical: %q", a)
	}
}

// TestPickSocketDir returns a real, writable directory (one of the candidates
// in the documented priority order), never an empty string.
func TestPickSocketDir(t *testing.T) {
	d := pickSocketDir()
	if d == "" {
		t.Fatal("pickSocketDir returned empty")
	}
	for _, want := range []string{"/tmp", "/run"} {
		if d == want || len(d) > len(want) && d[:len(want)] == want {
			if ok := writableDir(d); !ok {
				t.Errorf("pickSocketDir = %q is not writable", d)
			}
			return
		}
	}
	// XDG_RUNTIME_DIR case (custom prefix) — just assert it is a dir.
	if !isDir(d) {
		t.Errorf("pickSocketDir = %q is not a directory", d)
	}
}

// TestListenPrivateMode verifies the socket is created 0600 even under a
// permissive umask, and that the umask is restored afterwards.
func TestListenPrivateMode(t *testing.T) {
	old := unix.Umask(0)
	defer unix.Umask(old)

	path := filepath.Join(t.TempDir(), "s.sock")
	l, err := listenPrivate(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Fatalf("socket mode %o, want 600", perm)
	}
	if got := unix.Umask(0); got != 0 {
		t.Fatalf("umask not restored: %o", got)
	}
}
