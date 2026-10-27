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
from openvibe_computer.desk import Desks, PrivateDesk, default_max_desks  # noqa: E402
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
    def __init__(self, display=91):
        self.on = False
        self.display = display

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
        return {"running": True, "display": ":%d" % self.display, "width": 1000, "height": 500} if self.on else {"running": False, "available": True}

    def env(self):
        if not self.on:
            raise Fault("not_ready", "start the private desk first (desk start)")
        return {"DISPLAY": ":%d" % self.display}


class FakePageBrowser:
    """A browser desk's browser, recorded: its page is the desk's screen, mouse, keyboard and live view."""

    def __init__(self, binary, profile, headless, env=None):
        self.profile, self.headless, self.env, self.download_dir = profile, headless, env, None
        self.window = (1280, 900)
        self.calls = []
        self.on = True

    def alive(self):
        return self.on

    def page(self):
        self.calls.append(("page",))

    def viewport(self):
        return self.window

    def open(self, url):
        self.calls.append(("open", url))
        return {"url": url, "title": "Example", "text": "", "links": []}

    def mouse(self, action, x, y):
        self.calls.append(("mouse", action, x, y))
        return {"done": True, "receipt": {"url_changed": False, "appeared": ["Row added"], "gone": 0}}

    def wheel(self, x, y, dy):
        self.calls.append(("wheel", x, y, dy))
        return {"done": True}

    def insert(self, text):
        self.calls.append(("insert", text))
        return {"done": True}

    def keys(self, chord):
        self.calls.append(("keys", chord))
        return {"done": True}

    def screenshot(self, max_width):
        self.calls.append(("screenshot", max_width))
        return {"format": "jpeg", "width": 1000, "height": 500, "content_b64": "/9j/"}

    def frame(self, path, max_width, quality):
        self.calls.append(("frame", max_width, quality))
        with open(path, "wb") as f:
            f.write(b"\xff\xd8\xff")

    def close(self):
        self.on = False
        self.calls.append(("close",))


