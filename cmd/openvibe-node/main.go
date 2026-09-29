// Command openvibe-node connects a computer, server, Raspberry Pi or robot to OpenVibe.
//
//	openvibe-node pair <CODE>     redeem a one-time pairing code and store the device credential
//	openvibe-node run             run in the foreground (this is also what the service runs)
//	openvibe-node install         install and start the system service
//	openvibe-node uninstall       stop and remove the system service
//	openvibe-node status          show the link, the plugins, the stop latch and video
//	openvibe-node stop            local kill switch: stop every actuator and hold it stopped
//	openvibe-node resume          release the local kill switch (and a server e-stop)
//	openvibe-node plugins         list plugins and their capabilities
//	openvibe-node version
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/OpenVibers/OpenVibe.Node/internal/config"
	"github.com/OpenVibers/OpenVibe.Node/internal/credentials"
	"github.com/OpenVibers/OpenVibe.Node/internal/link"
	"github.com/OpenVibers/OpenVibe.Node/internal/localctl"
	"github.com/OpenVibers/OpenVibe.Node/internal/node"
	"github.com/OpenVibers/OpenVibe.Node/internal/plugins"
	"github.com/OpenVibers/OpenVibe.Node/internal/protocol"
	"github.com/OpenVibers/OpenVibe.Node/internal/safety"
	"github.com/OpenVibers/OpenVibe.Node/internal/service"
)

// version is set at build time: -ldflags "-X main.version=v0.1.0".
var version = "dev"

