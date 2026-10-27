package plugins

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/OpenVibers/OpenVibe.Node/internal/protocol"
)

// The test binary doubles as a fake plugin: with OPENVIBE_FAKE_PLUGIN set it speaks the plugin protocol and appends
// everything it receives to the file named by that variable.
func TestMain(m *testing.M) {
	if path := os.Getenv("OPENVIBE_FAKE_PLUGIN"); path != "" {
		fakePlugin(path, os.Stdin, os.Stdout)
		os.Exit(0)
	}
	HeartbeatInterval = 20 * time.Millisecond
	BackoffMin = 20 * time.Millisecond
	BackoffMax = 100 * time.Millisecond
	StopGrace = 500 * time.Millisecond
	os.Exit(m.Run())
}

func fakePlugin(logPath string, in io.Reader, out io.Writer) {
	f, _ := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	defer f.Close()
	// No fsync: the tests read the log through the OS, and fsync per line takes tens of ms on Windows, enough to push
	// replies to 50 concurrent commands past MinReplyTimeout.
	rec := func(s string) { fmt.Fprintln(f, s) }
	w := json.NewEncoder(out)
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20) // a filler line can be larger than a pipe
	mode := os.Getenv("OPENVIBE_FAKE_MODE")
	for sc.Scan() {
		// What a command had left when its line was read, on the clock shared with the core.
		mono, hasMono := monotonicNS()
		var m map[string]any
		d := json.NewDecoder(strings.NewReader(sc.Text()))
		d.UseNumber()
		d.Decode(&m)
		op, _ := m["op"].(string)
		if op != "heartbeat" {
			if len(sc.Text()) > 4096 {
				rec(fmt.Sprintf("LONG %v", m["id"]))
			} else {
				rec(sc.Text())
			}
		}
		if exp, ok := m["expires_at_mono_ns"].(json.Number); ok && hasMono {
			e, _ := exp.Int64()
			rec(fmt.Sprintf("LEFT %v %d", m["id"], e-mono))
		}
		switch op {
		case "hello":
			if mode == "mute" {
				continue
			}
			caps := map[string]any{"drive": map[string]any{}, "actuator": map[string]any{"names": []string{"lift"}}}
			if mode == "deskhost" { // a desk host's computer plugin: desks and files, no screen or input of its own
				caps = map[string]any{"desk": map[string]any{"screen": true, "input": true}, "files": map[string]any{}}
			}
			if mode == "aihost" { // the ai plugin: the person's own models
				caps = map[string]any{"ai": map[string]any{"models": []string{"llama3.1:8b"}, "ready": true}}
			}
			w.Encode(map[string]any{"op": "describe", "driver": "fake", "version": "1", "capabilities": caps})
			if m["probe"] == true {
				continue
			}
			if mode == "fault" {
				w.Encode(map[string]any{"op": "fault", "fault_code": "firmware_unsupported", "message": "v1 not v2"})
				continue
			}
			if mode == "recover" {
				w.Encode(map[string]any{"op": "fault", "fault_code": "not_connected", "message": "robot unreachable"})
			}
			w.Encode(map[string]any{"op": "ready"})
			w.Encode(map[string]any{"op": "event", "name": "booted"})
			if mode == "local_stop" {
				w.Encode(map[string]any{"op": "local_stop", "reason": "stop button"})
			}
		case "command":
			switch m["kind"] {
			case "crash":
				rec("CRASH")
				os.Exit(3)
			case "hang":
				time.Sleep(10 * time.Second)
			case "pause":
				// Stop reading stdin for a while, as a plugin stuck in a handler would.
				ms, _ := m["value"].(map[string]any)["ms"].(json.Number).Int64()
				time.Sleep(time.Duration(ms) * time.Millisecond)
				w.Encode(map[string]any{"op": "ack", "id": m["id"]})
			case "slow":
				continue
			case "display":
				w.Encode(map[string]any{"op": "nack", "id": m["id"], "fault_code": "unsupported"})
			default:
				w.Encode(map[string]any{"op": "ack", "id": m["id"]})
				w.Encode(map[string]any{"op": "telemetry", "sensors": map[string]any{"last": m["kind"]}})
			}
		}
	}
	rec("EOF-STOP")
}

