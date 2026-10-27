import io
import json
import os
import subprocess
import sys
import threading
import time
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.dirname(HERE))

import openvibe_plugin  # noqa: E402
from openvibe_plugin import SHARED_CLOCK, Deferred, Fault, Plugin, Runtime, StopInfo  # noqa: E402


class FakeClock:
    def __init__(self):
        self.t = 100.0

    def __call__(self):
        return self.t


class Recorder(Plugin):
    driver = "recorder"
    version = "1.0"

    def __init__(self):
        self.calls = []
        self.handled = []
        self.moving = False

    def describe(self, config):
        self.calls.append("describe")
        return {"drive": {"type": "differential"}}

    def setup(self, config):
        self.calls.append("setup")

    def handle(self, cmd):
        self.handled.append(cmd)
        if cmd.kind == "drive":
            self.moving = True
            return
        if cmd.kind == "say":
            return
        raise Fault("unsupported")

    def stop(self):
        self.calls.append("stop")
        self.moving = False


class FakeShared:
    """The clock shared with the core, in ns (a different origin from the runtime's own, as in life)."""
    def __init__(self):
        self.ns = 7_000_000_000_000

    def __call__(self):
        return self.ns


def make(plugin=None, shared=None):
    out = io.StringIO()
    clock = FakeClock()
    p = plugin or Recorder()
    rt = Runtime(p, infile=io.StringIO(""), outfile=out, clock=clock, shared_clock=shared)
    return rt, p, out, clock


def lines(out):
    return [json.loads(x) for x in out.getvalue().splitlines() if x]


