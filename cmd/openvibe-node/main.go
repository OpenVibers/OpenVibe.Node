// Command openvibe-node connects a computer, server, Raspberry Pi or robot to OpenVibe.
//
//	openvibe-node pair <CODE>     redeem a one-time pairing code and store the device credential
//	                              (--network/--pairing: pair on OpenVibe.Network and bind the device on Bot)
//	openvibe-node credential import  take a rotated credential: reads the rotate response JSON on stdin
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
  openvibe-node pair <CODE> [--robot rob_…] [--name NAME] [--server URL] [--kind onboard|bridge] [--force]
  openvibe-node pair --network URL --pairing pair_… --code CODE [--name NAME] [--server URL] [--force]
  openvibe-node credential import   store the credential from a rotate response, read as JSON on stdin
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
	return runWithStdin(args, os.Stdin, stdout, stderr)
}

// runWithStdin is run with the command's stdin, so tests can feed `credential import`.
func runWithStdin(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
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
		robot   = fs.String("robot", "", "the rob_… id of the robot to pair with, from the installer command (pair)")
		name    = fs.String("name", "", "a name for this device on openvibe.bot; default: the hostname (pair)")
		codeArg = fs.String("code", "", "the pairing code, instead of the argument (pair)")
		network = fs.String("network", "", "OpenVibe.Network origin: pair this machine there (pair)")
		pairing = fs.String("pairing", "", "the pair_… id from the installer command (pair, with --network)")
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
		if *codeArg != "" {
			positional = append(positional, *codeArg)
		}
		p := pairArgs{robot: *robot, name: *name, server: *server, kind: *kind, force: *force, network: *network, pairing: *pairing}
		// --pairing with no code is the code itself (`--pairing ABCD-1234`); a pair_… id needs --code beside it.
		if p.pairing != "" && len(positional) == 0 && !strings.HasPrefix(p.pairing, "pair_") {
			positional, p.pairing = []string{p.pairing}, ""
		}
		if len(positional) != 1 {
			fmt.Fprintln(stderr, "usage: openvibe-node pair <CODE> [--robot rob_…] [--name NAME]\n       openvibe-node pair --network URL --pairing pair_… --code CODE [--name NAME]")
			return 2
		}
		p.code = positional[0]
		if p.network != "" || p.pairing != "" {
			err = cmdPairNetwork(g, p, stdout)
		} else {
			err = cmdPair(g, p, stdout)
		}
	case "credential":
		if len(positional) != 1 || positional[0] != "import" {
			fmt.Fprintln(stderr, "usage: openvibe-node credential import  (reads the rotate response JSON on stdin)")
			return 2
		}
		err = cmdCredentialImport(g, stdin, stdout)
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
		if errors.Is(err, link.ErrNodeRevoked) {
			return service.ExitPairAgain
		}
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

// pairArgs are the flags of `openvibe-node pair`.
type pairArgs struct {
	code, robot, name, server, kind string
	network, pairing                string
	force                           bool
}

// maxNameLen is the longest device name OpenVibe.Bot stores.
const maxNameLen = 80

// deviceName is --name, else the hostname, cut to maxNameLen.
func deviceName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		name, _ = os.Hostname()
	}
	if r := []rune(name); len(r) > maxNameLen {
		name = string(r[:maxNameLen])
	}
	return name
}

// preparePair checks that this Node may pair (not paired yet, or --force) and returns the config with --server and
// --kind applied, and whether config.json was missing (then pairing writes it).
func preparePair(g *globals, a pairArgs) (*config.Config, bool, error) {
	if err := g.paths.Ensure(); err != nil {
		return nil, false, fmt.Errorf("cannot create %s (run with sudo, or use --home): %w", g.paths.ConfigDir, err)
	}
	if old, err := credentials.Load(g.paths.CredentialFile(), nil); err == nil && !a.force {
		return nil, false, fmt.Errorf("already paired as %s; use --force to pair again (the old credential stops working once the owner removes it)", old.Label())
	}
	cfg, err := loadConfig(g)
	if err != nil {
		return nil, false, err
	}
	_, statErr := os.Stat(g.paths.ConfigFile())
	if a.server != "" {
		cfg.Server = a.server
	}
	if a.kind != "" {
		cfg.DeviceKind = a.kind
	}
	if err := cfg.Validate(); err != nil {
		return nil, false, err
	}
	return cfg, statErr != nil, nil
}

// finishPair writes config.json when it was missing or --server/--kind changed it, and restarts a running service
// so it picks up the new credential.
func finishPair(g *globals, a pairArgs, cfg *config.Config, noConfig bool, stdout io.Writer) error {
	if noConfig || a.server != "" || a.kind != "" {
		if err := config.Save(g.paths.ConfigFile(), cfg); err != nil {
			return err
		}
		chownLikeDir(g.paths.ConfigFile(), g.paths.ConfigDir)
	}
	if service.Status(serviceOptions(g, "")) == "running" {
		_ = service.Control(serviceOptions(g, ""), "restart")
		fmt.Fprintln(stdout, "The service was running and has been restarted with the new credential.")
	}
	return nil
}

