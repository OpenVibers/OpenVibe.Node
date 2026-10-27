package plugins

// The original-deadline guarantee across the real IPC boundary, with the real Python runtime (plugins/sdk), on every OS
// CI runs (Linux, macOS and Windows: each job has Python; Maticus, 2026-10-27: "these tests must actually execute on
// Windows. A skipped test or a Unix-only equivalent is not sufficient"). Under GitHub Actions a missing Python fails the
// test instead of skipping it.
//
// The plugin script holds each drive line it has read for IPC_DELAY_MS before the runtime's reader sees it (time in the
// pipe and the reader), and holds the whole pipe for HOLD_MS on a "hold" command (so the core's writer queue backs up
// behind a full pipe). handle() records every command it is given in a file: an expired command must never be in it.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/OpenVibers/OpenVibe.Node/internal/mono"
	"github.com/OpenVibers/OpenVibe.Node/internal/protocol"
)

const ipcPlugin = `
import json, os, sys, time
from openvibe_plugin import Plugin, Runtime, SHARED_CLOCK, SHARED_CLOCK_NAME

DELAY = int(os.environ.get("IPC_DELAY_MS", "0")) / 1000.0
HOLD = int(os.environ.get("HOLD_MS", "0")) / 1000.0
HANDLED = os.environ["HANDLED_FILE"]


class Slow:
    """stdin as the runtime reads it: a drive line reaches the runtime IPC_DELAY_MS after it was read off the pipe, and
    a hold command stops reading the pipe for HOLD_MS (the pipe fills and the core's writer waits)."""
    def __init__(self, f):
        self.f = f

    def __iter__(self):
        for line in self.f:
            if '"kind":"drive"' in line:
                time.sleep(DELAY)
            if '"kind":"hold"' in line:
                time.sleep(HOLD)
            yield line


class Probe(Plugin):
    driver = "ipc-probe"
    version = "1"
    motion_kinds = frozenset({"drive"})
    strict_timing = os.environ.get("STRICT") == "1"

    def describe(self, config):
        # This process's reading of the shared clock: the core checks it falls between two readings of its own.
        return {"shared_clock_name": SHARED_CLOCK_NAME, "shared_clock_ns": SHARED_CLOCK() if SHARED_CLOCK else None}

    def handle(self, cmd):
        with open(HANDLED, "a") as f:
            f.write(cmd.id + "\n")
        return {"deadline_ms": cmd.deadline_ms}


shared = SHARED_CLOCK
if os.environ.get("BREAK_CLOCK") == "1":
    def shared():
        raise OSError("the shared clock read failed (test)")
Runtime(Probe(), infile=Slow(sys.stdin), shared_clock=shared).run()
`

// pythonForTest finds a Python 3, or skips (fails under GitHub Actions, where every job installs one).
func pythonForTest(t *testing.T) string {
	t.Helper()
	for _, name := range []string{"python3", "python"} {
		if p, err := exec.LookPath(name); err == nil {
			if out, err := exec.Command(p, "-c", "import sys; print(sys.version_info[0])").Output(); err == nil && strings.TrimSpace(string(out)) == "3" {
				return p
			}
		}
	}
	if os.Getenv("GITHUB_ACTIONS") == "true" {
		t.Fatal("no Python 3 on this CI runner: the cross-process deadline test must run here, not be skipped")
	}
	t.Skip("no Python 3")
	return ""
}

type ipcFixture struct {
	t       *testing.T
	m       *Manager
	p       *Plugin
	handled string
	before  int64
}

