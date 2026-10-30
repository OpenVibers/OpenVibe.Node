package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestWorkerPlanLogins: worker.plan_logins is off unless the owner names the files, and each named file must be a
// path under the Node user's home: the copy a job gets is made from it.
func TestWorkerPlanLogins(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	ok := []WorkerConfig{{}, {PlanLogins: []string{filepath.Join(home, ".claude", ".credentials.json")}},
		{PlanLogins: []string{filepath.Join(home, ".codex", "auth.json"), filepath.Join(home, ".local", "share", "opencode", "auth.json")}}}
	for _, w := range ok {
		c := Default()
		c.Worker = w
		if err := c.Validate(); err != nil {
			t.Fatalf("%+v: %v", w, err)
		}
	}
	bad := []WorkerConfig{{PlanLogins: []string{"relative/path.json"}},
		{PlanLogins: []string{filepath.Join(home, "..", "elsewhere", "auth.json")}},
		{PlanLogins: []string{home}}}
	for _, w := range bad {
		c := Default()
		c.Worker = w
		if err := c.Validate(); err == nil {
			t.Fatalf("%+v was accepted", w)
		}
	}
}
