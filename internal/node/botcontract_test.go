//go:build !windows

package node

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/OpenVibers/OpenVibe.Node/internal/fakebot"
	"github.com/OpenVibers/OpenVibe.Node/internal/localctl"
	"github.com/OpenVibers/OpenVibe.Node/internal/plugins"
	"github.com/OpenVibers/OpenVibe.Node/internal/protocol"
)

// These tests drive the Node with the frames OpenVibe.Bot sends, recorded from its docs/protocol.md into
// internal/protocol/testdata/bot (see its README for the Bot commit).

func botFrame(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "protocol", "testdata", "bot", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return bytes.TrimSpace(b)
}

// restamp moves a recorded frame to now: ts becomes now and deadline_ms keeps its distance from ts.
func restamp(t *testing.T, frame []byte) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(frame, &m); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	ts, _ := m["ts"].(float64)
	if d, ok := m["deadline_ms"].(float64); ok {
		m["deadline_ms"] = now + int64(d-ts)
	}
	m["ts"] = now
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (e *env) commandRecords(id string) []rec {
	var out []rec
	for _, r := range e.records() {
		if r.What == "command" && r.ID == id {
			out = append(out, r)
		}
	}
	return out
}

func (e *env) expectEstopState(latched, local bool) {
	e.t.Helper()
	if _, err := e.conn.Expect(protocol.TypeEstopState, 5*time.Second, func(m protocol.Message) bool {
		s := m.(protocol.EstopState)
		return s.Latched == latched && s.LocalStop == local
	}); err != nil {
		e.t.Fatalf("estop_state latched %v local %v: %v", latched, local, err)
	}
}

func (e *env) ctl(cmd string) {
	e.t.Helper()
	if r, err := localctl.Call(e.paths.SocketPath(), localctl.Request{Cmd: cmd}, 2*time.Second); err != nil || !r.OK {
		e.t.Fatalf("%s: %+v %v", cmd, r, err)
	}
}

