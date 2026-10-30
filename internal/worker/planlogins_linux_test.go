//go:build linux

package worker

import (
	"os"
	"path/filepath"
	"testing"
)

// The plan logins as the job sees them: copied into the job's home, read-only, and never able to leave it.

func TestPlanLoginsAreCopiedIntoTheJobHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	src := filepath.Join(home, ".codex", "auth.json")
	if err := os.MkdirAll(filepath.Dir(src), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte(`{"tokens":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	names, err := stagePlanLogins([]string{src}, root)
	if err != nil {
		t.Fatalf("staging: %v", err)
	}
	work := t.TempDir()
	dir, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	if err := copyPlanLogins(dir, work, names); err != nil {
		t.Fatalf("copying: %v", err)
	}
	got := filepath.Join(work, ".codex", "auth.json")
	b, err := os.ReadFile(got)
	if err != nil {
		t.Fatalf("reading the copy: %v", err)
	}
	if string(b) != `{"tokens":{}}` {
		t.Fatalf("the copy is not the file: %q", b)
	}
	st, err := os.Stat(got)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm()&0o222 != 0 {
		t.Fatalf("the copy is writable (%v)", st.Mode().Perm())
	}
}

func TestPlanLoginNameMayNotLeaveTheHome(t *testing.T) {
	dir := t.TempDir()
	if err := copyPlanLogins(nil, dir, []string{"../escape"}); err == nil {
		t.Fatal("a name that leaves the home was copied")
	}
}