type fixture struct {
	t    *testing.T
	log  string
	m    *Manager
	stop context.CancelFunc
	outs chan Output
	seen []Output
}

func start(t *testing.T, mode string) *fixture {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), "plugin.log")
	exe, _ := os.Executable()
	spec := Spec{Name: "fake", Argv: []string{exe}, Env: []string{"OPENVIBE_FAKE_PLUGIN=" + logPath, "OPENVIBE_FAKE_MODE=" + mode},
		Config: map[string]any{"k": "v"}}
	m := NewManager([]Spec{spec}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	m.Start(ctx)
	fx := &fixture{t: t, log: logPath, m: m, stop: cancel, outs: make(chan Output, 1024)}
	go func() {
		for o := range m.Output() {
			fx.outs <- o
		}
	}()
	t.Cleanup(func() { cancel(); m.Wait() })
	return fx
}

func (fx *fixture) waitState(s string) {
	fx.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if fx.m.Get("fake").Info().State == s {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	fx.t.Fatalf("state %s never reached (now %s)", s, fx.m.Get("fake").Info().State)
}

func (fx *fixture) lines() []string {
	b, _ := os.ReadFile(fx.log)
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func (fx *fixture) waitLog(substr string) {
	fx.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		b, _ := os.ReadFile(fx.log)
		if strings.Contains(string(b), substr) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	fx.t.Fatalf("plugin never logged %q; got:\n%s", substr, strings.Join(fx.lines(), "\n"))
}

func (fx *fixture) waitOutput(kind string) Output {
	fx.t.Helper()
	for i, o := range fx.seen {
		if o.Kind == kind {
			fx.seen = append(fx.seen[:i:i], fx.seen[i+1:]...)
			return o
		}
	}
	timeout := time.After(5 * time.Second)
	for {
		select {
		case o := <-fx.outs:
			if o.Kind == kind {
				return o
			}
			fx.seen = append(fx.seen, o)
		case <-timeout:
			fx.t.Fatalf("no %s output", kind)
		}
	}
}

func TestHelloDescribeCommand(t *testing.T) {
	fx := start(t, "")
	fx.waitState(StateReady)
	p := fx.m.Get("fake")
	if d := p.Info().Describe; d == nil || d.Driver != "fake" {
		t.Fatalf("describe %+v", d)
	}
	fx.waitLog(`"config":{"k":"v"}`)
	r := p.Command(context.Background(), "c1", "drive", json.RawMessage(`{"throttle":0.5}`), 300)
	if !r.OK {
		t.Fatalf("%+v", r)
	}
	fx.waitLog(`"deadline_ms":300`)
	fx.waitLog(`"value":{"throttle":0.5}`)
	if r := p.BeginAs(context.Background(), "c1b", "drive", nil, 300, &protocol.Operator{Subject: "usr_1", Role: "queue"})(); !r.OK {
		t.Fatalf("%+v", r)
	}
	fx.waitLog(`"operator":{"role":"queue","subject":"usr_1"}`)
	if r := p.BeginAs(context.Background(), "c1c", "drive", nil, 300, &protocol.Operator{Role: "system"})(); !r.OK {
		t.Fatalf("%+v", r)
	}
	fx.waitLog(`"operator":{"role":"system","subject":null}`)
	if r := p.BeginTurn(context.Background(), "c1d", "drive", nil, 300, &protocol.Operator{Subject: "usr_1", Role: "queue"}, 1792900000123)(); !r.OK {
		t.Fatalf("%+v", r)
	}
	fx.waitLog(`"turn_epoch":1792900000123`)
	if r := p.Command(context.Background(), "c2", "display", nil, 300); r.OK || r.FaultCode != "unsupported" {
		t.Fatalf("%+v", r)
	}
	if o := fx.waitOutput(OutTelemetry); o.Data["sensors"] == nil {
		t.Fatalf("%+v", o)
	}
	if o := fx.waitOutput(OutEvent); o.Data["name"] != "booted" {
		t.Fatalf("%+v", o)
	}
}

// A plugin that faults (the robot unreachable) and then reports ready again (it came back) carries no fault: status
// stops reporting it and commands reach the plugin again.
func TestReadyAfterFaultClearsIt(t *testing.T) {
	fx := start(t, "recover")
	fx.waitState(StateReady)
	p := fx.m.Get("fake")
	deadline := time.Now().Add(5 * time.Second)
	for p.Info().Fault != nil && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if f := p.Info().Fault; f != nil || p.Info().State != StateReady {
		t.Fatalf("after ready: state %s, fault %+v", p.Info().State, f)
	}
	if r := p.Command(context.Background(), "c1", "drive", json.RawMessage(`{"throttle":0.2}`), 300); !r.OK {
		t.Fatalf("a command after recovery: %+v", r)
	}
}

func TestLocalStopIsForwarded(t *testing.T) {
	fx := start(t, "local_stop")
	fx.waitState(StateReady)
	if o := fx.waitOutput(OutLocalStop); o.Data["reason"] != "stop button" || o.Plugin != "fake" {
		t.Fatalf("%+v", o)
	}
}

func TestRouting(t *testing.T) {
	fx := start(t, "")
	fx.waitState(StateReady)
	if fx.m.Route("drive", "", "") == nil || fx.m.Route("actuator", "", "lift") == nil {
		t.Fatal("not routed")
	}
	if fx.m.Route("say", "", "") != nil {
		t.Fatal("say routed to a plugin without it")
	}
	if fx.m.Route("drive", "nope", "") != nil {
		t.Fatal("unknown target routed")
	}
}

// A desk host offers no screen or input of its own, only its desks': screen, screen_stream and input still reach the
// plugin with desks (it acts on the desk named, or refuses the machine's own screen); other kinds are not routed there.
func TestRoutingOnADeskHost(t *testing.T) {
	fx := start(t, "deskhost")
	fx.waitState(StateReady)
	for _, kind := range []string{"screen", "screen_stream", "input", "desk", "fs"} {
		if fx.m.Route(kind, "", "") == nil {
			t.Fatalf("%s not routed on a desk host", kind)
		}
	}
	for _, kind := range []string{"exec", "browser", "drive"} {
		if fx.m.Route(kind, "", "") != nil {
			t.Fatalf("%s routed to a plugin that does not offer it", kind)
		}
	}
}

// Kind ai (contracts 0.219.0) reaches the plugin that offers "ai", and only it.
func TestRoutingAIToTheModelPlugin(t *testing.T) {
	fx := start(t, "aihost")
	fx.waitState(StateReady)
	if fx.m.Route("ai", "", "") == nil {
		t.Fatal("ai not routed to the plugin that offers it")
	}
	for _, kind := range []string{"exec", "screen", "drive"} {
		if fx.m.Route(kind, "", "") != nil {
			t.Fatalf("%s routed to the ai plugin", kind)
		}
	}
}

func TestShutdownStopsAndClosesStdin(t *testing.T) {
	fx := start(t, "")
	fx.waitState(StateReady)
	fx.stop()
	fx.m.Wait()
	l := fx.lines()
	if l[len(l)-2] != `{"cause":"shutdown","op":"stop"}` || l[len(l)-1] != "EOF-STOP" {
		t.Fatalf("want stop then EOF, got %v", l)
	}
}

func TestCrashRestartsWithEstop(t *testing.T) {
	fx := start(t, "")
	fx.waitState(StateReady)
	p := fx.m.Get("fake")
	p.Estop(EstopRemote, "staff: test")
	fx.waitLog(`{"cause":"remote","op":"estop","reason":"staff: test"}`)
	r := p.Command(context.Background(), "boom", "crash", nil, 300)
	if r.OK || r.FaultCode != protocol.FaultPluginDown {
		t.Fatalf("%+v", r)
	}
	fx.waitOutput(OutFault)
	fx.waitState(StateReady)
	if p.Info().Restarts < 1 {
		t.Fatal("no restart counted")
	}
	// After the restart the latched e-stop is sent again right after hello, as a replay of the same emergency.
	l := fx.lines()
	var afterCrash []string
	for i, s := range l {
		if s == "CRASH" {
			afterCrash = l[i+1:]
		}
	}
	replay := `{"cause":"latched","first_cause":"remote","op":"estop","reason":"staff: test","replayed":true}`
	if len(afterCrash) < 2 || !strings.Contains(afterCrash[0], `"op":"hello"`) || afterCrash[1] != replay {
		t.Fatalf("after restart: %v", afterCrash)
	}
}

// A latch set before the manager starts (the Node booting with its stop latched on disk) is the first thing each
// process hears after hello: the e-stop replayed, naming what set it, before the plugin can be routed any command.
func TestLatchSetBeforeStartIsReplayedFirst(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "plugin.log")
	exe, _ := os.Executable()
	spec := Spec{Name: "fake", Argv: []string{exe}, Env: []string{"OPENVIBE_FAKE_PLUGIN=" + logPath, "OPENVIBE_FAKE_MODE="}}
	m := NewManager([]Spec{spec}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.EstopAll(EstopRemote, "staff: it wobbled")
	ctx, cancel := context.WithCancel(context.Background())
	m.Start(ctx)
	t.Cleanup(func() { cancel(); m.Wait() })
	fx := &fixture{t: t, log: logPath, m: m}
	fx.waitState(StateReady)
	if r := m.Get("fake").Command(context.Background(), "after", "say", nil, 300); !r.OK {
		t.Fatalf("%+v", r)
	}
	l := fx.lines()
	replay := `{"cause":"latched","first_cause":"remote","op":"estop","reason":"staff: it wobbled","replayed":true}`
	if len(l) < 3 || !strings.Contains(l[0], `"op":"hello"`) || l[1] != replay || !strings.Contains(l[2], `"id":"after"`) {
		t.Fatalf("lines: %v", l)
	}
}

func TestReplyTimeout(t *testing.T) {
	fx := start(t, "")
	fx.waitState(StateReady)
	old := MinReplyTimeout
	MinReplyTimeout = 100 * time.Millisecond
	defer func() { MinReplyTimeout = old }()
	r := fx.m.Get("fake").Command(context.Background(), "s", "slow", nil, 50)
	if r.FaultCode != protocol.FaultPluginTimeout {
		t.Fatalf("%+v", r)
	}
	fx.waitLog(`{"cause":"timeout","op":"stop"}`)
}

// Time a command waits in the queue to the plugin counts against it (Maticus, 2026-10-27). The plugin stops reading
// for 400 ms and a line larger than the pipe fills it, so the writer is blocked and two motion commands wait in the
// queue: the one with 100 ms is answered expired and never written; the one with 1.5 s is written with only what is
// left (deadline_ms and expires_at_mono_ns, counted down to the moment it was written).
func TestWriterQueueDelayCountsAgainstTheDeadline(t *testing.T) {
	fx := start(t, "")
	fx.waitState(StateReady)
	p := fx.m.Get("fake")
	ctx := context.Background()
	held := time.Now()
	pause := p.Begin(ctx, "hold", "pause", json.RawMessage(`{"ms":400}`), 3000)
	fx.waitLog(`"id":"hold"`)
	filler, _ := json.Marshal(map[string]any{"text": strings.Repeat("x", 1<<20)})
	fill := p.Begin(ctx, "fill", "say", filler, 3000)
	queued := time.Now()
	late := p.BeginWith(ctx, "late", "drive", nil, queued.Add(100*time.Millisecond), nil, 0, nil)
	ontime := p.BeginWith(ctx, "ontime", "drive", nil, queued.Add(1500*time.Millisecond), nil, 0, nil)
	if r := late(); r.FaultCode != protocol.FaultExpired || !strings.Contains(r.Message, "never sent") {
		t.Fatalf("late: %+v", r)
	}
	if r := ontime(); !r.OK {
		t.Fatalf("ontime: %+v", r)
	}
	if r := pause(); !r.OK {
		t.Fatalf("pause: %+v", r)
	}
	if r := fill(); !r.OK {
		t.Fatalf("fill: %+v", r)
	}
	// Nothing could be written before the plugin read again, 400 ms after it read "hold" (which was after held).
	maxLeft := queued.Add(1500 * time.Millisecond).Sub(held.Add(400 * time.Millisecond))
	var deadlineMS int64 = -1
	sawLeft := false
	for _, l := range fx.lines() {
		if strings.Contains(l, `"id":"late"`) || strings.HasPrefix(l, "LEFT late") {
			t.Fatalf("the expired command reached the plugin: %s", l)
		}
		if strings.Contains(l, `"id":"ontime"`) {
			var m struct {
				DeadlineMS int64 `json:"deadline_ms"`
			}
			json.Unmarshal([]byte(l), &m)
			deadlineMS = m.DeadlineMS
		}
		if strings.HasPrefix(l, "LEFT ontime ") {
			var ns int64
			fmt.Sscanf(strings.TrimPrefix(l, "LEFT ontime "), "%d", &ns)
			sawLeft = true
			if ns <= 0 || time.Duration(ns) > maxLeft {
				t.Fatalf("the plugin saw %v left on the shared clock; at most %v can be", time.Duration(ns), maxLeft)
			}
		}
	}
	if deadlineMS < 1 || time.Duration(deadlineMS)*time.Millisecond > maxLeft {
		t.Fatalf("ontime was written with deadline_ms %d; at most %v was left", deadlineMS, maxLeft)
	}
	if _, ok := monotonicNS(); ok != sawLeft {
		t.Fatalf("expires_at_mono_ns sent %v, shared clock %v", sawLeft, ok)
	}
}

func TestFaultState(t *testing.T) {
	fx := start(t, "fault")
	fx.waitState(StateFaulted)
	r := fx.m.Get("fake").Command(context.Background(), "x", "drive", nil, 300)
	if r.FaultCode != "firmware_unsupported" {
		t.Fatalf("%+v", r)
	}
	if f := fx.m.Get("fake").Info().Fault; f == nil || f.Message != "v1 not v2" {
		t.Fatalf("%+v", f)
	}
}

func TestMuteKilledAfterDescribeTimeout(t *testing.T) {
	old := DescribeTimeout
	DescribeTimeout = 200 * time.Millisecond
	defer func() { DescribeTimeout = old }()
	fx := start(t, "mute")
	fx.waitOutput(OutFault)
}

func TestProbe(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "p.log")
	exe, _ := os.Executable()
	d, err := Probe(context.Background(), Spec{Name: "fake", Argv: []string{exe}, Env: []string{"OPENVIBE_FAKE_PLUGIN=" + logPath}})
	if err != nil || d.Driver != "fake" {
		t.Fatalf("%v %+v", err, d)
	}
	b, _ := os.ReadFile(logPath)
	if !strings.Contains(string(b), `"probe":true`) {
		t.Fatalf("%s", b)
	}
}

func TestConcurrentCommands(t *testing.T) {
	fx := start(t, "")
	fx.waitState(StateReady)
	p := fx.m.Get("fake")
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if r := p.Command(context.Background(), fmt.Sprint("id", i), "drive", nil, 300); !r.OK {
				t.Errorf("%d: %+v", i, r)
			}
		}(i)
	}
	wg.Wait()
}

