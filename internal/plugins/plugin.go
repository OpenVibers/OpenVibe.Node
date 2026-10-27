// Package plugins runs driver plugins: separate executables speaking JSON lines over stdin/stdout (docs/plugins.md).
//
// The supervisor starts each plugin, sends hello, waits for describe, sends a heartbeat every HeartbeatInterval,
// forwards commands and collects ack/nack, telemetry, events and video frames. A plugin that exits is restarted with
// exponential backoff; it starts stopped, and it is sent `estop` again at once if the latch is set.
package plugins

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/OpenVibers/OpenVibe.Node/internal/protocol"
)

// Timing. Variables so tests can shorten them.
var (
	HeartbeatInterval = 250 * time.Millisecond
	DescribeTimeout   = 15 * time.Second
	StopGrace         = 2 * time.Second
	BackoffMin        = 1 * time.Second
	BackoffMax        = 30 * time.Second
	StableAfter       = 60 * time.Second
	MinReplyTimeout   = 1 * time.Second
)

// Plugin states.
const (
	StateStarting = "starting"
	StateReady    = "ready"
	StateFaulted  = "faulted" // running but reported a fault (e.g. firmware unsupported, robot unreachable)
	StateDown     = "down"    // exited; waiting to restart
	StateStopped  = "stopped" // supervisor finished
)

// Spec says how to start a plugin.
type Spec struct {
	Name   string
	Argv   []string
	Env    []string // extra KEY=VALUE entries
	Dir    string
	Config map[string]any
}

// Describe is the plugin's first message.
type Describe struct {
	Driver       string         `json:"driver"`
	Version      string         `json:"version"`
	Protocol     int            `json:"protocol"`
	Capabilities map[string]any `json:"capabilities"`
	MotionKinds  []string       `json:"motion_kinds"`
}

// Reply is a plugin's answer to one command.
type Reply struct {
	OK        bool
	FaultCode string
	Message   string
	Data      json.RawMessage // an ack's data (a computer command's answer), as the plugin sent it
}

// Output kinds carried by Output.Kind.
const (
	OutTelemetry = "telemetry"
	OutEvent     = "event"
	OutVideo     = "video"
	OutFault     = "fault"
	OutState     = "state"
	// OutLocalStop: the driver latched the Node's local stop (its own stop button, a sensor it trusts). Only
	// `openvibe-node resume` on the machine releases it.
	OutLocalStop = "local_stop"
)

// Output is something a plugin said that the core forwards: telemetry, an event, a video frame, a fault or a state
// change. Data is the plugin's message without "op".
type Output struct {
	Plugin string
	Kind   string
	Data   map[string]any
	At     time.Time
}

// Plugin is one supervised driver process.
type Plugin struct {
	spec Spec
	log  *slog.Logger
	out  chan<- Output

	mu       sync.Mutex
	state    string
	desc     *Describe
	fault    *protocol.Fault
	estopped bool
	// The e-stop's cause and reason, sent again (replayed) to a restarted process while the stop is latched.
	estopCause, estopReason string
	// The newest turn_epoch a halt carried: hello tells a restarted process, so a stale turn stays refused there too.
	haltEpoch int64
	pending   map[string]chan Reply
	queue     chan line // to the current process's stdin; nil while down
	kill      func()
	restarts  int
	pid       int
}

func newPlugin(spec Spec, log *slog.Logger, out chan<- Output) *Plugin {
	return &Plugin{spec: spec, log: log.With("plugin", spec.Name), out: out, state: StateStarting,
		pending: map[string]chan Reply{}}
}

// Name is the configured name.
func (p *Plugin) Name() string { return p.spec.Name }

// Info is a snapshot for status.
type Info struct {
	Name     string
	State    string
	Describe *Describe
	Fault    *protocol.Fault
	Restarts int
	PID      int
}

func (p *Plugin) Info() Info {
	p.mu.Lock()
	defer p.mu.Unlock()
	return Info{Name: p.spec.Name, State: p.state, Describe: p.desc, Fault: p.fault, Restarts: p.restarts, PID: p.pid}
}

func (p *Plugin) setState(s string) {
	p.mu.Lock()
	changed := p.state != s
	p.state = s
	p.mu.Unlock()
	if changed {
		p.emit(Output{Kind: OutState, Data: map[string]any{"state": s}}, true)
	}
}