func startIPC(t *testing.T, strictNode bool, env ...string) *ipcFixture {
	t.Helper()
	py := pythonForTest(t)
	dir := t.TempDir()
	script := filepath.Join(dir, "probe.py")
	if err := os.WriteFile(script, []byte(ipcPlugin), 0o644); err != nil {
		t.Fatal(err)
	}
	sdk, _ := filepath.Abs(filepath.Join("..", "..", "plugins", "sdk"))
	handled := filepath.Join(dir, "handled.txt")
	spec := Spec{Name: "py", Argv: []string{py, script}, StrictTiming: strictNode,
		Env: append([]string{"PYTHONPATH=" + sdk, "HANDLED_FILE=" + handled, "PYTHONUNBUFFERED=1"}, env...)}
	before, _ := monotonicNS()
	m := NewManager([]Spec{spec}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	m.Start(ctx)
	go func() {
		for range m.Output() {
		}
	}()
	t.Cleanup(func() { cancel(); m.Wait() })
	fx := &ipcFixture{t: t, m: m, p: m.Get("py"), handled: handled, before: before}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) && fx.p.Info().State != StateReady {
		time.Sleep(10 * time.Millisecond)
	}
	if st := fx.p.Info().State; st != StateReady {
		t.Fatalf("the Python plugin never became ready (state %s, fault %+v)", st, fx.p.Info().Fault)
	}
	return fx
}

func (fx *ipcFixture) drive(id string, left time.Duration) Reply {
	return fx.p.BeginWith(context.Background(), id, protocol.KindDrive, json.RawMessage(`{"throttle":0.1,"steer":0}`), mono.Now().Add(left), nil, 0, nil)()
}

func (fx *ipcFixture) handledIDs() []string {
	b, _ := os.ReadFile(fx.handled)
	return strings.Fields(string(b))
}

// The plugin's reading of the shared clock lies between two of the core's: the same clock, epoch and unit in both
// processes (on Windows, the raw QueryPerformanceCounter through qpcNS and _qpc_ns).
func TestPythonRuntimeReadsTheCoresClock(t *testing.T) {
	fx := startIPC(t, false)
	after, ok := monotonicNS()
	if !ok {
		t.Fatalf("no shared clock on %s", runtime.GOOS)
	}
	var caps struct {
		Name string `json:"shared_clock_name"`
		NS   *int64 `json:"shared_clock_ns"`
	}
	b, _ := json.Marshal(fx.p.Info().Describe.Capabilities)
	_ = json.Unmarshal(b, &caps)
	if caps.Name != sharedClockName || caps.NS == nil {
		t.Fatalf("the runtime reads %q (%v), the core %q", caps.Name, caps.NS, sharedClockName)
	}
	if *caps.NS < fx.before || *caps.NS > after {
		t.Fatalf("the runtime's clock read %d, outside the core's %d…%d: not the same clock", *caps.NS, fx.before, after)
	}
	t.Logf("%s on %s: core %d ≤ runtime %d ≤ core %d", sharedClockName, runtime.GOOS, fx.before, *caps.NS, after)
}

// hello names the clock expires_at_mono_ns is on, on every start (a restarted plugin hears it again), and passes the
// Node's strict timing to the runtime.
func TestHelloNamesTheSharedClock(t *testing.T) {
	fx := start(t, "")
	fx.waitState(StateReady)
	hello := fx.lines()[0]
	if !strings.Contains(hello, `"op":"hello"`) || !strings.Contains(hello, `"shared_clock":"`+sharedClockName+`"`) {
		t.Fatalf("hello: %s", hello)
	}
}

// Test A and B: 200 ms in the pipe and reader. A command with 100 ms left never reaches handle(); one with a second
// left is handled with at most what remains of its original second.
func TestPipeDelayNeverExtendsADeadline(t *testing.T) {
	fx := startIPC(t, false, "IPC_DELAY_MS=200")
	late := fx.drive("late", 100*time.Millisecond)
	if late.FaultCode != protocol.FaultExpired || !strings.Contains(late.Message, "before the runtime read it") {
		t.Fatalf("late: %+v", late)
	}
	start := time.Now()
	ontime := fx.drive("ontime", 1000*time.Millisecond)
	if !ontime.OK {
		t.Fatalf("ontime: %+v", ontime)
	}
	var got struct {
		DeadlineMS int `json:"deadline_ms"`
	}
	_ = json.Unmarshal(ontime.Data, &got)
	// It waited at least the 200 ms delay: at most 800 ms can be left (plus nothing: the clocks only round down).
	if got.DeadlineMS < 1 || got.DeadlineMS > 800 {
		t.Fatalf("ontime was handled with deadline_ms %d after %v; at most 800 can be left", got.DeadlineMS, time.Since(start))
	}
	if ids := fx.handledIDs(); len(ids) != 1 || ids[0] != "ontime" {
		t.Fatalf("handle() saw %v; the expired command must never reach it", ids)
	}
	t.Logf("%s: late refused (%s), ontime handled with %d ms of its 1000", runtime.GOOS, late.Message, got.DeadlineMS)
}