// A plugin that dies while a child still holds its stdout is noticed at once, and the child is killed.
func TestCrashWithChildHoldingStdout(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	script := `sleep 30 & echo $! > ` + pidFile + `
echo '{"op":"describe","driver":"sh","capabilities":{}}'
echo '{"op":"ready"}'
read line
exit 1`
	m := NewManager([]Spec{{Name: "sh", Argv: []string{"/bin/sh", "-c", script}}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); m.Wait() }()
	go func() {
		for range m.Output() {
		}
	}()
	m.Start(ctx)
	deadline := time.Now().Add(3 * time.Second)
	for m.Get("sh").Info().Restarts < 1 {
		if time.Now().After(deadline) {
			t.Fatalf("crash not noticed: %+v", m.Get("sh").Info())
		}
		time.Sleep(10 * time.Millisecond)
	}
	b, _ := os.ReadFile(pidFile)
	var pid int
	fmt.Sscan(string(b), &pid)
	if pid > 0 {
		time.Sleep(100 * time.Millisecond)
		if p, err := os.FindProcess(pid); err == nil && p.Signal(syscall.Signal(0)) == nil {
			// Zombie children of the killed shell are reaped by init; a live one is a failure.
			stat, _ := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
			if !strings.Contains(string(stat), ") Z ") && len(stat) > 0 {
				t.Fatalf("child %d still running", pid)
			}
		}
	}
}
