import io
import json
import os
import sys
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.dirname(HERE))
sys.path.insert(0, os.path.join(os.path.dirname(os.path.dirname(HERE)), "sdk"))

from openvibe_dryrun import DryRun  # noqa: E402
from openvibe_plugin import Runtime  # noqa: E402


class Clock:
    t = 10.0

    def __call__(self):
        return self.t


class DryRunTest(unittest.TestCase):
    def setUp(self):
        self.out = io.StringIO()
        self.clock = Clock()
        self.p = DryRun()
        self.p.log = lambda *a: None
        self.rt = Runtime(self.p, infile=io.StringIO(""), outfile=self.out, clock=self.clock)
        self.send({"op": "hello", "config": {}})

    def send(self, m):
        self.rt.feed_line(json.dumps(m))

    def last(self):
        return json.loads(self.out.getvalue().splitlines()[-1])

    def test_accepts_every_kind(self):
        for i, (kind, value) in enumerate([
            ("drive", {"throttle": 0.5, "steer": -0.2}),
            ("drive", {"x": 0.1, "y": 0.2, "rotation": 0.3}),
            ("ptz", {"pan": 0.4, "tilt": -0.4}),
            ("actuator", {"name": "lift", "value": 0.5}),
            ("say", {"text": "hello"}),
            ("display", {"text": "hi"}),
            ("halt", {}),
        ]):
            self.send({"op": "command", "id": "c%d" % i, "kind": kind, "value": value, "deadline_ms": 300})
            self.assertEqual(self.last(), {"op": "ack", "id": "c%d" % i}, kind)

    def test_deadline_stop(self):
        self.send({"op": "command", "id": "d", "kind": "drive", "value": {"throttle": 1}, "deadline_ms": 300})
        self.assertEqual(self.p.state["throttle"], 1.0)
        self.clock.t += 0.31
        self.send({"op": "heartbeat", "t": 1})
        self.rt.tick()
        self.assertEqual(self.p.state["throttle"], 0.0)

    def test_bad_value(self):
        self.send({"op": "command", "id": "b", "kind": "drive", "value": {"throttle": "fast"}})
        self.assertEqual(self.last()["fault_code"], "bad_value")

    def test_telemetry(self):
        self.clock.t += 1
        self.send({"op": "heartbeat", "t": 1})
        self.rt.tick()
        t = self.last()
        self.assertEqual(t["op"], "telemetry")
        self.assertIn("percent", t["battery"])

    def test_describe(self):
        d = json.loads(self.out.getvalue().splitlines()[0])
        self.assertEqual(d["driver"], "dryrun")
        self.assertEqual(d["capabilities"]["camera"]["source"], "test_pattern")

    def test_record(self):
        import tempfile
        path = os.path.join(tempfile.mkdtemp(), "rec.jsonl")
        p = DryRun()
        p.log = lambda *a: None
        rt = Runtime(p, infile=io.StringIO(""), outfile=io.StringIO(), clock=self.clock)
        rt.feed_line(json.dumps({"op": "hello", "config": {"record": path}}))
        rt.feed_line(json.dumps({"op": "command", "id": "r", "kind": "drive", "value": {"throttle": 0.5}}))
        rt.feed_line(json.dumps({"op": "estop"}))
        whats = [json.loads(x)["what"] for x in open(path)]
        self.assertEqual(whats, ["setup", "stop", "command", "stop"])


if __name__ == "__main__":
    unittest.main()
