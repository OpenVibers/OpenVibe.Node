// Package config finds the Node's directories and reads its config file.
//
// Layout (system install):
//
//	Linux    /etc/openvibe-node/{config.json,credential.json}   /var/lib/openvibe-node/{latch.json,node.sock,venv}
//	macOS    /Library/Application Support/OpenVibe Node/ (both)
//	Windows  %ProgramData%\OpenVibe Node\ (both)
//
// OPENVIBE_NODE_HOME (or --home) puts everything in one directory, which is how tests and people trying the Node
// without root run it. A non-root user on a machine without a system install gets the per-user config directory.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	DefaultServer = "https://openvibe.bot"
	EnvHome       = "OPENVIBE_NODE_HOME"
)

// Paths are the Node's directories.
type Paths struct {
	ConfigDir string
	StateDir  string
}

func (p Paths) ConfigFile() string     { return filepath.Join(p.ConfigDir, "config.json") }
func (p Paths) CredentialFile() string { return filepath.Join(p.ConfigDir, "credential.json") }
func (p Paths) LatchFile() string      { return filepath.Join(p.StateDir, "latch.json") }
func (p Paths) SocketPath() string     { return filepath.Join(p.StateDir, "node.sock") }
func (p Paths) PluginsDir() string     { return filepath.Join(p.StateDir, "plugins") }
func (p Paths) VenvDir() string        { return filepath.Join(p.StateDir, "venv") }
func (p Paths) FramesDir() string      { return filepath.Join(p.StateDir, "frames") }

// Ensure creates the directories with owner-only permissions.
func (p Paths) Ensure() error {
	for _, d := range []string{p.ConfigDir, p.StateDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	return nil
}

// SystemPaths are the directories of a system-wide install on this OS.
func SystemPaths() Paths {
	switch runtime.GOOS {
	case "windows":
		base := os.Getenv("ProgramData")
		if base == "" {
			base = `C:\ProgramData`
		}
		d := filepath.Join(base, "OpenVibe Node")
		return Paths{ConfigDir: d, StateDir: d}
	case "darwin":
		d := "/Library/Application Support/OpenVibe Node"
		return Paths{ConfigDir: d, StateDir: d}
	default:
		return Paths{ConfigDir: "/etc/openvibe-node", StateDir: "/var/lib/openvibe-node"}
	}
}

// HomePaths puts config and state in one directory.
func HomePaths(dir string) Paths { return Paths{ConfigDir: dir, StateDir: dir} }

// DefaultPaths picks the directories: --home / OPENVIBE_NODE_HOME, else the system install when running as root (or
// when one exists), else the per-user config directory.
func DefaultPaths(home string) Paths {
	if home == "" {
		home = os.Getenv(EnvHome)
	}
	if home != "" {
		return HomePaths(home)
	}
	sys := SystemPaths()
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		return sys
	}
	if _, err := os.Stat(sys.ConfigDir); err == nil {
		return sys
	}
	if d, err := os.UserConfigDir(); err == nil {
		return HomePaths(filepath.Join(d, "openvibe-node"))
	}
	return sys
}

// Config is config.json.
type Config struct {
	// Server is the OpenVibe.Bot origin used for pairing (and the device link unless the pairing returns one).
	Server string `json:"server,omitempty"`
	// DeviceKind is "onboard" (the Node runs on the robot) or "bridge" (it drives a robot over the robot's own link).
	DeviceKind string         `json:"device_kind,omitempty"`
	Plugins    []PluginConfig `json:"plugins"`
	Video      VideoConfig    `json:"video"`
	// Limits are the local owner's caps, applied on top of the server's (the stricter value wins).
	Limits LocalLimits `json:"limits"`
	// Python is the interpreter for plugins without an explicit command (default: <state>/venv/bin/python, then python3).
	Python   string `json:"python,omitempty"`
	LogLevel string `json:"log_level,omitempty"`
}

type LocalLimits struct {
	MaxSpeed     *float64 `json:"max_speed,omitempty"`
	MaxTurn      *float64 `json:"max_turn,omitempty"`
	MaxCommandMS int      `json:"max_command_ms,omitempty"`
}

