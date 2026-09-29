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
}

// Output kinds carried by Output.Kind.
const (
	OutTelemetry = "telemetry"
	OutEvent     = "event"
	OutVideo     = "video"
	OutFault     = "fault"
	OutState     = "state"
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
	pending  map[string]chan Reply
	queue    chan []byte // to the current process's stdin; nil while down
	kill     func()
	restarts int
	pid      int
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

// send queues one line for the plugin. A plugin that stops reading its stdin is killed: it can no longer be told to
// stop, and its own heartbeat watchdog is the only thing left, so we make sure by ending it.
func (p *Plugin) send(msg map[string]any) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	p.mu.Lock()
	q, kill := p.queue, p.kill
	p.mu.Unlock()
	if q == nil {
		return errors.New(protocol.FaultPluginDown)
	}
	select {
	case q <- b:
		return nil
	default:
		p.log.Error("plugin is not reading its input; killing it")
		if kill != nil {
			kill()
		}
		return errors.New(protocol.FaultPluginDown)
	}
}

// Stop tells the plugin to stop every actuator.
func (p *Plugin) Stop() { _ = p.send(map[string]any{"op": "stop"}) }

// Estop latches the plugin's e-stop (the core also refuses motion itself).
func (p *Plugin) Estop() {
	p.mu.Lock()
	p.estopped = true
	p.mu.Unlock()
	_ = p.send(map[string]any{"op": "estop"})
}

// Resume clears the plugin's e-stop.
func (p *Plugin) Resume() {
	p.mu.Lock()
	p.estopped = false
	p.mu.Unlock()
	_ = p.send(map[string]any{"op": "resume"})
}

// Command sends a command and waits for the plugin's ack or nack.
func (p *Plugin) Command(ctx context.Context, id, kind string, value json.RawMessage, deadlineMS int) Reply {
	return p.Begin(ctx, id, kind, value, deadlineMS)()
}

// Begin queues a command for the plugin at once (so commands reach it in arrival order) and returns a function that
// waits for its ack or nack. A plugin that does not answer in max(deadline, MinReplyTimeout) is told to stop and the
// command fails with plugin_timeout.
func (p *Plugin) Begin(ctx context.Context, id, kind string, value json.RawMessage, deadlineMS int) func() Reply {
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
	if err := p.send(msg); err != nil {
		done()
		return now(Reply{FaultCode: protocol.FaultPluginDown})
	}
	return func() Reply {
		defer done()
		wait := time.Duration(deadlineMS) * time.Millisecond
		if wait < MinReplyTimeout {
			wait = MinReplyTimeout
		}
		t := time.NewTimer(wait)
		defer t.Stop()
		select {
		case r := <-ch:
			return r
		case <-t.C:
			p.Stop()
			return Reply{FaultCode: protocol.FaultPluginTimeout}
		case <-ctx.Done():
			p.Stop()
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
	queue := make(chan []byte, 256)
	var killOnce sync.Once
	kill := func() { killOnce.Do(func() { killProcess(cmd) }) }
	p.mu.Lock()
	p.queue, p.kill, p.pid, p.fault, p.desc = queue, kill, cmd.Process.Pid, nil, nil
	estopped := p.estopped
	p.mu.Unlock()

	// Writer: the only goroutine touching stdin. Closing stdin is the plugin's signal to stop and exit.
	writerDone := make(chan struct{})
	stopWriter := make(chan struct{})
	go func() {
		defer close(writerDone)
		defer stdin.Close()
		for {
			select {
			case b := <-queue:
				if _, err := stdin.Write(b); err != nil {
					return
				}
			case <-stopWriter:
				// Drain what is queued (a final stop) and close.
				for {
					select {
					case b := <-queue:
						if _, err := stdin.Write(b); err != nil {
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

	_ = p.send(map[string]any{"op": "hello", "config": nonNil(p.spec.Config), "probe": false})
	if estopped {
		_ = p.send(map[string]any{"op": "estop"})
	}

	exited := make(chan error, 1)
	go func() {
		<-readerDone
		exited <- cmd.Wait()
	}()

	hb := time.NewTicker(HeartbeatInterval)
	defer hb.Stop()
	describeTimer := time.NewTimer(DescribeTimeout)
	defer describeTimer.Stop()
	shutdown := func() error {
		_ = p.send(map[string]any{"op": "stop"})
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
			p.setState(StateReady)
		case "ack", "nack":
			id, _ := m["id"].(string)
			code, _ := m["fault_code"].(string)
			msg, _ := m["message"].(string)
			p.mu.Lock()
			ch := p.pending[id]
			p.mu.Unlock()
			if ch != nil {
				select {
				case ch <- Reply{OK: op == "ack", FaultCode: code, Message: msg}:
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
