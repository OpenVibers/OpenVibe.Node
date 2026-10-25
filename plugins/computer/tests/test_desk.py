"""The private desk (openvibe_computer/desk.py and the plugin's desk kind, plan track K step 2): it starts an Xvfb display
with its own bus and window manager and stops exactly what it started; the plugin starts and stops it under the
machine's level (refused at observe), reports it at every level, and sends screen, input and the browser there with
desk: private (refused until it runs), never to the owner's screen. A real desk runs where Xvfb is installed."""
import base64
import io
import os
import shutil
import sys
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
sys.path.insert(0, os.path.dirname(HERE))
sys.path.insert(0, os.path.join(os.path.dirname(os.path.dirname(HERE)), "sdk"))

from openvibe_computer import Computer  # noqa: E402
from openvibe_computer.desk import PrivateDesk  # noqa: E402
from openvibe_plugin import Fault, Runtime  # noqa: E402
from test_computer import Out, reply  # noqa: E402


class FakeProc:
    def __init__(self, argv, kw):
        self.argv, self.kw = argv, kw
        self.pid = 4242
        self.code = None
        self.stdout = io.StringIO("unix:path=/tmp/dbus-desk,guid=1\n")

    def poll(self):
        return self.code

    def wait(self, timeout=None):
        self.code = 0
        return 0

    def terminate(self):
        self.code = -15

    kill = terminate


class DeskModuleTest(unittest.TestCase):
    def make(self, have=("Xvfb", "dbus-daemon", "metacity")):
        self.started = []
        sockets = set()

        def popen(argv, **kw):
            p = FakeProc(argv, kw)
            self.started.append(p)
            if argv[0] == "Xvfb":
                sockets.add("/tmp/.X11-unix/X%s" % argv[1][1:])
            return p
        desk = PrivateDesk(which=lambda n: "/usr/bin/" + n if n in have else None, popen=popen, sleep=lambda s: None,
                           exists=lambda path: path in sockets, base_env={"WAYLAND_DISPLAY": "wayland-0", "DISPLAY": ":0", "HOME": "/home/a"})
        desk._kill = lambda proc: setattr(proc, "code", -15)
        return desk

    def test_start_status_env_stop(self):
        desk = self.make()
        st = desk.start(1024, 768)
        self.assertEqual(st["display"], ":90")
        self.assertEqual((st["width"], st["height"], st["window_manager"]), (1024, 768, "metacity"))
        xvfb = self.started[0]
        self.assertEqual(xvfb.argv, ["Xvfb", ":90", "-screen", "0", "1024x768x24", "-nolisten", "tcp", "-noreset"])
        self.assertTrue(xvfb.kw["start_new_session"], "its own process group")
        self.assertEqual([p.argv[0] for p in self.started], ["Xvfb", "dbus-daemon", "metacity"])
        env = desk.env()
        self.assertEqual((env["DISPLAY"], env["DBUS_SESSION_BUS_ADDRESS"]), (":90", "unix:path=/tmp/dbus-desk,guid=1"))
        self.assertNotIn("WAYLAND_DISPLAY", env, "nothing goes to the owner's session")
        self.assertEqual(self.started[2].kw["env"]["DISPLAY"], ":90", "the window manager runs on the desk")
        self.assertEqual(desk.start(1024, 768)["display"], ":90", "starting again keeps the running desk")
        self.assertEqual(len(self.started), 3)
        self.assertEqual(desk.stop(), {"running": False, "stopped": True})
        self.assertTrue(all(p.code is not None for p in self.started), "every process it started has ended")
        self.assertEqual(desk.status(), {"running": False, "available": True})
        with self.assertRaises(Fault) as e:
            desk.env()
        self.assertEqual(e.exception.code, "not_ready")

    def test_without_xvfb_or_a_window_manager(self):
        with self.assertRaises(Fault) as e:
            self.make(have=()).start()
        self.assertEqual(e.exception.code, "unsupported")
        desk = self.make(have=("Xvfb",))
        st = desk.start()
        self.assertIsNone(st["window_manager"])
        self.assertNotIn("DBUS_SESSION_BUS_ADDRESS", desk.env())


class FakeDesk:
    def __init__(self):
        self.on = False

    def available(self):
        return True

    def window_manager(self):
        return "metacity"

    def running(self):
        return self.on

    def start(self, width, height):
        self.on, self.size = True, (width, height)
        return self.status()

    def stop(self):
        was, self.on = self.on, False
        return {"running": False, "stopped": was}

    def status(self):
        return {"running": True, "display": ":91", "width": 1000, "height": 500} if self.on else {"running": False, "available": True}

    def env(self):
        if not self.on:
            raise Fault("not_ready", "start the private desk first (desk start)")
        return {"DISPLAY": ":91"}


