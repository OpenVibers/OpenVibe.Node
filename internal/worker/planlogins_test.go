package worker

import (
	"os"
	"path/filepath"
	"testing"
)

// The plan logins: the copy, the read-only mode, the cleanup and the opt-in.

// setHome points the Node user's home at dir: os.UserHomeDir reads HOME on Linux and macOS, USERPROFILE on Windows.
func setHome(t *testing.T, dir string) {
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
}

func TestPlanLoginsAreOptIn(t *testing.T) {
	// Off by default: nothing is staged, so a job sees no credential of any kind.
	names, err := stagePlanLogins(nil, t.TempDir())
	if err != nil {
		t.Fatalf("staging nothing: %v", err)
	}
	if len(names) != 0 {
		t.Fatalf("staged %v with no plan logins named", names)
	}
}

func TestPlanLoginsAreCopiedReadOnly(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	src := filepath.Join(home, ".claude", ".credentials.json")
	if err := os.MkdirAll(filepath.Dir(src), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte(`{"access_token":"a-token"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	names, err := stagePlanLogins([]string{src}, root)
	if err != nil {
		t.Fatalf("staging: %v", err)
	}
	if len(names) != 1 || names[0] != ".claude/.credentials.json" {
		t.Fatalf("staged %v", names)
	}
	staged := filepath.Join(root, names[0])
	b, err := os.ReadFile(staged)
	if err != nil {
		t.Fatalf("reading the staged copy: %v", err)
	}
	if string(b) != `{"access_token":"a-token"}` {
		t.Fatalf("the copy is not the file: %q", b)
	}
	// Read-only: the job may read it, never change it.
	st, err := os.Stat(staged)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm()&0o222 != 0 {
		t.Fatalf("the staged copy is writable (%v)", st.Mode().Perm())
	}
	if err := os.WriteFile(staged, []byte("x"), 0o600); err == nil {
		t.Fatal("the staged copy could be written")
	}
}

func TestPlanLoginMissingIsSkipped(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	root := t.TempDir()
	// A CLI that is not signed in has no file: that is not an error, the job simply is not signed in.
	names, err := stagePlanLogins([]string{filepath.Join(home, ".codex", "auth.json")}, root)
	if err != nil {
		t.Fatalf("staging: %v", err)
	}
	if len(names) != 0 {
		t.Fatalf("staged %v for a missing file", names)
	}
}

func TestPlanLoginOutsideHomeIsRefused(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(outside, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := stagePlanLogins([]string{outside}, root); err == nil {
		t.Fatal("a login outside the home was staged")
	}
}
