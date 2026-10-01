//go:build !windows

package node

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/OpenVibers/OpenVibe.Node/internal/config"
	"github.com/OpenVibers/OpenVibe.Node/internal/fakebot"
	"github.com/OpenVibers/OpenVibe.Node/internal/link"
	"github.com/OpenVibers/OpenVibe.Node/internal/localctl"
	"github.com/OpenVibers/OpenVibe.Node/internal/plugins"
	"github.com/OpenVibers/OpenVibe.Node/internal/protocol"
	"github.com/OpenVibers/OpenVibe.Node/internal/safety"
	"github.com/OpenVibers/OpenVibe.Node/internal/video"
)

func init() {
	plugins.HeartbeatInterval = 100 * time.Millisecond
	plugins.BackoffMin = 50 * time.Millisecond
}

type env struct {
	t      *testing.T
	srv    *fakebot.Server
	whip   *video.WHIPReceiver
	node   *Node
	conn   *fakebot.Conn
	record string
	paths  config.Paths
	cancel context.CancelFunc
	done   chan struct{}
}

// start pairs a Node running the real Python dry-run plugin with the fake Bot server and waits until the plugin is
// ready and the link is up.
func start(t *testing.T) *env {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("end-to-end tests run on unix")
	}
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	root, _ := filepath.Abs("../..")
	home, err := os.MkdirTemp("", "ovn") // short: the control socket path must stay under ~100 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	paths := config.HomePaths(home)

	srv := fakebot.New()
	t.Cleanup(srv.Close)
	srv.SetConfig(100, map[string]any{"max_speed": 0.5, "max_turn": 0.4, "max_command_ms": 1000}, nil)
	// Bot looks the credential up after the upgrade: anything the Node sent before hello would be refused.
	srv.SetHelloDelay(100 * time.Millisecond)
	whip := &video.WHIPReceiver{}
	srv.SetWHIP(whip) // so the pairing answer carries a whip_url (Bot does not send one yet)
	t.Cleanup(whip.Close)
	srv.AddRobotCode(fakebot.RobotID, "TEST2345")
	creds, err := link.Pair(context.Background(), nil, srv.URL(), protocol.PairRequest{Robot: fakebot.RobotID, Code: "TEST2345",
		AgentVersion: "test", DeviceKind: "onboard", Drivers: []string{"dryrun"}, Name: "rover"})
	if err != nil {
		t.Fatal(err)
	}
	whip.PublishKey = creds.PublishKey.Reveal()

	record := filepath.Join(home, "record.jsonl")
	cfg := config.Default()
	cfg.Python = py
	cfg.Plugins = []config.PluginConfig{{Name: "dryrun", Config: map[string]any{"record": record, "log_commands": false},
		Env: map[string]string{"PYTHONPATH": filepath.Join(root, "plugins/sdk") + string(os.PathListSeparator) + filepath.Join(root, "plugins/dryrun")}}}
	cfg.Video = config.VideoConfig{Source: "auto", Width: 64, Height: 48, FPS: 10}

	n, err := New(Options{Config: cfg, Paths: paths, Creds: creds, Version: "test", HeartbeatInterval: 100 * time.Millisecond,
		LinkBackoffMin: 50 * time.Millisecond, VideoLoopback: true, LatchPoll: 50 * time.Millisecond,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { n.Run(ctx); close(done) }()
	e := &env{t: t, srv: srv, whip: whip, node: n, record: record, paths: paths, cancel: cancel, done: done}
	t.Cleanup(e.stop)
	e.conn = e.nextConn()
	e.waitReady()
	return e
}

func (e *env) stop() {
	e.cancel()
	select {
	case <-e.done:
	case <-time.After(10 * time.Second):
		e.t.Error("node did not stop")
	}
}

func (e *env) nextConn() *fakebot.Conn {
	e.t.Helper()
	c, err := e.srv.NextConn(10 * time.Second)
	if err != nil {
		e.t.Fatal(err)
	}
	return c
}

func (e *env) waitReady() {
	e.t.Helper()
	_, err := e.conn.Expect(protocol.TypeStatus, 20*time.Second, func(m protocol.Message) bool {
		s := m.(protocol.Status)
		return len(s.Drivers) == 1 && s.Drivers[0].State == plugins.StateReady
	})
	if err != nil {
		e.t.Fatal(err)
	}
}

var seq int

// cmd sends a command as Bot does, with its deadline deadline ms from now (0 = none), and returns the reply.
func (e *env) cmd(kind, value string, deadline int) protocol.Message {
	e.t.Helper()
	seq++
	id := "cmd-" + strings.ReplaceAll(e.t.Name(), "/", "-") + "-" + string(rune('a'+seq%26)) + time.Now().Format("150405.000000")
	return e.cmdID(id, kind, value, deadline)
}

func (e *env) cmdID(id, kind, value string, deadline int) protocol.Message {
	e.t.Helper()
	var at time.Time
	if deadline != 0 {
		at = time.Now().Add(time.Duration(deadline) * time.Millisecond)
	}
	if _, err := e.conn.Command(fakebot.Cmd{ID: id, Kind: kind, Value: json.RawMessage(value), Deadline: at}); err != nil {
		e.t.Fatal(err)
	}
	r, err := e.conn.Reply(id, 5*time.Second)
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

type rec struct {
	T          float64         `json:"t"`
	What       string          `json:"what"`
	ID         string          `json:"id"`
	Kind       string          `json:"kind"`
	Value      json.RawMessage `json:"value"`
	DeadlineMS int             `json:"deadline_ms"`
	WasMoving  bool            `json:"was_moving"`
}

func (e *env) records() []rec {
	f, err := os.Open(e.record)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []rec
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r rec
		if json.Unmarshal(sc.Bytes(), &r) == nil {
			out = append(out, r)
		}
	}
	return out
}

// waitStop waits until the plugin records a stop after the given number of records, and returns how long it took.
func (e *env) waitStop(after int, within time.Duration) time.Duration {
	e.t.Helper()
	start := time.Now()
	for time.Since(start) < within {
		r := e.records()
		for i := after; i < len(r); i++ {
			if r[i].What == "stop" && r[i].WasMoving {
				return time.Since(start)
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.t.Fatalf("no stop within %s; records: %+v", within, e.records()[after:])
	return 0
}

func isAck(m protocol.Message) bool { _, ok := m.(protocol.Ack); return ok }

func nackCode(m protocol.Message) string {
	if n, ok := m.(protocol.Nack); ok {
		return n.FaultCode
	}
	return ""
}

func TestPairConnectAndStatus(t *testing.T) {
	e := start(t)
	reqs := e.srv.PairRequests()
	if len(reqs) != 1 || reqs[0].Code != "TEST2345" || reqs[0].Robot != fakebot.RobotID || reqs[0].Name != "rover" {
		t.Fatalf("%+v", reqs)
	}
	// status and estop_state wait for hello and config: nothing reached the server while it was checking the credential.
	if n := e.conn.NotPaired(); n != 0 {
		t.Fatalf("%d frames before hello", n)
	}
	var sawEstopState, sawStatus bool
	for _, f := range e.conn.Frames() {
		switch m := f.Msg.(type) {
		case protocol.EstopState:
			sawEstopState = true
			if m.Latched || m.By != "device" {
				t.Fatalf("estop_state %+v on a fresh device", m)
			}
		case protocol.Status:
			sawStatus = true
			if !strings.HasPrefix(m.Firmware, "openvibe-node-") || m.EstopLatched || m.Faults == nil {
				t.Fatalf("status %+v", m)
			}
		}
	}
	if !sawEstopState || !sawStatus {
		t.Fatal("no estop_state or status after hello and config")
	}
	if on, _ := e.srv.RobotEstop(); on {
		t.Fatal("the device latched the robot")
	}
	if e.srv.CredentialSeenInURL() {
		t.Fatal("credential in URL")
	}
	resp, err := localctl.Call(e.paths.SocketPath(), localctl.Request{Cmd: localctl.CmdStatus}, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var st Status
	json.Unmarshal(resp.Data, &st)
	if !st.Paired || !st.Link.Connected || len(st.Plugins) != 1 || st.Plugins[0].Driver != "dryrun" || st.Limits.MaxSpeed != 0.5 {
		t.Fatalf("%+v", st)
	}
	// The status (and so `openvibe-node status`) never shows the credential or publish key.
	if strings.Contains(string(resp.Data), e.node.opt.Creds.Credential.Reveal()) || strings.Contains(string(resp.Data), e.node.opt.Creds.PublishKey.Reveal()) {
		t.Fatal("secret in status")
	}
}

func TestCommandsClampedAndIdempotent(t *testing.T) {
	e := start(t)
	id := "dup-1"
	if r := e.cmdID(id, "drive", `{"throttle":2,"steer":-1}`, 300); !isAck(r) {
		t.Fatalf("%+v", r)
	}
	if r := e.cmdID(id, "drive", `{"throttle":2,"steer":-1}`, 300); !isAck(r) {
		t.Fatalf("duplicate: %+v", r)
	}
	var drives []rec
	for _, r := range e.records() {
		if r.What == "command" && r.ID == id {
			drives = append(drives, r)
		}
	}
	if len(drives) != 1 {
		t.Fatalf("executed %d times", len(drives))
	}
	var v map[string]float64
	json.Unmarshal(drives[0].Value, &v)
	if v["throttle"] != 0.5 || v["steer"] != -0.4 {
		t.Fatalf("not clamped to the owner limits: %s", drives[0].Value)
	}
	// The plugin gets what is left of the absolute deadline (a few ms of slack: the clock offset is read from hello).
	if d := drives[0].DeadlineMS; d > 310 || d < 200 {
		t.Fatalf("deadline %d", d)
	}
	// Deadlines are capped at max_command_ms.
	e.cmd("drive", `{"throttle":0.1}`, 60000)
	r := e.records()
	if last := r[len(r)-1]; last.DeadlineMS != 1000 {
		t.Fatalf("deadline not capped: %d", last.DeadlineMS)
	}
	if c := nackCode(e.cmd("drive", `{"throttle":"fast"}`, 300)); c != protocol.FaultBadValue {
		t.Fatalf("%q", c)
	}
	if c := nackCode(e.cmd("teleport", `{}`, 300)); c != protocol.FaultUnsupported {
		t.Fatalf("%q", c)
	}
	if r := e.cmd("say", `{"text":"hello"}`, 300); !isAck(r) {
		t.Fatalf("%+v", r)
	}
}

// TestExpiredNeverReachesPlugin: Bot's deadline_ms is an instant; a drive that arrives after it is refused.
func TestExpiredNeverReachesPlugin(t *testing.T) {
	e := start(t)
	id := "expired-1"
	if c := nackCode(e.cmdID(id, "drive", `{"throttle":0.3}`, -100)); c != protocol.FaultExpired {
		t.Fatalf("%q", c)
	}
	if c := nackCode(e.cmdID("expired-2", "drive", `{"throttle":0.3}`, -60000)); c != protocol.FaultExpired {
		t.Fatalf("%q", c)
	}
	time.Sleep(200 * time.Millisecond)
	for _, r := range e.records() {
		if r.What == "command" && strings.HasPrefix(r.ID, "expired-") {
			t.Fatalf("an expired command reached the plugin: %+v", r)
		}
	}
}

// TestAllowedCommands: a kind missing from config.allowed_commands is refused; halt never is.
func TestAllowedCommands(t *testing.T) {
	e := start(t)
	e.srv.SetConfig(0, nil, []string{"say"})
	e.conn.Close()
	e.conn = e.nextConn()
	e.waitReady()
	if c := nackCode(e.cmd("drive", `{"throttle":0.3}`, 300)); c != protocol.FaultNotAllowed {
		t.Fatalf("%q", c)
	}
	if !isAck(e.cmd("say", `{"text":"hi"}`, 0)) || !isAck(e.cmd("halt", `{}`, 0)) {
		t.Fatal("an allowed kind or halt was refused")
	}
}

func TestDeadlineStops(t *testing.T) {
	e := start(t)
	n := len(e.records())
	e.cmd("drive", `{"throttle":0.3}`, 300)
	d := e.waitStop(n, 2*time.Second)
	if d < 200*time.Millisecond {
		t.Fatalf("stopped after %s, before the deadline", d)
	}
}

func TestLinkLossStops(t *testing.T) {
	e := start(t)
	n := len(e.records())
	e.cmd("drive", `{"throttle":0.3}`, 1000)
	e.conn.Close()
	if d := e.waitStop(n, 2*time.Second); d > 500*time.Millisecond {
		t.Fatalf("link loss stop took %s", d)
	}
	// It reconnects, and nothing is replayed.
	e.conn = e.nextConn()
	e.waitReady()
}

func TestMissedHeartbeatsStop(t *testing.T) {
	e := start(t)
	n := len(e.records())
	e.cmd("drive", `{"throttle":0.3}`, 1000)
	e.conn.Mute()
	if d := e.waitStop(n, 2*time.Second); d > 600*time.Millisecond {
		t.Fatalf("deadman stop took %s", d)
	}
}

func TestEstopLatchedUntilOwnerClears(t *testing.T) {
	e := start(t)
	n := len(e.records())
	e.cmd("drive", `{"throttle":0.3}`, 1000)
	e.conn.Estop(true, "usr_owner")
	e.waitStop(n, time.Second)
	latched := func(want bool) func(protocol.Message) bool {
		return func(m protocol.Message) bool { return m.(protocol.Status).EstopLatched == want }
	}
	if _, err := e.conn.Expect(protocol.TypeStatus, 2*time.Second, latched(true)); err != nil {
		t.Fatal(err)
	}
	if c := nackCode(e.cmd("drive", `{"throttle":0.3}`, 300)); c != protocol.FaultEstopped {
		t.Fatalf("%q", c)
	}
	if !isAck(e.cmd("halt", `{}`, 300)) {
		t.Fatal("halt refused")
	}
	// Persisted: a restarted Node comes back latched.
	l, _ := safety.OpenLatch(e.paths.LatchFile())
	if !l.State().Remote {
		t.Fatal("e-stop not persisted")
	}
	// Survives a reconnect (config.estop_latched is true too); the device never echoes the server's latch back.
	e.conn.Close()
	e.conn = e.nextConn()
	if _, err := e.conn.Expect(protocol.TypeStatus, 5*time.Second, latched(true)); err != nil {
		t.Fatal(err)
	}
	if c := nackCode(e.cmd("drive", `{"throttle":0.3}`, 300)); c != protocol.FaultEstopped {
		t.Fatalf("after reconnect: %q", c)
	}
	for _, f := range e.conn.Frames() {
		if st, ok := f.Msg.(protocol.EstopState); ok && st.Latched {
			t.Fatalf("remote latch echoed: %+v", st)
		}
	}
	// The owner's clear is estop {latched:false}.
	e.conn.Estop(false, "usr_owner")
	if _, err := e.conn.Expect(protocol.TypeStatus, 2*time.Second, latched(false)); err != nil {
		t.Fatal(err)
	}
	if r := e.cmd("drive", `{"throttle":0.3}`, 300); !isAck(r) {
		t.Fatalf("after clear: %+v", r)
	}
	if l, _ := safety.OpenLatch(e.paths.LatchFile()); l.State().Remote {
		t.Fatal("clear not persisted")
	}
}

// TestConfigEstopOnReconnect: an e-stop set or cleared on the server while the device was away arrives in config and
// is applied before any command.
func TestConfigEstopOnReconnect(t *testing.T) {
	e := start(t)
	e.srv.SetRobotEstop(true, "usr_owner")
	e.conn.Close()
	e.conn = e.nextConn()
	e.waitReady()
	if c := nackCode(e.cmd("drive", `{"throttle":0.3}`, 300)); c != protocol.FaultEstopped {
		t.Fatalf("latched in config: %q", c)
	}
	if l, _ := safety.OpenLatch(e.paths.LatchFile()); !l.State().Remote {
		t.Fatal("config latch not persisted")
	}
	e.srv.SetRobotEstop(false, "usr_owner")
	e.conn.Close()
	e.conn = e.nextConn()
	e.waitReady()
	if r := e.cmd("drive", `{"throttle":0.3}`, 300); !isAck(r) {
		t.Fatalf("cleared in config: %+v", r)
	}
}

func TestLocalKillSwitch(t *testing.T) {
	e := start(t)
	n := len(e.records())
	e.cmd("drive", `{"throttle":0.3}`, 1000)
	if _, err := localctl.Call(e.paths.SocketPath(), localctl.Request{Cmd: localctl.CmdStop}, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	e.waitStop(n, time.Second)
	if c := nackCode(e.cmd("drive", `{"throttle":0.3}`, 300)); c != protocol.FaultLocalStop {
		t.Fatalf("%q", c)
	}
	// The device reports its own latch (estop_state), so the robot shows e-stopped.
	e.waitRobotEstop(true)
	if _, by := e.srv.RobotEstop(); by != "device" {
		t.Fatalf("robot e-stop by %q", by)
	}
	// The owner's clear does not clear the local kill switch.
	e.conn.Estop(false, "usr_owner")
	time.Sleep(100 * time.Millisecond)
	if c := nackCode(e.cmd("drive", `{"throttle":0.3}`, 300)); c != protocol.FaultLocalStop {
		t.Fatalf("after server clear: %q", c)
	}
	if _, err := localctl.Call(e.paths.SocketPath(), localctl.Request{Cmd: localctl.CmdResume}, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if r := e.cmd("drive", `{"throttle":0.3}`, 300); !isAck(r) {
		t.Fatalf("after resume: %+v", r)
	}
	e.waitRobotEstop(false) // resume reported with estop_state latched:false
}

// waitRobotEstop waits until the robot's e-stop on the server is want.
func (e *env) waitRobotEstop(want bool) {
	e.t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if on, _ := e.srv.RobotEstop(); on == want {
			return
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("robot e-stop never became %v", want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestKillSwitchThroughFileWhenSocketUnused(t *testing.T) {
	e := start(t)
	n := len(e.records())
	e.cmd("drive", `{"throttle":0.3}`, 1000)
	if err := safety.WriteLocalFile(e.paths.LatchFile(), true); err != nil {
		t.Fatal(err)
	}
	e.waitStop(n, time.Second)
	if c := nackCode(e.cmd("drive", `{"throttle":0.3}`, 300)); c != protocol.FaultLocalStop {
		t.Fatalf("%q", c)
	}
}

func TestPluginCrashRestartsStopped(t *testing.T) {
	e := start(t)
	pid := e.node.mgr.Get("dryrun").Info().PID
	syscall.Kill(pid, syscall.SIGKILL)
	deadline := time.Now().Add(10 * time.Second)
	for {
		i := e.node.mgr.Get("dryrun").Info()
		if i.Restarts >= 1 && i.State == plugins.StateReady && i.PID != pid {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not restarted: %+v", i)
		}
		time.Sleep(20 * time.Millisecond)
	}
	r := e.records()
	// The restarted plugin ran setup and stopped before taking commands.
	if len(r) < 2 || r[len(r)-2].What != "setup" || r[len(r)-1].What != "stop" {
		t.Fatalf("restart records: %+v", r)
	}
	if !isAck(e.cmd("drive", `{"throttle":0.1}`, 300)) {
		t.Fatal("not driving after restart")
	}
}

func TestShutdownStops(t *testing.T) {
	e := start(t)
	n := len(e.records())
	e.cmd("drive", `{"throttle":0.3}`, 1000)
	e.cancel()
	<-e.done
	found := false
	for _, r := range e.records()[n:] {
		found = found || (r.What == "stop" && r.WasMoving)
	}
	if !found {
		t.Fatalf("no stop on shutdown: %+v", e.records()[n:])
	}
}

func TestVideoAndTelemetry(t *testing.T) {
	e := start(t)
	deadline := time.Now().Add(20 * time.Second)
	for e.whip.Packets.Load() < 10 {
		if time.Now().After(deadline) {
			t.Fatalf("no video: %+v", e.node.Status().Video)
		}
		time.Sleep(50 * time.Millisecond)
	}
	f, err := e.conn.Expect(protocol.TypeTelemetry, 5*time.Second, func(m protocol.Message) bool { return m.(protocol.Telemetry).Battery != nil })
	if err != nil {
		t.Fatal(err)
	}
	if tl := f.Msg.(protocol.Telemetry); *tl.Battery < 0 || *tl.Battery > 1 || tl.Voltage == nil || tl.Sensors["distance_cm"] == nil {
		t.Fatalf("%+v", tl)
	}
	// ≤ 2 Hz.
	var times []int64
	for _, fr := range e.conn.Frames() {
		if fr.Type == protocol.TypeTelemetry && fr.Msg.(protocol.Telemetry).Battery != nil {
			times = append(times, fr.TS)
		}
	}
	for i := 1; i < len(times); i++ {
		if times[i]-times[i-1] < 450 {
			t.Fatalf("telemetry faster than 2 Hz: %v", times)
		}
	}
}
