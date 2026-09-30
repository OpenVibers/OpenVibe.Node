package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultWhenMissing(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "none.json"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Server != DefaultServer || len(c.Plugins) != 1 || c.Plugins[0].Name != "dryrun" {
		t.Fatalf("%+v", c)
	}
}

func TestSaveLoadValidate(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	c := Default()
	c.Plugins = append(c.Plugins, PluginConfig{Name: "custom", Command: []string{"/bin/true"}})
	if err := Save(p, c); err != nil {
		t.Fatal(err)
	}
	got, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Plugins) != 2 {
		t.Fatalf("%+v", got)
	}
	bad := []string{
		`{"device_kind":"robot"}`,
		`{"plugins":[{"name":"x"}]}`,
		`{"plugins":[{"name":"dryrun"},{"name":"dryrun"}]}`,
		`{"video":{"source":"webcam"}}`,
		`{"limits":{"max_speed":2}}`,
		`{"typo":1}`,
	}
	for _, b := range bad {
		os.WriteFile(p, []byte(b), 0o600)
		if _, err := Load(p); err == nil {
			t.Errorf("accepted %s", b)
		}
	}
}

func TestPaths(t *testing.T) {
	p := DefaultPaths("/tmp/x")
	if p.ConfigDir != "/tmp/x" || p.CredentialFile() != filepath.Join("/tmp/x", "credential.json") {
		t.Fatalf("%+v", p)
	}
	t.Setenv(EnvHome, "/tmp/y")
	if DefaultPaths("").StateDir != "/tmp/y" {
		t.Fatal("env ignored")
	}
}

func TestPluginCommand(t *testing.T) {
	c := Default()
	c.Python = "/usr/bin/python3"
	got := c.PluginCommand(PluginConfig{Name: "cozmo"}, HomePaths(t.TempDir()))
	if len(got) != 3 || got[2] != "openvibe_cozmo" {
		t.Fatalf("%v", got)
	}
}