// pair → hello + config → drive → ack, with Bot's own frames (only ts and deadline_ms moved to now).
func TestBotFramesDriveAndAck(t *testing.T) {
	e := startWith(t, func(s *fakebot.Server) {
		s.PairBody = protocol.PairBodyFromPaired(botFrame(t, "paired"))
		s.Greeting = [][]byte{restamp(t, botFrame(t, "hello")), restamp(t, botFrame(t, "config"))}
	}, true)
	if c := e.node.opt.Creds; c.DeviceID != "dev_01J8Z4…" || c.RobotID != "rob_01J8Z4M2Q0R7T9YV3K6N8P1W2X" || c.WHIPURL == "" {
		t.Fatalf("paired %+v", c)
	}
	// The server-minted id is the ack and dedup key: Bot never re-sends one, and if it came twice it would run once
	// and be acked twice.
	const id = "cmd_01J8Z4F…"
	for i := 0; i < 2; i++ {
		if err := e.conn.SendRaw(restamp(t, botFrame(t, "command_drive"))); err != nil {
			t.Fatal(err)
		}
		r, err := e.conn.Reply(id, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if !isAck(r) {
			t.Fatalf("reply %+v", r)
		}
	}
	got := e.commandRecords(id)
	if len(got) != 1 {
		t.Fatalf("ran %d times", len(got))
	}
	var v map[string]float64
	_ = json.Unmarshal(got[0].Value, &v)
	if v["throttle"] != 0.6 || v["steer"] != -0.25 || got[0].DeadlineMS < 150 || got[0].DeadlineMS > 300 {
		t.Fatalf("plugin got %s for %d ms", got[0].Value, got[0].DeadlineMS)
	}
}

// deadline_ms is an absolute instant: a drive that arrives after it is nacked and never reaches the plugin, and one
// that arrives late gets only what is left of its window.
func TestExpiredDriveNeverReachesPlugin(t *testing.T) {
	e := start(t)
	// Bot's example drive, replayed as recorded: its deadline passed in 2025.
	if err := e.conn.SendRaw(botFrame(t, "command_drive")); err != nil {
		t.Fatal(err)
	}
	r, err := e.conn.Reply("cmd_01J8Z4F…", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if c := nackCode(r); c != protocol.FaultExpired {
		t.Fatalf("recorded drive: %+v", r)
	}
	// A drive that sat 600 ms on the way: its 300 ms window closed before it arrived.
	send := func(id string, late time.Duration) protocol.Message {
		t.Helper()
		if err := e.conn.CommandLate(protocol.Command{ID: id, Kind: protocol.KindDrive, Value: json.RawMessage(`{"throttle":0.3}`)}, 300, late); err != nil {
			t.Fatal(err)
		}
		r, err := e.conn.Reply(id, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := send("late-600", 600*time.Millisecond); nackCode(r) != protocol.FaultExpired {
		t.Fatalf("late drive: %+v", r)
	}
	// One that sat 100 ms runs for the remaining ~200 ms (less transit, give or take the clock estimate's rounding),
	// not the full 300.
	if r := send("late-100", 100*time.Millisecond); !isAck(r) {
		t.Fatalf("partly late drive: %+v", r)
	}
	if got := e.commandRecords("late-100"); len(got) != 1 || got[0].DeadlineMS < 50 || got[0].DeadlineMS > 210 {
		t.Fatalf("partly late drive reached the plugin as %+v", got)
	}
	if len(e.commandRecords("cmd_01J8Z4F…")) != 0 || len(e.commandRecords("late-600")) != 0 {
		t.Fatalf("an expired drive reached the plugin: %+v", e.records())
	}
}

// The clock estimate keeps the timely hello/config for the whole connection: after a long run of frames that each
// sat 500 ms on the way, a drive whose 300 ms window closed on the way is still nacked, not handed 300 ms.
func TestExpiredDriveAfterDelayedFrames(t *testing.T) {
	e := start(t)
	const late = 500 * time.Millisecond
	for i := 0; i < 40; i++ {
		if err := e.conn.SendLate(protocol.Error{Code: "test.delayed", Detail: "a frame that sat in a queue"}, late); err != nil {
			t.Fatal(err)
		}
	}
	const id = "late-after-delayed"
	if err := e.conn.CommandLate(protocol.Command{ID: id, Kind: protocol.KindDrive, Value: json.RawMessage(`{"throttle":0.3}`)}, 300, late); err != nil {
		t.Fatal(err)
	}
	r, err := e.conn.Reply(id, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if c := nackCode(r); c != protocol.FaultExpired {
		t.Fatalf("expired drive after delayed frames: %+v", r)
	}
	if got := e.commandRecords(id); len(got) != 0 {
		t.Fatalf("an expired drive reached the plugin: %+v", got)
	}
}

// Bot's config for the Adeept allows drive, actuator and halt: any other kind is nacked on the device and never
// reaches the plugin.
func TestDisallowedKindNacked(t *testing.T) {
	e := startWith(t, func(s *fakebot.Server) {
		f, err := protocol.Decode(botFrame(t, "config"))
		if err != nil {
			t.Fatal(err)
		}
		s.Config = f.Msg.(protocol.Config)
	}, true)
	for _, kind := range []string{protocol.KindSay, protocol.KindPTZ, protocol.KindDisplay} {
		if c := nackCode(e.cmd(kind, `{"text":"hi"}`, 300)); c != protocol.FaultNotAllowed {
			t.Fatalf("%s: %q", kind, c)
		}
	}
	if r := e.cmd(protocol.KindDrive, `{"throttle":0.2}`, 300); !isAck(r) {
		t.Fatalf("drive: %+v", r)
	}
	if r := e.cmd(protocol.KindHalt, `{}`, 0); !isAck(r) {
		t.Fatalf("halt: %+v", r)
	}
	for _, r := range e.records() {
		if r.What == "command" && r.Kind != protocol.KindDrive {
			t.Fatalf("a disallowed kind reached the plugin: %+v", r)
		}
	}
}

// Bot sends config right after hello. Until it arrives the device reports nothing and runs nothing but halt, so an
// owner latch set while it was offline is in force before any motion.
func TestNothingBeforeConfig(t *testing.T) {
	e := startWith(t, func(s *fakebot.Server) {
		s.Greeting = [][]byte{restamp(t, botFrame(t, "hello"))}
		s.SetEstop(true)
	}, false)
	if c := nackCode(e.cmdID("early", protocol.KindDrive, `{"throttle":0.3}`, 300)); c != protocol.FaultNotReady {
		t.Fatalf("drive before config: %q", c)
	}
	if !isAck(e.cmdID("early-halt", protocol.KindHalt, `{}`, 0)) {
		t.Fatal("halt refused before config")
	}
	deadline := time.Now().Add(20 * time.Second)
	for e.node.mgr.Get("dryrun").Info().State != plugins.StateReady {
		if time.Now().After(deadline) {
			t.Fatal("plugin not ready")
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, f := range e.conn.Frames() {
		if f.Type == protocol.TypeStatus || f.Type == protocol.TypeEstopState {
			t.Fatalf("%s sent before config", f.Type)
		}
	}
	cfg := e.srv.Config
	cfg.EstopLatched = e.srv.EstopLatched()
	if err := e.conn.Send(cfg); err != nil {
		t.Fatal(err)
	}
	e.waitReady()
	e.expectEstopState(true, false)
	if c := nackCode(e.cmd(protocol.KindDrive, `{"throttle":0.3}`, 300)); c != protocol.FaultEstopped {
		t.Fatalf("drive after a latched config: %q", c)
	}
	if len(e.commandRecords("early")) != 0 {
		t.Fatal("the drive before config reached the plugin")
	}
}

// The owner's latch, set or cleared live or while the device is offline, is what the device holds after each
// reconnect.
func TestOwnerLatchAcrossReconnects(t *testing.T) {
	e := start(t)
	drive := func() protocol.Message { return e.cmd(protocol.KindDrive, `{"throttle":0.3}`, 300) }
	reconnect := func() {
		e.conn.Close()
		e.conn = e.nextConn()
	}
	e.conn.Send(protocol.Estop{Latched: true, By: "usr_owner", At: time.Now().UTC().Format(time.RFC3339Nano)})
	e.expectEstopState(true, false)
	if c := nackCode(drive()); c != protocol.FaultEstopped {
		t.Fatalf("latched live: %q", c)
	}
	// Cleared while offline: the next config says so.
	e.srv.SetEstop(false)
	reconnect()
	e.expectEstopState(false, false)
	if r := drive(); !isAck(r) {
		t.Fatalf("cleared offline: %+v", r)
	}
	// Latched while offline: the first drive after the reconnect is already refused.
	e.srv.SetEstop(true)
	reconnect()
	if c := nackCode(drive()); c != protocol.FaultEstopped {
		t.Fatalf("latched offline: %q", c)
	}
	// Cleared live, with Bot's recorded estop frame (latched:false).
	if err := e.conn.SendRaw(botFrame(t, "estop")); err != nil {
		t.Fatal(err)
	}
	e.expectEstopState(false, false)
	if r := drive(); !isAck(r) {
		t.Fatalf("cleared live: %+v", r)
	}
}

// A driver's own stop (`local_stop` from the plugin) is the local latch with the driver's reason: Bot sees it as a
// local stop, the owner's clear does not lift it, and only the local resume does.
func TestPluginLocalStop(t *testing.T) {
	e := start(t)
	e.node.output(plugins.Output{Kind: plugins.OutLocalStop, Plugin: "dryrun", Data: map[string]any{"reason": "LOOI stop button"}})
	m, err := e.conn.Expect(protocol.TypeEstopState, 5*time.Second, func(m protocol.Message) bool {
		s := m.(protocol.EstopState)
		return s.Latched && s.LocalStop
	})
	if err != nil {
		t.Fatal(err)
	}
	if r := m.Msg.(protocol.EstopState).Reason; r != "LOOI stop button" {
		t.Fatalf("reason %q", r)
	}
	if st := e.node.latch.State(); !st.Local || st.LocalReason != "LOOI stop button" {
		t.Fatalf("latch %+v", st)
	}
	e.conn.Send(protocol.Estop{Latched: false, By: "usr_owner", At: time.Now().UTC().Format(time.RFC3339Nano)})
	e.expectEstopState(true, true)
	if c := nackCode(e.cmd(protocol.KindDrive, `{"throttle":0.3}`, 300)); c != protocol.FaultLocalStop {
		t.Fatalf("after the owner's clear: %q", c)
	}
	e.ctl(localctl.CmdResume)
	e.expectEstopState(false, false)
}

// A remote clear never clears the local latch (`openvibe-node stop`): the device keeps refusing motion and keeps
// reporting itself latched, across a reconnect, until the local resume (which clears both latches on the device).
// Its latched:false after the resume is only a report; Bot's latch stays for the owner to clear.
func TestLocalLatchSurvivesRemoteClear(t *testing.T) {
	e := start(t)
	e.ctl(localctl.CmdStop)
	e.expectEstopState(true, true)
	if !e.srv.EstopLatched() {
		t.Fatal("Bot did not latch the robot from the device's report")
	}
	e.conn.Send(protocol.Estop{Latched: false, By: "usr_owner", At: time.Now().UTC().Format(time.RFC3339Nano)})
	e.expectEstopState(true, true)
	if !e.srv.EstopLatched() {
		t.Fatal("the device did not report its local latch after the owner's clear")
	}
	if c := nackCode(e.cmd(protocol.KindDrive, `{"throttle":0.3}`, 300)); c != protocol.FaultLocalStop {
		t.Fatalf("after the remote clear: %q", c)
	}
	e.conn.Close()
	e.conn = e.nextConn()
	e.expectEstopState(true, true)
	if c := nackCode(e.cmd(protocol.KindDrive, `{"throttle":0.3}`, 300)); c != protocol.FaultLocalStop {
		t.Fatalf("after a reconnect: %q", c)
	}
	e.ctl(localctl.CmdResume)
	e.expectEstopState(false, false)
	if !e.srv.EstopLatched() {
		t.Fatal("the device's latched:false cleared Bot's latch")
	}
}

// turn_epoch (contracts 0.213.0, Bot#155): once a halt carried a robot's new control epoch, a command decided under an
// older one is nacked expired and never reaches the plugin; the current epoch runs, and a command without one (an older
// Bot) runs as before.
func TestStaleTurnNeverReachesPlugin(t *testing.T) {
	e := start(t)
	send := func(c protocol.Command) protocol.Message {
		t.Helper()
		if err := e.conn.Command(c, 300); err != nil {
			t.Fatal(err)
		}
		r, err := e.conn.Reply(c.ID, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	drive := json.RawMessage(`{"throttle":0.3}`)
	if r := send(protocol.Command{ID: "before", Kind: protocol.KindDrive, Value: drive, TurnEpoch: 100}); !isAck(r) {
		t.Fatalf("a command before any halt: %+v", r)
	}
	if r := send(protocol.Command{ID: "halt-200", Kind: protocol.KindHalt, TurnEpoch: 200, Operator: &protocol.Operator{Role: "system"}}); !isAck(r) {
		t.Fatalf("the turn's halt: %+v", r)
	}
	if r := send(protocol.Command{ID: "stale", Kind: protocol.KindDrive, Value: drive, TurnEpoch: 100}); nackCode(r) != protocol.FaultExpired {
		t.Fatalf("a command from the ended turn: %+v", r)
	}
	if r := send(protocol.Command{ID: "current", Kind: protocol.KindDrive, Value: drive, TurnEpoch: 200}); !isAck(r) {
		t.Fatalf("a command of the current turn: %+v", r)
	}
	if r := send(protocol.Command{ID: "no-epoch", Kind: protocol.KindDrive, Value: drive}); !isAck(r) {
		t.Fatalf("a command without an epoch (an older Bot): %+v", r)
	}
	if len(e.commandRecords("stale")) != 0 || len(e.commandRecords("current")) != 1 {
		t.Fatalf("records: %+v", e.records())
	}
}

// Provenance (Maticus, 2026-10-26, for the LOOI driver): each command reaches the driver with the link session it came
// on (Bot's hello), its frame's seq (strictly increasing on that session), Bot's send time and its absolute deadline,
// so a driver can refuse a replay with constant state. A new connection has a new session.
func TestProvenanceReachesPlugin(t *testing.T) {
	e := start(t)
	drive := json.RawMessage(`{"throttle":0.3}`)
	for _, id := range []string{"p1", "p2"} {
		if err := e.conn.Command(protocol.Command{ID: id, Kind: protocol.KindDrive, Value: drive}, 300); err != nil {
			t.Fatal(err)
		}
		if r, err := e.conn.Reply(id, 5*time.Second); err != nil || !isAck(r) {
			t.Fatalf("%s: %+v %v", id, r, err)
		}
	}
	a, b := e.commandRecords("p1"), e.commandRecords("p2")
	if len(a) != 1 || len(b) != 1 || a[0].Provenance == nil || b[0].Provenance == nil {
		t.Fatalf("records: %+v", e.records())
	}
	pa, pb := a[0].Provenance, b[0].Provenance
	if !strings.HasPrefix(pa.Session, "sess_") || pa.Session != pb.Session || pb.Seq <= pa.Seq {
		t.Fatalf("session and seq: %+v %+v", pa, pb)
	}
	if pa.IssuedAtMS <= 0 || pa.ExpiresAtMS != pa.IssuedAtMS+300 {
		t.Fatalf("times: %+v", pa)
	}
	if pa.ClockUncertaintyMS == nil || *pa.ClockUncertaintyMS < 0 || *pa.ClockUncertaintyMS > protocol.MaxClockUncertaintyMS {
		t.Fatalf("clock uncertainty: %+v", pa)
	}
	e.conn.Close()
	e.conn = e.nextConn()
	e.waitSynced()
	if err := e.conn.Command(protocol.Command{ID: "p3", Kind: protocol.KindDrive, Value: drive}, 300); err != nil {
		t.Fatal(err)
	}
	if r, err := e.conn.Reply("p3", 5*time.Second); err != nil || !isAck(r) {
		t.Fatalf("p3: %+v %v", r, err)
	}
	if c := e.commandRecords("p3"); len(c) != 1 || c[0].Provenance == nil || c[0].Provenance.Session == pa.Session {
		t.Fatalf("a new connection, a new session: %+v", c)
	}
}

// Motion is timed only on a bounded clock (Maticus, 2026-10-27): before the connection's first measured round trip a
// drive with a deadline is refused as not_ready and never reaches the plugin; once one is measured, the same drive runs,
// with the bound in its provenance, and its window is the deadline less that bound.
func TestMotionWaitsForABoundedClock(t *testing.T) {
	e := startWith(t, func(s *fakebot.Server) { s.HoldHeartbeatAcks = true }, true)
	drive := json.RawMessage(`{"throttle":0.3}`)
	if err := e.conn.Command(protocol.Command{ID: "unsynced", Kind: protocol.KindDrive, Value: drive}, 300); err != nil {
		t.Fatal(err)
	}
	r, err := e.conn.Reply("unsynced", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if n, ok := r.(protocol.Nack); !ok || n.FaultCode != protocol.FaultNotReady || !strings.Contains(n.Message, "not synchronized") {
		t.Fatalf("before a round trip: %+v", r)
	}
	if got := e.commandRecords("unsynced"); len(got) != 0 {
		t.Fatalf("an unsynchronized drive reached the plugin: %+v", got)
	}
	// A say is not motion: it is not held for the clock.
	if r := e.cmdID("say-unsynced", protocol.KindSay, `{"text":"hi"}`, 300); !isAck(r) {
		t.Fatalf("say before a round trip: %+v", r)
	}
	e.conn.SetAutoAck(true)
	e.waitSynced()
	if err := e.conn.Command(protocol.Command{ID: "synced", Kind: protocol.KindDrive, Value: drive}, 300); err != nil {
		t.Fatal(err)
	}
	if r, err := e.conn.Reply("synced", 5*time.Second); err != nil || !isAck(r) {
		t.Fatalf("after a round trip: %+v %v", r, err)
	}
	got := e.commandRecords("synced")
	if len(got) != 1 || got[0].Provenance == nil || got[0].Provenance.ClockUncertaintyMS == nil {
		t.Fatalf("records: %+v", got)
	}
	if unc := *got[0].Provenance.ClockUncertaintyMS; got[0].DeadlineMS > 300-int(unc) {
		t.Fatalf("the window %d ms ignores the clock bound %d ms", got[0].DeadlineMS, unc)
	}
}