func (p *Plugin) emit(o Output, important bool) {
	o.Plugin, o.At = p.spec.Name, time.Now()
	if important {
		select {
		case p.out <- o:
		case <-time.After(200 * time.Millisecond):
			p.log.Warn("output channel full; dropped", "kind", o.Kind)
		}
		return
	}
	select {
	case p.out <- o:
	default:
	}
}

// line is one write to the plugin's stdin: bytes, or a timed command the writer renders at the moment it writes it.
type line struct {
	b   []byte
	cmd *timed
}

// timed is a command that expires at an instant on this machine. Lines can wait in the queue to the plugin while the
// writer is blocked on a full pipe (a plugin slow to read), so the writer, not the sender, stamps what is left: a timed
// command is written with deadline_ms = the whole milliseconds left at that moment and expires_at_mono_ns = the instant
// on the clock the plugin shares (monotonicNS), and is never written once less than a millisecond is left (Maticus,
// 2026-10-27: queue and pipe time must not extend a command's validity).
type timed struct {
	id      string
	msg     map[string]any
	expires time.Time // on this process's monotonic clock
	queued  time.Time
}

// render is the command's line as it is written now, or ok false when less than a millisecond is left.
func (c *timed) render() (b []byte, ok bool) {
	// The shared clock is read before the time left, so expires_at_mono_ns can only come out early.
	mono, hasMono := monotonicNS()
	left := time.Until(c.expires)
	if left < time.Millisecond {
		return nil, false
	}
	c.msg["deadline_ms"] = int64(left / time.Millisecond)
	if hasMono {
		c.msg["expires_at_mono_ns"] = mono + int64(left)
	}
	b, err := json.Marshal(c.msg)
	if err != nil {
		return nil, false
	}
	return append(b, '\n'), true
}

// expired answers a timed command that was never written: its time ran out in the queue to the plugin.
func (p *Plugin) expired(c *timed) {
	p.mu.Lock()
	ch := p.pending[c.id]
	p.mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- Reply{FaultCode: protocol.FaultExpired, Message: fmt.Sprintf("its deadline passed while it waited %d ms in the queue to the driver; it was never sent", time.Since(c.queued).Milliseconds())}:
	default:
	}
}

// send queues one line for the plugin. A plugin that stops reading its stdin is killed: it can no longer be told to
// stop, and its own heartbeat watchdog is the only thing left, so we make sure by ending it.
func (p *Plugin) send(msg map[string]any) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	return p.enqueue(line{b: append(b, '\n')})
}

func (p *Plugin) enqueue(l line) error {
	p.mu.Lock()
	q, kill := p.queue, p.kill
	p.mu.Unlock()
	if q == nil {
		return errors.New(protocol.FaultPluginDown)
	}
	select {
	case q <- l:
		return nil
	default:
		p.log.Error("plugin is not reading its input; killing it")
		if kill != nil {
			kill()
		}
		return errors.New(protocol.FaultPluginDown)
	}
}

// Why a plugin is told to stop (the `cause` of its stop and estop ops; docs/plugins.md "Why a plugin is stopped"). A
// `stop` is ordinary: revoke remote motion, latch nothing. An `estop` is the emergency: the platform's latch is set and
// stays set until the owner clears it, which a driver only hears as `resume` and must never treat as a re-arm.
const (
	StopHalt     = "halt"      // Bot's halt: the controlling turn ended (release, handoff, timeout, unbind)
	StopLinkLost = "link_lost" // the link to Bot was lost
	StopShutdown = "shutdown"  // the Node is stopping (service stop, `openvibe-node quit`)
	StopTimeout  = "timeout"   // a command got no answer from the driver within its window
	EstopRemote  = "remote"    // an e-stop from the platform (a robot owner or staff)
	EstopLocal   = "local"     // `openvibe-node stop` on the device
	EstopDriver  = "driver"    // a driver's own local stop (a plugin reported one)
	EstopLatched = "latched"   // the stop was already latched when the Node or this process started
)

// Stop tells the plugin to stop every actuator, saying why.
func (p *Plugin) Stop(cause string) { _ = p.send(map[string]any{"op": "stop", "cause": cause}) }

// Halt is Stop for a halt command: it carries the halt's turn_epoch (when Bot sent one), the newest epoch a driver
// should take commands from.
func (p *Plugin) Halt(turnEpoch int64) {
	msg := map[string]any{"op": "stop", "cause": StopHalt}
	if turnEpoch > 0 {
		msg["turn_epoch"] = turnEpoch
		p.mu.Lock()
		p.haltEpoch = max(p.haltEpoch, turnEpoch)
		p.mu.Unlock()
	}
	_ = p.send(msg)
}

