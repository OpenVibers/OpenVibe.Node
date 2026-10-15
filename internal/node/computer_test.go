//go:build !windows

package node

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

	"github.com/OpenVibers/OpenVibe.Node/internal/config"
	"github.com/OpenVibers/OpenVibe.Node/internal/fakebot"
	"github.com/OpenVibers/OpenVibe.Node/internal/link"
	"github.com/OpenVibers/OpenVibe.Node/internal/localctl"
	"github.com/OpenVibers/OpenVibe.Node/internal/plugins"
	"github.com/OpenVibers/OpenVibe.Node/internal/protocol"
)

// startComputer runs a Node with only the real Python computer plugin at the given control level, its roots the one
// folder it returns, against the fake Bot.
func startComputer(t *testing.T, level string) (*env, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("end-to-end tests run on unix")
	}
	if os.Geteuid() == 0 {
		t.Skip("the computer plugin never runs as root")
	}
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	root, _ := filepath.Abs("../..")
	home, err := os.MkdirTemp("", "ovc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	work := filepath.Join(home, "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "readme.txt"), []byte("hi\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	paths := config.HomePaths(home)
	srv := fakebot.New()
	t.Cleanup(srv.Close)
	srv.Config = protocol.Config{HeartbeatMS: 100, Limits: protocol.Limits{MaxSpeed: f64(0.5), MaxTurn: f64(0.4), MaxCommandMS: 1000},
		AllowedCommands: append([]string(nil), protocol.Kinds...)}
	srv.AddCode("TEST2345")
	creds, err := link.Pair(context.Background(), nil, srv.URL(), protocol.PairRequest{Code: "TEST2345", AgentVersion: "test",
		DeviceKind: "onboard", Drivers: []string{"computer"}})
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Python = py
	cfg.Plugins = []config.PluginConfig{{Name: "computer", Config: map[string]any{"level": level, "roots": []string{work}, "timeout_s": 20},
		Env: map[string]string{"PYTHONPATH": filepath.Join(root, "plugins/sdk") + string(os.PathListSeparator) + filepath.Join(root, "plugins/computer")}}}
	cfg.Video = config.VideoConfig{Source: "off"}
	n, err := New(Options{Config: cfg, Paths: paths, Creds: creds, Version: "test", HeartbeatInterval: 100 * time.Millisecond,
		LinkBackoffMin: 50 * time.Millisecond, LatchPoll: 50 * time.Millisecond, Log: testLog()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { n.Run(ctx); close(done) }()
	e := &env{t: t, srv: srv, node: n, paths: paths, cancel: cancel, done: done}
	t.Cleanup(e.stop)
	e.conn = e.nextConn()
	if _, err := e.conn.Expect(protocol.TypeStatus, 20*time.Second, func(m protocol.Message) bool {
		s := m.(protocol.Status)
		if os.Getenv("OV_DEBUG") != "" {
			b, _ := json.Marshal(s.Drivers)
			t.Logf("status drivers %s", b)
		}
		return len(s.Drivers) == 1 && s.Drivers[0].State == plugins.StateReady
	}); err != nil {
		t.Fatal(err)
	}
	real, _ := filepath.EvalSymlinks(work)
	return e, real
}

// computerCmd sends a computer command with a 20 s deadline and returns its reply (waiting up to wait).
func (e *env) computerCmd(id, kind string, value any, wait time.Duration) protocol.Message {
	e.t.Helper()
	b, _ := json.Marshal(value)
	if err := e.conn.Command(protocol.Command{ID: id, Kind: kind, Value: b}, 20000); err != nil {
		e.t.Fatal(err)
	}
	r, err := e.conn.Reply(id, wait)
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

func testLog() *slog.Logger {
	if os.Getenv("OV_DEBUG") != "" {
		return slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func dataOf(t *testing.T, m protocol.Message) map[string]any {
	t.Helper()
	a, ok := m.(protocol.AckData)
	if !ok {
		t.Fatalf("want an ack with data, got %#v", m)
	}
	var d map[string]any
	if err := json.Unmarshal(a.Data, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func nackOf(t *testing.T, m protocol.Message) protocol.Nack {
	t.Helper()
	nk, ok := m.(protocol.Nack)
	if !ok {
		t.Fatalf("want a nack, got %#v", m)
	}
	return nk
}

func TestComputerTrustedRunsAndAnswersWithData(t *testing.T) {
	e, work := startComputer(t, "trusted")
	d := dataOf(t, e.computerCmd("x1", protocol.KindExec, map[string]any{"argv": []string{"python3", "-c", "import os; print(os.getcwd())"}}, 15*time.Second))
	if strings.TrimSpace(d["stdout"].(string)) != work || d["exit_code"].(float64) != 0 {
		t.Fatalf("exec: %v", d)
	}
	d = dataOf(t, e.computerCmd("x2", protocol.KindFS, map[string]any{"op": "list", "path": work}, 5*time.Second))
	if entries := d["entries"].([]any); len(entries) != 1 || entries[0].(map[string]any)["name"] != "readme.txt" {
		t.Fatalf("list: %v", d)
	}
	if nk := nackOf(t, e.computerCmd("x3", protocol.KindFS, map[string]any{"op": "read", "path": "/etc/hostname"}, 5*time.Second)); nk.FaultCode != protocol.FaultNotAllowed {
		t.Fatalf("outside the roots: %+v", nk)
	}
	if nk := nackOf(t, e.computerCmd("x4", protocol.KindExec, "rm -rf /", 5*time.Second)); nk.FaultCode != protocol.FaultBadValue {
		t.Fatalf("a value that is not an object: %+v", nk)
	}
}

func TestComputerAskWaitsForTheOwnerOnTheMachine(t *testing.T) {
	e, _ := startComputer(t, "ask")
	b, _ := json.Marshal(map[string]any{"argv": []string{"echo", "approved"}})
	if err := e.conn.Command(protocol.Command{ID: "a1", Kind: protocol.KindExec, Value: b}, 20000); err != nil {
		t.Fatal(err)
	}
	var list []Approval
	deadline := time.Now().Add(10 * time.Second)
	for len(list) == 0 && time.Now().Before(deadline) {
		r, err := localctl.Call(e.paths.SocketPath(), localctl.Request{Cmd: localctl.CmdApprovals}, 2*time.Second)
		if err != nil || !r.OK {
			t.Fatal(err, r.Error)
		}
		_ = json.Unmarshal(r.Data, &list)
		if len(list) == 0 {
			time.Sleep(50 * time.Millisecond)
		}
	}
	if len(list) != 1 || list[0].ID != "a1" || list[0].Kind != protocol.KindExec || !strings.Contains(list[0].Summary, "echo approved") {
		t.Fatalf("approvals: %+v", list)
	}
	if r, _ := localctl.Call(e.paths.SocketPath(), localctl.Request{Cmd: localctl.CmdApprove, ID: "nope"}, 2*time.Second); r.OK {
		t.Fatal("an unknown id was approved")
	}
	if r, err := localctl.Call(e.paths.SocketPath(), localctl.Request{Cmd: localctl.CmdApprove, ID: "a1"}, 2*time.Second); err != nil || !r.OK {
		t.Fatal(err, r.Error)
	}
	r, err := e.conn.Reply("a1", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if d := dataOf(t, r); d["stdout"] != "approved\n" {
		t.Fatalf("approved exec: %v", d)
	}
	if got := e.node.Approvals(); len(got) != 0 {
		t.Fatalf("still listed after its answer: %+v", got)
	}
	if err := e.conn.Command(protocol.Command{ID: "a2", Kind: protocol.KindExec, Value: b}, 20000); err != nil {
		t.Fatal(err)
	}
	for time.Now().Before(deadline.Add(5*time.Second)) && len(e.node.Approvals()) == 0 {
		time.Sleep(50 * time.Millisecond)
	}
	if r, err := localctl.Call(e.paths.SocketPath(), localctl.Request{Cmd: localctl.CmdDeny, ID: "a2"}, 2*time.Second); err != nil || !r.OK {
		t.Fatal(err, r.Error)
	}
	r, err = e.conn.Reply("a2", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if nk := nackOf(t, r); nk.FaultCode != protocol.FaultNotAllowed {
		t.Fatalf("denied: %+v", nk)
	}
}

// An approval is refused while the agent drives the mouse or keyboard and just after: an approved click must not be
// able to press another request's Approve button (in the tray or a notification). Deny always goes through.
func TestComputerApproveWaitsOutTheMouse(t *testing.T) {
	e, _ := startComputer(t, "ask")
	b, _ := json.Marshal(map[string]any{"argv": []string{"echo", "later"}})
	if err := e.conn.Command(protocol.Command{ID: "g1", Kind: protocol.KindExec, Value: b}, 20000); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && len(e.node.Approvals()) == 0 {
		time.Sleep(50 * time.Millisecond)
	}
	e.node.mu.Lock()
	e.node.inputEnded = time.Now() // a click just ran
	e.node.mu.Unlock()
	r, err := localctl.Call(e.paths.SocketPath(), localctl.Request{Cmd: localctl.CmdApprove, ID: "g1"}, 2*time.Second)
	if err == nil || r.OK || !strings.Contains(r.Error, "mouse or keyboard") {
		t.Fatalf("approve right after a click: %+v %v", r, err)
	}
	if len(e.node.Approvals()) != 1 {
		t.Fatal("the refused approval no longer waits")
	}
	e.node.mu.Lock()
	e.node.inputs["i1"] = true // an input command in flight, not waiting for approval: it is running
	e.node.inputEnded = time.Now().Add(-InputGuard - time.Second)
	e.node.mu.Unlock()
	if r, _ := localctl.Call(e.paths.SocketPath(), localctl.Request{Cmd: localctl.CmdApprove, ID: "g1"}, 2*time.Second); r.OK {
		t.Fatal("approved while an input command runs")
	}
	e.node.mu.Lock()
	delete(e.node.inputs, "i1")
	e.node.mu.Unlock()
	if r, err := localctl.Call(e.paths.SocketPath(), localctl.Request{Cmd: localctl.CmdApprove, ID: "g1"}, 2*time.Second); err != nil || !r.OK {
		t.Fatal(err, r.Error)
	}
	if d := dataOf(t, func() protocol.Message {
		m, err := e.conn.Reply("g1", 10*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}()); d["stdout"] != "later\n" {
		t.Fatalf("approved exec: %v", d)
	}
	if err := e.conn.Command(protocol.Command{ID: "g2", Kind: protocol.KindExec, Value: b}, 20000); err != nil {
		t.Fatal(err)
	}
	for time.Now().Before(deadline.Add(5*time.Second)) && len(e.node.Approvals()) == 0 {
		time.Sleep(50 * time.Millisecond)
	}
	e.node.mu.Lock()
	e.node.inputEnded = time.Now()
	e.node.mu.Unlock()
	if r, err := localctl.Call(e.paths.SocketPath(), localctl.Request{Cmd: localctl.CmdDeny, ID: "g2"}, 2*time.Second); err != nil || !r.OK {
		t.Fatal("deny is never held back:", err, r.Error)
	}
}

// The tray says it is there on every poll; the core tells the computer plugin at most every trayRetell, and at once
// when the tray quits.
func TestComputerTrayPresenceReachesThePlugin(t *testing.T) {
	e, _ := startComputer(t, "ask")
	if r, err := localctl.Call(e.paths.SocketPath(), localctl.Request{Cmd: localctl.CmdTray, On: true}, 2*time.Second); err != nil || !r.OK {
		t.Fatal(err, r.Error)
	}
	e.node.mu.Lock()
	first := e.node.trayToldAt
	e.node.mu.Unlock()
	if first.IsZero() {
		t.Fatal("the tray's presence was not passed on")
	}
	_, _ = localctl.Call(e.paths.SocketPath(), localctl.Request{Cmd: localctl.CmdTray, On: true}, 2*time.Second)
	e.node.mu.Lock()
	again := e.node.trayToldAt
	e.node.mu.Unlock()
	if !again.Equal(first) {
		t.Fatal("told again within trayRetell")
	}
	_, _ = localctl.Call(e.paths.SocketPath(), localctl.Request{Cmd: localctl.CmdTray}, 2*time.Second)
	e.node.mu.Lock()
	off := e.node.trayToldAt
	e.node.mu.Unlock()
	if !off.IsZero() {
		t.Fatal("the tray quitting was not passed on")
	}
}

func TestComputerLocalStopLeavesObserveOnly(t *testing.T) {
	e, work := startComputer(t, "trusted")
	if r, err := localctl.Call(e.paths.SocketPath(), localctl.Request{Cmd: localctl.CmdStop}, 2*time.Second); err != nil || !r.OK {
		t.Fatal(err, r.Error)
	}
	if nk := nackOf(t, e.computerCmd("s1", protocol.KindExec, map[string]any{"argv": []string{"true"}}, 5*time.Second)); nk.FaultCode != protocol.FaultLocalStop {
		t.Fatalf("exec while stopped: %+v", nk)
	}
	if nk := nackOf(t, e.computerCmd("s2", protocol.KindFS, map[string]any{"op": "write", "path": filepath.Join(work, "x"), "content_b64": "eA=="}, 5*time.Second)); nk.FaultCode != protocol.FaultLocalStop {
		t.Fatalf("write while stopped: %+v", nk)
	}
	if nk := nackOf(t, e.computerCmd("s2i", protocol.KindInput, map[string]any{"action": "key", "keys": "Return"}, 5*time.Second)); nk.FaultCode != protocol.FaultLocalStop {
		t.Fatalf("input while stopped: %+v", nk)
	}
	if nk := nackOf(t, e.computerCmd("s2b", protocol.KindBrowser, map[string]any{"action": "open", "url": "https://example.org/"}, 5*time.Second)); nk.FaultCode != protocol.FaultLocalStop {
		t.Fatalf("browser open while stopped: %+v", nk)
	}
	if d := dataOf(t, e.computerCmd("s3", protocol.KindFS, map[string]any{"op": "stat", "path": work}, 5*time.Second)); d["type"] != "dir" {
		t.Fatalf("observing while stopped: %v", d)
	}
	if d := dataOf(t, e.computerCmd("s3p", protocol.KindProbe, map[string]any{"target": "tcp://127.0.0.1:1", "timeout_s": 2}, 10*time.Second)); d["open"] != false {
		t.Fatalf("a probe is read-only and runs while stopped: %v", d)
	}
	if nk := nackOf(t, e.computerCmd("s3q", protocol.KindProbe, map[string]any{"target": "http://1.1.1.1/"}, 10*time.Second)); nk.FaultCode != protocol.FaultNotAllowed {
		t.Fatalf("a public probe: %+v", nk)
	}
	if r, err := localctl.Call(e.paths.SocketPath(), localctl.Request{Cmd: localctl.CmdResume}, 2*time.Second); err != nil || !r.OK {
		t.Fatal(err, r.Error)
	}
	time.Sleep(200 * time.Millisecond) // the plugin hears the resume
	if d := dataOf(t, e.computerCmd("s4", protocol.KindExec, map[string]any{"argv": []string{"echo", "back"}}, 10*time.Second)); d["stdout"] != "back\n" {
		t.Fatalf("after resume: %v", d)
	}
}

func TestComputerKindsNeedTheComputerPlugin(t *testing.T) {
	e := start(t)
	if nk := nackOf(t, e.cmd(protocol.KindExec, `{"argv":["true"]}`, 5000)); nk.FaultCode != protocol.FaultUnsupported {
		t.Fatalf("exec without the computer plugin: %+v", nk)
	}
}
