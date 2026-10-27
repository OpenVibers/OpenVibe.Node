"""Node 0.30 (Maticus, 2026-10-27, Windows 10): the clock shared with the core on Windows, the hello check that both read
the same clock, and strict timing, which refuses a motion command before handle() whenever its original expiry cannot be
checked, instead of running it on deadline_ms alone."""
import io
import json
import os
import sys
import unittest

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))
import openvibe_plugin  # noqa: E402
from openvibe_plugin import Plugin, Runtime, TIMING_UNAVAILABLE, _qpc_ns  # noqa: E402

from test_runtime import FakeClock, FakeShared, Recorder, lines  # noqa: E402

# The same vectors as internal/plugins/clock_test.go TestQPCNSMatchesThePythonRuntime: both sides must give these.
QPC_VECTORS = [
    (0, 10_000_000, 0),
    (9_999_999, 10_000_000, 999_999_900),
    (30_000_005, 10_000_000, 3_000_000_500),
    (3_579_545 * 3600 + 1, 3_579_545, 3_600_000_000_279),
    (31_536_000_000_000_007, 10_000_000, 3_153_600_000_000_000_700),  # a hundred years of uptime at 10 MHz
    (2_441_406 * 86_400 + 2_441_405, 2_441_406, 86_400_999_999_590),
]


class Strict(Recorder):
    strict_timing = True


def make(plugin=None, shared=None, name="clock_monotonic"):
    out = io.StringIO()
    clock = FakeClock()
    p = plugin or Recorder()
    rt = Runtime(p, infile=io.StringIO(""), outfile=out, clock=clock, shared_clock=shared, shared_clock_name=name)
    return rt, p, out, clock


def drive(cid, shared_ns=None, left_ns=None, deadline_ms=1000):
    m = {"op": "command", "id": cid, "kind": "drive", "deadline_ms": deadline_ms}
    if shared_ns is not None:
        m["expires_at_mono_ns"] = shared_ns + left_ns
    return json.dumps(m)


class QPCTest(unittest.TestCase):
    def test_conversion_is_nodes_exactly(self):
        for ticks, freq, ns in QPC_VECTORS:
            self.assertEqual(_qpc_ns(ticks, freq), ns, (ticks, freq))

    def test_windows_shares_the_raw_counter(self):
        # On Windows the shared clock is the raw QueryPerformanceCounter (never time.monotonic, which is GetTickCount64
        # before Python 3.13); elsewhere CLOCK_MONOTONIC.
        if sys.platform == "win32":
            self.assertEqual(openvibe_plugin.SHARED_CLOCK_NAME, "qpc")
            a = openvibe_plugin.SHARED_CLOCK()
            b = openvibe_plugin.SHARED_CLOCK()
            self.assertTrue(0 < a <= b)
        elif hasattr(__import__("time"), "CLOCK_MONOTONIC"):
            self.assertEqual(openvibe_plugin.SHARED_CLOCK_NAME, "clock_monotonic")