// Estop latches the plugin's e-stop (the core also refuses motion itself), saying why.
func (p *Plugin) Estop(cause, reason string) {
	p.mu.Lock()
	p.estopped, p.estopCause, p.estopReason = true, cause, reason
	p.mu.Unlock()
	_ = p.send(map[string]any{"op": "estop", "cause": cause, "reason": reason})
}

// Resume tells the plugin the platform's e-stop latch was cleared by its owner. It is information, never a re-arm: the
// driver keeps its own hardware latch and grants as they are.
func (p *Plugin) Resume() {
	p.mu.Lock()
	p.estopped, p.estopCause, p.estopReason = false, "", ""
	p.mu.Unlock()
	_ = p.send(map[string]any{"op": "resume", "cause": "cleared"})
}

// Answer gives the plugin its owner's answer to an approval it asked for (`openvibe-node approve|deny <id>`).
func (p *Plugin) Answer(id string, approved bool) error {
	return p.send(map[string]any{"op": "approval", "id": id, "approved": approved})
}

// Voice tells a voice plugin whether its owner lets Vibe answer their voice here, and the call word to listen for.
func (p *Plugin) Voice(on bool, callWord string) error {
	return p.send(map[string]any{"op": "voice", "on": on, "call_word": callWord})
}

// VoiceAnswer gives a voice plugin Vibe's answer to a voice_ask it raised (to speak and show).
func (p *Plugin) VoiceAnswer(id, state, text, url string) error {
	return p.send(map[string]any{"op": "voice_answer", "id": id, "state": state, "text": text, "url": url})
}

// HasCapability says whether the plugin's describe lists the capability (false before it described itself).
func (p *Plugin) HasCapability(name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.desc == nil {
		return false
	}
	_, ok := p.desc.Capabilities[name]
	return ok
}

// Tray tells the plugin whether the Node's tray shows its approvals, for the next ttlS seconds unless told again.
func (p *Plugin) Tray(on bool, ttlS int) error {
	return p.send(map[string]any{"op": "tray", "attached": on, "ttl_s": ttlS})
}

// Command sends a command and waits for the plugin's ack or nack.
func (p *Plugin) Command(ctx context.Context, id, kind string, value json.RawMessage, deadlineMS int) Reply {
	return p.Begin(ctx, id, kind, value, deadlineMS)()
}

// Begin queues a command for the plugin at once (so commands reach it in arrival order) and returns a function that
// waits for its ack or nack. A plugin that does not answer in max(deadline, MinReplyTimeout) is told to stop and the
// command fails with plugin_timeout.
func (p *Plugin) Begin(ctx context.Context, id, kind string, value json.RawMessage, deadlineMS int) func() Reply {
	return p.BeginAs(ctx, id, kind, value, deadlineMS, nil)
}

// BeginAs is Begin with who sent the command, as Bot's gate saw them: the plugin gets it as `operator`
// {subject, role} (subject null for Bot's own commands, role "system"), and nothing when op is nil.
func (p *Plugin) BeginAs(ctx context.Context, id, kind string, value json.RawMessage, deadlineMS int, op *protocol.Operator) func() Reply {
	return p.BeginTurn(ctx, id, kind, value, deadlineMS, op, 0)
}

// BeginTurn is BeginAs with the robot's control epoch the command was decided under (protocol.Command.TurnEpoch), sent
// to the plugin as turn_epoch when it is set, so a driver can refuse a command older than the newest turn halt itself.
func (p *Plugin) BeginTurn(ctx context.Context, id, kind string, value json.RawMessage, deadlineMS int, op *protocol.Operator, turnEpoch int64) func() Reply {
	return p.begin(ctx, id, kind, value, deadlineMS, time.Time{}, op, turnEpoch, nil)
}

