// Package node ties the Node together: the control link, the plugins, the safety rules, video and the local control
// socket. Every path that should stop the motors ends in Manager.StopAll or EstopAll:
//
//	link lost (disconnect, deadman)       → StopAll
//	server estop / config.estop_latched   → latch (persisted) → EstopAll
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
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/OpenVibers/OpenVibe.Node/internal/config"
	"github.com/OpenVibers/OpenVibe.Node/internal/credentials"
	"github.com/OpenVibers/OpenVibe.Node/internal/link"
	"github.com/OpenVibers/OpenVibe.Node/internal/localctl"
	"github.com/OpenVibers/OpenVibe.Node/internal/plugins"
	"github.com/OpenVibers/OpenVibe.Node/internal/protocol"
	"github.com/OpenVibers/OpenVibe.Node/internal/safety"
	"github.com/OpenVibers/OpenVibe.Node/internal/video"
)

// Options configure a Node.
type Options struct {
	Config  *config.Config
	Paths   config.Paths
	Creds   *credentials.Credentials
	Log     *slog.Logger
	Version string

	// For tests.
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
	mgr   *plugins.Manager
	link  *link.Link
	pub   *video.Publisher

	ctx context.Context

	mu        sync.Mutex
	limits    safety.Limits
	allowed   map[string]bool
	telemetry map[string]map[string]any // per plugin, latest
	events    []protocol.Event
	telemMS   int
	jpeg      *video.JPEGSource
	jpegFrom  string
	videoSrc  string
	lastTelem time.Time
	started   time.Time

	// Per connection: status and estop_state wait for hello and config (until then the server may still be checking
	// the credential), and motion waits for config (its estop_latched must be applied first).
	helloOK     bool
	configOK    bool
	ready       bool
	clockOffset time.Duration // server clock − local clock, from hello.server_time
	robotIDs    []string
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
		telemetry: map[string]map[string]any{}, telemMS: 500}
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
		specs = append(specs, plugins.Spec{Name: p.Name, Argv: opt.Config.PluginCommand(p, opt.Paths), Env: env, Dir: p.Dir, Config: cfg})
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
		n.link = link.New(link.Options{URL: u, Credential: opt.Creds.Credential, UserAgent: "openvibe-node/" + opt.Version,
			Log: opt.Log, HeartbeatInterval: opt.HeartbeatInterval, BackoffMin: opt.LinkBackoffMin}, n)
		if opt.Creds.WHIPURL != "" && opt.Config.Video.Source != "off" {
			n.pub = video.NewPublisher(video.Options{WHIPURL: opt.Creds.WHIPURL, PublishKey: opt.Creds.PublishKey,
				ICEServers: opt.Creds.ICEServers, Log: opt.Log, IncludeLoopback: opt.VideoLoopback})
		}
	}
	n.latch.OnChange(n.onLatch)
	return n, nil
}

// Run runs until ctx ends, then stops every actuator and waits for the plugins to exit.
func (n *Node) Run(ctx context.Context) error {
	n.ctx = ctx
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
	n.mgr.Start(pctx)
	if n.latch.State().Stopped() {
		n.log.Warn("starting with the stop latched", "remote", n.latch.State().Remote, "local", n.latch.State().Local)
		n.mgr.EstopAll()
	}

	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); n.pump(ctx) }()
	go func() { defer wg.Done(); n.pollLatch(ctx) }()
	go func() { defer wg.Done(); n.startVideo(ctx) }()
	if n.link != nil {
		wg.Add(1)
		go func() { defer wg.Done(); n.link.Run(ctx) }()
	} else {
		n.log.Warn("not paired: running local only (run `openvibe-node pair <CODE>`)")
	}
	<-ctx.Done()
	n.log.Info("shutting down: stopping every actuator")
	n.mgr.StopAll()
	cancelPlugins()
	n.mgr.Wait()
	wg.Wait()
	return nil
}

// ---- link.Handler ----

// Connected sends nothing: status and estop_state go out once hello and config have arrived (markReady).
func (n *Node) Connected() {
	n.resetSession()
}

func (n *Node) Disconnected(err error) {
	n.resetSession()
	if link.IsCredentialError(err) {
		n.log.Error("link lost: the credential was refused; re-pair or update the credential", "err", err)
	} else {
		n.log.Warn("link lost: stopping every actuator", "err", err)
	}
	n.mgr.StopAll()
}

func (n *Node) resetSession() {
	n.mu.Lock()
	n.helloOK, n.configOK, n.ready = false, false, false
	n.mu.Unlock()
}

// markReady records hello or config; when both have arrived on this connection it sends the first status and
// estop_state.
func (n *Node) markReady(hello, config bool) {
	n.mu.Lock()
	n.helloOK = n.helloOK || hello
	n.configOK = n.configOK || config
	first := n.helloOK && n.configOK && !n.ready
	if first {
		n.ready = true
	}
	n.mu.Unlock()
	if first {
		n.sendStatus()
		n.sendEstopState()
	}
}