const usage = `openvibe-node %s: connect this device to OpenVibe.

Usage:
  openvibe-node pair <CODE> [--server URL] [--kind onboard|bridge] [--force]
  openvibe-node run [--dry-run]
  openvibe-node install [--user NAME]
  openvibe-node uninstall
  openvibe-node status [--json]
  openvibe-node stop        stop every actuator now and hold them stopped
  openvibe-node resume      release the stop
  openvibe-node plugins [--json]
  openvibe-node version

Global flags (before or after the command):
  --home DIR        keep config and state in DIR (also OPENVIBE_NODE_HOME)
  --log-level LVL   debug, info, warn or error (default info)
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

type globals struct {
	home     string
	logLevel string
	paths    config.Paths
	log      *slog.Logger
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprintf(stdout, usage, version)
		return 0
	}
	cmd, rest := args[0], args[1:]
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	g := &globals{}
	fs.StringVar(&g.home, "home", "", "config and state directory")
	fs.StringVar(&g.logLevel, "log-level", "info", "log level")
	var (
		server  = fs.String("server", "", "OpenVibe.Bot origin (pair)")
		kind    = fs.String("kind", "", "onboard or bridge (pair)")
		force   = fs.Bool("force", false, "pair again even if already paired")
		dryRun  = fs.Bool("dry-run", false, "run only the dry-run plugin and the test pattern (run)")
		asJSON  = fs.Bool("json", false, "print JSON (status, plugins)")
		svcUser = fs.String("user", "", "run the service as this account (install)")
	)
	// Accept flags anywhere: split positionals out.
	var positional []string
	for len(rest) > 0 {
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		rest = fs.Args()
		if len(rest) > 0 {
			positional = append(positional, rest[0])
			rest = rest[1:]
		}
	}
	g.paths = config.DefaultPaths(g.home)
	g.log = newLogger(stderr, g.logLevel)

	var err error
	switch cmd {
	case "pair":
		if len(positional) != 1 {
			fmt.Fprintln(stderr, "usage: openvibe-node pair <CODE>")
			return 2
		}
		err = cmdPair(g, positional[0], *server, *kind, *force, stdout)
	case "run":
		err = cmdRun(g, *dryRun)
	case "install":
		err = cmdInstall(g, *svcUser, stdout)
	case "uninstall":
		err = service.Control(serviceOptions(g, ""), "stop")
		if uerr := service.Control(serviceOptions(g, ""), "uninstall"); uerr != nil {
			err = uerr
		} else {
			err = nil
			fmt.Fprintln(stdout, "Service removed. Config and credential are kept in", g.paths.ConfigDir)
		}
	case "status":
		err = cmdStatus(g, *asJSON, stdout)
	case "stop":
		err = cmdStop(g, stdout)
	case "resume":
		err = cmdResume(g, stdout)
	case "plugins":
		err = cmdPlugins(g, *asJSON, stdout)
	case "version", "--version":
		fmt.Fprintf(stdout, "openvibe-node %s %s/%s\n", version, runtime.GOOS, runtime.GOARCH)
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n", cmd)
		fmt.Fprintf(stderr, usage, version)
		return 2
	}
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	return 0
}

func newLogger(w io.Writer, level string) *slog.Logger {
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil {
		l = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: l}))
}

func loadConfig(g *globals) (*config.Config, error) {
	return config.Load(g.paths.ConfigFile())
}

func pluginSpecs(cfg *config.Config, paths config.Paths) []plugins.Spec {
	var out []plugins.Spec
	for _, p := range cfg.Plugins {
		if p.Disabled {
			continue
		}
		var env []string
		for k, v := range p.Env {
			env = append(env, k+"="+v)
		}
		out = append(out, plugins.Spec{Name: p.Name, Argv: cfg.PluginCommand(p, paths), Env: env, Dir: p.Dir, Config: p.Config})
	}
	return out
}

// probeAll asks every plugin to describe itself without touching hardware.
func probeAll(cfg *config.Config, paths config.Paths, log *slog.Logger) (map[string]*plugins.Describe, []string) {
	out := map[string]*plugins.Describe{}
	var failed []string
	for _, s := range pluginSpecs(cfg, paths) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		d, err := plugins.Probe(ctx, s)
		cancel()
		if err != nil {
			log.Warn("plugin did not describe itself", "plugin", s.Name, "err", err)
			failed = append(failed, s.Name)
			continue
		}
		out[s.Name] = d
	}
	return out, failed
}

func cmdPair(g *globals, code, server, kind string, force bool, stdout io.Writer) error {
	code, err := link.NormalizeCode(code)
	if err != nil {
		return err
	}
	if err := g.paths.Ensure(); err != nil {
		return fmt.Errorf("cannot create %s (run with sudo, or use --home): %w", g.paths.ConfigDir, err)
	}
	if old, err := credentials.Load(g.paths.CredentialFile(), nil); err == nil && !force {
		return fmt.Errorf("already paired as %s; use --force to pair again (the old credential stops working once the owner removes it)", old.DeviceID)
	}
	cfg, err := loadConfig(g)
	if err != nil {
		return err
	}
	_, statErr := os.Stat(g.paths.ConfigFile())
	if server != "" {
		cfg.Server = server
	}
	if kind != "" {
		cfg.DeviceKind = kind
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	descs, _ := probeAll(cfg, g.paths, g.log)
	req := protocol.PairRequest{Code: code, AgentVersion: version, DeviceKind: cfg.DeviceKind, Drivers: []string{},
		Capabilities: map[string]any{}, OS: runtime.GOOS, Arch: runtime.GOARCH}
	req.Hostname, _ = os.Hostname()
	names := make([]string, 0, len(descs))
	for n := range descs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		req.Drivers = append(req.Drivers, descs[n].Driver)
		req.Capabilities[n] = descs[n].Capabilities
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	creds, err := link.Pair(ctx, nil, cfg.Server, req)
	if err != nil {
		return err
	}
	if err := credentials.Save(g.paths.CredentialFile(), creds); err != nil {
		return err
	}
	chownLikeDir(g.paths.CredentialFile(), g.paths.ConfigDir)
	if statErr != nil || server != "" || kind != "" {
		if err := config.Save(g.paths.ConfigFile(), cfg); err != nil {
			return err
		}
		chownLikeDir(g.paths.ConfigFile(), g.paths.ConfigDir)
	}
	fmt.Fprintf(stdout, "Paired as %s. Credential saved to %s (mode 600).\n", creds.DeviceID, g.paths.CredentialFile())
	fmt.Fprintln(stdout, "Confirm the device on openvibe.bot, then start it: `openvibe-node run` or `sudo openvibe-node install`.")
	if service.Status(serviceOptions(g, "")) == "running" {
		_ = service.Control(serviceOptions(g, ""), "restart")
		fmt.Fprintln(stdout, "The service was running and has been restarted with the new credential.")
	}
	return nil
}

func cmdRun(g *globals, dryRun bool) error {
	if err := g.paths.Ensure(); err != nil {
		return fmt.Errorf("cannot create %s (run with sudo, or use --home): %w", g.paths.StateDir, err)
	}
	cfg, err := loadConfig(g)
	if err != nil {
		return err
	}
	if dryRun {
		var keep []config.PluginConfig
		for _, p := range cfg.Plugins {
			if p.Name == "dryrun" {
				keep = append(keep, p)
			}
		}
		if len(keep) == 0 {
			keep = []config.PluginConfig{{Name: "dryrun"}}
		}
		cfg.Plugins = keep
		cfg.Video.Source = "test"
	}
	creds, err := credentials.Load(g.paths.CredentialFile(), func(s string) { g.log.Warn(s) })
	if err != nil && !errors.Is(err, credentials.ErrNotPaired) {
		return err
	}
	n, err := node.New(node.Options{Config: cfg, Paths: g.paths, Creds: creds, Log: g.log, Version: version})
	if err != nil {
		return err
	}
	g.log.Info("openvibe-node starting", "version", version, "config", g.paths.ConfigDir, "plugins", len(cfg.Plugins))
	return service.Run(serviceOptions(g, ""), n.Run)
}

func serviceOptions(g *globals, user string) service.Options {
	args := []string{"run"}
	if g.home != "" {
		abs, _ := filepath.Abs(g.home)
		args = append(args, "--home", abs)
	}
	if g.logLevel != "" && g.logLevel != "info" {
		args = append(args, "--log-level", g.logLevel)
	}
	return service.Options{Arguments: args, UserName: user}
}

func cmdInstall(g *globals, user string, stdout io.Writer) error {
	if err := g.paths.Ensure(); err != nil {
		return fmt.Errorf("cannot create %s (run with sudo): %w", g.paths.ConfigDir, err)
	}
	if user != "" {
		if err := chownTree(user, g.paths.ConfigDir, g.paths.StateDir); err != nil {
			return err
		}
	}
	opt := serviceOptions(g, user)
	if err := service.Control(opt, "install"); err != nil {
		return fmt.Errorf("install: %w (are you root?)", err)
	}
	if err := service.Control(opt, "start"); err != nil {
		return fmt.Errorf("installed, but could not start: %w", err)
	}
	fmt.Fprintln(stdout, "Installed and started the openvibe-node service.")
	if _, err := credentials.Load(g.paths.CredentialFile(), nil); err != nil {
		fmt.Fprintln(stdout, "Not paired yet: run `sudo openvibe-node pair <CODE>`.")
	}
	return nil
}

func cmdStop(g *globals, stdout io.Writer) error {
	_, err := localctl.Call(g.paths.SocketPath(), localctl.Request{Cmd: localctl.CmdStop}, 2*time.Second)
	if err == nil {
		fmt.Fprintln(stdout, "Stopped. Every actuator is held stopped until `openvibe-node resume`.")
		return nil
	}
	// No running Node answered: latch it in the file, which a running Node polls and a starting Node obeys.
	if ferr := safety.WriteLocalFile(g.paths.LatchFile(), true); ferr != nil {
		return fmt.Errorf("could not reach the node (%v) nor write %s (%v); run with sudo", err, g.paths.LatchFile(), ferr)
	}
	fmt.Fprintln(stdout, "The node did not answer; the stop is latched in", g.paths.LatchFile())
	fmt.Fprintln(stdout, "A running node obeys it within a quarter second; a starting node starts stopped. Release with `openvibe-node resume`.")
	return nil
}

func cmdResume(g *globals, stdout io.Writer) error {
	_, err := localctl.Call(g.paths.SocketPath(), localctl.Request{Cmd: localctl.CmdResume}, 2*time.Second)
	if err != nil {
		if ferr := safety.WriteLocalFile(g.paths.LatchFile(), false); ferr != nil {
			return fmt.Errorf("could not reach the node (%v) nor write %s (%v); run with sudo", err, g.paths.LatchFile(), ferr)
		}
	}
	fmt.Fprintln(stdout, "Resumed: the local stop and any server e-stop are released. Operators can drive again.")
	return nil
}

func cmdStatus(g *globals, asJSON bool, stdout io.Writer) error {
	resp, err := localctl.Call(g.paths.SocketPath(), localctl.Request{Cmd: localctl.CmdStatus}, 2*time.Second)
	if err != nil {
		return offlineStatus(g, asJSON, stdout)
	}
	if asJSON {
		_, err := stdout.Write(append(resp.Data, '\n'))
		return err
	}
	var s node.Status
	if err := json.Unmarshal(resp.Data, &s); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "OpenVibe Node %s, running for %s (service: %s)\n", s.Version, s.Uptime, service.Status(serviceOptions(g, "")))
	if s.Paired {
		fmt.Fprintf(stdout, "Device:   %s on %s\n", s.DeviceID, s.Server)
	} else {
		fmt.Fprintln(stdout, "Device:   not paired (openvibe-node pair <CODE>)")
	}
	link := "down"
	if s.Link.Connected {
		link = fmt.Sprintf("up (rtt %d ms)", s.Link.RTTMS)
	} else if s.Link.LastError != "" {
		link = "down: " + s.Link.LastError
	}
	fmt.Fprintf(stdout, "Link:     %s, %d reconnects\n", link, s.Link.Reconnects)
	fmt.Fprintf(stdout, "Stop:     %s\n", latchText(s.Latch))
	fmt.Fprintf(stdout, "Limits:   speed %.2f, turn %.2f, command deadline ≤ %d ms\n", s.Limits.MaxSpeed, s.Limits.MaxTurn, s.Limits.MaxCommandMS)
	fmt.Fprintf(stdout, "Video:    %s", s.Video.State)
	if s.Video.Source != "" {
		fmt.Fprintf(stdout, " from %s, %d frames", s.Video.Source, s.Video.Frames)
	}
	if s.Video.LastError != "" && s.Video.State != "live" {
		fmt.Fprintf(stdout, " (%s)", s.Video.LastError)
	}
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "Plugins:")
	for _, p := range s.Plugins {
		fmt.Fprintf(stdout, "  %-15s %-9s %s %s", p.Name, p.State, p.Driver, p.Version)
		if p.Restarts > 0 {
			fmt.Fprintf(stdout, ", %d restarts", p.Restarts)
		}
		if p.Fault != nil {
			fmt.Fprintf(stdout, ", fault %s: %s", p.Fault.Code, p.Fault.Message)
		}
		fmt.Fprintln(stdout)
	}
	if b := s.Telemetry.Battery; b != nil && b.Percent != nil {
		fmt.Fprintf(stdout, "Battery:  %.0f%%\n", *b.Percent)
	}
	return nil
}

func latchText(l safety.LatchState) string {
	switch {
	case l.Local && l.Remote:
		return "STOPPED locally and by the server (" + l.RemoteReason + "); `openvibe-node resume` releases both"
	case l.Local:
		return "STOPPED locally; `openvibe-node resume` releases it"
	case l.Remote:
		return "E-STOPPED by the server (" + l.RemoteReason + "); the owner clears it on openvibe.bot"
	}
	return "clear"
}

func offlineStatus(g *globals, asJSON bool, stdout io.Writer) error {
	type offline struct {
		Running  bool              `json:"running"`
		Service  string            `json:"service"`
		Paired   bool              `json:"paired"`
		DeviceID string            `json:"device_id,omitempty"`
		Server   string            `json:"server,omitempty"`
		Latch    safety.LatchState `json:"latch"`
		Config   string            `json:"config_dir"`
	}
	o := offline{Service: service.Status(serviceOptions(g, "")), Config: g.paths.ConfigDir}
	if c, err := credentials.Load(g.paths.CredentialFile(), nil); err == nil {
		o.Paired, o.DeviceID, o.Server = true, c.DeviceID, c.Server
	}
	if l, _ := safety.OpenLatch(g.paths.LatchFile()); l != nil {
		o.Latch = l.State()
	}
	if asJSON {
		return json.NewEncoder(stdout).Encode(o)
	}
	fmt.Fprintf(stdout, "OpenVibe Node %s is not running (service: %s).\n", version, o.Service)
	if o.Paired {
		fmt.Fprintf(stdout, "Device:   %s on %s\n", o.DeviceID, o.Server)
	} else {
		fmt.Fprintln(stdout, "Device:   not paired (openvibe-node pair <CODE>)")
	}
	fmt.Fprintf(stdout, "Stop:     %s\n", latchText(o.Latch))
	fmt.Fprintf(stdout, "Config:   %s\n", o.Config)
	return nil
}

func cmdPlugins(g *globals, asJSON bool, stdout io.Writer) error {
	type entry struct {
		Name         string         `json:"name"`
		State        string         `json:"state"`
		Driver       string         `json:"driver,omitempty"`
		Version      string         `json:"version,omitempty"`
		Capabilities map[string]any `json:"capabilities,omitempty"`
	}
	var list []entry
	if resp, err := localctl.Call(g.paths.SocketPath(), localctl.Request{Cmd: localctl.CmdPlugins}, 2*time.Second); err == nil {
		var s node.Status
		if err := json.Unmarshal(resp.Data, &s); err != nil {
			return err
		}
		for _, p := range s.Plugins {
			list = append(list, entry{p.Name, p.State, p.Driver, p.Version, p.Capabilities})
		}
	} else {
		cfg, err := loadConfig(g)
		if err != nil {
			return err
		}
		descs, _ := probeAll(cfg, g.paths, g.log)
		for _, s := range pluginSpecs(cfg, g.paths) {
			e := entry{Name: s.Name, State: "not running"}
			if d := descs[s.Name]; d != nil {
				e.Driver, e.Version, e.Capabilities = d.Driver, d.Version, d.Capabilities
			} else {
				e.State = "not installed or broken (" + strings.Join(s.Argv, " ") + ")"
			}
			list = append(list, e)
		}
	}
	if asJSON {
		return json.NewEncoder(stdout).Encode(list)
	}
	for _, e := range list {
		fmt.Fprintf(stdout, "%s  [%s]  %s %s\n", e.Name, e.State, e.Driver, e.Version)
		keys := make([]string, 0, len(e.Capabilities))
		for k := range e.Capabilities {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			b, _ := json.Marshal(e.Capabilities[k])
			fmt.Fprintf(stdout, "    %-9s %s\n", k, b)
		}
	}
	if len(list) == 0 {
		fmt.Fprintln(stdout, "No plugins configured in", g.paths.ConfigFile())
	}
	return nil
}