// Provenance is where a command came from, passed unchanged from Bot to the driver as `provenance` (plugins.md): the
// link session it arrived on (Bot's hello; a new one on every connection, and session ids sort by time), the frame's
// seq on that session (strictly increasing, with gaps), Bot's clock when it sent the command (and minted its id), and
// the absolute deadline on that clock for a motion kind. Bot sends a command once and never again under any id, so
// a driver can refuse a replay with constant state: the same session with a seq not above the last one it handled.
type Provenance struct {
	Session     string `json:"session,omitempty"`
	Seq         uint64 `json:"seq,omitempty"`
	IssuedAtMS  int64  `json:"issued_at_ms,omitempty"`
	ExpiresAtMS int64  `json:"expires_at_ms,omitempty"`
	// How far Node's estimate of the server's clock may trail it (the connection's fastest round trip): the deadline
	// was already checked against estimate + this, and a driver may apply a stricter cap of its own. Absent when the
	// Node has no link clock (local commands).
	ClockUncertaintyMS *int64 `json:"clock_uncertainty_ms,omitempty"`
}

// BeginWith is BeginTurn for a command that expires at an instant on this machine (the Node's receipt of it plus the
// time its deadline left), with its provenance (nil: none). The plugin gets what is left when the line is written to
// it, never more, and a command whose time runs out before then is answered expired without reaching the plugin.
func (p *Plugin) BeginWith(ctx context.Context, id, kind string, value json.RawMessage, expires time.Time, op *protocol.Operator, turnEpoch int64, prov *Provenance) func() Reply {
	return p.begin(ctx, id, kind, value, 0, expires, op, turnEpoch, prov)
}

// begin queues a command: with deadline_ms as given when expires is zero, else timed (BeginWith).
func (p *Plugin) begin(ctx context.Context, id, kind string, value json.RawMessage, deadlineMS int, expires time.Time, op *protocol.Operator, turnEpoch int64, prov *Provenance) func() Reply {
	now := func(r Reply) func() Reply { return func() Reply { return r } }
	p.mu.Lock()
	if p.state != StateReady && kind != protocol.KindHalt {
		st, fault := p.state, p.fault
		p.mu.Unlock()
		if st == StateFaulted && fault != nil {
			return now(Reply{FaultCode: fault.Code, Message: fault.Message})
		}
		if st == StateDown || st == StateStopped {
			return now(Reply{FaultCode: protocol.FaultPluginDown})
		}
		return now(Reply{FaultCode: protocol.FaultNotReady})
	}
	ch := make(chan Reply, 1)
	p.pending[id] = ch
	p.mu.Unlock()
	done := func() {
		p.mu.Lock()
		delete(p.pending, id)
		p.mu.Unlock()
	}
	msg := map[string]any{"op": "command", "id": id, "kind": kind, "deadline_ms": deadlineMS}
	if len(value) > 0 {
		msg["value"] = value
	}
	if op != nil {
		var subject any
		if op.Subject != "" {
			subject = op.Subject
		}
		msg["operator"] = map[string]any{"subject": subject, "role": op.Role}
	}
	if turnEpoch > 0 {
		msg["turn_epoch"] = turnEpoch
	}
	if prov != nil {
		msg["provenance"] = prov
	}
	var err error
	if expires.IsZero() {
		err = p.send(msg)
	} else {
		err = p.enqueue(line{cmd: &timed{id: id, msg: msg, expires: expires, queued: time.Now()}})
	}
	if err != nil {
		done()
		return now(Reply{FaultCode: protocol.FaultPluginDown})
	}
	return func() Reply {
		defer done()
		wait := time.Duration(deadlineMS) * time.Millisecond
		if !expires.IsZero() {
			wait = time.Until(expires)
		}
		if wait < MinReplyTimeout {
			wait = MinReplyTimeout
		}
		t := time.NewTimer(wait)
		defer t.Stop()
		select {
		case r := <-ch:
			return r
		case <-t.C:
			p.Stop(StopTimeout)
			return Reply{FaultCode: protocol.FaultPluginTimeout}
		case <-ctx.Done():
			p.Stop(StopShutdown)
			return Reply{FaultCode: protocol.FaultShuttingDown}
		}
	}
}

func (p *Plugin) failPending(code string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, ch := range p.pending {
		select {
		case ch <- Reply{FaultCode: code}:
		default:
		}
		delete(p.pending, id)
	}
}