func cmdPair(g *globals, a pairArgs, stdout io.Writer) error {
	code, err := link.NormalizeCode(a.code)
	if err != nil {
		return err
	}
	if a.robot != "" && !link.RobotRe.MatchString(a.robot) {
		return fmt.Errorf("--robot must be a robot id like rob_01J8Z4M2Q0R7T9YV3K6N8P1W2X (a driver kind such as %q goes in the config's plugins)", a.robot)
	}
	name := deviceName(a.name)
	cfg, noConfig, err := preparePair(g, a)
	if err != nil {
		return err
	}
	descs, _ := probeAll(cfg, g.paths, g.log)
	req := protocol.PairRequest{Robot: a.robot, Code: code, AgentVersion: version, DeviceKind: cfg.DeviceKind, Drivers: []string{},
		Capabilities: map[string]any{}, Name: name}
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
	fmt.Fprintf(stdout, "Paired as %s (robot %s). Credential saved to %s (mode 600).\n", creds.DeviceID, creds.RobotID, g.paths.CredentialFile())
	fmt.Fprintln(stdout, "Confirm the device on openvibe.bot, then start it: `openvibe-node run` or `sudo openvibe-node install`.")
	return finishPair(g, a, cfg, noConfig, stdout)
}

// cmdPairNetwork pairs through OpenVibe.Network: it redeems the code at <network>/api/v1/node-pairing, stores the
// node principal and credential, then binds the device on Bot (POST /api/v1/devices/bind with a node token). A
// failed bind keeps the credential: `openvibe-node run` binds on start.
func cmdPairNetwork(g *globals, a pairArgs, stdout io.Writer) error {
	code, err := link.NormalizeCode(a.code)
	if err != nil {
		return err
	}
	if a.pairing != "" && !link.PairingIDRe.MatchString(a.pairing) {
		return errors.New("--pairing must be the pair_… id from the installer command on openvibe.bot")
	}
	network := strings.TrimSuffix(strings.TrimSpace(a.network), "/")
	if network == "" {
		return errors.New("--pairing needs --network, the OpenVibe.Network URL from the installer command")
	}
	cfg, noConfig, err := preparePair(g, a)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ua := "openvibe-node/" + version
	creds, err := link.PairNetwork(ctx, nil, network, protocol.NodePairingRequest{Code: code, Pairing: a.pairing, Name: deviceName(a.name)}, ua)
	if err != nil {
		return err
	}
	creds.Server = strings.TrimSuffix(cfg.Server, "/")
	if err := saveCredentials(g, creds); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Paired on %s as %s. Credential saved to %s (mode 600).\n", network, creds.Principal, g.paths.CredentialFile())
	if bound, err := link.Bind(ctx, nil, creds, link.NewTokenSource(creds, nil, ua), ua); err != nil {
		if errors.Is(err, link.ErrNodeRevoked) {
			return err
		}
		fmt.Fprintf(stdout, "Could not bind the device on %s yet (%v); the node binds it when it starts.\n", creds.Server, err)
	} else {
		if err := saveCredentials(g, bound); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Bound as %s (robot %s).\n", bound.DeviceID, bound.RobotID)
	}
	fmt.Fprintln(stdout, "Start it: `openvibe-node run` or `sudo openvibe-node install`.")
	return finishPair(g, a, cfg, noConfig, stdout)
}

func saveCredentials(g *globals, c *credentials.Credentials) error {
	if err := credentials.Save(g.paths.CredentialFile(), c); err != nil {
		return err
	}
	chownLikeDir(g.paths.CredentialFile(), g.paths.ConfigDir)
	return nil
}

