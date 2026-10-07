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
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
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
	// Worker runs `function`, `code` and `linux` jobs from the control link (docs/worker.md). Off unless enabled.
	Worker WorkerConfig `json:"worker,omitzero"`
}

// WorkerConfig is the job worker. Disabled (the default), every job is refused "class not available". Only the
// entries listed here ever run: a job names one by name, exact version and class, and nothing is downloaded.
type WorkerConfig struct {
	Enabled   bool             `json:"enabled,omitempty"`
	Functions []FunctionConfig `json:"functions,omitempty"`
	// RunAs is the dedicated unprivileged uid/gid jobs run as; it needs the Node to run as root. Without it the Node
	// refuses to run jobs unless AllowSameUser is set.
	RunAs *RunAs `json:"run_as,omitempty"`
	// AllowSameUser runs jobs as the Node's own user (no run_as), with its groups (gpio, dialout, video), on what the
	// job's private root holds: for development only.
	AllowSameUser bool       `json:"allow_same_user,omitempty"`
	Caps          WorkerCaps `json:"caps,omitzero"`
	// Egress is the most a job may reach on the network, all three enforced (docs/worker.md): none (the default; a
	// network namespace with only a loopback that is down), public (IPv4 to public addresses, through a veth the Node
	// NATs; private, shared, link-local and reserved ranges, EgressDeny and the host itself refused) or openvibe-only
	// (public restricted to EgressAllow). A job runs under the stricter of the net it names and this: a job naming no
	// net (platform.job@1's declared default, deny), or deny or none, gets no network whatever this says; a job naming
	// public or openvibe-only is refused when this is none, never run with less than was asked. A policy the host cannot
	// enforce fails the probe, so the worker stays off.
	Egress string `json:"egress,omitempty"`
	// EgressAllow is the IPv4 CIDRs an openvibe-only job may reach (the OpenVibe network's own ranges, configured
	// rather than guessed); required with openvibe-only and refused with any other policy.
	EgressAllow []string `json:"egress_allow,omitempty"`
	// EgressDeny is more IPv4 CIDRs a public or openvibe-only job is refused (the Node's cell, WireGuard peers with
	// public addresses), on top of the ranges always refused.
	EgressDeny []string `json:"egress_deny,omitempty"`
	// Media is where a job's inputs come from and a result over 256 KiB is uploaded to. Off (the default), a job
	// naming inputs is refused and a result over 256 KiB ends the job limit.
	Media MediaConfig `json:"media,omitzero"`
	// NodeDirs are the Node's own directories (config, credential, state, control socket), which no job may see; the
	// Node sets them, the config file does not.
	NodeDirs []string `json:"-"`
}

// MediaConfig is the OpenVibe.Media objects client the worker fetches inputs and uploads large results with. The
// token is read from the environment variable TokenEnv names when a job needs it: it never lies in the config file
// and appears in no job's environment, log line or error. Which app and token a Node uses is the operator's choice
// (Run's own app, never Node's), made when they enable it.
type MediaConfig struct {
	Enabled  bool   `json:"enabled,omitempty"`
	Endpoint string `json:"endpoint,omitempty"`  // https only, e.g. https://media.openvibe.network
	App      string `json:"app,omitempty"`       // the :app of /api/v2/:app/objects
	TokenEnv string `json:"token_env,omitempty"` // default DefaultMediaTokenEnv
	MaxBytes int64  `json:"max_bytes,omitempty"` // the most bytes one input or uploaded result may take; default 64 MiB
}

// DefaultMediaTokenEnv is the variable worker.media's token is read from when token_env is not set.
const DefaultMediaTokenEnv = "OPENVIBE_MEDIA_TOKEN"

// WithDefaults fills token_env and max_bytes.
func (m MediaConfig) WithDefaults() MediaConfig {
	if m.TokenEnv == "" {
		m.TokenEnv = DefaultMediaTokenEnv
	}
	if m.MaxBytes == 0 {
		m.MaxBytes = 64 << 20
	}
	return m
}

var (
	mediaAppRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)
	envNameRe  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
)

func (m MediaConfig) validate() error {
	if m.Endpoint != "" {
		u, err := url.Parse(m.Endpoint)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return errors.New("config: worker.media.endpoint must be an https:// URL with no credentials, query or fragment")
		}
	}
	switch {
	case m.App != "" && !mediaAppRe.MatchString(m.App):
		return fmt.Errorf("config: worker.media.app %q is not an app id", m.App)
	case m.TokenEnv != "" && !envNameRe.MatchString(m.TokenEnv):
		return fmt.Errorf("config: worker.media.token_env %q is not an environment variable name", m.TokenEnv)
	case m.MaxBytes < 0:
		return errors.New("config: worker.media.max_bytes must not be negative")
	case m.Enabled && (m.Endpoint == "" || m.App == ""):
		return errors.New("config: worker.media.enabled needs worker.media.endpoint and worker.media.app")
	}
	return nil
}