// run supervises the plugin until ctx ends.
func (p *Plugin) run(ctx context.Context) {
	backoff := BackoffMin
	for {
		started := time.Now()
		err := p.runOnce(ctx)
		p.failPending(protocol.FaultPluginDown)
		if ctx.Err() != nil {
			p.setState(StateStopped)
			return
		}
		p.mu.Lock()
		p.restarts++
		p.fault = &protocol.Fault{Code: protocol.FaultPluginDown, Driver: p.spec.Name, Message: errString(err)}
		p.mu.Unlock()
		p.setState(StateDown)
		p.emit(Output{Kind: OutFault, Data: map[string]any{"fault_code": protocol.FaultPluginDown, "message": errString(err)}}, true)
		if time.Since(started) > StableAfter {
			backoff = BackoffMin
		}
		p.log.Warn("plugin exited; restarting", "err", errString(err), "in", backoff)
		select {
		case <-ctx.Done():
			p.setState(StateStopped)
			return
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > BackoffMax {
			backoff = BackoffMax
		}
	}
}

func errString(err error) string {
	if err == nil {
		return "exited"
	}
	return err.Error()
}

func (p *Plugin) runOnce(ctx context.Context) error {
	if len(p.spec.Argv) == 0 {
		return errors.New("no command")
	}
	p.setState(StateStarting)
	cmd := exec.Command(p.spec.Argv[0], p.spec.Argv[1:]...)
	cmd.Dir = p.spec.Dir
	cmd.Env = append(os.Environ(), p.spec.Env...)
	cmd.Env = append(cmd.Env, "PYTHONUNBUFFERED=1")
	setProcAttr(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", p.spec.Argv[0], err)
	}
	queue := make(chan line, 256)
	var killOnce sync.Once
	kill := func() { killOnce.Do(func() { killProcess(cmd) }) }
	p.mu.Lock()
	p.queue, p.kill, p.pid, p.fault, p.desc = queue, kill, cmd.Process.Pid, nil, nil
	estopped, estopCause, estopReason, haltEpoch := p.estopped, p.estopCause, p.estopReason, p.haltEpoch
	p.mu.Unlock()

	// Writer: the only goroutine touching stdin. Closing stdin is the plugin's signal to stop and exit.
	writerDone := make(chan struct{})
	stopWriter := make(chan struct{})
	write := func(l line) error {
		b := l.b
		if l.cmd != nil {
			var ok bool
			if b, ok = l.cmd.render(); !ok {
				p.expired(l.cmd)
				return nil
			}
		}
		_, err := stdin.Write(b)
		return err
	}
	go func() {
		defer close(writerDone)
		defer stdin.Close()
		for {
			select {
			case l := <-queue:
				if err := write(l); err != nil {
					return
				}
			case <-stopWriter:
				// Drain what is queued (a final stop) and close.
				for {
					select {
					case l := <-queue:
						if err := write(l); err != nil {
							return
						}
					default:
						return
					}
				}
			}
		}
	}()
	go p.pipeStderr(stderr)

	described := make(chan struct{})
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		p.readStdout(stdout, described)
	}()

	hello := map[string]any{"op": "hello", "config": nonNil(p.spec.Config), "probe": false}
	if haltEpoch > 0 {
		hello["halt_epoch"] = haltEpoch
	}
	_ = p.send(hello)
	if estopped {
		// Still latched: the new process hears the same emergency again, marked as a replay, never a fresh one.
		_ = p.send(map[string]any{"op": "estop", "cause": EstopLatched, "reason": estopReason, "first_cause": estopCause, "replayed": true})
	}

	// Watch the plugin process itself, not its stdout: a child it started (speech, a camera helper) can hold stdout
	// open after the plugin died. When the plugin exits, its whole process group is killed, which also frees any
	// motor bus or serial port a child still holds, and ends the stdout reader.
	exited := make(chan error, 1)
	go func() {
		ps, err := cmd.Process.Wait()
		kill()
		select {
		case <-readerDone:
		case <-time.After(2 * time.Second):
		}
		stdout.Close()
		stderr.Close()
		<-readerDone
		if err == nil && ps != nil && !ps.Success() {
			err = errors.New(ps.String())
		}
		exited <- err
	}()

	hb := time.NewTicker(HeartbeatInterval)
	defer hb.Stop()
	describeTimer := time.NewTimer(DescribeTimeout)
	defer describeTimer.Stop()
	shutdown := func() error {
		_ = p.send(map[string]any{"op": "stop", "cause": StopShutdown})
		p.mu.Lock()
		p.queue = nil
		p.mu.Unlock()
		close(stopWriter)
		select {
		case err := <-exited:
			return err
		case <-time.After(StopGrace):
			kill()
			return <-exited
		}
	}
	for {
		select {
		case <-ctx.Done():
			return shutdown()
		case err := <-exited:
			p.mu.Lock()
			p.queue = nil
			p.mu.Unlock()
			close(stopWriter)
			<-writerDone
			if err == nil {
				err = errors.New("plugin exited")
			}
			return err
		case <-hb.C:
			_ = p.send(map[string]any{"op": "heartbeat", "t": time.Now().UnixMilli()})
		case <-described:
			described = nil
			describeTimer.Stop()
		case <-describeTimer.C:
			if described != nil {
				p.log.Error("plugin did not describe itself in time; killing it")
				kill()
			}
		}
	}
}