class DeskPluginTest(unittest.TestCase):
    def boot(self, level):
        self.out = Out()
        self.desk = FakeDesk()
        self.calls = []

        def popen(argv, **kw):
            self.calls.append((argv, kw.get("env")))
            p = FakeProc(argv, kw)
            p.returncode = 0
            p.communicate = lambda timeout=None: (b"", b"")
            return p
        self.plugin = Computer(env={"DISPLAY": ":0"}, platform="linux", desk=self.desk, popen=popen,
                               which=lambda n: "/usr/bin/" + n, screen_size=lambda tool: (2000, 1000))
        self.rt = Runtime(self.plugin, infile=io.StringIO(""), outfile=self.out)
        self.rt.dispatch({"op": "hello", "config": {"level": level, "roots": [tempfile.gettempdir()], "notify": False}})
        self.n = 0
        return self.out.msgs()

    def cmd(self, kind, value):
        self.n += 1
        cid = "d%d" % self.n
        self.rt.dispatch({"op": "command", "id": cid, "kind": kind, "value": value, "deadline_ms": 30000})
        return cid

    def test_describe_offers_the_desk(self):
        caps = self.boot("trusted")[0]["capabilities"]
        self.assertEqual(caps["desk"], {"level": "trusted", "window_manager": "metacity", "input": True, "screen": True})

    def test_observe_reports_but_does_not_start(self):
        self.boot("observe")
        self.assertEqual(reply(self.out, self.cmd("desk", {"action": "status"}))["data"], {"running": False, "available": True})
        r = reply(self.out, self.cmd("desk", {"action": "start"}))
        self.assertEqual((r["op"], r["fault_code"]), ("nack", "not_allowed"))
        self.assertFalse(self.desk.on)

    def test_private_desk_input_screen_and_refusals(self):
        self.boot("trusted")
        r = reply(self.out, self.cmd("input", {"action": "click", "x": 10, "y": 10, "desk": "private"}))
        self.assertEqual(r["fault_code"], "not_ready", "the desk has to run first")
        r = reply(self.out, self.cmd("desk", {"action": "start", "width": 1000, "height": 500}))
        self.assertEqual(r["data"]["running"], True)
        r = reply(self.out, self.cmd("input", {"action": "click", "x": 50, "y": 50, "width": 100, "height": 100, "desk": "private"}))
        self.assertEqual(r["op"], "ack")
        argv, env = self.calls[-1]
        self.assertEqual(argv, ["xdotool", "mousemove", "--sync", "500", "250", "click", "1"], "scaled to the desk's own 1000×500")
        self.assertEqual(env["DISPLAY"], ":91", "on the desk, never the owner's :0")
        r = reply(self.out, self.cmd("input", {"action": "click", "x": 50, "y": 50, "width": 100, "height": 100}))
        argv, env = self.calls[-1]
        self.assertEqual(argv[3:5], ["1000", "500"], "desk main is the owner's 2000×1000 screen")
        self.assertIsNone(env)
        r = reply(self.out, self.cmd("screen", {"desk": "neighbour"}))
        self.assertEqual(r["fault_code"], "bad_value")
        r = reply(self.out, self.cmd("desk", {"action": "stop"}))
        self.assertEqual(r["data"], {"running": False, "stopped": True})

    def test_the_kill_switch_stops_the_desk(self):
        self.boot("trusted")
        reply(self.out, self.cmd("desk", {"action": "start"}))
        self.assertTrue(self.desk.on)
        self.plugin.stop()
        self.assertFalse(self.desk.on)


@unittest.skipUnless(all(shutil.which(t) for t in ("Xvfb", "import")) and sys.platform.startswith("linux"), "needs Xvfb and ImageMagick")
class RealDeskTest(unittest.TestCase):
    def test_a_real_desk_shows_its_own_screen(self):
        out = Out()
        plugin = Computer(platform="linux")
        rt = Runtime(plugin, infile=io.StringIO(""), outfile=out)
        rt.dispatch({"op": "hello", "config": {"level": "trusted", "roots": [tempfile.gettempdir()], "notify": False}})
        self.addCleanup(plugin.desk.stop)
        rt.dispatch({"op": "command", "id": "r1", "kind": "desk", "value": {"action": "start", "width": 800, "height": 600}, "deadline_ms": 30000})
        st = reply(out, "r1")["data"]
        self.assertTrue(st["running"], st)
        rt.dispatch({"op": "command", "id": "r2", "kind": "screen", "value": {"desk": "private", "max_width": 800}, "deadline_ms": 30000})
        shot = reply(out, "r2")["data"]
        self.assertEqual((shot["width"], shot["height"]), (800, 600), "the desk's own 800×600, not the owner's screen")
        self.assertTrue(base64.b64decode(shot["content_b64"]))
        pids = [p.pid for _, p in plugin.desk.procs]
        rt.dispatch({"op": "command", "id": "r3", "kind": "desk", "value": {"action": "stop"}, "deadline_ms": 30000})
        self.assertEqual(reply(out, "r3")["data"]["stopped"], True)
        for pid in pids:
            with self.assertRaises(ProcessLookupError):
                os.kill(pid, 0)


if __name__ == "__main__":
    unittest.main()