// The worker.egress policies.
const (
	EgressNone         = "none"
	EgressPublic       = "public"
	EgressOpenVibeOnly = "openvibe-only"
)

// FunctionConfig is one artifact a job may run: Command is started with the job's args as JSON on stdin.
type FunctionConfig struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	// Class is the runtime class this entry implements: function (the default), code or linux. A job runs it only when
	// its class matches; browser, desktop and gpu are refused at load. A linux entry is an arbitrary declared Linux
	// command, not a language runtime.
	Class   string            `json:"class,omitempty"`
	Command []string          `json:"command"` // Command[0] is an absolute path
	Env     map[string]string `json:"env,omitempty"`
	// ArtifactDir is the directory each job gets a read-only copy of, made as it starts, in its private root (default:
	// the directory of Command[0]). It must not be / nor hold, or lie inside, the Node's own directories.
	ArtifactDir string `json:"artifact_dir,omitempty"`
}

// The classes a declared entry may implement.
const (
	ClassFunction = "function"
	ClassCode     = "code"
	ClassLinux    = "linux"
)

// EffectiveClass is the entry's runtime class, function when none is set.
func (f FunctionConfig) EffectiveClass() string {
	if f.Class == "" {
		return ClassFunction
	}
	return f.Class
}

// Artifact is the directory a job of the function gets a read-only copy of.
func (f FunctionConfig) Artifact() string {
	if f.ArtifactDir != "" {
		return filepath.Clean(f.ArtifactDir)
	}
	if len(f.Command) == 0 {
		return ""
	}
	return filepath.Dir(f.Command[0])
}

// RunAs is a uid and gid on this machine.
type RunAs struct {
	UID uint32 `json:"uid"`
	GID uint32 `json:"gid"`
}

// WorkerCaps are the owner's local caps on every job; a job's own ttl_ms and limits are clamped to them (the
// stricter value wins). Zero means the default (DefaultWorkerCaps).
type WorkerCaps struct {
	MaxTTLMS       int64 `json:"max_ttl_ms,omitempty"`
	MaxWallMS      int64 `json:"max_wall_ms,omitempty"`
	MaxCPUMS       int64 `json:"max_cpu_ms,omitempty"`
	MaxMemBytes    int64 `json:"max_mem_bytes,omitempty"`
	MaxOutputBytes int64 `json:"max_output_bytes,omitempty"` // stdout of one job, in bytes
	MaxJobs        int   `json:"max_jobs,omitempty"`         // jobs running at once
	// The kernel's limits on every job (cgroup v2 and rlimits): CPU in thousandths of a core (cpu.max), processes and
	// threads (pids.max and RLIMIT_NPROC), bytes in its /tmp (tmpfs size and RLIMIT_FSIZE), disk I/O per block
	// device (io.max, bytes and operations per second), and address space (RLIMIT_AS, always set: a runtime that
	// reserves a large address space up front, such as Node.js, the JVM or a race-instrumented binary, needs a large
	// value, never none).
	MaxCPUMillis int64 `json:"max_cpu_millis,omitempty"`
	MaxPids      int64 `json:"max_pids,omitempty"`
	MaxDiskBytes int64 `json:"max_disk_bytes,omitempty"`
	MaxIOBps     int64 `json:"max_io_bps,omitempty"`
	MaxIOPS      int64 `json:"max_io_iops,omitempty"`
	MaxVMBytes   int64 `json:"max_vm_bytes,omitempty"`
}

// DefaultWorkerCaps are the caps for every field the config leaves at zero.
var DefaultWorkerCaps = WorkerCaps{MaxTTLMS: 600000, MaxWallMS: 300000, MaxCPUMS: 300000, MaxMemBytes: 512 << 20,
	MaxOutputBytes: 1 << 20, MaxJobs: 1, MaxCPUMillis: 1000, MaxPids: 64, MaxDiskBytes: 64 << 20, MaxIOBps: 64 << 20,
	MaxIOPS: 1000, MaxVMBytes: 8 << 30}

// WithDefaults fills the zero fields of c from DefaultWorkerCaps.
func (c WorkerCaps) WithDefaults() WorkerCaps {
	d := DefaultWorkerCaps
	for _, f := range []struct{ v, def *int64 }{{&c.MaxTTLMS, &d.MaxTTLMS}, {&c.MaxWallMS, &d.MaxWallMS},
		{&c.MaxCPUMS, &d.MaxCPUMS}, {&c.MaxMemBytes, &d.MaxMemBytes}, {&c.MaxOutputBytes, &d.MaxOutputBytes},
		{&c.MaxCPUMillis, &d.MaxCPUMillis}, {&c.MaxPids, &d.MaxPids}, {&c.MaxDiskBytes, &d.MaxDiskBytes},
		{&c.MaxIOBps, &d.MaxIOBps}, {&c.MaxIOPS, &d.MaxIOPS}, {&c.MaxVMBytes, &d.MaxVMBytes}} {
		if *f.v <= 0 {
			*f.v = *f.def
		}
	}
	if c.MaxJobs <= 0 {
		c.MaxJobs = d.MaxJobs
	}
	return c
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
	// WHIPURL is where the camera publishes (with the pairing's publish key). The pair response carries it (Bot
	// mints one per device); when it is empty, video stays off until it is set here.
	WHIPURL string `json:"whip_url,omitempty"`
}

