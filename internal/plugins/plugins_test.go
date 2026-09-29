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
	mode := os.Getenv("OPENVIBE_FAKE_MODE")
	for sc.Scan() {
		var m map[string]any
		json.Unmarshal(sc.Bytes(), &m)
		op, _ := m["op"].(string)
		if op != "heartbeat" {
			rec(sc.Text())
		}
		switch op {
		case "hello":
			if mode == "mute" {
				continue
			}
			w.Encode(map[string]any{"op": "describe", "driver": "fake", "version": "1",
				"capabilities": map[string]any{"drive": map[string]any{}, "actuator": map[string]any{"names": []string{"lift"}}}})
			if m["probe"] == true {
				continue
			}
			if mode == "fault" {
				w.Encode(map[string]any{"op": "fault", "fault_code": "firmware_unsupported", "message": "v1 not v2"})
				continue
			}
			w.Encode(map[string]any{"op": "ready"})
			w.Encode(map[string]any{"op": "event", "name": "booted"})
		case "command":
			switch m["kind"] {
			case "crash":
				rec("CRASH")
				os.Exit(3)
			case "hang":
				time.Sleep(10 * time.Second)
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

func TestShutdownStopsAndClosesStdin(t *testing.T) {
	fx := start(t, "")
	fx.waitState(StateReady)
	fx.stop()
	fx.m.Wait()
	l := fx.lines()
	if !strings.Contains(l[len(l)-2], `"op":"stop"`) || l[len(l)-1] != "EOF-STOP" {
		t.Fatalf("want stop then EOF, got %v", l)
	}
}

func TestCrashRestartsWithEstop(t *testing.T) {
	fx := start(t, "")
	fx.waitState(StateReady)
	p := fx.m.Get("fake")
	p.Estop()
	fx.waitLog(`{"op":"estop"}`)
	r := p.Command(context.Background(), "boom", "crash", nil, 300)
	if r.OK || r.FaultCode != protocol.FaultPluginDown {
		t.Fatalf("%+v", r)
	}
	fx.waitOutput(OutFault)
	fx.waitState(StateReady)
	if p.Info().Restarts < 1 {
		t.Fatal("no restart counted")
	}
	// After the restart the latched e-stop is sent again right after hello.
	l := fx.lines()
	var afterCrash []string
	for i, s := range l {
		if s == "CRASH" {
			afterCrash = l[i+1:]
		}
	}
	if len(afterCrash) < 2 || !strings.Contains(afterCrash[0], `"op":"hello"`) || afterCrash[1] != `{"op":"estop"}` {
		t.Fatalf("after restart: %v", afterCrash)
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
	fx.waitLog(`{"op":"stop"}`)
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
