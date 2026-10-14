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

    def test_probe_reaches_the_machines_own_network_only(self):
        import http.server
        import socket as socketlib

        class Handler(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                if self.path == "/moved":
                    self.send_response(302)
                    self.send_header("Location", "http://1.1.1.1/")
                    self.end_headers()
                    return
                body = b'{"up": true}'
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("ETag", '"v1"')
                self.end_headers()
                self.wfile.write(body)

            def log_message(self, *a):
                pass

        server = http.server.HTTPServer(("127.0.0.1", 0), Handler)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        self.addCleanup(server.shutdown)
        port = server.server_address[1]
        self.boot("observe")
        r = reply(self.out, self.cmd("probe", {"target": "http://127.0.0.1:%d/status" % port}))
        self.assertEqual(r["op"], "ack", r)
        self.assertEqual((r["data"]["status"], r["data"]["body"], r["data"]["etag"], r["data"]["content_type"]), (200, '{"up": true}', '"v1"', "application/json"))
        r = reply(self.out, self.cmd("probe", {"target": "http://127.0.0.1:%d/moved" % port}))
        self.assertEqual((r["data"]["status"], r["data"]["location"]), (302, "http://1.1.1.1/"), "a redirect is reported, never followed")
        r = reply(self.out, self.cmd("probe", {"target": "tcp://127.0.0.1:%d" % port}))
        self.assertTrue(r["data"]["open"])
        closed = socketlib.socket()
        closed.bind(("127.0.0.1", 0))
        free = closed.getsockname()[1]
        closed.close()
        r = reply(self.out, self.cmd("probe", {"target": "tcp://127.0.0.1:%d" % free, "timeout_s": 2}))
        self.assertFalse(r["data"]["open"])
        for public in ("http://1.1.1.1/", "tcp://8.8.8.8:53", "https://[2606:4700:4700::1111]/"):
            r = reply(self.out, self.cmd("probe", {"target": public}))
            self.assertEqual((r["op"], r["fault_code"]), ("nack", "not_allowed"), public)
        for bad in ("ftp://127.0.0.1/", "http://user:pw@127.0.0.1/", "tcp://127.0.0.1", "nonsense"):
            r = reply(self.out, self.cmd("probe", {"target": bad}))
            self.assertEqual(r["fault_code"], "bad_value", bad)
        self.rt.dispatch({"op": "estop"})
        r = reply(self.out, self.cmd("probe", {"target": "tcp://127.0.0.1:%d" % port}))
        self.assertTrue(r["data"]["open"], "a probe is read-only: it runs while stopped")

    def test_probe_can_be_turned_off(self):
        self.boot("trusted", probe=False)
        self.assertNotIn("probe", self.out.msgs()[0]["capabilities"])
        r = reply(self.out, self.cmd("probe", {"target": "tcp://127.0.0.1:1"}))
        self.assertEqual(r["fault_code"], "unsupported")

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


    # ---- input ---------------------------------------------------------------------------------------------------
    def boot_input(self, level, platform="linux", tool="xdotool", size=(2560, 1440)):
        self.argvs = []

        class Proc:
            returncode = 0
            pid = 1

            def communicate(self_, timeout=None):
                return b"", b""

        def popen(argv, **kw):
            self.argvs.append(argv)
            return Proc()

        self.dir = tempfile.mkdtemp(prefix="ov-computer-")
        self.addCleanup(lambda: __import__("shutil").rmtree(self.dir, ignore_errors=True))
        self.out = Out()
        env = {"DISPLAY": ":0"} if platform == "linux" else {}
        self.plugin = Computer(which=lambda n: "/usr/bin/" + n if n == tool else None, popen=popen, env=env,
                               platform=platform, screen_size=lambda t: size)
        self.rt = Runtime(self.plugin, infile=io.StringIO(""), outfile=self.out)
        self.rt.dispatch({"op": "hello", "config": {"level": level, "roots": [self.dir], "notify": False}})
        self.n = 0
        return self.out.msgs()

    def test_input_tool_detection(self):
        xdo = lambda n: "/usr/bin/xdotool" if n == "xdotool" else None
        self.assertEqual(Computer(which=xdo, env={"DISPLAY": ":0"}, platform="linux")._input_tool(), "xdotool")
        self.assertIsNone(Computer(which=xdo, env={"DISPLAY": ":0", "WAYLAND_DISPLAY": "wayland-0"}, platform="linux")._input_tool(), "not on Wayland")
        self.assertIsNone(Computer(which=xdo, env={}, platform="linux")._input_tool(), "no session")
        self.assertEqual(Computer(which=lambda n: "/opt/homebrew/bin/cliclick" if n == "cliclick" else None, env={}, platform="darwin")._input_tool(), "cliclick")
        caps = self.boot_input("trusted")[0]["capabilities"]
        self.assertEqual(caps["input"], {"level": "trusted", "tool": "xdotool", "scroll": True})

    def test_input_is_an_acting_command(self):
        self.boot_input("observe")
        r = reply(self.out, self.cmd("input", {"action": "click", "x": 10, "y": 10}))
        self.assertEqual((r["op"], r["fault_code"]), ("nack", "not_allowed"))
        self.boot_input("ask")
        cid = self.cmd("input", {"action": "click", "x": 640, "y": 360, "width": 1280, "height": 720})
        ev = self.out.wait_for(lambda m: m.get("op") == "event" and m.get("name") == "approval")
        self.assertEqual((ev["kind"], ev["summary"]), ("input", "click at 640, 360 of a 1280\u00d7720 picture of the screen"))
        self.assertEqual(self.argvs, [], "nothing touches the screen before the owner approves")
        self.plugin.on_approval(cid, True)
        self.assertEqual(reply(self.out, cid)["op"], "ack")

    def test_input_scales_the_picture_to_the_screen(self):
        self.boot_input("trusted")
        r = reply(self.out, self.cmd("input", {"action": "click", "x": 640, "y": 360, "width": 1280, "height": 720}))
        self.assertEqual(r["data"], {"done": True, "action": "click", "x": 1280, "y": 720})
        self.assertEqual(self.argvs[-1], ["xdotool", "mousemove", "--sync", "1280", "720", "click", "1"])
        reply(self.out, self.cmd("input", {"action": "double_click", "x": 5, "y": 5}))
        self.assertEqual(self.argvs[-1], ["xdotool", "mousemove", "--sync", "5", "5", "click", "--repeat", "2", "1"], "no picture: screen pixels")
        reply(self.out, self.cmd("input", {"action": "scroll", "dy": -3}))
        self.assertEqual(self.argvs[-1], ["xdotool", "click", "--repeat", "3", "4"])
        reply(self.out, self.cmd("input", {"action": "type", "text": "-hello"}))
        self.assertEqual(self.argvs[-1], ["xdotool", "type", "--delay", "12", "--", "-hello"], "text is never an option")
        reply(self.out, self.cmd("input", {"action": "key", "keys": "ctrl+s"}))
        self.assertEqual(self.argvs[-1], ["xdotool", "key", "--", "ctrl+s"])

    def test_input_refuses_what_it_should_not_send(self):
        self.boot_input("trusted")
        before = len(self.argvs)
        for value, code in [({"action": "key", "keys": "ctrl+s; rm -rf ~"}, "bad_value"),
                            ({"action": "key", "keys": "--help"}, "bad_value"),
                            ({"action": "drag"}, "bad_value"),
                            ({"action": "click"}, "bad_value"),
                            ({"action": "click", "x": 1, "y": 1, "width": 100}, "bad_value"),
                            ({"action": "type", "text": "x" * 2001}, "bad_value"),
                            ({"action": "scroll", "dy": 0}, "bad_value")]:
            r = reply(self.out, self.cmd("input", value))
            self.assertEqual((r["op"], r["fault_code"]), ("nack", code), value)
        self.assertEqual(len(self.argvs), before)

    def test_input_on_macos_uses_cliclick(self):
        self.boot_input("trusted", platform="darwin", tool="cliclick", size=(1440, 900))
        reply(self.out, self.cmd("input", {"action": "right_click", "x": 640, "y": 400, "width": 1280, "height": 800}))
        self.assertEqual(self.argvs[-1], ["cliclick", "rc:720,450"])
        reply(self.out, self.cmd("input", {"action": "key", "keys": "cmd+shift+s"}))
        self.assertEqual(self.argvs[-1], ["cliclick", "kd:cmd", "kd:shift", "t:s", "ku:shift", "ku:cmd"])
        reply(self.out, self.cmd("input", {"action": "key", "keys": "Return"}))
        self.assertEqual(self.argvs[-1], ["cliclick", "kp:return"])
        r = reply(self.out, self.cmd("input", {"action": "scroll", "dy": 2}))
        self.assertEqual((r["op"], r["fault_code"]), ("nack", "unsupported"))


if __name__ == "__main__":
    unittest.main()