class DeskPluginTest(unittest.TestCase):
    def boot(self, level, **config):
        self.out = Out()
        self.desk = FakeDesk()
        self.made_desks = []
        self.calls = []

        def desk_factory():
            d = FakeDesk(display=92 + len(self.made_desks))
            self.made_desks.append(d)
            return d
        self.desk_dir = tempfile.mkdtemp(prefix="ov-desks-")
        self.addCleanup(lambda: shutil.rmtree(self.desk_dir, ignore_errors=True))

        def popen(argv, **kw):
            self.calls.append((argv, kw.get("env")))
            p = FakeProc(argv, kw)
            p.returncode = 0
            p.communicate = lambda timeout=None: (b"", b"")
            return p
        self.plugin = Computer(env={"DISPLAY": ":0"}, platform="linux", desk=self.desk, desk_factory=desk_factory, popen=popen,
                               which=lambda n: "/usr/bin/" + n, screen_size=lambda tool: (2000, 1000))
        self.rt = Runtime(self.plugin, infile=io.StringIO(""), outfile=self.out)
        self.rt.dispatch({"op": "hello", "config": {"level": level, "roots": [tempfile.gettempdir()], "notify": False,
                                                    "desk_dir": self.desk_dir, **config}})
        self.n = 0
        return self.out.msgs()

    def cmd(self, kind, value):
        self.n += 1
        cid = "d%d" % self.n
        self.rt.dispatch({"op": "command", "id": cid, "kind": kind, "value": value, "deadline_ms": 30000})
        return cid

    def test_describe_offers_the_desk(self):
        caps = self.boot("trusted")[0]["capabilities"]
        self.assertEqual(caps["desk"], {"level": "trusted", "window_manager": "metacity", "input": True, "screen": True,
                                        "named": True, "max_desks": default_max_desks(), "kinds": ["desktop", "browser"]})

    def test_observe_reports_but_does_not_start(self):
        self.boot("observe")
        self.assertEqual(reply(self.out, self.cmd("desk", {"action": "status"}))["data"], {"running": False, "available": True, "desk": "private", "kind": "desktop"})
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
        self.assertEqual(r["data"], {"running": False, "stopped": True, "desk": "private"})

    def test_several_desks_each_with_its_own_display_temp_folder_and_browser(self):
        # Desks plan D1: two agents at once on one machine, each on a desk of its own.
        self.boot("trusted", max_desks=2)
        for name in ("desk-a", "desk-b"):
            r = reply(self.out, self.cmd("desk", {"action": "start", "desk": name, "width": 1000, "height": 500}))
            self.assertEqual((r["op"], r["data"]["desk"], r["data"]["running"]), ("ack", name, True))
        self.assertEqual([d.display for d in self.made_desks], [92, 93], "a display each")
        # A third is refused while two run (max_desks 2), and the private desk counts like any other.
        r = reply(self.out, self.cmd("desk", {"action": "start", "desk": "private"}))
        self.assertEqual(r["fault_code"], "not_ready")
        self.assertIn("max_desks", r["message"])
        # Input goes to the named desk's own display.
        reply(self.out, self.cmd("input", {"action": "click", "x": 1, "y": 1, "width": 100, "height": 100, "desk": "desk-a"}))
        self.assertEqual(self.calls[-1][1]["DISPLAY"], ":92")
        reply(self.out, self.cmd("input", {"action": "click", "x": 1, "y": 1, "width": 100, "height": 100, "desk": "desk-b"}))
        self.assertEqual(self.calls[-1][1]["DISPLAY"], ":93")
        # A temp folder each, 0700, short enough for a Unix socket in it (Chrome's SingletonSocket: 108 bytes).
        tmp_a = self.calls[-2][1]["TMPDIR"]
        tmp_b = self.calls[-1][1]["TMPDIR"]
        self.assertNotEqual(tmp_a, tmp_b)
        self.assertTrue(os.path.isdir(tmp_a) and os.path.basename(tmp_a).startswith("ov-desk-a-"))
        self.assertEqual(os.stat(tmp_a).st_mode & 0o777, 0o700)
        self.assertLess(len(os.path.join(tmp_a, ".com.google.Chrome.XXXXXX", "SingletonSocket")), 108)
        # list reports both; a desk that never started is not_ready; names are checked.
        listed = reply(self.out, self.cmd("desk", {"action": "list"}))["data"]
        self.assertEqual(([d["desk"] for d in listed["desks"]], listed["max_desks"]), (["desk-a", "desk-b"], 2))
        self.assertEqual(reply(self.out, self.cmd("screen", {"desk": "desk-c"}))["fault_code"], "not_ready")
        for bad in ("desk-", "Desk-A", "desk-a/../b", "desk-" + "x" * 30, "other"):
            self.assertEqual(reply(self.out, self.cmd("screen", {"desk": bad}))["fault_code"], "bad_value", bad)
        # Stopping one leaves the other running; the kill switch stops every desk.
        self.assertEqual(reply(self.out, self.cmd("desk", {"action": "stop", "desk": "desk-a"}))["data"]["stopped"], True)
        self.assertEqual([d.on for d in self.made_desks], [False, True])
        self.assertFalse(os.path.exists(tmp_a), "a stopped desk's temp folder is removed")
        self.plugin.stop()
        self.assertEqual([d.on for d in self.made_desks], [False, False])

    def test_a_desk_nobody_uses_stops_by_itself(self):
        # An agent that crashed or restarted mid-task never stops its desk: desk_idle_s does.
        self.boot("trusted", max_desks=2, desk_idle_s=60)
        now = [1000.0]
        self.plugin.clock = lambda: now[0]
        reply(self.out, self.cmd("desk", {"action": "start", "desk": "desk-a"}))
        reply(self.out, self.cmd("desk", {"action": "start", "desk": "desk-b"}))
        now[0] += 40
        reply(self.out, self.cmd("input", {"action": "move", "x": 1, "y": 1, "width": 10, "height": 10, "desk": "desk-b"}))
        now[0] += 30
        self.plugin.poll()
        self.assertEqual([d.on for d in self.made_desks], [False, True], "a, 70 s without a command, stopped; b was used 30 s ago")
        self.assertTrue(any(m.get("op") == "event" and m.get("name") == "desk_idle_stopped" and m.get("desk") == "desk-a" for m in self.out.msgs()))
        now[0] += 61
        self.plugin.poll()
        self.assertEqual([d.on for d in self.made_desks], [False, False])

    def test_each_desk_has_a_browser_profile_and_downloads_of_its_own(self):
        made = []

        class FakeBrowser:
            def __init__(self, binary, profile, headless, env=None):
                self.profile, self.headless, self.env, self.download_dir = profile, headless, env, None
                made.append(self)

            def alive(self):
                return True

            def open(self, url):
                return {"url": url}

            def close(self):
                pass
        self.boot("trusted", max_desks=3)
        self.plugin.browser_factory = FakeBrowser
        for name in ("private", "desk-a"):
            reply(self.out, self.cmd("desk", {"action": "start", "desk": name}))
            r = reply(self.out, self.cmd("browser", {"action": "open", "url": "https://example.org/", "desk": name}))
            self.assertEqual(r["op"], "ack", r)
        self.assertEqual(len(made), 2)
        self.assertTrue(made[0].profile.endswith("-desk"), "the private desk keeps its profile from before v0.9.0")
        self.assertTrue(made[1].profile.endswith("-desk-a"))
        self.assertEqual([b.headless for b in made], [False, False], "shown on the desk")
        self.assertEqual(made[1].download_dir, os.path.join(self.desk_dir, "desk-a", "downloads"))
        self.assertNotEqual(made[0].download_dir, made[1].download_dir)
        self.assertNotEqual(made[0].env["DISPLAY"], made[1].env["DISPLAY"])

    def test_browser_desks_their_page_is_the_screen(self):
        # Desks plan D2: a browser alone, with no display. Its page is its screen, mouse, keyboard and live view.
        self.boot("trusted", max_desks=1)
        made = []

        def factory(binary, profile, headless, env=None):
            b = FakePageBrowser(binary, profile, headless, env)
            made.append(b)
            return b
        self.plugin.browser_factory = factory
        before = len(self.calls)
        r = reply(self.out, self.cmd("desk", {"action": "start", "desk": "desk-a", "kind": "browser", "width": 1000, "height": 500}))
        self.assertEqual((r["op"], r["data"]["kind"], r["data"]["running"], r["data"]["width"]), ("ack", "browser", True, 1000), r)
        r = reply(self.out, self.cmd("desk", {"action": "start", "desk": "desk-b", "kind": "browser"}))
        self.assertEqual(r["op"], "ack", r)
        self.assertEqual((self.made_desks, len(self.calls)), ([], before), "no display, no process started")
        # Two browser desks fill max_desks 1 (each counts as half): a whole desk more is refused.
        r = reply(self.out, self.cmd("desk", {"action": "start", "desk": "desk-c"}))
        self.assertEqual(r["fault_code"], "not_ready")
        self.assertIn("counts as half", r["message"])
        listed = reply(self.out, self.cmd("desk", {"action": "list"}))["data"]["desks"]
        self.assertEqual([(d["desk"], d["kind"]) for d in listed], [("desk-a", "browser"), ("desk-b", "browser")])
        # Its browser: headless, the desk's size, a profile of its own, and never the owner's display.
        r = reply(self.out, self.cmd("browser", {"action": "open", "url": "https://example.org/", "desk": "desk-a"}))
        self.assertEqual(r["op"], "ack", r)
        a = made[0]
        self.assertEqual((a.headless, a.window), (True, (1000, 500)))
        self.assertTrue(a.profile.endswith("-desk-a"))
        self.assertNotIn("DISPLAY", a.env)
        self.assertTrue(os.path.basename(a.env["TMPDIR"]).startswith("ov-desk-a-"))
        # The mouse and keyboard are the page's: a point in a 100×100 picture lands on the 1000×500 page.
        r = reply(self.out, self.cmd("input", {"action": "click", "x": 50, "y": 50, "width": 100, "height": 100, "desk": "desk-a"}))
        self.assertEqual((r["op"], r["data"]["x"], r["data"]["y"], r["data"]["receipt"]["appeared"]), ("ack", 500, 250, ["Row added"]), r)
        self.assertIn(("mouse", "click", 500, 250), a.calls)
        reply(self.out, self.cmd("input", {"action": "scroll", "dy": 3, "desk": "desk-a"}))
        reply(self.out, self.cmd("input", {"action": "type", "text": "hello", "desk": "desk-a"}))
        reply(self.out, self.cmd("input", {"action": "key", "keys": "ctrl+a", "desk": "desk-a"}))
        self.assertEqual([c for c in a.calls if c[0] in ("wheel", "insert", "keys")],
                         [("wheel", 500, 250, 3), ("insert", "hello"), ("keys", "ctrl+a")], "a scroll with no point is at the middle")
        self.assertEqual(len(self.calls), before, "never xdotool")
        # A look and the live view are pictures of the page.
        shot = reply(self.out, self.cmd("screen", {"desk": "desk-a", "max_width": 800}))
        self.assertEqual(shot["data"]["format"], "jpeg")
        self.assertIn(("screenshot", 800), a.calls)
        live = reply(self.out, self.cmd("screen_stream", {"action": "start", "seconds": 30, "desk": "desk-a"}))
        self.assertEqual((live["data"]["streaming"], live["data"]["desk"], live["data"]["tool"]), (True, "desk-a", "page"), live)
        reply(self.out, self.cmd("screen_stream", {"action": "stop"}))
        frame = os.path.join(self.desk_dir, "frame.jpg")
        self.plugin._stream_frame(("page", None), frame, desk="desk-a")
        with open(frame, "rb") as f:
            self.assertEqual(f.read(2), b"\xff\xd8")
        # desk-b has a browser of its own.
        reply(self.out, self.cmd("screen", {"desk": "desk-b"}))
        self.assertEqual(len(made), 2)
        self.assertTrue(made[1].profile.endswith("-desk-b"))
        # Stopping a browser desk closes its browser. The private desk is always a whole desk; kinds are checked.
        reply(self.out, self.cmd("desk", {"action": "stop", "desk": "desk-a"}))
        self.assertFalse(a.alive())
        self.assertEqual(reply(self.out, self.cmd("desk", {"action": "start", "kind": "browser"}))["fault_code"], "bad_value")
        self.assertEqual(reply(self.out, self.cmd("desk", {"action": "start", "desk": "desk-z", "kind": "vm"}))["fault_code"], "bad_value")
        self.plugin.stop()
        self.assertFalse(made[1].alive())

    def test_a_named_desk_scales_points_to_its_own_size(self):
        # A click on desk-a scales to desk-a's size, not to the private desk's (which may not even run).
        self.boot("trusted", max_desks=2)
        reply(self.out, self.cmd("desk", {"action": "start", "desk": "desk-a", "width": 1000, "height": 500}))
        reply(self.out, self.cmd("input", {"action": "click", "x": 50, "y": 50, "width": 100, "height": 100, "desk": "desk-a"}))
        self.assertEqual(self.calls[-1][0][3:5], ["500", "250"])

    def test_a_mac_runs_browser_desks(self):
        out = Out()
        plugin = Computer(env={}, platform="darwin", which=lambda n: "/usr/bin/chromium" if n == "chromium" else None,
                          screen_size=lambda tool: (100, 100))
        rt = Runtime(plugin, infile=io.StringIO(""), outfile=out)
        rt.dispatch({"op": "hello", "config": {"level": "trusted", "roots": [tempfile.gettempdir()], "notify": False,
                                                "desk_dir": tempfile.mkdtemp(prefix="ov-desks-")}})
        desk = out.msgs()[0]["capabilities"]["desk"]
        self.assertEqual((desk["kinds"], desk["window_manager"], desk["screen"], desk["input"]), (["browser"], None, True, True))
        rt.dispatch({"op": "command", "id": "m1", "kind": "desk", "value": {"action": "start", "desk": "desk-a"}, "deadline_ms": 30000})
        r = reply(out, "m1")
        self.assertEqual(r["fault_code"], "unsupported")
        self.assertIn("browser desks", r["message"])
        rt.dispatch({"op": "command", "id": "m2", "kind": "desk", "value": {"action": "start", "desk": "desk-a", "kind": "browser"}, "deadline_ms": 30000})
        self.assertEqual(reply(out, "m2")["data"]["kind"], "browser")
        plugin.stop()

    def test_the_kill_switch_stops_the_desk(self):
        self.boot("trusted")
        reply(self.out, self.cmd("desk", {"action": "start"}))
        self.assertTrue(self.desk.on)
        self.plugin.stop()
        self.assertFalse(self.desk.on)


