// Package node ties the Node together: the control link, the plugins, the safety rules, video and the local control
// socket. Every path that should stop the motors ends in Manager.StopAll or EstopAll:
//
//	link lost (disconnect, deadman)       → StopAll
//	server estop                          → latch (persisted) → EstopAll
//	`openvibe-node stop` (socket or file) → latch (persisted) → EstopAll
//	core shutdown                         → StopAll, then each plugin's stdin is closed (plugins stop on EOF)
//	plugin crash                          → restarted stopped, estop re-sent if latched
//	core dies without cleanup             → plugins stop on EOF or after 1 s without a heartbeat
package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/OpenVibers/OpenVibe.Node/internal/config"
	"github.com/OpenVibers/OpenVibe.Node/internal/credentials"
	"github.com/OpenVibers/OpenVibe.Node/internal/link"
	"github.com/OpenVibers/OpenVibe.Node/internal/localctl"
	"github.com/OpenVibers/OpenVibe.Node/internal/mono"
	"github.com/OpenVibers/OpenVibe.Node/internal/plugins"
	"github.com/OpenVibers/OpenVibe.Node/internal/protocol"
	"github.com/OpenVibers/OpenVibe.Node/internal/safety"
	"github.com/OpenVibers/OpenVibe.Node/internal/video"
	"github.com/OpenVibers/OpenVibe.Node/internal/worker"
)

// Options configure a Node.
type Options struct {
	Config  *config.Config
	Paths   config.Paths
	Creds   *credentials.Credentials
	Log     *slog.Logger
	Version string

	// Tokens are a Network-paired machine's node tokens; nil builds them from Creds.
	Tokens link.Tokens

	// CanQuit lets `openvibe-node quit` end the process: true when it runs from a terminal or a sign-in start, false
	// under a service manager, which would only start it again.
	CanQuit bool

	// For tests.
	ReauthInterval    time.Duration
	HeartbeatInterval time.Duration
	LinkBackoffMin    time.Duration
	VideoLoopback     bool
	LatchPoll         time.Duration
}

// Node is a running device agent.
type Node struct {
	opt   Options
	log   *slog.Logger
	latch *safety.Latch
	dedup *safety.Dedup
	jobs  *jobs
	// worker runs function jobs; nil unless config.worker is enabled on Linux and its boot-time probe passed.
	worker *worker.Worker
	mgr    *plugins.Manager
	link   *link.Link
	pub    *video.Publisher
	// screen is the owner's live view of the machine's screen (screen.go), apart from a robot's camera.
	screen screenState

	ctx context.Context
	// quit ends Run on `openvibe-node quit` (control), and quitting tells Run to answer ErrQuit.
	quit     context.CancelFunc
	quitting atomic.Bool

	mu     sync.Mutex
	limits safety.Limits
	// haltEpochs: per robot, the newest turn_epoch a halt carried (noteHaltEpoch); older commands are refused.
	haltEpochs map[string]int64
	allowed    map[string]bool
	telemetry  map[string]map[string]any // per plugin, latest
	events     []protocol.Event
	// Vibe by voice (contracts 0.214.0): the owner's setting from Bot's config, and which plugin asked each voice_ask
	// still waiting for its answer.
	voiceOn      bool
	callWord     string
	voiceAskedBy map[string]string
	telemMS      int
	jpeg         *video.JPEGSource
	jpegFrom     string
	videoSrc     string
	lastTelem    time.Time
	started      time.Time

	// configured is set by the first config on a connection; until then only halt runs, so a latch the owner set
	// while the link was down is applied before any motion.
	configured bool

	robotIDs []string // the robots this device serves, from the connection's hello
	session  string   // the connection's session id, from its hello: each command's provenance

	// approvals waiting on the machine (the computer plugin at level "ask"), by command id: what
	// `openvibe-node approvals` lists and `approve`/`deny` answer.
	approvals map[string]Approval
	// input commands in flight, by id, and when the last one finished: `approve` is refused while the agent drives
	// the mouse or keyboard and for InputGuard after, so an approved click cannot press another request's Approve.
	inputs     map[string]bool
	inputEnded time.Time
	trayToldAt time.Time // when the computer plugin was last told the tray shows its approvals
}

// ErrQuit is what Run returns after `openvibe-node quit`: every actuator stopped, nothing latched, the process may exit.
var ErrQuit = errors.New("quit by its owner")

// InputGuard is how long after an input command `approve` stays refused.
const InputGuard = 2 * time.Second

// trayTTL is how long the computer plugin trusts one "the tray shows your approvals"; the tray says it again
// every poll and the core passes it on at most every trayRetell.
const (
	trayTTL    = 15
	trayRetell = 5 * time.Second
)

// Approval is one computer command waiting for its owner's answer on the machine.
type Approval struct {
	ID       string `json:"id"`
	Plugin   string `json:"plugin"`
	Kind     string `json:"kind,omitempty"`
	Summary  string `json:"summary,omitempty"`
	At       int64  `json:"at"`
	ExpiresS int64  `json:"expires_s,omitempty"`
}