// Bundled lists the plugins that ship with the Node.
var Bundled = map[string]string{
	"dryrun":        "openvibe_dryrun",
	"adeept_adr036": "openvibe_adeept_adr036",
	"cozmo":         "openvibe_cozmo",
	"relay":         "openvibe_relay",
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
		if p.Name == "worker" { // status.capabilities.worker holds the job runtime classes
			return fmt.Errorf("config: plugin name %q is reserved", p.Name)
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
	if c.Video.WHIPURL != "" && !strings.HasPrefix(c.Video.WHIPURL, "https://") && !strings.HasPrefix(c.Video.WHIPURL, "http://") {
		return errors.New("config: video.whip_url must be an https:// URL")
	}
	for _, l := range []*float64{c.Limits.MaxSpeed, c.Limits.MaxTurn} {
		if l != nil && (*l < 0 || *l > 1) {
			return errors.New("config: limits must be between 0 and 1")
		}
	}
	return c.Worker.validate()
}

var (
	functionNameRe    = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)
	functionVersionRe = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.+-]{0,63}$`)
)

func (w WorkerConfig) validate() error {
	seen := map[string]bool{}
	for _, f := range w.Functions {
		if !functionNameRe.MatchString(f.Name) || !functionVersionRe.MatchString(f.Version) {
			return fmt.Errorf("config: worker function %q version %q: name or version is not a valid artifact name or exact version", f.Name, f.Version)
		}
		if c := f.EffectiveClass(); c != ClassFunction && c != ClassCode && c != ClassLinux {
			return fmt.Errorf("config: worker function %s@%s has class %q: only function, code or linux are implemented", f.Name, f.Version, f.Class)
		}
		key := f.Name + "@" + f.Version + "@" + f.EffectiveClass()
		if seen[key] {
			return fmt.Errorf("config: worker function %s@%s (class %s) listed twice", f.Name, f.Version, f.EffectiveClass())
		}
		seen[key] = true
		if len(f.Command) == 0 || !filepath.IsAbs(f.Command[0]) {
			return fmt.Errorf("config: worker function %s@%s needs a command starting with an absolute path", f.Name, f.Version)
		}
		if f.ArtifactDir != "" && (!filepath.IsAbs(f.ArtifactDir) || filepath.Clean(f.ArtifactDir) == "/") {
			return fmt.Errorf("config: worker function %s@%s: artifact_dir must be an absolute path other than /", f.Name, f.Version)
		}
	}
	switch w.Egress {
	case "", EgressNone, EgressPublic, EgressOpenVibeOnly:
	default:
		return fmt.Errorf("config: worker.egress %q is not none, public or openvibe-only", w.Egress)
	}
	for key, cidrs := range map[string][]string{"egress_allow": w.EgressAllow, "egress_deny": w.EgressDeny} {
		for _, c := range cidrs {
			if p, err := netip.ParsePrefix(c); err != nil || !p.Addr().Is4() || p != p.Masked() {
				return fmt.Errorf("config: worker.%s %q is not an IPv4 CIDR such as 203.0.113.0/24 (jobs have no IPv6)", key, c)
			}
		}
	}
	switch {
	case w.Egress == EgressOpenVibeOnly && len(w.EgressAllow) == 0:
		return errors.New("config: worker.egress openvibe-only needs worker.egress_allow, the CIDRs a job may reach")
	case w.Egress != EgressOpenVibeOnly && len(w.EgressAllow) > 0:
		return errors.New("config: worker.egress_allow applies to egress openvibe-only only")
	}
	if w.RunAs != nil && (w.RunAs.UID == 0 || w.RunAs.GID == 0) {
		return errors.New("config: worker.run_as must not be root (uid or gid 0)")
	}
	c := w.Caps
	if c.MaxTTLMS < 0 || c.MaxWallMS < 0 || c.MaxCPUMS < 0 || c.MaxMemBytes < 0 || c.MaxOutputBytes < 0 || c.MaxJobs < 0 ||
		c.MaxCPUMillis < 0 || c.MaxPids < 0 || c.MaxDiskBytes < 0 || c.MaxIOBps < 0 || c.MaxIOPS < 0 || c.MaxVMBytes < 0 {
		return errors.New("config: worker.caps must not be negative")
	}
	return w.Media.validate()
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