def _pillow():
    try:
        import PIL  # noqa: F401
        return True
    except ImportError:
        return False


# The screen's size comes from Pillow (without it a capture is a bare PNG): the real-desk tests need it like CI has it.
@unittest.skipUnless(all(shutil.which(t) for t in ("Xvfb", "import")) and sys.platform.startswith("linux") and _pillow(),
                     "needs Xvfb, ImageMagick and Pillow")
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

    def test_two_real_desks_have_their_own_screens(self):
        # Desks plan D1 for real: two desks at once, each its own X display at its own size; stopping one ends only its
        # processes.
        out = Out()
        plugin = Computer(platform="linux")
        rt = Runtime(plugin, infile=io.StringIO(""), outfile=out)
        rt.dispatch({"op": "hello", "config": {"level": "trusted", "roots": [tempfile.gettempdir()], "notify": False, "max_desks": 2}})
        self.addCleanup(plugin.desks.stop_all)
        sizes = {"desk-one": (800, 600), "desk-two": (1024, 640)}
        n = 0
        for name, (w, h) in sizes.items():
            n += 1
            rt.dispatch({"op": "command", "id": "s%d" % n, "kind": "desk", "value": {"action": "start", "desk": name, "width": w, "height": h}, "deadline_ms": 30000})
            self.assertTrue(reply(out, "s%d" % n)["data"]["running"])
        displays = {d["desk"]: d["display"] for d in plugin.desks.list()["desks"]}
        self.assertEqual(len(set(displays.values())), 2, displays)
        for name, (w, h) in sizes.items():
            n += 1
            rt.dispatch({"op": "command", "id": "s%d" % n, "kind": "screen", "value": {"desk": name, "max_width": 1280}, "deadline_ms": 30000})
            shot = reply(out, "s%d" % n)["data"]
            self.assertEqual((shot["width"], shot["height"]), (w, h), name)
        one = [p.pid for _, p in plugin.desks.desks["desk-one"].procs]
        rt.dispatch({"op": "command", "id": "end", "kind": "desk", "value": {"action": "stop", "desk": "desk-one"}, "deadline_ms": 30000})
        self.assertTrue(reply(out, "end")["data"]["stopped"])
        for pid in one:
            with self.assertRaises(ProcessLookupError):
                os.kill(pid, 0)
        self.assertEqual([d["desk"] for d in plugin.desks.list()["desks"]], ["desk-two"])