// New builds a Node. Run starts it.
func New(opt Options) (*Node, error) {
	if opt.Log == nil {
		opt.Log = slog.Default()
	}
	if opt.LatchPoll == 0 {
		opt.LatchPoll = 250 * time.Millisecond
	}
	latch, err := safety.OpenLatch(opt.Paths.LatchFile())
	if err != nil {
		opt.Log.Error("latch", "err", err)
	}
	n := &Node{opt: opt, log: opt.Log, latch: latch, dedup: safety.NewDedup(4096, 10*time.Minute),
		jobs: newJobs(nil), telemetry: map[string]map[string]any{}, telemMS: 500, approvals: map[string]Approval{}, inputs: map[string]bool{}}
	n.limits = safety.Merge(protocol.Limits{}, opt.Config.Limits.MaxSpeed, opt.Config.Limits.MaxTurn, opt.Config.Limits.MaxCommandMS)

	var specs []plugins.Spec
	for _, p := range opt.Config.Plugins {
		if p.Disabled {
			continue
		}
		var env []string
		for k, v := range p.Env {
			env = append(env, k+"="+v)
		}
		sort.Strings(env)
		env = append(env, "OPENVIBE_NODE_STATE="+opt.Paths.StateDir)
		cfg := map[string]any{}
		for k, v := range p.Config {
			cfg[k] = v
		}
		if _, ok := cfg["frame_dir"]; !ok {
			cfg["frame_dir"] = opt.Paths.FramesDir() + string(os.PathSeparator) + p.Name
		}
		specs = append(specs, plugins.Spec{Name: p.Name, Argv: opt.Config.PluginCommand(p, opt.Paths), Env: env, Dir: p.Dir, Config: cfg, StrictTiming: p.StrictTiming})
	}
	n.mgr = plugins.NewManager(specs, opt.Log)

	if opt.Creds != nil {
		u := opt.Creds.DeviceURL
		if u == "" {
			server := opt.Creds.Server
			if server == "" {
				server = opt.Config.Server
			}
			if u, err = link.DeviceURL(server); err != nil {
				return nil, err
			}
		}
		lo := link.Options{URL: u, Credential: opt.Creds.Credential, UserAgent: "openvibe-node/" + opt.Version,
			Log: opt.Log, HeartbeatInterval: opt.HeartbeatInterval, BackoffMin: opt.LinkBackoffMin, ReauthInterval: opt.ReauthInterval}
		if opt.Creds.NetworkPaired() {
			// A Network-paired machine authenticates with node tokens, never a Bot credential.
			lo.Credential = credentials.Secret{}
			if opt.Tokens != nil {
				lo.Tokens = opt.Tokens
			} else {
				lo.Tokens = link.NewTokenSource(opt.Creds, nil, lo.UserAgent)
			}
		}
		n.link = link.New(lo, n)
		// The WHIP endpoint is the pairing's whip_url (built by Bot from the publish key) unless the config sets one.
		whip := opt.Config.Video.WHIPURL
		if whip == "" {
			whip = opt.Creds.WHIPURL
		}
		if whip == "" && opt.Config.Video.Source != "off" {
			opt.Log.Info("video off: no video.whip_url in the config")
		}
		if whip != "" && opt.Config.Video.Source != "off" {
			n.pub = video.NewPublisher(video.Options{WHIPURL: whip, PublishKey: opt.Creds.PublishKey,
				ICEServers: opt.Creds.ICEServers, Log: opt.Log, IncludeLoopback: opt.VideoLoopback})
		}
	}
	if opt.Config.Worker.Enabled {
		wcfg := opt.Config.Worker
		// No job may see the Node's config, credential, state or control socket (they all lie in these).
		wcfg.NodeDirs = []string{opt.Paths.ConfigDir, opt.Paths.StateDir}
		w := worker.New(wcfg, n.sendJobFrame, opt.Log)
		if err := w.Probe(); err != nil {
			opt.Log.Error("worker off: jobs cannot run isolated here, every job is refused", "err", err)
		} else {
			n.worker = w
			w.SetStopped(stopFault(n.latch.State()))
			n.jobs.admit = w.Admit
			// Advertise a class only when an entry implements it, and only here, after the sandbox probe passed.
			n.jobs.classes = w.Classes()
			opt.Log.Info("worker on", "functions", len(opt.Config.Worker.Functions))
		}
	}
	n.latch.OnChange(n.onLatch)
	return n, nil
}

// Run runs until ctx ends, then stops every actuator and waits for the plugins to exit. It ends on its own, with
// link.ErrNodeRevoked, when OpenVibe.Network refuses the machine's node credential.
func (n *Node) Run(parent context.Context) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	var fatal error
	n.ctx = ctx
	n.quit = cancel
	n.started = time.Now()
	ctlErr := make(chan error, 1)
	go func() { ctlErr <- localctl.Serve(ctx, n.opt.Paths.SocketPath(), n.control) }()
	select {
	case err := <-ctlErr:
		if err != nil {
			return err
		}
	case <-time.After(50 * time.Millisecond):
	}

	pctx, cancelPlugins := context.WithCancel(context.Background())
	// A stop latched on disk is set on every plugin before any starts: each process's first lines are then hello and
	// the e-stop replayed (cause latched, first_cause what set it), before it can be routed any command.
	if st := n.latch.State(); st.Stopped() {
		n.log.Warn("starting with the stop latched", "remote", st.Remote, "local", st.Local)
		first := plugins.EstopLocal
		if st.Remote {
			first = plugins.EstopRemote
		}
		n.mgr.EstopAll(first, latchReason(st))
	}
	n.mgr.Start(pctx)

	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); n.pump(ctx) }()
	go func() { defer wg.Done(); n.pollLatch(ctx) }()
	go func() { defer wg.Done(); n.startVideo(ctx) }()
	if n.link != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := n.link.Run(ctx); err != nil {
				fatal = err
				cancel()
			}
		}()
	} else {
		n.log.Warn("not paired: running local only (run `openvibe-node pair <CODE>`)")
	}
	<-ctx.Done()
	n.log.Info("shutting down: stopping every actuator")
	n.mgr.StopAll(plugins.StopShutdown)
	if n.worker != nil {
		n.worker.Close()
	}
	cancelPlugins()
	n.mgr.Wait()
	wg.Wait()
	if fatal == nil && n.quitting.Load() {
		return ErrQuit
	}
	return fatal
}

// ---- link.Handler ----

// Connected sends nothing: Bot sends config right after hello, and the first status and estop_state go out once it
// has been applied (applyConfig), so they report the latch the owner may have set while the link was down.
func (n *Node) Connected() {
	n.mu.Lock()
	n.configured, n.robotIDs, n.session = false, nil, ""
	n.mu.Unlock()
}

func (n *Node) Disconnected(err error) {
	n.log.Warn("link lost: stopping every actuator", "err", err)
	n.mgr.StopAll(plugins.StopLinkLost)
	n.mu.Lock()
	n.configured = false
	n.mu.Unlock()
}