func (n *Node) isReady() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.ready
}

func (n *Node) Frame(f protocol.Frame) {
	switch m := f.Msg.(type) {
	case protocol.Hello:
		n.hello(m, time.Now())
	case protocol.Config:
		n.applyConfig(m)
	case protocol.Command:
		n.command(m, time.Now())
	case protocol.Estop:
		n.remoteEstop(m.Latched, m.By)
	case protocol.Error:
		n.log.Warn("the server refused a frame", "code", m.Code, "detail", m.Detail)
	}
}

func (n *Node) hello(h protocol.Hello, received time.Time) {
	var offset time.Duration
	if t, ok := h.Time(); ok {
		offset = t.Sub(received)
	}
	n.mu.Lock()
	n.clockOffset, n.robotIDs = offset, h.RobotIDs
	n.mu.Unlock()
	n.log.Info("server hello", "device", h.DeviceID, "session", h.SessionID, "robots", strings.Join(h.RobotIDs, ","),
		"clock_offset", offset.Round(time.Millisecond))
	n.markReady(true, false)
}

// remoteEstop applies the server's e-stop: latched:true latches (motors stop at once, before the disk write);
// latched:false is the owner's clear. The local kill switch is never touched from here.
func (n *Node) remoteEstop(latched bool, by string) {
	if latched {
		reason := "e-stop latched on the server"
		if by != "" {
			reason += " by " + by
		}
		n.log.Warn("e-stop from the server", "by", by)
		n.mgr.EstopAll()
		if _, err := n.latch.SetRemote(reason); err != nil {
			n.log.Error("could not persist the e-stop (it is held in memory)", "err", err)
		}
		return
	}
	if !n.latch.State().Remote {
		return
	}
	n.log.Warn("e-stop cleared by the server", "by", by)
	if _, err := n.latch.ClearRemote(); err != nil {
		n.log.Error("could not persist the e-stop clear", "err", err)
	}
}

