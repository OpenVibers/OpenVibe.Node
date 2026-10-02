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

	// configured is set by the first config on a connection; until then only halt runs, so a latch the owner set
	// while the link was down is applied before any motion.
	configured bool
	robotIDs   []string // from this connection's hello
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

// Connected sends nothing: Bot sends config right after hello, and the first status and estop_state go out once it
// has been applied (applyConfig), so they report the latch the owner may have set while the link was down.
func (n *Node) Connected() {
	n.mu.Lock()
	n.configured, n.robotIDs = false, nil
	n.mu.Unlock()
}

func (n *Node) Disconnected(err error) {
	n.log.Warn("link lost: stopping every actuator", "err", err)
	n.mgr.StopAll()
	n.mu.Lock()
	n.configured = false
	n.mu.Unlock()
}

func (n *Node) Frame(f protocol.Frame) {
	switch m := f.Msg.(type) {
	case protocol.Hello:
		n.mu.Lock()
		n.robotIDs = m.RobotIDs
		n.mu.Unlock()
		n.log.Info("server hello", "device", m.DeviceID, "robots", m.RobotIDs, "session", m.SessionID)
	case protocol.Config:
		n.applyConfig(m)
	case protocol.Command:
		n.command(m, f.TS, time.Now())
	case protocol.Estop:
		// latched:false is the owner's clear. It clears only the remote latch: a local (CLI or file) stop stays.
		if m.Latched {
			n.remoteEstop("by "+m.By, m.By)
		} else {
			n.remoteClear(m.By)
		}
		n.sendEstopState()
	}
}

func (n *Node) remoteEstop(reason, by string) {
	n.log.Warn("e-stop from the server", "by", by)
	n.mgr.EstopAll() // at once, before the disk write
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
}

// command validates and routes one command. The synchronous part (checks, clamping, queueing to the plugin) keeps
// commands in arrival order; the reply is awaited on its own goroutine. deadline_ms is an absolute instant on the
// server's clock: it is compared with the link's estimate of that clock at receipt (serverNow), so a command that
// arrives after its deadline is nacked and never reaches a plugin, and a plugin gets only the time that is left.
func (n *Node) command(c protocol.Command, sentMS int64, received time.Time) {
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
	limits, allowed, configured := n.limits, n.allowed, n.configured
	n.mu.Unlock()
	if c.Kind == protocol.KindHalt {
		n.mgr.StopAll()
		r := protocol.Ack{ID: c.ID, LatencyMS: time.Since(received).Milliseconds()}
		n.dedup.Complete(entry, r)
		n.send(r)
		return
	}
	if !configured {
		nack(protocol.FaultNotReady, "the server's config has not arrived on this connection yet")
		return
	}
	if allowed != nil && !allowed[c.Kind] {
		nack(protocol.FaultNotAllowed, c.Kind+" is not in this robot's allowed_commands")
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
	if c.DeadlineMS > 0 {
		left := c.DeadlineMS - n.serverNow(sentMS, received)
		if left <= 0 {
			nack(protocol.FaultExpired, fmt.Sprintf("the deadline passed %d ms before the command arrived", -left))
			return
		}
		deadline = limits.Deadline(int(min(left, int64(limits.MaxCommandMS)+1)))
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
	wait := p.Begin(n.ctx, c.ID, c.Kind, value, deadline)
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

// serverNow is the server's clock when a frame stamped sentMS was received: the link's estimate, else the frame's
// own ts.
func (n *Node) serverNow(sentMS int64, received time.Time) int64 {
	if n.link != nil {
		if ms, ok := n.link.ServerNow(received); ok {
			return ms
		}
	}
	if sentMS > 0 {
		return sentMS
	}
	return received.UnixMilli()
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

// sendEstopState reports the device's latch. Bot reads latched:true as "this device is stopped" and latches the robot
// (so the local kill switch shows on the panel and the gate stops sending motion); latched:false is a report only
// and never clears the owner's latch.
func (n *Node) sendEstopState() {
	if !n.reporting() {
		return
	}
	st := n.latch.State()
	// robot_id names the robot only when the device serves exactly one; a bridge for several leaves it out.
	var robot string
	n.mu.Lock()
	if len(n.robotIDs) == 1 {
		robot = n.robotIDs[0]
	}
	n.mu.Unlock()
	n.send(protocol.EstopState{Latched: st.Stopped(), By: "device", At: time.Now().UTC().Format(time.RFC3339Nano),
		RobotID: robot, LocalStop: st.Local, Reason: st.RemoteReason})
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