func (n *Node) Frame(f protocol.Frame) {
	switch m := f.Msg.(type) {
	case protocol.Hello:
		n.mu.Lock()
		n.robotIDs, n.session = m.RobotIDs, m.SessionID
		n.mu.Unlock()
		n.log.Info("server hello", "device", m.DeviceID, "robots", m.RobotIDs, "session", m.SessionID)
	case protocol.Config:
		n.applyConfig(m)
	case protocol.Command:
		n.command(m, f.TS, f.Seq, mono.Now())
	case protocol.Estop:
		// latched:false is the owner's clear. It clears only the remote latch: a local (CLI or file) stop stays.
		if m.Latched {
			n.remoteEstop("by "+m.By, m.By)
		} else {
			n.remoteClear(m.By)
		}
		n.sendEstopState()
	case protocol.JobRequest:
		n.job(m)
	case protocol.JobCancel:
		n.jobCancel(m)
	case protocol.VoiceAnswer:
		n.voiceAnswer(m)
	case protocol.JobExitAck:
		if n.worker != nil {
			n.worker.ExitAck(m.ID)
		} else {
			n.log.Debug("job_exit_ack ignored: no worker", "id", m.ID)
		}
	}
}

func (n *Node) remoteEstop(reason, by string) {
	n.log.Warn("e-stop from the server", "by", by)
	n.mgr.EstopAll(plugins.EstopRemote, safety.CleanReason(reason)) // at once, before the disk write
	if _, err := n.latch.SetRemote(reason); err != nil {
		n.log.Error("could not persist the e-stop (it is held in memory)", "err", err)
	}
}

func (n *Node) remoteClear(by string) {
	n.log.Warn("e-stop cleared by the server", "by", by)
	if _, err := n.latch.ClearRemote(); err != nil {
		n.log.Error("could not persist the e-stop clear", "err", err)
	}
}

func (n *Node) applyConfig(c protocol.Config) {
	// The server's estop_latched is the owner's latch: it may have been set or cleared while the link was down.
	if st := n.latch.State(); c.EstopLatched && !st.Remote {
		n.remoteEstop("latched on the server", "server")
	} else if !c.EstopLatched && st.Remote {
		n.remoteClear("server")
	}
	l := safety.Merge(c.Limits, n.opt.Config.Limits.MaxSpeed, n.opt.Config.Limits.MaxTurn, n.opt.Config.Limits.MaxCommandMS)
	n.mu.Lock()
	n.limits = l
	n.allowed = nil
	if c.AllowedCommands != nil {
		n.allowed = map[string]bool{protocol.KindHalt: true}
		for _, k := range c.AllowedCommands {
			n.allowed[k] = true
		}
	}
	first := !n.configured
	n.configured = true
	n.mu.Unlock()
	if first {
		n.sendStatus()
		if n.worker != nil {
			n.worker.Resend()
		}
	}
	n.sendEstopState()
	if n.link != nil {
		hb := c.HeartbeatMS
		if hb <= 0 {
			hb = c.Limits.HeartbeatMS
		}
		n.link.SetTiming(hb)
	}
	n.log.Info("config", "max_speed", l.MaxSpeed, "max_turn", l.MaxTurn, "max_command_ms", l.MaxCommandMS,
		"allowed", c.AllowedCommands, "estop_latched", c.EstopLatched)
	n.mu.Lock()
	n.voiceOn, n.callWord = c.Voice, c.CallWord
	n.mu.Unlock()
	n.tellVoice(nil)
}

// tellVoice gives the voice plugins (or just p) the owner's voice setting: they listen for the call word only while on.
func (n *Node) tellVoice(only *plugins.Plugin) {
	n.mu.Lock()
	on, word := n.voiceOn, n.callWord
	n.mu.Unlock()
	for _, p := range n.mgr.Plugins() {
		if (only == nil || p == only) && p.HasCapability("voice") {
			if err := p.Voice(on, word); err != nil {
				n.log.Debug("voice setting not delivered", "plugin", p.Name(), "err", err)
			}
		}
	}
}

// voiceAsk sends a voice plugin's question to Bot (never into telemetry: what the person said is theirs) and remembers
// which plugin asked, so the answers reach it.
func (n *Node) voiceAsk(pluginName string, data map[string]any) {
	id, _ := data["id"].(string)
	text, _ := data["text"].(string)
	lang, _ := data["lang"].(string)
	if id == "" || text == "" || len(id) > 64 || len(text) > 2000 {
		n.log.Warn("voice_ask from a plugin without id or text, or too long: dropped", "plugin", pluginName)
		return
	}
	n.mu.Lock()
	if n.voiceAskedBy == nil {
		n.voiceAskedBy = map[string]string{}
	}
	if len(n.voiceAskedBy) > 100 {
		n.voiceAskedBy = map[string]string{}
	}
	n.voiceAskedBy[id] = pluginName
	n.mu.Unlock()
	n.log.Info("voice question to Vibe", "plugin", pluginName, "id", id, "chars", len(text))
	n.send(protocol.VoiceAsk{ID: id, Text: text, Lang: lang})
}

// voiceAnswer hands Bot's answer to the plugin that asked; a final answer (not working) forgets the question.
func (n *Node) voiceAnswer(a protocol.VoiceAnswer) {
	n.mu.Lock()
	name := n.voiceAskedBy[a.ID]
	if a.State != "working" {
		delete(n.voiceAskedBy, a.ID)
	}
	n.mu.Unlock()
	p := n.mgr.Get(name)
	if name == "" || p == nil {
		n.log.Debug("voice_answer for no waiting question", "id", a.ID)
		return
	}
	if err := p.VoiceAnswer(a.ID, a.State, a.Text, a.URL); err != nil {
		n.log.Warn("voice answer not delivered", "plugin", name, "err", err)
	}
}