if __name__ == "__main__":
    unittest.main()


def _a_browser():
    from openvibe_computer.browser import find_binary
    return find_binary(shutil.which, sys.platform)


@unittest.skipUnless(shutil.which("Xvfb") and sys.platform.startswith("linux") and _a_browser(), "needs Xvfb and a Chromium-family browser")
class RealDeskBrowsersTest(unittest.TestCase):
    def test_two_desks_never_share_cookies(self):
        # Desks plan D1's proof: two agents signed in to the same site at once, on two desks of one machine, each with
        # its own cookie; neither browser ever sends the other's.
        import http.server
        import threading

        class Site(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                body = "cookie=[%s]" % (self.headers.get("Cookie") or "")
                self.send_response(200)
                if self.path.startswith("/set/"):
                    self.send_header("Set-Cookie", "who=%s; Path=/" % self.path[len("/set/"):])
                self.send_header("Content-Type", "text/plain")
                self.end_headers()
                self.wfile.write(body.encode())

            def log_message(self, *a):
                pass
        server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Site)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        self.addCleanup(server.shutdown)
        base = "http://127.0.0.1:%d" % server.server_address[1]
        profiles = tempfile.mkdtemp(prefix="ov-desk-browsers-")
        self.addCleanup(lambda: shutil.rmtree(profiles, ignore_errors=True))
        out = Out()
        plugin = Computer(platform="linux")
        # CI runners' Chrome crashes inside their user-namespace policy unless its sandbox is off (as RealBrowserTest).
        from openvibe_computer.browser import Browser
        extra = ["--no-sandbox"] if os.environ.get("CI") else []
        plugin.browser_factory = lambda binary, profile, headless, env=None: Browser(binary, profile, headless, env=env, extra_args=extra)
        rt = Runtime(plugin, infile=io.StringIO(""), outfile=out)
        # Level full: the test site is on this machine (lower levels refuse the machine's own network).
        rt.dispatch({"op": "hello", "config": {"level": "full", "roots": [tempfile.gettempdir()], "notify": False, "max_desks": 2,
                                               "browser_profile": os.path.join(profiles, "browser"), "desk_dir": os.path.join(profiles, "desks")}})
        self.addCleanup(plugin.stop)
        n = [0]

        def cmd(kind, value):
            n[0] += 1
            cid = "b%d" % n[0]
            rt.dispatch({"op": "command", "id": cid, "kind": kind, "value": value, "deadline_ms": 60000})
            r = out.wait_for(lambda m: m.get("id") == cid and m.get("op") in ("ack", "nack"), timeout=60)
            self.assertEqual(r["op"], "ack", r)
            return r.get("data")
        for name, who in (("desk-alpha", "alpha"), ("desk-beta", "beta")):
            cmd("desk", {"action": "start", "desk": name})
            cmd("browser", {"action": "open", "url": base + "/set/" + who, "desk": name})
        for name, who, other in (("desk-alpha", "alpha", "beta"), ("desk-beta", "beta", "alpha")):
            text = cmd("browser", {"action": "open", "url": base + "/show", "desk": name})["text"]
            self.assertIn("who=" + who, text, name)
            self.assertNotIn("who=" + other, text, name)