class StrictTimingTest(unittest.TestCase):
    def ready(self, rt, **hello):
        rt.feed_line(json.dumps({"op": "hello", "config": {}, **hello}))
        rt.feed_line(json.dumps({"op": "heartbeat", "t": 1}))

    def test_strict_needs_the_original_expiry(self):
        shared = FakeShared()
        rt, p, out, clock = make(Strict(), shared)
        self.ready(rt, shared_clock="clock_monotonic")
        describe = next(m for m in lines(out) if m["op"] == "describe")
        self.assertEqual(describe.get("timing"), "strict", "the core refuses too, before writing")
        # With its expiry on the shared clock: handled, with what is left.
        rt.feed_line(drive("ok", shared.ns, 500_000_000), clock.t, shared.ns)
        self.assertEqual(lines(out)[-1], {"op": "ack", "id": "ok"})
        self.assertEqual(p.handled[-1].deadline_ms, 500)
        # Without one (an older core, or a core with no shared clock): refused before handle(), never on deadline_ms.
        rt.feed_line(drive("bare"), clock.t, shared.ns)
        last = lines(out)[-1]
        self.assertEqual((last["op"], last["id"], last["fault_code"]), ("nack", "bare", TIMING_UNAVAILABLE))
        self.assertIn("no expiry on the shared clock", last["message"])
        self.assertEqual([c.id for c in p.handled], ["ok"])

    def test_strict_with_no_clock_here_refuses(self):
        # Missing clock: this machine has none the runtime can read.
        rt, p, out, clock = make(Strict(), shared=None)
        self.ready(rt)
        rt.feed_line(drive("m", 1_000, 500_000_000))
        last = lines(out)[-1]
        self.assertEqual(last["fault_code"], TIMING_UNAVAILABLE)
        self.assertIn("no clock this runtime shares", last["message"])
        self.assertEqual(p.handled, [])

    def test_strict_with_a_failed_read_refuses(self):
        # Failed clock: the read raised when the line arrived (shared_now answers None).
        def broken():
            raise OSError("QueryPerformanceCounter failed")
        rt, p, out, clock = make(Strict(), shared=broken)
        self.ready(rt)
        rt.feed_line(drive("m", 1_000, 500_000_000))
        last = lines(out)[-1]
        self.assertEqual(last["fault_code"], TIMING_UNAVAILABLE)
        self.assertIn("could not be read", last["message"])
        self.assertEqual(p.handled, [])

    def test_a_different_clock_is_never_compared(self):
        # The core says its expiry is on QueryPerformanceCounter, this runtime reads CLOCK_MONOTONIC: the field counts
        # as absent (strict refuses; a non-strict plugin runs on deadline_ms, as before Node 0.18).
        shared = FakeShared()
        rt, p, out, clock = make(Strict(), shared)
        self.ready(rt, shared_clock="qpc")
        rt.feed_line(drive("m", shared.ns, 500_000_000), clock.t, shared.ns)
        last = lines(out)[-1]
        self.assertEqual(last["fault_code"], TIMING_UNAVAILABLE)
        self.assertIn("is not this runtime's", last["message"])
        rt2, p2, out2, clock2 = make(Recorder(), shared)
        self.ready(rt2, shared_clock="qpc")
        rt2.feed_line(drive("m", shared.ns, -500_000_000, deadline_ms=300), clock2.t, shared.ns)
        self.assertEqual(lines(out2)[-1], {"op": "ack", "id": "m"}, "an instant on another clock is not read as expired")
        self.assertEqual(p2.handled[-1].deadline_ms, 300)

    def test_strict_from_the_deployment(self):
        shared = FakeShared()
        # The Node's plugin configuration (strict_timing in hello)…
        rt, p, out, clock = make(Recorder(), shared)
        self.ready(rt, strict_timing=True)
        rt.feed_line(drive("bare"), clock.t, shared.ns)
        self.assertEqual(lines(out)[-1]["fault_code"], TIMING_UNAVAILABLE)
        # …or the plugin's environment.
        os.environ["OPENVIBE_STRICT_TIMING"] = "1"
        try:
            rt2, p2, out2, clock2 = make(Recorder(), shared)
        finally:
            del os.environ["OPENVIBE_STRICT_TIMING"]
        self.ready(rt2)
        rt2.feed_line(drive("bare"), clock2.t, shared.ns)
        self.assertEqual(lines(out2)[-1]["fault_code"], TIMING_UNAVAILABLE)
        self.assertEqual(p.handled + p2.handled, [])

    def test_not_strict_keeps_working_without_a_clock(self):
        # Compatibility: a plugin that does not ask for strict timing still runs on deadline_ms where there is no
        # shared clock (an older core, a machine without one).
        rt, p, out, clock = make(Recorder(), shared=None)
        self.ready(rt)
        rt.feed_line(drive("m", deadline_ms=250))
        self.assertEqual(lines(out)[-1], {"op": "ack", "id": "m"})
        self.assertEqual(p.handled[-1].deadline_ms, 250)

    def test_boundaries(self):
        shared = FakeShared()
        rt, p, out, clock = make(Strict(), shared)
        self.ready(rt, shared_clock="clock_monotonic")
        # Exactly at the deadline, and less than a millisecond before it: expired, never handled.
        for cid, left in (("at", 0), ("under", 999_999), ("past", -1)):
            rt.feed_line(drive(cid, shared.ns, left), clock.t, shared.ns)
            last = lines(out)[-1]
            self.assertEqual((last["id"], last["fault_code"]), (cid, "expired"), cid)
        # One millisecond left: handled, with exactly that.
        rt.feed_line(drive("one", shared.ns, 1_000_000), clock.t, shared.ns)
        self.assertEqual(lines(out)[-1], {"op": "ack", "id": "one"})
        self.assertEqual(p.handled[-1].deadline_ms, 1)
        # Never more than deadline_ms, whatever the expiry says: the core's count-down is a cap too.
        rt.feed_line(drive("cap", shared.ns, 900_000_000, deadline_ms=400), clock.t, shared.ns)
        self.assertEqual(p.handled[-1].deadline_ms, 400)
        self.assertEqual([c.id for c in p.handled], ["one", "cap"])

    def test_a_restart_needs_the_clock_again(self):
        # A restarted process starts over: a new hello names the clock again, and a hello naming another clock (or none
        # from an older core, then no expiry) is held to the same rule.
        shared = FakeShared()
        rt, p, out, clock = make(Strict(), shared)
        self.ready(rt, shared_clock="qpc")
        rt.feed_line(drive("m", shared.ns, 500_000_000), clock.t, shared.ns)
        self.assertEqual(lines(out)[-1]["fault_code"], TIMING_UNAVAILABLE)
        rt.feed_line(json.dumps({"op": "hello", "config": {}, "shared_clock": "clock_monotonic"}))
        rt.feed_line(drive("n", shared.ns, 500_000_000), clock.t, shared.ns)
        self.assertEqual(lines(out)[-1], {"op": "ack", "id": "n"})


if __name__ == "__main__":
    unittest.main()