// command validates and routes one command. The synchronous part (checks, clamping, queueing to the plugin) keeps
// commands in arrival order; the reply is awaited on its own goroutine. deadline_ms is an absolute instant on the
// server's clock: it is compared with the link's estimate of that clock at receipt (serverNow), so a command that
// arrives after its deadline is nacked and never reaches a plugin, and a plugin gets only the time that is left. What is
// left becomes an instant on this machine (receipt + left): time the command then spends in the Node and in the queue
// to the plugin counts against it, and the plugin is told the instant on the clock it shares with the Node (plugins.md).
func (n *Node) command(c protocol.Command, sentMS int64, seq uint64, received mono.Instant) {
	if c.ID == "" {
		n.log.Warn("command without id ignored", "kind", c.Kind)
		return
	}
	if c.Operator != nil {
		n.log.Debug("command", "id", c.ID, "ref", c.Ref, "kind", c.Kind, "robot", c.RobotID, "operator", c.Operator.Subject, "role", c.Operator.Role)
	}
	entry, first := n.dedup.Begin(c.ID)
	if !first {
		go func() {
			select {
			case <-entry.Done():
				n.send(entry.Result())
			case <-time.After(10 * time.Second):
			}
		}()
		return
	}
	nack := func(code, msg string) {
		r := protocol.Nack{ID: c.ID, FaultCode: code, Message: msg}
		n.dedup.Complete(entry, r)
		n.send(r)
	}
	known := false
	for _, k := range protocol.Kinds {
		known = known || k == c.Kind
	}
	if !known {
		nack(protocol.FaultUnsupported, "unknown kind "+c.Kind)
		return
	}
	n.mu.Lock()
	limits, allowed, configured, session := n.limits, n.allowed, n.configured, n.session
	n.mu.Unlock()
	if c.Kind == protocol.KindHalt {
		n.noteHaltEpoch(c.RobotID, c.TurnEpoch)
		n.mgr.HaltAll(c.TurnEpoch)
		r := protocol.Ack{ID: c.ID, LatencyMS: mono.Since(received).Milliseconds()}
		n.dedup.Complete(entry, r)
		n.send(r)
		return
	}
	if !configured {
		nack(protocol.FaultNotReady, "the server's config has not arrived on this connection yet")
		return
	}
	// A command decided under a turn that has since ended (its epoch is older than the newest halt's for this robot) never
	// reaches a driver. Bot already guarantees it is not sent after that halt; this holds even if frames were delayed.
	if last := n.haltEpoch(c.RobotID); c.TurnEpoch > 0 && c.TurnEpoch < last {
		nack(protocol.FaultExpired, fmt.Sprintf("from a turn that ended: epoch %d is older than the newest halt's %d", c.TurnEpoch, last))
		return
	}
	if allowed != nil && !allowed[c.Kind] {
		nack(protocol.FaultNotAllowed, c.Kind+" is not in this robot's allowed_commands")
		return
	}
	if protocol.ComputerKinds[c.Kind] {
		n.computer(c, entry, sentMS, received, nack)
		return
	}
	if protocol.GuardedKinds[c.Kind] {
		st := n.latch.State()
		if st.Local {
			nack(protocol.FaultLocalStop, "stopped on the device with `openvibe-node stop`")
			return
		}
		if st.Remote {
			nack(protocol.FaultEstopped, st.RemoteReason)
			return
		}
	}
	deadline := limits.Deadline(0)
	var uncertainty *int64
	var unc int64
	if c.DeadlineMS > 0 {
		left, u, synced := n.timeLeft(c.DeadlineMS, sentMS, received)
		unc = u
		if protocol.GuardedKinds[c.Kind] {
			// Motion is timed only on a clock Node can bound (Maticus, 2026-10-27): not before the connection's first
			// round trip, and not on a link whose fastest round trip is slower than MaxClockUncertaintyMS.
			if !synced {
				nack(protocol.FaultNotReady, "the link's clock is not synchronized yet: no round trip has been measured on this connection")
				return
			}
			if unc > protocol.MaxClockUncertaintyMS {
				nack(protocol.FaultNotReady, fmt.Sprintf("the link is too slow to time motion: its fastest round trip is %d ms (at most %d)", unc, protocol.MaxClockUncertaintyMS))
				return
			}
		}
		// deadline_ms is whole milliseconds, at least 1: a command with less than that left has none.
		if left < time.Millisecond {
			nack(protocol.FaultExpired, fmt.Sprintf("the deadline passed %d ms before the command arrived (counting %d ms of clock uncertainty)", max(0, ceilMS(-left)), unc))
			return
		}
		deadline = limits.Deadline(int(min(int64(left/time.Millisecond), int64(limits.MaxCommandMS)+1)))
		if synced && n.link != nil {
			uncertainty = &unc
		}
	}
	value, err := limits.Clamp(c.Kind, c.Value)
	if err != nil {
		var fe *safety.FaultError
		if errors.As(err, &fe) {
			nack(fe.Code, fe.Message)
		} else {
			nack(protocol.FaultBadValue, err.Error())
		}
		return
	}
	var actuator string
	if c.Kind == protocol.KindActuator {
		var v struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(value, &v)
		actuator = v.Name
	}
	p := n.mgr.Route(c.Kind, c.Target, actuator)
	if p == nil {
		nack(protocol.FaultUnsupported, "no driver on this device handles "+c.Kind)
		return
	}
	// Where it came from, unchanged, so a driver can check it without keeping old ids (Maticus, 2026-10-26, LOOI).
	prov := &plugins.Provenance{Session: session, Seq: seq, IssuedAtMS: sentMS, ExpiresAtMS: c.DeadlineMS, ClockUncertaintyMS: uncertainty}
	// The window counts from receipt, not from now: nothing the Node does after the frame arrived extends it.
	expires := received.Add(time.Duration(deadline) * time.Millisecond)
	wait := p.BeginWith(n.ctx, c.ID, c.Kind, value, expires, c.Operator, c.TurnEpoch, prov)
	go func() {
		r := wait()
		var msg protocol.Message
		if r.OK {
			msg = protocol.Ack{ID: c.ID, LatencyMS: mono.Since(received).Milliseconds()}
		} else {
			msg = protocol.Nack{ID: c.ID, FaultCode: r.FaultCode, Message: r.Message}
		}
		n.dedup.Complete(entry, msg)
		n.send(msg)
	}()
}