func (n *Node) applyConfig(c protocol.Config) {
	// The robot's e-stop first, so no command on this connection can move a latched robot.
	if c.EstopLatched != nil {
		n.remoteEstop(*c.EstopLatched, "")
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
	n.mu.Unlock()
	if n.link != nil {
		n.link.SetTiming(c.HeartbeatMS)
	}
	n.log.Info("config", "max_speed", l.MaxSpeed, "max_turn", l.MaxTurn, "max_command_ms", l.MaxCommandMS,
		"allowed_commands", strings.Join(c.AllowedCommands, ","))
	n.markReady(false, true)
}

// command validates and routes one command. The synchronous part (checks, clamping, queueing to the plugin) keeps
// commands in arrival order; the reply is awaited on its own goroutine.
func (n *Node) command(c protocol.Command, received time.Time) {
	if c.ID == "" {
		n.log.Warn("command without id ignored", "kind", c.Kind)
		return
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
	limits, allowed, configured, offset := n.limits, n.allowed, n.configOK, n.clockOffset
	n.mu.Unlock()
	if c.Kind == protocol.KindHalt {
		n.mgr.StopAll()
		r := protocol.Ack{ID: c.ID, LatencyMS: time.Since(received).Milliseconds()}
		n.dedup.Complete(entry, r)
		n.send(r)
		return
	}
	if !configured {
		nack(protocol.FaultNotReady, "the server's config has not arrived")
		return
	}
	if allowed != nil && !allowed[c.Kind] {
		nack(protocol.FaultNotAllowed, c.Kind+" is not in allowed_commands")
		return
	}
	// deadline_ms is an instant on the server's clock: a command that arrives after it never reaches a plugin.
	remaining, expired := limits.Remaining(c.DeadlineMS, received.Add(offset).UnixMilli())
	if expired {
		nack(protocol.FaultExpired, "the deadline had passed when the command arrived")
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
	wait := p.Begin(n.ctx, c.ID, c.Kind, value, remaining)
	go func() {
		r := wait()
		var msg protocol.Message
		if r.OK {
			msg = protocol.Ack{ID: c.ID, LatencyMS: time.Since(received).Milliseconds()}
		} else {
			msg = protocol.Nack{ID: c.ID, FaultCode: r.FaultCode, Message: r.Message}
		}
		n.dedup.Complete(entry, msg)
		n.send(msg)
	}()
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
	if st.Stopped() {
		n.log.Warn("stop latched", "remote", st.Remote, "local", st.Local)
		n.mgr.EstopAll()
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

// sendEstopState reports the device's own latch. Bot sets the robot's e-stop to whatever estop_state says, so the
// server's latch is never echoed back: latched:true is the local kill switch, latched:false goes out only when nothing
// is latched (after `openvibe-node resume`, or as a confirmation the server ignores).
func (n *Node) sendEstopState() {
	if !n.isReady() {
		return
	}
	st := n.latch.State()
	if st.Remote && !st.Local {
		return
	}
	at := time.Now()
	if st.Local && !st.LocalAt.IsZero() {
		at = st.LocalAt
	}
	n.mu.Lock()
	var robot string
	if len(n.robotIDs) == 1 {
		robot = n.robotIDs[0]
	}
	n.mu.Unlock()
	n.send(protocol.EstopState{Latched: st.Local, By: "device", At: at.UTC().Format(isoMillis), RobotID: robot})
}

// isoMillis is the server's timestamp format, e.g. 2026-09-29T19:20:01.200Z.
const isoMillis = "2006-01-02T15:04:05.000Z07:00"

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
		n.mu.Lock()
		n.events = append(n.events, protocol.Event{Name: name, Driver: o.Plugin, TS: o.At.UnixMilli(), Fields: fields})
		n.mu.Unlock()
		n.flushTelemetry(true)
	case plugins.OutVideo:
		n.mu.Lock()
		j, from := n.jpeg, n.jpegFrom
		n.mu.Unlock()
		if j != nil && (from == "" || from == o.Plugin) {
			if p, ok := o.Data["path"].(string); ok {
				j.Push(p)
			}
		}
	case plugins.OutFault, plugins.OutState:
		n.sendStatus()
	}
}

// flushTelemetry sends at most one telemetry frame per telemetry interval (≥ 500 ms: ≤ 2 Hz). Events go out at the
// next 100 ms tick even inside the interval, so a cliff is not held back by the rate limit of sensor data. Nothing goes
// out before hello and config (Bot refuses it as bot.not_paired); telemetry and events stay buffered until then.
func (n *Node) flushTelemetry(eventsOnly bool) {
	if !n.isReady() {
		return
	}
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
		// Plugins report battery {volts, percent 0..100}; Bot reads a scalar charge 0..1 and the voltage.
		if b, ok := d["battery"].(map[string]any); ok && t.Battery == nil && t.Voltage == nil {
			if p := num(b["percent"]); p != nil {
				f := *p / 100
				t.Battery = &f
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
	Server    string             `json:"server,omitempty"`
	Paired    bool               `json:"paired"`
	Uptime    string             `json:"uptime"`
	Link      LinkStatus         `json:"link"`
	Latch     safety.LatchState  `json:"latch"`
	Limits    safety.Limits      `json:"limits"`
	Plugins   []PluginStatus     `json:"plugins"`
	Video     VideoStatus        `json:"video"`
	ConfigDir string             `json:"config_dir"`
	StateDir  string             `json:"state_dir"`
	Telemetry protocol.Telemetry `json:"telemetry"`
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
		s.Paired, s.DeviceID, s.Server = true, n.opt.Creds.DeviceID, n.opt.Creds.Server
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
	s.Video.State = video.StateOff
	if n.pub != nil {
		s.Video.State, s.Video.Frames, s.Video.LastError = n.pub.State(), n.pub.Frames(), n.pub.LastError()
	}
	return s
}

func (n *Node) sendStatus() {
	if !n.isReady() {
		return
	}
	st := n.latch.State()
	s := protocol.Status{Firmware: "openvibe-node-" + n.opt.Version, Capabilities: map[string]any{},
		AgentVersion: n.opt.Version, DeviceKind: n.opt.Config.DeviceKind, OS: runtime.GOOS, Arch: runtime.GOARCH,
		EstopLatched: st.Stopped(), LocalStop: st.Local, Drivers: []protocol.DriverStatus{}, Faults: []protocol.Fault{}}
	for _, i := range n.mgr.Infos() {
		d := protocol.DriverStatus{Name: i.Name, State: i.State}
		if i.Describe != nil {
			d.Driver, d.Version, d.Capabilities = i.Describe.Driver, i.Describe.Version, i.Describe.Capabilities
			for k, v := range i.Describe.Capabilities { // the first driver with a capability serves it
				if _, taken := s.Capabilities[k]; !taken {
					s.Capabilities[k] = v
				}
			}
		}
		s.Drivers = append(s.Drivers, d)
		if i.Fault != nil {
			s.Faults = append(s.Faults, *i.Fault)
		}
	}
	if n.pub != nil {
		s.Video = n.pub.State()
	}
	n.send(s)
}

func (n *Node) control(r localctl.Request) localctl.Response {
	switch r.Cmd {
	case localctl.CmdStop:
		n.mgr.EstopAll() // at once, then persist
		if _, err := n.latch.SetLocal(); err != nil {
			return localctl.Response{OK: true, Error: "stopped, but the latch could not be saved: " + err.Error()}
		}
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
