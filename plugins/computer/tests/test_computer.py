import base64
import io
import json
import os
import sys
import tempfile
import threading
import time
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.dirname(HERE))
sys.path.insert(0, os.path.join(os.path.dirname(os.path.dirname(HERE)), "sdk"))

from openvibe_computer import Computer  # noqa: E402
from openvibe_plugin import Runtime  # noqa: E402


class Out(io.StringIO):
    """The plugin's stdout, readable while worker threads write to it."""

    def __init__(self):
        super().__init__()
        self.cond = threading.Condition()

    def write(self, s):
        with self.cond:
            n = super().write(s)
            self.cond.notify_all()
            return n

    def msgs(self):
        with self.cond:
            return [json.loads(x) for x in self.getvalue().splitlines() if x]

    def wait_for(self, pred, timeout=10):
        end = time.monotonic() + timeout
        with self.cond:
            while True:
                found = [m for m in (json.loads(x) for x in self.getvalue().splitlines() if x) if pred(m)]
                if found:
                    return found[0]
                left = end - time.monotonic()
                if left <= 0:
                    raise AssertionError("no matching message; got %r" % self.getvalue()[-2000:])
                self.cond.wait(left)


def reply(out, cid):
    return out.wait_for(lambda m: m.get("id") == cid and m.get("op") in ("ack", "nack"))