// PluginConfig is one driver.
type PluginConfig struct {
	// Name identifies the plugin in commands (command.target), logs and status.
	Name string `json:"name"`
	// Command is the executable and arguments. Empty for a bundled plugin: <python> -m openvibe_<name>.
	Command  []string          `json:"command,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
	Dir      string            `json:"dir,omitempty"`
	Config   map[string]any    `json:"config,omitempty"` // sent to the plugin in hello
	Disabled bool              `json:"disabled,omitempty"`
}

// VideoConfig selects the camera source published over WHIP.
type VideoConfig struct {
	// Source: "auto" (default: a plugin's h264_command if its program exists, else a plugin's JPEG frames, else the
	// test pattern when a plugin asks for it), "test", "command", "plugin" or "off".
	Source  string   `json:"source,omitempty"`
	Command []string `json:"command,omitempty"` // for "command": a program writing H.264 Annex B to stdout
	Plugin  string   `json:"plugin,omitempty"`  // for "plugin": which plugin's frames (default: the first with a camera)
	FFmpeg  string   `json:"ffmpeg,omitempty"`  // path to ffmpeg for JPEG → H.264 (default: ffmpeg on PATH)
	FPS     int      `json:"fps,omitempty"`
	Width   int      `json:"width,omitempty"`
	Height  int      `json:"height,omitempty"`
}

// Bundled lists the plugins that ship with the Node.
var Bundled = map[string]string{
	"dryrun":        "openvibe_dryrun",
	"adeept_adr036": "openvibe_adeept_adr036",
	"cozmo":         "openvibe_cozmo",
}

// Default is the config written by `openvibe-node pair` when none exists: the dry-run plugin and its test pattern.
func Default() *Config {
	return &Config{
		Server:     DefaultServer,
		DeviceKind: "onboard",
		Plugins:    []PluginConfig{{Name: "dryrun"}},
		Video:      VideoConfig{Source: "auto"},
	}
}

// Load reads config.json; a missing file yields Default().
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Default(), nil
	}
	if err != nil {
		return nil, err
	}
	c := &Config{}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(c); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	c.fill()
	return c, c.Validate()
}

// Save writes config.json (mode 0600: it can hold plugin settings such as Wi-Fi names).
func Save(path string, c *Config) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

func (c *Config) fill() {
	if c.Server == "" {
		c.Server = DefaultServer
	}
	if c.DeviceKind == "" {
		c.DeviceKind = "onboard"
	}
	if c.Video.Source == "" {
		c.Video.Source = "auto"
	}
}

// Validate checks names and values.
func (c *Config) Validate() error {
	if c.DeviceKind != "onboard" && c.DeviceKind != "bridge" {
		return fmt.Errorf("config: device_kind must be onboard or bridge, not %q", c.DeviceKind)
	}
	seen := map[string]bool{}
	for _, p := range c.Plugins {
		if p.Name == "" || strings.ContainsAny(p.Name, " /\\") {
			return fmt.Errorf("config: plugin name %q is empty or has spaces or slashes", p.Name)
		}
		if seen[p.Name] {
			return fmt.Errorf("config: plugin %q listed twice", p.Name)
		}
		seen[p.Name] = true
		if len(p.Command) == 0 && Bundled[p.Name] == "" {
			return fmt.Errorf("config: plugin %q needs a command (it is not a bundled plugin)", p.Name)
		}
	}
	switch c.Video.Source {
	case "auto", "test", "command", "plugin", "off":
	default:
		return fmt.Errorf("config: video.source %q is not auto, test, command, plugin or off", c.Video.Source)
	}
	if c.Video.Source == "command" && len(c.Video.Command) == 0 {
		return errors.New("config: video.source command needs video.command")
	}
	for _, l := range []*float64{c.Limits.MaxSpeed, c.Limits.MaxTurn} {
		if l != nil && (*l < 0 || *l > 1) {
			return errors.New("config: limits must be between 0 and 1")
		}
	}
	return nil
}

// PluginCommand resolves a plugin's argv.
func (c *Config) PluginCommand(p PluginConfig, paths Paths) []string {
	if len(p.Command) > 0 {
		return p.Command
	}
	return []string{c.PythonPath(paths), "-m", Bundled[p.Name]}
}

// PythonPath is the interpreter for bundled plugins.
func (c *Config) PythonPath(paths Paths) string {
	if c.Python != "" {
		return c.Python
	}
	venv := filepath.Join(paths.VenvDir(), "bin", "python")
	if runtime.GOOS == "windows" {
		venv = filepath.Join(paths.VenvDir(), "Scripts", "python.exe")
	}
	if _, err := os.Stat(venv); err == nil {
		return venv
	}
	if runtime.GOOS == "windows" {
		return "python"
	}
	return "python3"
}
