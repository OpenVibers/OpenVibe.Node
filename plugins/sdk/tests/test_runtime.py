import io
import json
import os
import subprocess
import sys
import time
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.dirname(HERE))

from openvibe_plugin import Fault, Plugin, Runtime  # noqa: E402


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


if __name__ == "__main__":
    unittest.main()