class RuntimeTest(unittest.TestCase):
    def hello(self, rt):
        rt.feed_line(json.dumps({"op": "hello", "config": {}}))

    def test_describe_then_setup_stopped(self):
        rt, p, out, _ = make()
        self.hello(rt)
        msgs = lines(out)
        self.assertEqual(msgs[0]["op"], "describe")
        self.assertEqual(msgs[0]["driver"], "recorder")
        self.assertEqual(msgs[1]["op"], "ready")
        self.assertEqual(p.calls[:3], ["describe", "setup", "stop"])

    def test_probe_never_touches_hardware(self):
        rt, p, out, _ = make()
        rt.feed_line(json.dumps({"op": "hello", "config": {}, "probe": True}))
        self.assertEqual(p.calls, ["describe"])
        self.assertEqual(lines(out)[-1]["op"], "describe")

    def test_deadline_stops(self):
        rt, p, out, clock = make()
        self.hello(rt)
        rt.feed_line(json.dumps({"op": "command", "id": "c1", "kind": "drive", "value": {}, "deadline_ms": 300}))
        self.assertTrue(p.moving)
        clock.t += 0.29
        rt.feed_line(json.dumps({"op": "heartbeat", "t": 1}))
        rt.tick()
        self.assertTrue(p.moving)
        clock.t += 0.02
        rt.tick()
        self.assertFalse(p.moving)
        self.assertIn("deadline", rt.stop_reasons)

    def test_operator_reaches_handle(self):
        seen = []

        class Who(Recorder):
            def handle(self, cmd):
                seen.append(cmd.operator)

        rt, p, out, clock = make(Who())
        self.hello(rt)
        rt.feed_line(json.dumps({"op": "command", "id": "c1", "kind": "drive", "value": {}, "deadline_ms": 300,
                                 "operator": {"subject": "usr_1", "role": "queue"}}))
        rt.feed_line(json.dumps({"op": "command", "id": "c2", "kind": "drive", "value": {}, "deadline_ms": 300}))
        rt.feed_line(json.dumps({"op": "command", "id": "c3", "kind": "drive", "value": {}, "deadline_ms": 300,
                                 "operator": "nobody"}))
        self.assertEqual(seen, [{"subject": "usr_1", "role": "queue"}, None, None])

    def test_turn_epoch_reaches_handle(self):
        seen = []

        class Epoch(Recorder):
            def handle(self, cmd):
                seen.append(cmd.turn_epoch)

        rt, p, out, clock = make(Epoch())
        self.hello(rt)
        rt.feed_line(json.dumps({"op": "command", "id": "c1", "kind": "drive", "value": {}, "deadline_ms": 300,
                                 "turn_epoch": 1792900000123}))
        rt.feed_line(json.dumps({"op": "command", "id": "c2", "kind": "drive", "value": {}, "deadline_ms": 300}))
        rt.feed_line(json.dumps({"op": "command", "id": "c3", "kind": "drive", "value": {}, "deadline_ms": 300,
                                 "turn_epoch": "soon"}))
        self.assertEqual(seen, [1792900000123, None, None])

    def test_provenance_reaches_handle(self):
        seen = []

        class Prov(Recorder):
            def handle(self, cmd):
                seen.append(cmd.provenance)

        rt, p, out, clock = make(Prov())
        self.hello(rt)
        prov = {"session": "sess_01J9", "seq": 7, "issued_at_ms": 1792900000000, "expires_at_ms": 1792900000300}
        rt.feed_line(json.dumps({"op": "command", "id": "c1", "kind": "drive", "value": {}, "deadline_ms": 300,
                                 "provenance": prov}))
        rt.feed_line(json.dumps({"op": "command", "id": "c2", "kind": "drive", "value": {}, "deadline_ms": 300}))
        rt.feed_line(json.dumps({"op": "command", "id": "c3", "kind": "drive", "value": {}, "deadline_ms": 300,
                                 "provenance": {"session": 5, "seq": "8", "issued_at_ms": True}}))
        self.assertEqual(seen, [prov, None, None])

    def test_replay_on_the_same_session_is_refused(self):
        # Bot sends a command once and never again; if a frame came back anyway, its seq is not above the last one
        # handled on its session, and the runtime refuses it with no state but that pair.
        rt, p, out, clock = make()
        self.hello(rt)

        def cmd(cid, session, seq):
            rt.feed_line(json.dumps({"op": "command", "id": cid, "kind": "drive", "value": {}, "deadline_ms": 300,
                                     "provenance": {"session": session, "seq": seq, "issued_at_ms": 1792900000000}}))
            return lines(out)[-1]

        self.assertEqual(cmd("c1", "sess_A", 5)["op"], "ack")
        replay = cmd("c1-again", "sess_A", 5)
        self.assertEqual((replay["op"], replay["fault_code"]), ("nack", "expired"))
        self.assertIn("replay", replay["message"])
        self.assertEqual(cmd("c0", "sess_A", 3)["op"], "nack", "an older frame on the same session")
        self.assertEqual(cmd("c2", "sess_A", 9)["op"], "ack", "seq has gaps")
        self.assertEqual(cmd("c3", "sess_B", 1)["op"], "ack", "a new connection starts its own count")

    def test_a_halts_epoch_refuses_older_turns(self):
        rt, p, out, clock = make()
        self.hello(rt)

        def drive(cid, epoch):
            rt.feed_line(json.dumps({"op": "command", "id": cid, "kind": "drive", "value": {}, "deadline_ms": 300,
                                     "turn_epoch": epoch}))
            return lines(out)[-1]

        self.assertEqual(drive("c1", 100)["op"], "ack")
        rt.feed_line(json.dumps({"op": "stop", "turn_epoch": 200}))
        stale = drive("c2", 100)
        self.assertEqual((stale["op"], stale["fault_code"]), ("nack", "expired"))
        self.assertEqual(drive("c3", 200)["op"], "ack")
        rt.feed_line(json.dumps({"op": "command", "id": "h", "kind": "halt", "turn_epoch": 300}))
        self.assertEqual(drive("c4", 200)["op"], "nack", "a halt command's epoch counts too")
        rt.feed_line(json.dumps({"op": "stop"}))
        self.assertEqual(drive("c5", 300)["op"], "ack", "a stop without an epoch changes nothing")

    def test_stop_causes_reach_on_stop(self):
        """A driver tells an ordinary stop from an emergency (Maticus, 2026-10-27, P7): every stop still stops; only an
        emergency latches the driver's own hardware stop; resume reports the platform's clear and re-arms nothing."""

        class P7Like(Recorder):
            def __init__(self):
                super().__init__()
                self.infos, self.resumes, self.physical_latch = [], [], False

            def on_stop(self, info):
                self.infos.append(info)
                if info.emergency:
                    self.physical_latch = True
                self.stop()

            def on_resume(self, info):
                self.resumes.append(info)

        rt, p, out, clock = make(P7Like())
        self.hello(rt)
        feed = lambda m: rt.feed_line(json.dumps(m))  # noqa: E731
        feed({"op": "stop", "cause": "halt", "turn_epoch": 7})
        feed({"op": "stop", "cause": "link_lost"})
        self.assertEqual([(i.cause, i.emergency, i.latched) for i in p.infos],
                         [("setup", False, False), ("halt", False, False), ("link_lost", False, False)])
        self.assertEqual((p.infos[1].turn_epoch, p.infos[1].halt_epoch, p.infos[2].halt_epoch), (7, 7, 7))
        self.assertFalse(p.physical_latch, "an ordinary stop latches nothing")
        feed({"op": "estop", "cause": "remote", "reason": "staff: it wobbled"})
        e = p.infos[-1]
        self.assertEqual((e.cause, e.emergency, e.latched, e.reason, e.replayed), ("remote", True, True, "staff: it wobbled", False))
        self.assertTrue(p.physical_latch)
        feed({"op": "resume", "cause": "cleared"})
        self.assertEqual([r.cause for r in p.resumes], ["cleared"])
        self.assertTrue(p.physical_latch, "resume never clears the driver's own latch")
        self.assertFalse(rt.estopped)
        # The same emergency replayed to a restarted process, and an older core's bare ops.
        feed({"op": "estop", "cause": "latched", "first_cause": "remote", "reason": "staff: it wobbled", "replayed": True})
        self.assertEqual((p.infos[-1].cause, p.infos[-1].replayed, p.infos[-1].first_cause), ("latched", True, "remote"))
        feed({"op": "stop"})
        feed({"op": "estop"})
        self.assertEqual([(i.cause, i.emergency) for i in p.infos[-2:]], [("stop", False), ("estop", True)])
        self.assertEqual(p.calls.count("stop"), len(p.infos), "every stop stopped")
        # The runtime's own stops say why too: the driver's own local stop is an emergency.
        rt.local_stop("bumper")
        self.assertEqual((p.infos[-1].cause, p.infos[-1].emergency, p.infos[-1].reason), ("driver", True, "bumper"))

    def test_on_stop_defaults_to_stop_and_stop_motion(self):
        """A plugin that overrides only stop() and stop_motion() behaves as before; the runtime still records why."""

        class Wheels(Recorder):
            def stop_motion(self):
                self.calls.append("stop_motion")

        rt, p, out, clock = make(Wheels())
        self.hello(rt)
        rt.feed_line(json.dumps({"op": "command", "id": "d", "kind": "drive", "value": {}, "deadline_ms": 300}))
        clock.t += 1.0
        rt.tick()
        rt.feed_line(json.dumps({"op": "estop", "cause": "remote"}))
        self.assertEqual(p.calls[-2:], ["stop_motion", "stop"])
        self.assertEqual([(i.cause, i.motion_only, i.emergency) for i in rt.stop_infos[-2:]],
                         [("deadline", True, False), ("remote", False, True)])
        self.assertIsInstance(rt.stop_infos[-1], StopInfo)

    def test_halt_epoch_from_hello_survives_a_restart(self):
        rt, p, out, clock = make()
        rt.feed_line(json.dumps({"op": "hello", "config": {}, "halt_epoch": 50}))

        def drive(cid, epoch):
            rt.feed_line(json.dumps({"op": "command", "id": cid, "kind": "drive", "value": {}, "deadline_ms": 300,
                                     "turn_epoch": epoch}))
            return lines(out)[-1]

        self.assertEqual(drive("old", 40)["fault_code"], "expired")
        self.assertEqual(drive("now", 50)["op"], "ack")

    def test_clock_uncertainty_reaches_handle(self):
        seen = []

        class Keep(Recorder):
            def handle(self, cmd):
                seen.append(cmd.provenance)

        rt, p, out, clock = make(Keep())
        self.hello(rt)
        for i, u in enumerate((0, 42)):
            rt.feed_line(json.dumps({"op": "command", "id": "c%d" % i, "kind": "say", "value": {}, "deadline_ms": 300,
                                     "provenance": {"session": "s", "seq": i + 1, "clock_uncertainty_ms": u}}))
        self.assertEqual([s.get("clock_uncertainty_ms") for s in seen], [0, 42])

    def test_newer_command_extends_deadline(self):
        rt, p, out, clock = make()
        self.hello(rt)
        rt.feed_line(json.dumps({"op": "command", "id": "c1", "kind": "drive", "deadline_ms": 300}))
        clock.t += 0.15
        rt.feed_line(json.dumps({"op": "command", "id": "c2", "kind": "drive", "deadline_ms": 300}))
        clock.t += 0.2
        rt.feed_line(json.dumps({"op": "heartbeat", "t": 1}))
        rt.tick()
        self.assertTrue(p.moving)
        clock.t += 0.2
        rt.tick()
        self.assertFalse(p.moving)

    def test_backlog_counts_from_arrival(self):
        # A command that waited in the inbox while a handler blocked keeps its arrival-based deadline, and one whose
        # deadline passed while it waited is refused without reaching the driver: it never moves the robot at all.
        rt, p, out, clock = make()
        self.hello(rt)
        arrived = clock.t
        clock.t += 0.5  # the handler was stuck for 500 ms
        rt.feed_line(json.dumps({"op": "heartbeat", "t": 1}), arrived)
        rt.feed_line(json.dumps({"op": "command", "id": "old", "kind": "drive", "deadline_ms": 300}), arrived)
        last = lines(out)[-1]
        self.assertEqual((last["op"], last["id"], last["fault_code"]), ("nack", "old", "expired"))
        self.assertIn("500 ms after it arrived", last["message"])
        self.assertFalse(p.moving)
        # One that waited less than its deadline still runs, with only the time it has left.
        rt.feed_line(json.dumps({"op": "command", "id": "fresh", "kind": "drive", "deadline_ms": 300}), clock.t - 0.1)
        self.assertEqual(lines(out)[-1], {"op": "ack", "id": "fresh"})
        self.assertTrue(p.moving)
        clock.t += 0.21
        rt.tick()
        self.assertFalse(p.moving)
        self.assertIn("deadline", rt.stop_reasons)

    def test_pipe_delay_counts_against_the_deadline(self):
        # Node 0.18 (Maticus, 2026-10-27): the core writes deadline_ms as what was left when it wrote the line, and
        # expires_at_mono_ns as the instant on the clock it shares with this process. Time the line then sat in the pipe
        # counts: one that expired there never reaches handle(), and one still valid gets only what is left.
        shared = FakeShared()
        rt, p, out, clock = make(shared=shared)
        self.hello(rt)
        rt.feed_line(json.dumps({"op": "heartbeat", "t": 1}))
        # Written with 100 ms left; read 200 ms later: expired in the pipe.
        written = shared.ns
        shared.ns += 200_000_000
        clock.t += 0.2
        rt.feed_line(json.dumps({"op": "command", "id": "late", "kind": "drive", "deadline_ms": 100,
                                 "expires_at_mono_ns": written + 100_000_000}), clock.t, shared.ns)
        last = lines(out)[-1]
        self.assertEqual((last["op"], last["id"], last["fault_code"]), ("nack", "late", "expired"))
        self.assertIn("before the runtime read it (100 ms late", last["message"])
        self.assertEqual(p.handled, [])
        self.assertFalse(p.moving)
        # Written with 300 ms left; read 260 ms later: 40 ms left, and the motion stops when they run out.
        written = shared.ns
        shared.ns += 260_000_000
        clock.t += 0.26
        rt.feed_line(json.dumps({"op": "heartbeat", "t": 2}))
        rt.feed_line(json.dumps({"op": "command", "id": "ontime", "kind": "drive", "deadline_ms": 300,
                                 "expires_at_mono_ns": written + 300_000_000}), clock.t, shared.ns)
        self.assertEqual(lines(out)[-1], {"op": "ack", "id": "ontime"})
        cmd = p.handled[-1]
        self.assertEqual(cmd.deadline_ms, 40)
        self.assertAlmostEqual(cmd.deadline, clock.t + 0.040, places=9)
        clock.t += 0.039
        rt.tick()
        self.assertTrue(p.moving)
        clock.t += 0.002
        rt.tick()
        self.assertFalse(p.moving)
        self.assertEqual(rt.stop_reasons[-1], "deadline")
        # Less than a millisecond left counts as none (deadline_ms is whole milliseconds, at least 1).
        rt.feed_line(json.dumps({"op": "command", "id": "edge", "kind": "drive", "deadline_ms": 300,
                                 "expires_at_mono_ns": shared.ns + 999_999}), clock.t, shared.ns)
        self.assertEqual(lines(out)[-1]["fault_code"], "expired")
        # A line that waited in the pipe and then in the inbox: both count, the inbox on the runtime's clock as before.
        written = shared.ns
        rt.feed_line(json.dumps({"op": "heartbeat", "t": 3}))
        arrived, shared_arrived = clock.t, shared.ns + 150_000_000
        clock.t += 0.2
        rt.feed_line(json.dumps({"op": "command", "id": "both", "kind": "drive", "deadline_ms": 300,
                                 "expires_at_mono_ns": written + 300_000_000}), arrived, shared_arrived)
        last = lines(out)[-1]
        self.assertEqual((last["id"], last["fault_code"]), ("both", "expired"))
        self.assertEqual([c.id for c in p.handled], ["ontime"])

    def test_motion_kinds_only_and_deadline_ms_alone_without_a_shared_clock(self):
        # say is not motion: its deadline is a reply window, and expires_at_mono_ns does not apply to it.
        shared = FakeShared()
        rt, p, out, clock = make(shared=shared)
        self.hello(rt)
        rt.feed_line(json.dumps({"op": "command", "id": "s", "kind": "say", "deadline_ms": 300,
                                 "expires_at_mono_ns": shared.ns - 1}), clock.t, shared.ns)
        self.assertEqual(lines(out)[-1], {"op": "ack", "id": "s"})
        # No clock shared with the core (Windows): deadline_ms alone, from arrival.
        rt, p, out, clock = make()
        self.hello(rt)
        rt.feed_line(json.dumps({"op": "heartbeat", "t": 1}))
        rt.feed_line(json.dumps({"op": "command", "id": "d", "kind": "drive", "deadline_ms": 300,
                                 "expires_at_mono_ns": 1}), clock.t)
        self.assertEqual(lines(out)[-1], {"op": "ack", "id": "d"})
        self.assertEqual(p.handled[-1].deadline_ms, 300)

    @unittest.skipIf(SHARED_CLOCK is None, "no clock shared with the core on this platform")
    def test_a_real_pipe_delay_never_extends_a_deadline(self):
        # The real thing: an OS pipe, the real clocks, the runtime's own reader thread. Lines are written as the core
        # writes them and left in the pipe for 200 ms before the runtime starts reading.
        r, w = os.pipe()
        infile = os.fdopen(r, "r", encoding="utf-8")
        out = io.StringIO()
        p = Recorder()
        rt = Runtime(p, infile=infile, outfile=out)
        now = SHARED_CLOCK()
        msgs = [
            {"op": "hello", "config": {}},
            {"op": "heartbeat", "t": 1},
            {"op": "command", "id": "late", "kind": "drive", "deadline_ms": 100, "expires_at_mono_ns": now + 100_000_000},
            {"op": "command", "id": "ontime", "kind": "drive", "deadline_ms": 1000, "expires_at_mono_ns": now + 1_000_000_000},
        ]
        os.write(w, "".join(json.dumps(m) + "\n" for m in msgs).encode())
        time.sleep(0.2)
        t = threading.Thread(target=rt.run)
        t.start()
        time.sleep(0.1)
        os.close(w)
        t.join(5)
        infile.close()
        self.assertFalse(t.is_alive())
        got = {m["id"]: m for m in lines(out) if m.get("op") in ("ack", "nack")}
        self.assertEqual((got["late"]["op"], got["late"]["fault_code"]), ("nack", "expired"))
        self.assertEqual(got["ontime"]["op"], "ack")
        self.assertEqual([c.id for c in p.handled], ["ontime"])
        # It got only what was left of its original second: at most 800 ms after 200 ms in the pipe.
        self.assertLessEqual(p.handled[0].deadline_ms, 800)
        self.assertGreater(p.handled[0].deadline_ms, 0)

    def test_a_replayed_estop_is_applied_before_any_motion(self):
        # A restarted process hears, in this order: hello (with the newest halt epoch), the latched e-stop replayed,
        # and only then commands (the core routes none to it before its ready, and lines reach it in the order they
        # were queued). No motion handler runs while the replayed latch holds; resume is information only.
        rt, p, out, clock = make()
        rt.feed_line(json.dumps({"op": "hello", "config": {}, "halt_epoch": 1792900000500}))
        rt.feed_line(json.dumps({"op": "estop", "cause": "latched", "first_cause": "remote", "reason": "staff: it wobbled",
                                 "replayed": True}))
        rt.feed_line(json.dumps({"op": "heartbeat", "t": 1}))
        rt.feed_line(json.dumps({"op": "command", "id": "m1", "kind": "drive", "deadline_ms": 300,
                                 "turn_epoch": 1792900000500}))
        self.assertEqual(lines(out)[-1], {"op": "nack", "id": "m1", "fault_code": "estopped"})
        self.assertEqual(p.handled, [])
        self.assertFalse(p.moving)
        self.assertTrue(rt.stop_infos[-1].emergency and rt.stop_infos[-1].replayed and rt.stop_infos[-1].latched)
        self.assertEqual(rt.stop_infos[-1].halt_epoch, 1792900000500)

    def test_the_package_reports_the_node_release(self):
        self.assertEqual(openvibe_plugin.__version__, "0.22.0")
        with open(os.path.join(os.path.dirname(HERE), "pyproject.toml")) as f:
            self.assertIn('version = "%s"' % openvibe_plugin.__version__, f.read())

    def test_only_motion_expires_in_the_backlog(self):
        # say has no motion deadline to miss (its deadline is a reply window): a late one still runs.
        rt, p, out, clock = make()
        self.hello(rt)
        arrived = clock.t
        clock.t += 0.5
        rt.feed_line(json.dumps({"op": "heartbeat", "t": 1}), arrived)
        rt.feed_line(json.dumps({"op": "command", "id": "s1", "kind": "say", "deadline_ms": 300}), arrived)
        self.assertEqual(lines(out)[-1], {"op": "ack", "id": "s1"})

    def test_heartbeat_loss_stops_and_refuses(self):
        rt, p, out, clock = make()
        self.hello(rt)
        rt.feed_line(json.dumps({"op": "command", "id": "c1", "kind": "drive", "deadline_ms": 5000}))
        clock.t += 1.01
        rt.tick()
        self.assertFalse(p.moving)
        self.assertIn("heartbeat", rt.stop_reasons)
        rt.feed_line(json.dumps({"op": "command", "id": "c2", "kind": "drive"}))
        self.assertEqual(lines(out)[-1], {"op": "nack", "id": "c2", "fault_code": "no_heartbeat"})
        rt.feed_line(json.dumps({"op": "heartbeat", "t": 2}))
        rt.feed_line(json.dumps({"op": "command", "id": "c3", "kind": "drive"}))
        self.assertEqual(lines(out)[-1], {"op": "ack", "id": "c3"})

    def test_estop_latches_until_resume(self):
        rt, p, out, clock = make()
        self.hello(rt)
        rt.feed_line(json.dumps({"op": "command", "id": "c1", "kind": "drive"}))
        rt.feed_line(json.dumps({"op": "estop"}))
        self.assertFalse(p.moving)
        rt.feed_line(json.dumps({"op": "command", "id": "c2", "kind": "drive"}))
        self.assertEqual(lines(out)[-1]["fault_code"], "estopped")
        rt.feed_line(json.dumps({"op": "command", "id": "c3", "kind": "say", "value": {"text": "hi"}}))
        self.assertEqual(lines(out)[-1]["op"], "ack")
        rt.feed_line(json.dumps({"op": "resume"}))
        rt.feed_line(json.dumps({"op": "command", "id": "c4", "kind": "drive"}))
        self.assertEqual(lines(out)[-1]["op"], "ack")

    def test_local_stop_latches_and_ignores_a_crossing_resume(self):
        rt, p, out, clock = make()
        self.hello(rt)
        rt.feed_line(json.dumps({"op": "command", "id": "c1", "kind": "drive"}))
        p.local_stop("stop button")
        self.assertFalse(p.moving)
        self.assertEqual(lines(out)[-1], {"op": "local_stop", "reason": "stop button"})
        rt.feed_line(json.dumps({"op": "resume"}))  # sent before the core saw the local stop
        rt.feed_line(json.dumps({"op": "command", "id": "c2", "kind": "drive"}))
        self.assertEqual(lines(out)[-1]["fault_code"], "estopped")
        rt.feed_line(json.dumps({"op": "estop"}))  # the core latched it
        rt.feed_line(json.dumps({"op": "resume"}))  # `openvibe-node resume` on the machine
        rt.feed_line(json.dumps({"op": "command", "id": "c3", "kind": "drive"}))
        self.assertEqual(lines(out)[-1]["op"], "ack")

    def test_a_failed_setup_stays_failed_without_retry_setup_s(self):
        class Down(Recorder):
            def setup(self, config):
                raise Fault("unreachable", "not up yet")
        rt, p, out, clock = make(Down())
        self.hello(rt)
        clock.t += 60
        rt.tick()
        self.assertEqual([m["op"] for m in lines(out)], ["describe", "fault"])
        self.assertEqual(p.calls.count("describe"), 1)

    def test_retry_setup_s_sets_up_again_and_ready_ends_the_fault(self):
        class Late(Recorder):
            retry_setup_s = 2
            tries = 0

            def setup(self, config):
                Late.tries += 1
                if Late.tries < 3:
                    raise Fault("unreachable", "not up yet")
                self.calls.append("setup")
        rt, p, out, clock = make(Late())
        self.hello(rt)
        rt.feed_line(json.dumps({"op": "command", "id": "c0", "kind": "drive", "value": {}}))
        self.assertEqual(lines(out)[-1]["fault_code"], "not_ready")
        clock.t += 1
        rt.tick()
        self.assertEqual(Late.tries, 1, "not before retry_setup_s")
        clock.t += 1.5
        rt.tick()
        self.assertEqual(Late.tries, 2)
        self.assertEqual([m["op"] for m in lines(out)].count("fault"), 1, "the fault is sent once, not on every retry")
        clock.t += 2.5
        rt.tick()
        self.assertEqual(Late.tries, 3)
        self.assertEqual(lines(out)[-1]["op"], "ready")
        self.assertTrue(rt.ready)
        rt.feed_line(json.dumps({"op": "command", "id": "c1", "kind": "drive", "value": {}}))
        self.assertEqual(lines(out)[-1], {"op": "ack", "id": "c1"})
        clock.t += 10
        rt.tick()
        self.assertEqual(Late.tries, 3, "no more attempts once ready")

    def test_halt_always_accepted(self):
        rt, p, out, clock = make()
        self.hello(rt)
        rt.feed_line(json.dumps({"op": "estop"}))
        rt.feed_line(json.dumps({"op": "command", "id": "h", "kind": "halt"}))
        self.assertEqual(lines(out)[-1], {"op": "ack", "id": "h"})

    def test_unsupported_nack(self):
        rt, p, out, clock = make()
        self.hello(rt)
        rt.feed_line(json.dumps({"op": "command", "id": "x", "kind": "display"}))
        self.assertEqual(lines(out)[-1]["fault_code"], "unsupported")

    def test_eof_stops(self):
        p = Recorder()
        out = io.StringIO()
        inp = io.StringIO(json.dumps({"op": "hello", "config": {}}) + "\n" +
                          json.dumps({"op": "command", "id": "c", "kind": "drive", "deadline_ms": 5000}) + "\n")
        Runtime(p, infile=inp, outfile=out).run()
        self.assertFalse(p.moving)
        self.assertEqual(p.calls[-1], "stop")