// computer runs one exec, fs or screen command: no clamping, a long reply window (the server's deadline, at most
// MaxComputerMS, else DefaultComputerMS), refused while the local stop or an e-stop is latched when it would change the
// machine, and answered with the plugin's data. The plugin applies its owner's control level.
func (n *Node) computer(c protocol.Command, entry *safety.Entry, sentMS int64, received mono.Instant, nack func(code, msg string)) {
	if len(c.Value) > protocol.MaxComputerValue {
		nack(protocol.FaultBadValue, fmt.Sprintf("the value is larger than %d KB", protocol.MaxComputerValue/1024))
		return
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(c.Value, &obj) != nil || obj == nil {
		nack(protocol.FaultBadValue, "value must be a JSON object")
		return
	}
	if c.Kind == protocol.KindScreenStream {
		n.screenCommand(c, entry, received, nack)
		return
	}
	if protocol.ComputerActs(c.Kind, c.Value) {
		st := n.latch.State()
		if st.Local {
			nack(protocol.FaultLocalStop, "stopped on the machine with `openvibe-node stop`: observe only")
			return
		}
		if st.Remote {
			nack(protocol.FaultEstopped, st.RemoteReason)
			return
		}
	}
	window := int64(protocol.DefaultComputerMS)
	if c.DeadlineMS > 0 {
		left, _, _ := n.timeLeft(c.DeadlineMS, sentMS, received)
		if left < time.Millisecond {
			nack(protocol.FaultExpired, fmt.Sprintf("the deadline passed %d ms before the command arrived", max(0, ceilMS(-left))))
			return
		}
		window = min(int64(left/time.Millisecond), int64(protocol.MaxComputerMS))
	}
	p := n.mgr.Route(c.Kind, c.Target, "")
	if p == nil {
		nack(protocol.FaultUnsupported, "no driver on this device handles "+c.Kind+" (the computer plugin is off)")
		return
	}
	if c.Kind == protocol.KindInput {
		n.mu.Lock()
		n.inputs[c.ID] = true
		n.mu.Unlock()
	}
	wait := p.BeginAs(n.ctx, c.ID, c.Kind, c.Value, int(window), c.Operator)
	go func() {
		r := wait()
		if c.Kind == protocol.KindInput {
			n.mu.Lock()
			delete(n.inputs, c.ID)
			if r.OK || (r.FaultCode != protocol.FaultNotAllowed && r.FaultCode != protocol.FaultLocalStop) {
				n.inputEnded = time.Now() // it ran (or may have run part way)
			}
			n.mu.Unlock()
		}
		var msg protocol.Message
		switch {
		case r.OK && len(r.Data) > protocol.MaxAckData:
			msg = protocol.Nack{ID: c.ID, FaultCode: protocol.FaultBadValue, Message: fmt.Sprintf("the answer is larger than %d KB", protocol.MaxAckData/1024)}
		case r.OK:
			msg = protocol.AckData{ID: c.ID, LatencyMS: mono.Since(received).Milliseconds(), Data: r.Data}
		default:
			msg = protocol.Nack{ID: c.ID, FaultCode: r.FaultCode, Message: r.Message}
		}
		n.mu.Lock()
		delete(n.approvals, c.ID)
		n.mu.Unlock()
		n.dedup.Complete(entry, msg)
		n.send(msg)
	}()
}

// noteHaltEpoch records a halt's turn_epoch for its robot (the newest wins); haltEpoch reads it (0: none seen).
func (n *Node) noteHaltEpoch(robotID string, epoch int64) {
	if epoch <= 0 {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.haltEpochs == nil {
		n.haltEpochs = map[string]int64{}
	}
	if epoch > n.haltEpochs[robotID] {
		n.haltEpochs[robotID] = epoch
	}
}

func (n *Node) haltEpoch(robotID string) int64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.haltEpochs[robotID]
}

// timeLeft is how long a command stamped sentMS and received at received has before its deadline_ms, an instant on the
// server's clock, counted from the LATEST the server's clock can be at receipt: the link's estimate plus its bound
// (Link.ClockUncertainty: the connection's fastest round trip and the server's ts resolution). Everything is kept in
// nanoseconds, so no rounding can add time: left is exact, and unc, the bound reported in ms, is rounded up from
// nanoseconds (Maticus, 2026-10-27: 1,000,001 ns is 2 ms). synced is false while no round trip has been measured on the
// connection (left then counts from the bare estimate). Without a link (local commands) it is synced with no uncertainty.
func (n *Node) timeLeft(deadlineMS, sentMS int64, received mono.Instant) (left time.Duration, unc int64, synced bool) {
	end := deadlineMS * int64(time.Millisecond)
	now := n.serverNow(sentMS, received)
	if n.link == nil {
		return time.Duration(end - now), 0, true
	}
	d, ok := n.link.ClockUncertainty()
	if !ok {
		return time.Duration(end - now), 0, false
	}
	return time.Duration(end-now) - d, ceilMS(d), true
}

// ceilMS is d in whole milliseconds, rounded up (d ≥ 0): a bound of 1,000,001 ns is 2 ms.
func ceilMS(d time.Duration) int64 {
	return int64((d + time.Millisecond - 1) / time.Millisecond)
}

// latchReason is the reason a latched stop gives a driver: the remote one when the platform set it, else the local one.
func latchReason(st safety.LatchState) string {
	if st.Remote {
		return st.RemoteReason
	}
	return st.LocalReason
}

// serverNow is the server's clock (epoch ns) when a frame stamped sentMS was received: the link's estimate, else the
// frame's own ts, else the wall clock now (later than the receipt, so never more time).
func (n *Node) serverNow(sentMS int64, received mono.Instant) int64 {
	if n.link != nil {
		if ns, ok := n.link.ServerNow(received); ok {
			return ns
		}
	}
	if sentMS > 0 {
		return sentMS * int64(time.Millisecond)
	}
	return time.Now().UnixNano()
}

func (n *Node) send(m protocol.Message) {
	if n.link == nil {
		return
	}
	if err := n.link.Send(m); err != nil && !errors.Is(err, link.ErrOffline) {
		n.log.Warn("send failed", "type", m.MessageType(), "err", err)
	}
}

// ---- latch ----

func (n *Node) onLatch(st safety.LatchState) {
	if n.worker != nil {
		n.worker.SetStopped(stopFault(st)) // kills every job at once while stopped
	}
	if st.Stopped() {
		n.log.Warn("stop latched", "remote", st.Remote, "local", st.Local)
		cause := plugins.EstopRemote
		if st.Local {
			cause = plugins.EstopLocal
		}
		n.mgr.EstopAll(cause, latchReason(st))
	} else {
		n.log.Warn("stop cleared; motion allowed again")
		n.mgr.ResumeAll()
	}
	n.sendEstopState()
	n.sendStatus()
}

func (n *Node) pollLatch(ctx context.Context) {
	t := time.NewTicker(n.opt.LatchPoll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := n.latch.Reload(); err != nil {
				n.log.Error("latch file", "err", err)
			}
		}
	}
}