func nonNil(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

func (p *Plugin) pipeStderr(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		p.log.Info(sc.Text(), "stream", "stderr")
	}
}

func (p *Plugin) readStdout(r io.Reader, described chan struct{}) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	gotDescribe := false
	for sc.Scan() {
		line := sc.Bytes()
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			p.log.Warn("plugin wrote a line that is not JSON", "line", truncate(string(line), 200))
			continue
		}
		op, _ := m["op"].(string)
		delete(m, "op")
		switch op {
		case "describe":
			var d Describe
			b, _ := json.Marshal(m)
			_ = json.Unmarshal(b, &d)
			p.mu.Lock()
			p.desc = &d
			p.mu.Unlock()
			if !gotDescribe {
				gotDescribe = true
				close(described)
			}
			p.log.Info("plugin described", "driver", d.Driver, "version", d.Version)
		case "ready":
			// Ready again after a fault (the robot came back): the fault is over, so status stops reporting it.
			p.mu.Lock()
			p.fault = nil
			p.mu.Unlock()
			p.setState(StateReady)
		case "ack", "nack":
			id, _ := m["id"].(string)
			code, _ := m["fault_code"].(string)
			msg, _ := m["message"].(string)
			var data json.RawMessage
			if d, ok := m["data"]; ok && op == "ack" && d != nil {
				data, _ = json.Marshal(d)
			}
			p.mu.Lock()
			ch := p.pending[id]
			p.mu.Unlock()
			if ch != nil {
				select {
				case ch <- Reply{OK: op == "ack", FaultCode: code, Message: msg, Data: data}:
				default:
				}
			}
		case "fault":
			code, _ := m["fault_code"].(string)
			msg, _ := m["message"].(string)
			if code == "" {
				code = protocol.FaultHardware
			}
			p.mu.Lock()
			p.fault = &protocol.Fault{Code: code, Driver: p.spec.Name, Message: msg}
			p.mu.Unlock()
			p.setState(StateFaulted)
			p.emit(Output{Kind: OutFault, Data: m}, true)
			p.log.Error("plugin fault", "code", code, "message", msg)
		case "telemetry":
			if fl, ok := m["faults"].([]any); ok && len(fl) > 0 {
				p.log.Debug("plugin reports faults", "faults", fl)
			}
			p.emit(Output{Kind: OutTelemetry, Data: m}, false)
		case "event":
			p.emit(Output{Kind: OutEvent, Data: m}, true)
		case "local_stop":
			reason, _ := m["reason"].(string)
			p.log.Warn("plugin latched the local stop", "reason", truncate(reason, 120))
			p.emit(Output{Kind: OutLocalStop, Data: m}, true)
		case "video":
			p.emit(Output{Kind: OutVideo, Data: m}, false)
		default:
			p.log.Warn("unknown op from plugin", "op", op)
		}
	}
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// Probe starts a plugin in probe mode and returns its describe without letting it touch hardware.
func Probe(ctx context.Context, spec Spec) (*Describe, error) {
	ctx, cancel := context.WithTimeout(ctx, DescribeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, spec.Argv[0], spec.Argv[1:]...)
	cmd.Dir = spec.Dir
	cmd.Env = append(append(os.Environ(), spec.Env...), "PYTHONUNBUFFERED=1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	defer func() { _ = cmd.Wait() }()
	defer stdin.Close()
	b, _ := json.Marshal(map[string]any{"op": "hello", "config": nonNil(spec.Config), "probe": true})
	if _, err := stdin.Write(append(b, '\n')); err != nil {
		return nil, err
	}
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var m struct {
			Op string `json:"op"`
			Describe
		}
		if json.Unmarshal(sc.Bytes(), &m) == nil && m.Op == "describe" {
			d := m.Describe
			return &d, nil
		}
	}
	if ctx.Err() != nil {
		return nil, errors.New("plugin did not describe itself in time")
	}
	return nil, errors.New("plugin exited without describing itself")
}
