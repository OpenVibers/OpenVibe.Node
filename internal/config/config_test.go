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

// TestWorkerVMLimitRequired: RLIMIT_AS is always set: max_vm_bytes has no "none" value and defaults when zero.
func TestWorkerVMLimitRequired(t *testing.T) {
	c := Default()
	c.Worker.Caps.MaxVMBytes = -1
	if err := c.Validate(); err == nil {
		t.Fatal("max_vm_bytes -1 was accepted")
	}
	if v := (WorkerCaps{}).WithDefaults().MaxVMBytes; v != DefaultWorkerCaps.MaxVMBytes {
		t.Fatalf("default max_vm_bytes %d", v)
	}
}

// TestWorkerNameReserved: status.capabilities.worker holds the job runtime classes, so no plugin may be named worker.
func TestWorkerNameReserved(t *testing.T) {
	c := Default()
	c.Plugins = append(c.Plugins, PluginConfig{Name: "worker", Command: []string{"/bin/true"}})
	if err := c.Validate(); err == nil {
		t.Fatal("a plugin named worker was accepted")
	}
}

// TestWorkerEntryClass: an entry is a function unless it says code or linux; browser, desktop and gpu are refused at
// load, and one name@version may be declared once per class.
func TestWorkerEntryClass(t *testing.T) {
	run := filepath.Join(t.TempDir(), "run") // absolute on every platform: Validate wants an absolute command
	entry := func(class string) FunctionConfig {
		return FunctionConfig{Name: "thumbnail", Version: "1.2.0", Class: class, Command: []string{run}}
	}
	if c := entry("").EffectiveClass(); c != ClassFunction {
		t.Fatalf("no class is %q", c)
	}
	for _, class := range []string{"", "function", "code", "linux"} {
		c := Default()
		c.Worker.Functions = []FunctionConfig{entry(class)}
		if err := c.Validate(); err != nil {
			t.Fatalf("class %q: %v", class, err)
		}
	}
	for _, class := range []string{"browser", "desktop", "gpu", "wasm", "Code"} {
		c := Default()
		c.Worker.Functions = []FunctionConfig{entry(class)}
		if err := c.Validate(); err == nil {
			t.Fatalf("class %q was accepted", class)
		}
	}
	c := Default()
	c.Worker.Functions = []FunctionConfig{entry(""), entry("code")}
	if err := c.Validate(); err != nil {
		t.Fatalf("one artifact as function and code: %v", err)
	}
	c.Worker.Functions = append(c.Worker.Functions, entry("function"))
	if err := c.Validate(); err == nil {
		t.Fatal("a function entry with no class and one with class function were both accepted")
	}
}

// TestWorkerEgress: the three policies load; openvibe-only needs its allowlist, which no other policy takes; both
// lists hold IPv4 CIDRs only.
func TestWorkerEgress(t *testing.T) {
	ok := []WorkerConfig{{}, {Egress: EgressNone}, {Egress: EgressPublic, EgressDeny: []string{"203.0.113.0/24"}},
		{Egress: EgressOpenVibeOnly, EgressAllow: []string{"198.51.100.7/32"}, EgressDeny: []string{"198.51.100.0/24"}}}
	for _, w := range ok {
		c := Default()
		c.Worker = w
		if err := c.Validate(); err != nil {
			t.Fatalf("%+v: %v", w, err)
		}
	}
	bad := []WorkerConfig{{Egress: "open"}, {Egress: EgressOpenVibeOnly},
		{Egress: EgressPublic, EgressAllow: []string{"198.51.100.0/24"}}, {EgressAllow: []string{"198.51.100.0/24"}},
		{Egress: EgressOpenVibeOnly, EgressAllow: []string{"openvibe.network"}},
		{Egress: EgressOpenVibeOnly, EgressAllow: []string{"2001:db8::/32"}},
		{Egress: EgressOpenVibeOnly, EgressAllow: []string{"198.51.100.7/24"}},
		{Egress: EgressPublic, EgressDeny: []string{"10.0.0.1"}}}
	for _, w := range bad {
		c := Default()
		c.Worker = w
		if err := c.Validate(); err == nil {
			t.Fatalf("%+v was accepted", w)
		}
	}
}

// TestWorkerMedia: worker.media is off unless enabled; enabled it needs an https endpoint (no credentials in it) and an
// app, and its token is named, never written: a key the block does not know fails the load.
func TestWorkerMedia(t *testing.T) {
	ok := []MediaConfig{{}, {Endpoint: "https://media.openvibe.network"},
		{Enabled: true, Endpoint: "https://media.openvibe.network", App: "prj_01JAB2C3D4E5F6G7H8J9K0MNPQ", TokenEnv: "RUN_MEDIA_TOKEN", MaxBytes: 1 << 20}}
	for _, m := range ok {
		c := Default()
		c.Worker.Media = m
		if err := c.Validate(); err != nil {
			t.Fatalf("%+v: %v", m, err)
		}
	}
	bad := []MediaConfig{{Enabled: true}, {Enabled: true, Endpoint: "https://media.openvibe.network"},
		{Enabled: true, Endpoint: "http://media.openvibe.network", App: "run"},
		{Enabled: true, Endpoint: "https://user:pw@media.openvibe.network", App: "run"},
		{Enabled: true, Endpoint: "https://media.openvibe.network?token=x", App: "run"},
		{Enabled: true, Endpoint: "https://media.openvibe.network", App: "../admin"},
		{Enabled: true, Endpoint: "https://media.openvibe.network", App: "run", TokenEnv: "A=B"},
		{Enabled: true, Endpoint: "https://media.openvibe.network", App: "run", MaxBytes: -1}}
	for _, m := range bad {
		c := Default()
		c.Worker.Media = m
		if err := c.Validate(); err == nil {
			t.Fatalf("%+v validated", m)
		}
	}
	if d := (MediaConfig{}).WithDefaults(); d.TokenEnv != "OPENVIBE_MEDIA_TOKEN" || d.MaxBytes != 64<<20 {
		t.Fatalf("defaults %+v", d)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"worker":{"media":{"enabled":true,"endpoint":"https://media.openvibe.network","app":"run"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if c, err := Load(path); err != nil || !c.Worker.Media.Enabled || c.Worker.Media.App != "run" {
		t.Fatalf("%+v %v", c, err)
	}
	if err := os.WriteFile(path, []byte(`{"worker":{"media":{"enabled":true,"endpoint":"https://media.openvibe.network","app":"run","token":"x"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("a token written in the config file loaded")
	}
}