class ProcessTest(unittest.TestCase):
    """The real thing: a child process whose stdin closes must stop and exit."""

    def test_child_exits_on_eof(self):
        code = ("import os, sys; sys.path.insert(0, %r)\n"
                "from openvibe_plugin import Plugin, run\n"
                "class P(Plugin):\n"
                "    driver='t'\n"
                "    def handle(self, cmd): pass\n"
                "    def stop(self): sys.stderr.write('STOPPED\\n'); sys.stderr.flush()\n"
                "    def setup(self, config): print('library noise on stdout'); os.write(1, b'C noise\\n')\n"
                "sys.exit(run(P()))\n") % os.path.dirname(HERE)
        proc = subprocess.Popen([sys.executable, "-c", code], stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                stderr=subprocess.PIPE, text=True)
        proc.stdin.write(json.dumps({"op": "hello", "config": {}}) + "\n")
        proc.stdin.write(json.dumps({"op": "command", "id": "a", "kind": "drive", "deadline_ms": 5000}) + "\n")
        proc.stdin.flush()
        time.sleep(0.2)
        proc.stdin.close()
        rc = proc.wait(timeout=5)
        err = proc.stderr.read()
        out = proc.stdout.read()
        self.assertEqual(rc, 0)
        self.assertIn('"op":"ack"', out)
        self.assertIn("STOPPED", err)
        # Everything on stdout is protocol; library prints went to stderr.
        for line in out.splitlines():
            json.loads(line)
        self.assertIn("library noise on stdout", err)
        self.assertIn("C noise", err)