class ComputerTest(unittest.TestCase):
    def boot(self, level="observe", **config):
        self.dir = tempfile.mkdtemp(prefix="ov-computer-")
        self.addCleanup(lambda: __import__("shutil").rmtree(self.dir, ignore_errors=True))
        with open(os.path.join(self.dir, "notes.txt"), "w") as f:
            f.write("hello computer\n")
        os.mkdir(os.path.join(self.dir, "sub"))
        self.out = Out()
        self.plugin = Computer(env={}, platform="linux")
        self.rt = Runtime(self.plugin, infile=io.StringIO(""), outfile=self.out)
        cfg = {"level": level, "roots": [self.dir], **config}
        self.rt.dispatch({"op": "hello", "config": cfg})
        self.n = 0
        return self.out.msgs()

    def cmd(self, kind, value, deadline_ms=30000):
        self.n += 1
        cid = "c%d" % self.n
        self.rt.dispatch({"op": "command", "id": cid, "kind": kind, "value": value, "deadline_ms": deadline_ms})
        return cid

    def test_describe_and_ready(self):
        msgs = self.boot("trusted")
        describe = msgs[0]
        self.assertEqual(describe["driver"], "computer")
        caps = describe["capabilities"]
        self.assertEqual(caps["files"]["roots"], [os.path.realpath(self.dir)])
        self.assertEqual(caps["terminal"]["level"], "trusted")
        self.assertNotIn("screen", caps, "no display in this environment: no screen capability")
        self.assertEqual(msgs[-1]["op"], "ready")

    def test_bad_level_is_a_fault(self):
        out = Out()
        rt = Runtime(Computer(env={}), infile=io.StringIO(""), outfile=out)
        rt.dispatch({"op": "hello", "config": {"level": "god"}})
        self.assertEqual(out.msgs()[-1]["op"], "fault")

    def test_observe_reads_inside_roots_only(self):
        self.boot("observe")
        r = reply(self.out, self.cmd("fs", {"op": "list", "path": self.dir}))
        self.assertEqual(r["op"], "ack")
        self.assertEqual([e["name"] for e in r["data"]["entries"]], ["notes.txt", "sub"])
        r = reply(self.out, self.cmd("fs", {"op": "read", "path": os.path.join(self.dir, "notes.txt")}))
        self.assertEqual(base64.b64decode(r["data"]["content_b64"]), b"hello computer\n")
        r = reply(self.out, self.cmd("fs", {"op": "read", "path": os.path.join(self.dir, "notes.txt"), "offset": 6, "length": 3}))
        self.assertEqual((base64.b64decode(r["data"]["content_b64"]), r["data"]["truncated"]), (b"com", True))
        r = reply(self.out, self.cmd("fs", {"op": "stat", "path": "/etc/hostname"}))
        self.assertEqual((r["op"], r["fault_code"]), ("nack", "not_allowed"), "outside the roots")
        link = os.path.join(self.dir, "escape")
        os.symlink("/etc", link)
        r = reply(self.out, self.cmd("fs", {"op": "list", "path": link}))
        self.assertEqual(r["fault_code"], "not_allowed", "a symlink out of the roots is outside them")
        r = reply(self.out, self.cmd("fs", {"op": "list", "path": "relative/path"}))
        self.assertEqual(r["fault_code"], "bad_value")

    def test_observe_refuses_acting(self):
        self.boot("observe")
        r = reply(self.out, self.cmd("exec", {"argv": ["echo", "hi"]}))
        self.assertEqual((r["op"], r["fault_code"]), ("nack", "not_allowed"))
        r = reply(self.out, self.cmd("fs", {"op": "write", "path": os.path.join(self.dir, "x"), "content_b64": "eA=="}))
        self.assertEqual(r["fault_code"], "not_allowed")
        self.assertFalse(os.path.exists(os.path.join(self.dir, "x")))

    def test_trusted_runs_programs_and_writes(self):
        self.boot("trusted")
        r = reply(self.out, self.cmd("exec", {"argv": [sys.executable, "-c", "import os,sys; print(os.getcwd()); sys.stderr.write('e'); sys.exit(3)"]}))
        self.assertEqual(r["op"], "ack")
        d = r["data"]
        self.assertEqual((d["exit_code"], d["stdout"].strip(), d["stderr"], d["timed_out"]), (3, os.path.realpath(self.dir), "e", False))
        r = reply(self.out, self.cmd("exec", {"argv": ["cat"], "stdin": "piped"}))
        self.assertEqual(r["data"]["stdout"], "piped")
        r = reply(self.out, self.cmd("exec", {"argv": [sys.executable, "-c", "print('x' * 5000)"]}))
        self.assertFalse(r["data"]["truncated"])
        r = reply(self.out, self.cmd("exec", {"argv": ["definitely-not-a-program-ov"]}))
        self.assertEqual(r["fault_code"], "bad_value")
        r = reply(self.out, self.cmd("exec", {"argv": "rm -rf /"}))
        self.assertEqual(r["fault_code"], "bad_value", "a shell line is not argv")
        target = os.path.join(self.dir, "sub", "new.txt")
        r = reply(self.out, self.cmd("fs", {"op": "write", "path": target, "content_b64": base64.b64encode(b"one").decode()}))
        self.assertEqual((r["op"], r["data"]["written"]), ("ack", 3))
        r = reply(self.out, self.cmd("fs", {"op": "write", "path": target, "content_b64": "dHdv"}))
        self.assertEqual(r["fault_code"], "bad_value", "create does not replace")
        reply(self.out, self.cmd("fs", {"op": "write", "path": target, "content_b64": "dHdv", "mode": "append"}))
        with open(target, "rb") as f:
            self.assertEqual(f.read(), b"onetwo")
        r = reply(self.out, self.cmd("fs", {"op": "mkdir", "path": os.path.join(self.dir, "a", "b")}))
        self.assertEqual(r["data"]["created"], True)
        r = reply(self.out, self.cmd("fs", {"op": "write", "path": "/tmp/ov-outside-roots", "content_b64": "eA=="}))
        self.assertEqual(r["fault_code"], "not_allowed", "trusted stays inside the roots")

    def test_output_is_capped_and_timeouts_kill(self):
        self.boot("trusted", max_output_bytes=1024, timeout_s=1)
        r = reply(self.out, self.cmd("exec", {"argv": [sys.executable, "-c", "print('y' * 100000)"]}))
        self.assertEqual((len(r["data"]["stdout"]), r["data"]["truncated"]), (1024, True))
        r = reply(self.out, self.cmd("exec", {"argv": [sys.executable, "-c", "import time; time.sleep(30)"], "timeout_s": 60}))
        self.assertEqual((r["data"]["timed_out"], r["data"]["exit_code"] != 0), (True, True))
        self.assertLess(r["data"]["duration_ms"], 5000, "the owner's timeout_s caps the request's")

    def test_ask_holds_until_the_owner_answers(self):
        self.boot("ask")
        r = reply(self.out, self.cmd("fs", {"op": "list", "path": self.dir}))
        self.assertEqual(r["op"], "ack", "observing never asks")
        cid = self.cmd("exec", {"argv": ["echo", "approved"]})
        ev = self.out.wait_for(lambda m: m.get("op") == "event" and m.get("name") == "approval")
        self.assertEqual((ev["id"], ev["kind"]), (cid, "exec"))
        self.assertIn("echo approved", ev["summary"])
        self.assertFalse([m for m in self.out.msgs() if m.get("id") == cid and m.get("op") in ("ack", "nack")])
        self.rt.dispatch({"op": "approval", "id": cid, "approved": True})
        self.assertEqual(reply(self.out, cid)["data"]["stdout"], "approved\n")
        cid = self.cmd("fs", {"op": "write", "path": os.path.join(self.dir, "denied.txt"), "content_b64": "eA=="})
        self.out.wait_for(lambda m: m.get("name") == "approval" and m.get("id") == cid)
        self.rt.dispatch({"op": "approval", "id": cid, "approved": False})
        r = reply(self.out, cid)
        self.assertEqual((r["fault_code"], r["message"]), ("not_allowed", "the owner denied it on the machine"))
        self.assertFalse(os.path.exists(os.path.join(self.dir, "denied.txt")))

    def test_a_waiting_request_notifies_the_desktop(self):
        self.boot("ask")
        calls = []
        real = self.plugin.popen
        self.plugin.popen = lambda argv, **kw: calls.append(argv) if argv[0] in ("notify-send", "osascript") else real(argv, **kw)
        self.plugin.which = lambda name: "/usr/bin/" + name
        self.plugin.env = {"DISPLAY": ":0"}
        cid = self.cmd("exec", {"argv": ["echo", "hi"]})
        self.out.wait_for(lambda m: m.get("name") == "approval")
        self.assertEqual(calls[0][:3], ["notify-send", "--app-name=OpenVibe Node", "OpenVibe: your agent asks"])
        self.assertIn("openvibe-node approve %s" % cid, calls[0][3])
        self.plugin.env = {}
        self.cmd("exec", {"argv": ["echo", "again"]})
        self.assertEqual(len(calls), 1, "no graphical session: no notification")
        self.plugin.notify = False
        self.plugin.env = {"DISPLAY": ":0"}
        self.cmd("exec", {"argv": ["echo", "quiet"]})
        self.assertEqual(len(calls), 1, "notify off")

    def test_unanswered_requests_expire(self):
        self.boot("ask", approval_timeout_s=10)
        t = [1000.0]
        self.plugin.clock = lambda: t[0]
        cid = self.cmd("exec", {"argv": ["echo", "late"]})
        self.plugin.pending[cid] = (self.plugin.pending[cid][0], t[0] + 10, "x")
        t[0] += 11
        tele = self.plugin.poll()
        self.assertEqual((tele["sensors"]["computer_waiting"], tele["sensors"]["computer_level"]), (0, "ask"))
        r = reply(self.out, cid)
        self.assertEqual(r["fault_code"], "not_allowed")

    def test_the_kill_switch_drops_to_observe_and_denies_what_waits(self):
        self.boot("ask")
        cid = self.cmd("exec", {"argv": ["echo", "never"]})
        self.out.wait_for(lambda m: m.get("name") == "approval")
        self.rt.dispatch({"op": "estop"})
        self.assertEqual(reply(self.out, cid)["fault_code"], "local_stop")
        self.assertEqual(self.plugin.effective_level(), "observe")
        r = reply(self.out, self.cmd("exec", {"argv": ["echo", "no"]}))
        self.assertEqual(r["fault_code"], "local_stop")
        self.assertEqual(reply(self.out, self.cmd("fs", {"op": "stat", "path": self.dir}))["op"], "ack", "observing still works")
        self.rt.dispatch({"op": "resume"})
        self.assertEqual(self.plugin.effective_level(), "ask")

    def test_halt_kills_running_programs(self):
        self.boot("trusted", timeout_s=60)
        cid = self.cmd("exec", {"argv": [sys.executable, "-c", "import time; time.sleep(60)"]})
        end = time.monotonic() + 5
        while not self.plugin.procs and time.monotonic() < end:
            time.sleep(0.02)
        self.rt.dispatch({"op": "command", "id": "h", "kind": "halt"})
        r = reply(self.out, cid)
        self.assertNotEqual(r["data"]["exit_code"], 0)

    def test_full_reaches_outside_the_roots(self):
        self.boot("full")
        r = reply(self.out, self.cmd("fs", {"op": "stat", "path": "/"}))
        self.assertEqual((r["op"], r["data"]["type"]), ("ack", "dir"))

    def test_screen_tool_detection(self):
        which = lambda name: "/usr/bin/" + name if name in ("grim", "scrot") else None
        self.assertEqual(Computer(which=which, env={"WAYLAND_DISPLAY": "wayland-0"}, platform="linux")._screen_tool()[0], "grim")
        self.assertEqual(Computer(which=which, env={"DISPLAY": ":0"}, platform="linux")._screen_tool()[0], "scrot")
        self.assertIsNone(Computer(which=which, env={}, platform="linux")._screen_tool())
        mac = Computer(which=lambda n: "/usr/sbin/screencapture" if n == "screencapture" else None, env={}, platform="darwin")
        self.assertEqual(mac._screen_tool()[1]("/tmp/x.png"), ["screencapture", "-x", "-t", "png", "/tmp/x.png"])

    def test_screen_capture_encodes_a_jpeg(self):
        try:
            from PIL import Image
        except ImportError:
            self.skipTest("pillow not installed")
        self.boot("observe")
        src = os.path.join(self.dir, "frame.png")
        Image.new("RGB", (2560, 1440), (30, 90, 200)).save(src)
        tool = ("fake", lambda out: [sys.executable, "-c", "import shutil,sys; shutil.copy(sys.argv[1], sys.argv[2])", src, out])
        self.plugin._screen_tool = lambda: tool
        r = reply(self.out, self.cmd("screen", {"max_width": 1280}))
        d = r["data"]
        self.assertEqual((d["format"], d["width"], d["height"]), ("jpeg", 1280, 720))
        self.assertEqual(base64.b64decode(d["content_b64"])[:2], b"\xff\xd8")


if __name__ == "__main__":
    unittest.main()