// bindOnStart binds a Network-paired machine whose device record is missing (the bind at pairing failed) and saves
// the answer. A refused node credential (ErrNodeRevoked) is returned; any other failure is logged and the Node
// starts without a device record (no video); the next start binds again.
func bindOnStart(g *globals, creds *credentials.Credentials, tokens *link.TokenSource, server string) (*credentials.Credentials, error) {
	if !creds.NetworkPaired() || creds.Bound() {
		return creds, nil
	}
	if creds.Server == "" {
		creds.Server = server
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	bound, err := link.Bind(ctx, nil, creds, tokens, "openvibe-node/"+version)
	if errors.Is(err, link.ErrNodeRevoked) {
		return nil, err
	}
	if err != nil {
		g.log.Warn("could not bind the device on Bot; starting without video until the next start", "err", err)
		return creds, nil
	}
	if err := saveCredentials(g, bound); err != nil {
		return nil, err
	}
	g.log.Info("bound the device on Bot", "device", bound.DeviceID, "robot", bound.RobotID)
	return bound, nil
}

// maxRotateJSON caps what `credential import` reads from stdin: Bot's rotate answer is a few hundred bytes.
const maxRotateJSON = 64 << 10

// cmdCredentialImport replaces the stored credential and publish key with the pair from a credential rotation.
// It reads OpenVibe.Bot's `POST /api/v1/devices/:id/rotate` response from stdin and keeps every other stored field,
// so a rotated Node does not have to pair again. Neither secret is ever printed.
func cmdCredentialImport(g *globals, stdin io.Reader, stdout io.Writer) error {
	// The rotate answer is small; refuse a huge stream instead of buffering it.
	b, err := io.ReadAll(io.LimitReader(stdin, maxRotateJSON+1))
	if err != nil {
		return fmt.Errorf("reading the rotate response from stdin: %w", err)
	}
	if len(b) > maxRotateJSON {
		return fmt.Errorf("the rotate response is over %d KiB; pipe just the POST /api/v1/devices/:id/rotate body", maxRotateJSON>>10)
	}
	// The plain strings below live only in this function; nothing formats them.
	var resp struct {
		Device struct {
			ID string `json:"id"`
		} `json:"device"`

		Credential string `json:"credential"`
		PublishKey string `json:"publish_key"`
	}
	// Never wrap the decoder error: it can quote the response body, secrets and all.
	if err := json.Unmarshal(b, &resp); err != nil {
		return errors.New("stdin is not the JSON that POST /api/v1/devices/:id/rotate answered with")
	}
	if resp.Credential == "" || resp.PublishKey == "" {
		return errors.New("the rotate response has no credential or publish_key; pipe the whole response body")
	}
	old, err := credentials.Load(g.paths.CredentialFile(), nil)
	if errors.Is(err, credentials.ErrNotPaired) {
		return errors.New("this Node is not paired yet; run `openvibe-node pair <CODE>` first")
	}
	if err != nil {
		return err
	}
	if old.NetworkPaired() {
		return errors.New("this Node is paired through OpenVibe.Network: it has no Bot credential to import")
	}
	if resp.Device.ID != old.DeviceID {
		return fmt.Errorf("this rotation is for device %s, this Node is paired as %s", resp.Device.ID, old.DeviceID)
	}
	next := *old
	next.Credential = credentials.NewSecret(resp.Credential)
	next.PublishKey = credentials.NewSecret(resp.PublishKey)
	if err := credentials.Save(g.paths.CredentialFile(), &next); err != nil {
		return err
	}
	chownLikeDir(g.paths.CredentialFile(), g.paths.ConfigDir)
	fmt.Fprintf(stdout, "Imported the rotated credential for %s. The old credential stops working after 60 s; restart the node (or it reconnects on its own).\n", old.DeviceID)
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
	var tokens link.Tokens
	if creds.NetworkPaired() {
		ts := link.NewTokenSource(creds, nil, "openvibe-node/"+version)
		if creds, err = bindOnStart(g, creds, ts, cfg.Server); err != nil {
			return err
		}
		tokens = ts
	}
	n, err := node.New(node.Options{Config: cfg, Paths: g.paths, Creds: creds, Tokens: tokens, Log: g.log, Version: version})
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
	chownLikeDir(g.paths.LatchFile(), g.paths.StateDir) // the service account must be able to read it
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
		chownLikeDir(g.paths.LatchFile(), g.paths.StateDir)
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
		fmt.Fprintf(stdout, "Device:   %s\n", deviceLine(s.DeviceID, s.Principal, s.Server))
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
	if len(s.Jobs) > 0 {
		fmt.Fprintln(stdout, "Jobs:")
		for _, j := range s.Jobs {
			fmt.Fprintf(stdout, "  %s %s %s\n", j.ID, j.Function, j.State)
		}
	}
	if b := s.Telemetry.Battery; b != nil {
		fmt.Fprintf(stdout, "Battery:  %.0f%%\n", *b*100)
	}
	return nil
}

// deviceLine is "dev_… on <server>", with the node principal of a Network-paired machine ("not bound yet" before
// Bot has bound it).
func deviceLine(device, principal, server string) string {
	switch {
	case principal == "":
		return device + " on " + server
	case device == "":
		return principal + " (not bound yet) on " + server
	}
	return device + " (" + principal + ") on " + server
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
		Running   bool              `json:"running"`
		Service   string            `json:"service"`
		Paired    bool              `json:"paired"`
		DeviceID  string            `json:"device_id,omitempty"`
		Principal string            `json:"principal,omitempty"`
		Server    string            `json:"server,omitempty"`
		Latch     safety.LatchState `json:"latch"`
		Config    string            `json:"config_dir"`
	}
	o := offline{Service: service.Status(serviceOptions(g, "")), Config: g.paths.ConfigDir}
	if c, err := credentials.Load(g.paths.CredentialFile(), nil); err == nil {
		o.Paired, o.DeviceID, o.Principal, o.Server = true, c.DeviceID, c.Principal, c.Server
	}
	if l, _ := safety.OpenLatch(g.paths.LatchFile()); l != nil {
		o.Latch = l.State()
	}
	if asJSON {
		return json.NewEncoder(stdout).Encode(o)
	}
	fmt.Fprintf(stdout, "OpenVibe Node %s is not running (service: %s).\n", version, o.Service)
	if o.Paired {
		fmt.Fprintf(stdout, "Device:   %s\n", deviceLine(o.DeviceID, o.Principal, o.Server))
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
