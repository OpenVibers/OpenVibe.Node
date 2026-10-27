import io
import json
import os
import subprocess
import sys
import time
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.dirname(HERE))

from openvibe_plugin import Deferred, Fault, Plugin, Runtime  # noqa: E402


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
        self.moving = False

    def describe(self, config):
        self.calls.append("describe")
        return {"drive": {"type": "differential"}}

    def setup(self, config):
        self.calls.append("setup")

    def handle(self, cmd):
        if cmd.kind == "drive":
            self.moving = True
            return
        if cmd.kind == "say":
            return
        raise Fault("unsupported")

    def stop(self):
        self.calls.append("stop")
        self.moving = False


def make(plugin=None):
    out = io.StringIO()
    clock = FakeClock()
    p = plugin or Recorder()
    rt = Runtime(p, infile=io.StringIO(""), outfile=out, clock=clock)
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
        # A command that waited in the inbox while a handler blocked keeps its arrival-based deadline.
        rt, p, out, clock = make()
        self.hello(rt)
        arrived = clock.t
        clock.t += 0.5  # the handler was stuck for 500 ms
        rt.feed_line(json.dumps({"op": "heartbeat", "t": 1}), arrived)
        rt.feed_line(json.dumps({"op": "command", "id": "old", "kind": "drive", "deadline_ms": 300}), arrived)
        rt.tick()
        self.assertFalse(p.moving)
        self.assertIn("deadline", rt.stop_reasons)

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