class Later(Plugin):
    """Answers `ask` later (with data) and `look` at once with data; remembers approvals."""
    driver = "later"
    motion_kinds = frozenset()

    def __init__(self):
        self.held = []
        self.answers = []

    def handle(self, cmd):
        if cmd.kind == "look":
            return {"seen": cmd.value.get("what"), "deadline_ms": cmd.deadline_ms}
        if cmd.kind == "ask":
            self.held.append(cmd.id)
            raise Deferred()
        raise Fault("unsupported")

    def on_approval(self, cid, approved):
        self.answers.append((cid, approved))
        if approved:
            self.complete(cid, {"done": True})
        else:
            self.fail(cid, "not_allowed", "denied")

    def on_tray(self, attached, ttl_s):
        self.answers.append(("tray", attached, ttl_s))


class DeferredTest(unittest.TestCase):
    def test_data_deferred_and_approval(self):
        rt, p, out, clock = make(Later())
        rt.dispatch({"op": "hello", "config": {}})
        rt.dispatch({"op": "command", "id": "a", "kind": "look", "value": {"what": "screen"}, "deadline_ms": 60000})
        ack = lines(out)[-1]
        self.assertEqual(ack, {"op": "ack", "id": "a", "data": {"seen": "screen", "deadline_ms": 60000}},
                         "a kind that moves nothing keeps its long deadline")
        rt.dispatch({"op": "command", "id": "b", "kind": "ask", "value": {}})
        self.assertEqual(p.held, ["b"])
        self.assertFalse([m for m in lines(out) if m.get("id") == "b"], "no answer until the plugin gives one")
        rt.dispatch({"op": "approval", "id": "b", "approved": True})
        self.assertEqual(lines(out)[-1], {"op": "ack", "id": "b", "data": {"done": True}})
        rt.dispatch({"op": "command", "id": "c", "kind": "ask", "value": {}})
        rt.dispatch({"op": "approval", "id": "c", "approved": "yes"})
        self.assertEqual(p.answers[-1], ("c", False), "only a JSON true approves")
        self.assertEqual(lines(out)[-1], {"op": "nack", "id": "c", "fault_code": "not_allowed", "message": "denied"})

    def test_the_tray_op_reaches_the_plugin(self):
        rt, p, out, clock = make(Later())
        rt.dispatch({"op": "hello", "config": {}})
        rt.dispatch({"op": "tray", "attached": True, "ttl_s": 15})
        rt.dispatch({"op": "tray", "attached": "yes"})
        self.assertEqual(p.answers, [("tray", True, 15), ("tray", False, None)], "only a JSON true attaches")
        rt2, p2, out2, _ = make()
        rt2.dispatch({"op": "hello", "config": {}})
        rt2.dispatch({"op": "tray", "attached": True, "ttl_s": 15})  # a plugin without on_tray: the base no-op

    def test_motion_kinds_keep_the_short_cap(self):
        rt, p, out, clock = make()
        rt.dispatch({"op": "hello", "config": {}})
        rt.dispatch({"op": "command", "id": "d", "kind": "drive", "value": {}, "deadline_ms": 60000})
        self.assertEqual(rt.motion_deadline, clock() + 5.0)


if __name__ == "__main__":
    unittest.main()