// sendEstopState reports the device's latch. Bot reads latched:true as "this device is stopped" and latches the robot
// (so the local kill switch shows on the panel and the gate stops sending motion); latched:false is a report only
// and never clears the owner's latch. robot_id names the robot when the device serves exactly one.
func (n *Node) sendEstopState() {
	if !n.reporting() {
		return
	}
	n.mu.Lock()
	var robot string
	if len(n.robotIDs) == 1 {
		robot = n.robotIDs[0]
	}
	n.mu.Unlock()
	st := n.latch.State()
	n.send(protocol.EstopState{Latched: st.Stopped(), By: "device", At: time.Now().UTC().Format(time.RFC3339Nano),
		RobotID: robot, LocalStop: st.Local, Reason: estopReason(st)})
}

// estopReason is what stopped the device: the local stop's reason while it holds, else the server's.
func estopReason(st safety.LatchState) string {
	if st.Local && st.LocalReason != "" {
		return st.LocalReason
	}
	return st.RemoteReason
}

// ---- plugin output ----

func (n *Node) pump(ctx context.Context) {
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case o := <-n.mgr.Output():
			n.output(o)
		case <-t.C:
			n.flushTelemetry(false)
		}
	}
}

func (n *Node) output(o plugins.Output) {
	switch o.Kind {
	case plugins.OutTelemetry:
		n.mu.Lock()
		n.telemetry[o.Plugin] = o.Data
		n.mu.Unlock()
	case plugins.OutEvent:
		name, _ := o.Data["name"].(string)
		// Vibe by voice: a question goes to Bot on its own (never as a telemetry event); a voice plugin that has just
		// started asks for the owner's setting.
		if name == "voice_ask" {
			n.voiceAsk(o.Plugin, o.Data)
			return
		}
		if name == "voice_ready" {
			n.tellVoice(n.mgr.Get(o.Plugin))
			return
		}
		fields := map[string]any{}
		for k, v := range o.Data {
			if k != "name" {
				fields[k] = v
			}
		}
		if len(fields) == 0 {
			fields = nil
		}
		n.log.Info("plugin event", "plugin", o.Plugin, "event", name)
		if name == "screen_stream_ended" {
			sid, _ := o.Data["sid"].(string)
			n.screenEnded(o.Plugin, sid)
		}
		n.trackApproval(o.Plugin, name, o.Data, o.At)
		n.mu.Lock()
		n.events = append(n.events, protocol.Event{Name: name, Driver: o.Plugin, TS: o.At.UnixMilli(), Fields: fields})
		n.mu.Unlock()
		n.flushTelemetry(true)
	case plugins.OutVideo:
		if stream, _ := o.Data["stream"].(string); stream == "screen" {
			n.screenFrame(o)
			return
		}
		n.mu.Lock()
		j, from := n.jpeg, n.jpegFrom
		n.mu.Unlock()
		if j != nil && (from == "" || from == o.Plugin) {
			if p, ok := o.Data["path"].(string); ok {
				j.Push(p)
			}
		}
	case plugins.OutLocalStop:
		// A driver's own stop: everything stops at once, then the local latch is persisted with its reason. It is
		// never a clear: nothing a plugin says releases a latch.
		reason, _ := o.Data["reason"].(string)
		if reason = safety.CleanReason(reason); reason == "" {
			reason = o.Plugin + " stop"
		}
		n.mgr.EstopAll(plugins.EstopDriver, reason)
		if _, err := n.latch.SetLocalWith(reason); err != nil {
			n.log.Error("a plugin latched the local stop, but the latch could not be saved", "plugin", o.Plugin, "err", err)
		}
	case plugins.OutFault, plugins.OutState:
		n.sendStatus()
	}
}

// flushTelemetry sends at most one telemetry frame per telemetry interval (≥ 500 ms: ≤ 2 Hz). Events go out at the
// next 100 ms tick even inside the interval, so a cliff is not held back by the rate limit of sensor data.
func (n *Node) flushTelemetry(eventsOnly bool) {
	n.mu.Lock()
	due := time.Since(n.lastTelem) >= time.Duration(n.telemMS)*time.Millisecond
	if !due && len(n.events) == 0 {
		n.mu.Unlock()
		return
	}
	if eventsOnly && !due && time.Since(n.lastTelem) < 100*time.Millisecond {
		n.mu.Unlock()
		return
	}
	t := protocol.Telemetry{Events: n.events}
	n.events = nil
	if due {
		t = mergeTelemetry(n.telemetry, t.Events)
		n.lastTelem = time.Now()
	}
	n.mu.Unlock()
	if t.Battery == nil && t.Voltage == nil && len(t.Sensors) == 0 && t.RSSI == nil && len(t.Events) == 0 {
		return
	}
	n.send(t)
}