// Test D: the core's writer queue (a full pipe while the plugin is not reading) and then the pipe delay, together.
func TestWriterQueueThenPipeDelay(t *testing.T) {
	fx := startIPC(t, false, "IPC_DELAY_MS=200", "HOLD_MS=300")
	ctx := context.Background()
	hold := fx.p.Begin(ctx, "hold", "hold", nil, 3000)
	filler, _ := json.Marshal(map[string]any{"text": strings.Repeat("x", 1<<20)})
	fill := fx.p.Begin(ctx, "fill", "say", filler, 5000)
	queued := mono.Now()
	late := fx.p.BeginWith(ctx, "late", protocol.KindDrive, nil, queued.Add(250*time.Millisecond), nil, 0, nil)
	combo := fx.p.BeginWith(ctx, "combo", protocol.KindDrive, nil, queued.Add(1500*time.Millisecond), nil, 0, nil)
	if r := late(); r.FaultCode != protocol.FaultExpired {
		t.Fatalf("late: %+v", r)
	}
	r := combo()
	hold()
	fill()
	if !r.OK {
		t.Fatalf("combo: %+v", r)
	}
	var got struct {
		DeadlineMS int `json:"deadline_ms"`
	}
	_ = json.Unmarshal(r.Data, &got)
	// The pipe was held for 300 ms, then the line waited 200 ms more: at most 1000 ms of its 1500 can be left.
	if got.DeadlineMS < 1 || got.DeadlineMS > 1000 {
		t.Fatalf("combo was handled with deadline_ms %d; at most 1000 can be left", got.DeadlineMS)
	}
	for _, id := range fx.handledIDs() {
		if id == "late" {
			t.Fatal("the expired command reached handle()")
		}
	}
	t.Logf("%s: writer queue then pipe: late refused, combo handled with %d ms of its 1500", runtime.GOOS, got.DeadlineMS)
}

// Test C: strict timing, when the shared clock is missing (on the core's side, or the runtime's read fails): timed
// motion is refused, never run on deadline_ms alone; a plugin that does not ask for it keeps running as before.
func TestStrictTimingRefusesWithoutTheClock(t *testing.T) {
	real := monotonicNS
	t.Cleanup(func() { monotonicNS = real })

	// The runtime's read fails: the runtime refuses, before handle().
	fx := startIPC(t, false, "STRICT=1", "BREAK_CLOCK=1")
	r := fx.drive("broken", time.Second)
	if r.FaultCode != protocol.FaultTimingUnavailable || !strings.Contains(r.Message, "could not be read") {
		t.Fatalf("broken clock: %+v", r)
	}
	if ids := fx.handledIDs(); len(ids) != 0 {
		t.Fatalf("handle() saw %v", ids)
	}

	// The core has no shared clock: a strict plugin refuses the line that comes without an expiry…
	monotonicNS = func() (int64, bool) { return 0, false }
	fx2 := startIPC(t, false, "STRICT=1")
	if r := fx2.drive("bare", time.Second); r.FaultCode != protocol.FaultTimingUnavailable {
		t.Fatalf("no clock, strict plugin: %+v", r)
	}
	// …and a core configured for strict timing does not write it at all.
	fx3 := startIPC(t, true)
	if r := fx3.drive("unsent", time.Second); r.FaultCode != protocol.FaultTimingUnavailable || !strings.Contains(r.Message, "was not sent") {
		t.Fatalf("no clock, strict core: %+v", r)
	}
	if ids := append(fx2.handledIDs(), fx3.handledIDs()...); len(ids) != 0 {
		t.Fatalf("handle() saw %v", ids)
	}
	// A plugin that does not ask for strict timing still runs on deadline_ms where there is no shared clock.
	fx4 := startIPC(t, false)
	if r := fx4.drive("compat", time.Second); !r.OK {
		t.Fatalf("not strict, no clock: %+v", r)
	}
}