func mergeTelemetry(per map[string]map[string]any, events []protocol.Event) protocol.Telemetry {
	t := protocol.Telemetry{Events: events}
	names := make([]string, 0, len(per))
	for k := range per {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, name := range names {
		d := per[name]
		// Plugins report battery {percent, volts}; Bot wants battery as a 0..1 fraction and voltage in volts.
		if b, ok := d["battery"].(map[string]any); ok && t.Battery == nil && t.Voltage == nil {
			if p := num(b["percent"]); p != nil {
				frac := math.Max(0, math.Min(1, *p/100))
				t.Battery = &frac
			}
			t.Voltage = num(b["volts"])
		}
		if r := num(d["rssi"]); r != nil && t.RSSI == nil {
			v := int(*r)
			t.RSSI = &v
		}
		if s, ok := d["sensors"].(map[string]any); ok {
			if t.Sensors == nil {
				t.Sensors = map[string]any{}
			}
			for k, v := range s {
				if _, taken := t.Sensors[k]; taken {
					k = name + "." + k
				}
				t.Sensors[k] = v
			}
		}
		if f, ok := d["faults"].([]any); ok && len(f) > 0 {
			if t.Sensors == nil {
				t.Sensors = map[string]any{}
			}
			t.Sensors[name+".faults"] = f
		}
	}
	return t
}

func num(v any) *float64 {
	if f, ok := v.(float64); ok {
		return &f
	}
	return nil
}

// ---- video ----

func (n *Node) startVideo(ctx context.Context) {
	if n.pub == nil {
		return
	}
	src := n.chooseSource(ctx)
	if src == nil {
		n.log.Info("no camera to publish")
		return
	}
	n.mu.Lock()
	n.videoSrc = src.Name()
	n.mu.Unlock()
	n.log.Info("publishing video", "source", src.Name())
	n.pub.Run(ctx, src)
}

// chooseSource applies video.source. For "auto" it waits (up to 20 s) for the plugins to describe their cameras.
func (n *Node) chooseSource(ctx context.Context) video.Source {
	vc := n.opt.Config.Video
	fps := vc.FPS
	switch vc.Source {
	case "test":
		return &video.TestPattern{W: vc.Width, H: vc.Height, FPS: fps}
	case "command":
		return &video.CommandSource{Argv: vc.Command, Log: n.log}
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		all := true
		for _, info := range n.mgr.Infos() {
			if info.Describe == nil {
				all = false
				continue
			}
			if vc.Source == "plugin" && vc.Plugin != "" && info.Name != vc.Plugin {
				continue
			}
			cam, _ := info.Describe.Capabilities["camera"].(map[string]any)
			if cam == nil {
				continue
			}
			if argv := strSlice(cam["h264_command"]); len(argv) > 0 && vc.Source == "auto" {
				if _, err := exec.LookPath(argv[0]); err == nil {
					return &video.CommandSource{Argv: argv, Log: n.log}
				}
				n.log.Warn("camera program not found", "plugin", info.Name, "program", argv[0])
			}
			if j, _ := cam["jpeg"].(bool); j {
				src := &video.JPEGSource{FFmpeg: vc.FFmpeg, FPS: fps, Log: n.log}
				n.mu.Lock()
				n.jpeg, n.jpegFrom = src, info.Name
				n.mu.Unlock()
				return src
			}
			if s, _ := cam["source"].(string); s == "test_pattern" {
				return &video.TestPattern{W: vc.Width, H: vc.Height, FPS: fps}
			}
		}
		if all || time.Now().After(deadline) {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func strSlice(v any) []string {
	a, ok := v.([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, x := range a {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// ---- status and local control ----

// Status is what `openvibe-node status` prints. It never contains the credential or the publish key.
type Status struct {
	Version   string             `json:"version"`
	DeviceID  string             `json:"device_id,omitempty"`
	Principal string             `json:"principal,omitempty"` // nod_… of a Network-paired machine
	Server    string             `json:"server,omitempty"`
	Paired    bool               `json:"paired"`
	Uptime    string             `json:"uptime"`
	Link      LinkStatus         `json:"link"`
	Latch     safety.LatchState  `json:"latch"`
	Limits    safety.Limits      `json:"limits"`
	Plugins   []PluginStatus     `json:"plugins"`
	Video     VideoStatus        `json:"video"`
	Screen    ScreenStatus       `json:"screen"`
	ConfigDir string             `json:"config_dir"`
	StateDir  string             `json:"state_dir"`
	Telemetry protocol.Telemetry `json:"telemetry"`
	// Jobs are the worker's jobs not ended yet; empty when the worker is off.
	Jobs []worker.JobInfo `json:"jobs"`
}

type LinkStatus struct {
	Connected  bool   `json:"connected"`
	RTTMS      int64  `json:"rtt_ms,omitempty"`
	Reconnects int64  `json:"reconnects"`
	LastError  string `json:"last_error,omitempty"`
}

type PluginStatus struct {
	Name         string          `json:"name"`
	State        string          `json:"state"`
	Driver       string          `json:"driver,omitempty"`
	Version      string          `json:"version,omitempty"`
	Restarts     int             `json:"restarts"`
	PID          int             `json:"pid,omitempty"`
	Fault        *protocol.Fault `json:"fault,omitempty"`
	Capabilities map[string]any  `json:"capabilities,omitempty"`
}

type VideoStatus struct {
	State     string `json:"state"`
	Source    string `json:"source,omitempty"`
	Frames    int64  `json:"frames"`
	LastError string `json:"last_error,omitempty"`
}

func (n *Node) Status() Status {
	s := Status{Version: n.opt.Version, Latch: n.latch.State(), ConfigDir: n.opt.Paths.ConfigDir, StateDir: n.opt.Paths.StateDir,
		Uptime: time.Since(n.started).Round(time.Second).String()}
	if n.opt.Creds != nil {
		s.Paired, s.DeviceID, s.Principal, s.Server = true, n.opt.Creds.DeviceID, n.opt.Creds.Principal, n.opt.Creds.Server
	}
	n.mu.Lock()
	s.Limits = n.limits
	s.Video.Source = n.videoSrc
	s.Telemetry = mergeTelemetry(n.telemetry, nil)
	n.mu.Unlock()
	if n.link != nil {
		st := n.link.Stats()
		s.Link = LinkStatus{Connected: st.Connected, RTTMS: st.RTT.Milliseconds(), Reconnects: st.Reconnects, LastError: st.LastError}
	}
	for _, i := range n.mgr.Infos() {
		ps := PluginStatus{Name: i.Name, State: i.State, Restarts: i.Restarts, PID: i.PID, Fault: i.Fault}
		if i.Describe != nil {
			ps.Driver, ps.Version, ps.Capabilities = i.Describe.Driver, i.Describe.Version, i.Describe.Capabilities
		}
		s.Plugins = append(s.Plugins, ps)
	}
	s.Jobs = []worker.JobInfo{}
	if n.worker != nil {
		s.Jobs = n.worker.Jobs()
	}
	s.Screen = n.screenStatus()
	s.Video.State = video.StateOff
	if n.pub != nil {
		s.Video.State, s.Video.Frames, s.Video.LastError = n.pub.State(), n.pub.Frames(), n.pub.LastError()
	}
	return s
}

// reporting is whether status and estop_state may go out: only once this connection's config has been applied.
func (n *Node) reporting() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.configured
}

func (n *Node) sendStatus() {
	if !n.reporting() {
		return
	}
	st := n.latch.State()
	s := protocol.Status{Firmware: "openvibe-node-" + n.opt.Version, Capabilities: map[string]any{}, AgentVersion: n.opt.Version,
		DeviceKind: n.opt.Config.DeviceKind, OS: runtime.GOOS, Arch: runtime.GOARCH, EstopLatched: st.Stopped(), LocalStop: st.Local,
		Drivers: []protocol.DriverStatus{}, Faults: []protocol.Fault{}}
	for _, i := range n.mgr.Infos() {
		d := protocol.DriverStatus{Name: i.Name, State: i.State}
		if i.Describe != nil {
			d.Driver, d.Version, d.Capabilities = i.Describe.Driver, i.Describe.Version, i.Describe.Capabilities
			s.Capabilities[i.Name] = i.Describe.Capabilities
		}
		s.Drivers = append(s.Drivers, d)
		if i.Fault != nil {
			s.Faults = append(s.Faults, *i.Fault)
		}
	}
	if c := n.jobs.runtimeClasses(); len(c) > 0 {
		caps := n.opt.Config.Worker.Caps.WithDefaults()
		s.Capabilities[protocol.CapWorker] = protocol.WorkerCapabilities{Capabilities: protocol.WorkerCapabilityNames(c), RuntimeClasses: c, MaxJobs: caps.MaxJobs,
			MaxTTLMS: caps.MaxTTLMS, MaxWallMS: caps.MaxWallMS, MaxCPUMS: caps.MaxCPUMS,
			MaxMemBytes: caps.MaxMemBytes, MaxOutputBytes: caps.MaxOutputBytes}
	}
	if n.pub != nil {
		s.Video = n.pub.State()
	}
	n.send(s)
}

// trackApproval keeps the list `openvibe-node approvals` shows from the computer plugin's approval events.
func (n *Node) trackApproval(plugin, name string, data map[string]any, at time.Time) {
	id, _ := data["id"].(string)
	if id == "" || (name != "approval" && name != "approval_done") {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if name == "approval_done" {
		delete(n.approvals, id)
		return
	}
	if len(n.approvals) >= 64 {
		return
	}
	a := Approval{ID: id, Plugin: plugin, At: at.UnixMilli()}
	a.Kind, _ = data["kind"].(string)
	a.Summary, _ = data["summary"].(string)
	if e, ok := data["expires_s"].(float64); ok {
		a.ExpiresS = int64(e)
	}
	n.approvals[id] = a
}

// Approvals lists the computer commands waiting for an answer on the machine, oldest first.
func (n *Node) Approvals() []Approval {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]Approval, 0, len(n.approvals))
	for _, a := range n.approvals {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At < out[j].At || (out[i].At == out[j].At && out[i].ID < out[j].ID) })
	return out
}

// drivingLocked: an input command runs (it is in flight and not waiting for approval) or finished under InputGuard ago.
func (n *Node) drivingLocked() bool {
	for id := range n.inputs {
		if _, waiting := n.approvals[id]; !waiting {
			return true
		}
	}
	return !n.inputEnded.IsZero() && time.Since(n.inputEnded) < InputGuard
}

// tellTray passes the tray's presence to the plugins that handle computer kinds: at once when it quits, at most every
// trayRetell while it runs.
func (n *Node) tellTray(on bool) {
	n.mu.Lock()
	if on && !n.trayToldAt.IsZero() && time.Since(n.trayToldAt) < trayRetell {
		n.mu.Unlock()
		return
	}
	if on {
		n.trayToldAt = time.Now()
	} else {
		n.trayToldAt = time.Time{}
	}
	n.mu.Unlock()
	if n.mgr == nil {
		return
	}
	seen := map[*plugins.Plugin]bool{}
	for kind := range protocol.ComputerKinds {
		if p := n.mgr.Route(kind, "", ""); p != nil && !seen[p] {
			seen[p] = true
			_ = p.Tray(on, trayTTL)
		}
	}
}

func (n *Node) control(r localctl.Request) localctl.Response {
	switch r.Cmd {
	case localctl.CmdApprovals:
		b, err := json.Marshal(n.Approvals())
		if err != nil {
			return localctl.Response{Error: err.Error()}
		}
		return localctl.Response{OK: true, Data: b}
	case localctl.CmdApprove, localctl.CmdDeny:
		n.mu.Lock()
		a, ok := n.approvals[r.ID]
		if ok && r.Cmd == localctl.CmdApprove && n.drivingLocked() {
			n.mu.Unlock()
			return localctl.Response{Error: "the agent is using the mouse or keyboard (or just did): answer again in a moment, so that its click cannot approve this"}
		}
		if ok {
			delete(n.approvals, r.ID)
		}
		n.mu.Unlock()
		if !ok {
			return localctl.Response{Error: fmt.Sprintf("nothing with the id %q waits for approval", strings.TrimSpace(r.ID))}
		}
		p := n.mgr.Get(a.Plugin)
		if p == nil {
			return localctl.Response{Error: "the plugin that asked is gone"}
		}
		if err := p.Answer(a.ID, r.Cmd == localctl.CmdApprove); err != nil {
			return localctl.Response{Error: err.Error()}
		}
		return localctl.Response{OK: true}
	case localctl.CmdTray:
		n.tellTray(r.On)
		return localctl.Response{OK: true}
	case localctl.CmdStop:
		n.mgr.EstopAll(plugins.EstopLocal, "openvibe-node stop") // at once, then persist
		if _, err := n.latch.SetLocalWith("openvibe-node stop"); err != nil {
			return localctl.Response{OK: true, Error: "stopped, but the latch could not be saved: " + err.Error()}
		}
		return localctl.Response{OK: true}
	case localctl.CmdQuit:
		// Not a stop: nothing is latched, so the next start drives as before (Maticus, 2026-10-26: a script that
		// stopped the Node with `stop` left it latched, and had to kill the process to end it).
		if !n.opt.CanQuit {
			return localctl.Response{Error: "this node runs as a system service, which would start it again: stop it there " +
				"(sudo systemctl stop openvibe-node; on Windows, Services), or remove it with `openvibe-node uninstall`"}
		}
		n.log.Info("quit by its owner (openvibe-node quit)")
		n.mgr.StopAll(plugins.StopShutdown) // at once; Run stops everything again on the way out
		n.quitting.Store(true)
		go func() { time.Sleep(100 * time.Millisecond); n.quit() }() // after this answer is written
		return localctl.Response{OK: true}
	case localctl.CmdResume:
		if _, err := n.latch.Resume(); err != nil {
			return localctl.Response{Error: err.Error()}
		}
		return localctl.Response{OK: true}
	case localctl.CmdStatus, localctl.CmdPlugins:
		b, err := json.Marshal(n.Status())
		if err != nil {
			return localctl.Response{Error: err.Error()}
		}
		return localctl.Response{OK: true, Data: b}
	}
	return localctl.Response{Error: fmt.Sprintf("unknown command %q", strings.TrimSpace(r.Cmd))}
}
