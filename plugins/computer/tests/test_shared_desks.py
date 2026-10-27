"""A desk host for other people (desk_isolation: sandbox, Node v0.32, Desks plan D5): browser desks only, each one's
browser under `openvibe-node desk-sandbox`, its profile and downloads inside the sandbox, and nothing of the machine
itself offered. The last test runs a real desk through the sandbox; it needs root, a delegated cgroup and the Node
(OPENVIBE_DESK_SANDBOX_TESTS=require with OPENVIBE_NODE_EXE, as CI's root step runs it) and is skipped otherwise."""
import io
import os
import shutil
import sys
import tempfile
import unittest
from unittest import mock

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
sys.path.insert(0, os.path.dirname(HERE))
sys.path.insert(0, os.path.join(os.path.dirname(os.path.dirname(HERE)), "sdk"))

from openvibe_computer import SANDBOX_DOWNLOADS, SANDBOX_PROFILE, Computer  # noqa: E402
from openvibe_computer.browser import Browser  # noqa: E402
from openvibe_plugin import Fault, Runtime  # noqa: E402
from test_computer import Out, reply  # noqa: E402
from test_desk import FakePageBrowser  # noqa: E402

NODE = "/usr/local/bin/openvibe-node"


class SharedDeskHostTest(unittest.TestCase):
    def boot(self, level="trusted", env=None, **config):
        self.out = Out()
        self.desk_dir = tempfile.mkdtemp(prefix="ov-desks-")
        self.addCleanup(lambda: shutil.rmtree(self.desk_dir, ignore_errors=True))
        self.made = []

        def factory(binary, profile, headless, env=None):
            b = FakePageBrowser(binary, profile, headless, env)
            self.made.append(b)
            return b
        self.plugin = Computer(env={"OPENVIBE_NODE_EXE": NODE} if env is None else env, platform="linux",
                               which=lambda n: "/usr/bin/" + n, browser_factory=factory)
        self.rt = Runtime(self.plugin, infile=io.StringIO(""), outfile=self.out)
        self.rt.dispatch({"op": "hello", "config": {"level": level, "notify": False, "desk_dir": self.desk_dir,
                                                    "desk_isolation": "sandbox", **config}})
        self.n = 0
        return self.out.msgs()

    def cmd(self, kind, value):
        self.n += 1
        cid = "s%d" % self.n
        self.rt.dispatch({"op": "command", "id": cid, "kind": kind, "value": value, "deadline_ms": 30000})
        return reply(self.out, cid)

    def test_it_offers_browser_desks_and_nothing_of_the_machine(self):
        msgs = self.boot(desk_memory_mb=768, desk_pids=99999)
        caps = msgs[0]["capabilities"]
        self.assertEqual(list(caps), ["desk"], "no files, terminal, screen, input, probe or main browser")
        self.assertEqual((caps["desk"]["kinds"], caps["desk"]["isolation"], caps["desk"]["profiles"], caps["desk"]["egress"]),
                         (["browser"], "sandbox", False, "public"))
        self.assertEqual(caps["desk"]["limits"], {"memory_mb": 768, "cpu_millis": 1000, "disk_mb": 512, "pids": 8192})
        self.assertEqual(msgs[-1]["op"], "ready")
        for kind, value in (("exec", {"argv": ["id"]}), ("fs", {"op": "list", "path": "/"}),
                            ("probe", {"target": "http://192.168.1.1/"}), ("screen", {}), ("input", {"action": "click", "x": 1, "y": 1}),
                            ("browser", {"action": "open", "url": "https://example.org/"}),
                            ("screen_stream", {"action": "start", "desk": "desk-a"})):
            r = self.cmd(kind, value)
            self.assertEqual((r["op"], r["fault_code"]), ("nack", "not_allowed"), (kind, r))
        self.assertIn("pictures", self.cmd("screen_stream", {"action": "start", "desk": "desk-a"})["message"])
        r = self.cmd("desk", {"action": "start", "desk": "desk-a"})
        self.assertEqual((r["fault_code"], "browser desks only" in r["message"]), ("unsupported", True), r)
        r = self.cmd("desk", {"action": "start", "desk": "desk-a", "kind": "browser", "profile": "agent"})
        self.assertEqual(r["fault_code"], "unsupported", "no kept profiles: a desk's sign-ins end with it")
        self.assertEqual(self.made, [])

    def test_a_desk_keeps_its_profile_and_downloads_inside_its_sandbox(self):
        self.boot()
        r = self.cmd("desk", {"action": "start", "desk": "desk-a", "kind": "browser", "width": 1000, "height": 600})
        self.assertEqual(r["op"], "ack", r)
        r = self.cmd("browser", {"action": "open", "url": "https://example.org/", "desk": "desk-a"})
        self.assertEqual(r["op"], "ack", r)
        self.assertEqual(self.cmd("screen", {"desk": "desk-a"})["data"]["format"], "jpeg")
        self.assertEqual(self.cmd("input", {"action": "type", "text": "hi", "desk": "desk-a"})["op"], "ack")
        b = self.made[0]
        self.assertEqual((b.profile, b.download_dir, b.headless), (SANDBOX_PROFILE, SANDBOX_DOWNLOADS, True))
        self.assertEqual(os.listdir(self.desk_dir), [], "nothing of the desk's on the host's disk")
        self.cmd("desk", {"action": "stop", "desk": "desk-a"})
        self.assertFalse(b.alive())

    def test_what_a_shared_host_must_be(self):
        for config, why in (({"level": "ask"}, "approve"), ({"level": "full"}, "approve"), ({"desk_egress": "lan"}, "desk_egress"),
                            ({"desk_isolation": "vm"}, "desk_isolation")):
            out = Out()
            rt = Runtime(Computer(env={}, platform="linux"), infile=io.StringIO(""), outfile=out)
            rt.dispatch({"op": "hello", "config": {"level": "trusted", "desk_isolation": "sandbox", **config}})
            self.assertEqual(out.msgs()[-1]["op"], "fault", config)
            self.assertIn(why, out.msgs()[-1]["message"])
        out = Out()
        Runtime(Computer(env={}, platform="darwin"), infile=io.StringIO(""), outfile=out).dispatch(
            {"op": "hello", "config": {"level": "trusted", "desk_isolation": "sandbox"}})
        self.assertIn("needs Linux", out.msgs()[-1]["message"])

    @unittest.skipUnless(hasattr(os, "geteuid"), "no root on this system")
    def test_root_only_on_a_shared_host(self):
        with mock.patch("os.geteuid", return_value=0):
            plugin = Computer(env={}, platform="linux")
            plugin.setup({"level": "trusted", "desk_isolation": "sandbox"})
            with self.assertRaises(Fault) as e:
                Computer(env={}, platform="linux").setup({"level": "trusted"})
            self.assertIn("never runs as root", e.exception.message)

    @unittest.skipIf(os.name == "nt", "paths of a Linux desk host")
    def test_the_sandbox_command(self):
        self.boot(desk_memory_mb=1536, desk_cpu_millis=500, desk_disk_mb=256, desk_pids=300)
        argv = self.plugin._sandbox_argv("/opt/google/chrome/google-chrome")
        self.assertEqual(argv[:10], [NODE, "desk-sandbox", "--memory-mb", "1536", "--cpu-millis", "500", "--disk-mb", "256", "--pids", "300"])
        self.assertEqual(argv[-1], "--")
        pairs = list(zip(argv[10:-1:2], argv[11:-1:2]))
        self.assertIn(("--egress", "public"), pairs)
        self.assertIn(("--bind", "/opt/google/chrome"), pairs)
        self.assertIn(("--setenv", "HOME=/tmp/desk"), pairs)
        under_usr = self.plugin._sandbox_argv("/usr/lib/ov-test-browser/chrome")   # (no such file: not a link elsewhere)
        self.assertEqual([b for f, b in zip(under_usr, under_usr[1:]) if f == "--bind" and b != "/etc/fonts"], [],
                         "a browser under /usr binds nothing: /usr is the system's, bound already")
        self.assertEqual(self.plugin._browser_args(), ["--no-sandbox", "--no-zygote", "--disable-dev-shm-usage", "--disable-gpu"],
                         "no egress proxy: 127.0.0.1 is the sandbox's own there")
        for bad in ("/snap/bin/chromium", "/home/someone/chrome/chrome"):
            with self.assertRaises(Fault) as e:
                self.plugin._sandbox_argv(bad)
            self.assertEqual(e.exception.code, "unsupported", bad)
        self.boot(desk_egress="none")
        self.assertNotIn("--egress", self.plugin._sandbox_argv("/usr/bin/chromium"))
        self.boot(env={})
        self.assertEqual(self.plugin._sandbox_argv("/usr/bin/chromium")[0], "/usr/bin/openvibe-node", "on the PATH")
        self.plugin.which = lambda n: None
        with self.assertRaises(Fault):
            self.plugin._sandbox_argv("/usr/bin/chromium")

    @unittest.skipIf(os.name == "nt", "a Linux desk host")
    def test_the_browser_runs_inside_it_and_ends_it_gently(self):
        started = []

        class Proc:
            pid, stderr = 4321, None

            def __init__(self):
                self.signals = []

            def poll(self):
                return None if not self.signals else 0

            def terminate(self):
                self.signals.append("TERM")

            def wait(self, timeout=None):
                return 0

        def popen(argv, **kw):
            started.append(argv)
            raise OSError("not starting a browser here")
        prefix = [NODE, "desk-sandbox", "--memory-mb", "512", "--"]
        b = Browser("/opt/google/chrome/chrome", SANDBOX_PROFILE, True, popen=popen, extra_args=["--no-sandbox"], sandbox=prefix)
        with self.assertRaises(OSError):
            b.start()
        argv = started[0]
        self.assertEqual(argv[:5], prefix)
        self.assertEqual(argv[5], "/opt/google/chrome/chrome")
        self.assertIn("--user-data-dir=" + SANDBOX_PROFILE, argv)
        self.assertIn("--no-sandbox", argv)
        self.assertFalse(os.path.exists(SANDBOX_PROFILE), "the profile is made inside the sandbox, never on the host")
        p = b.proc = Proc()
        b.close()
        self.assertEqual(p.signals, ["TERM"], "SIGTERM first: the sandbox removes its cgroup, network and root")


@unittest.skipUnless(os.environ.get("OPENVIBE_DESK_SANDBOX_TESTS"), "a real sandboxed desk needs root, a delegated cgroup and the Node")
class RealSharedDeskTest(unittest.TestCase):
    def test_a_real_desk_in_the_sandbox(self):
        from openvibe_computer.browser import find_binary
        # Google Chrome's .deb, as a desk host installs it (a runner's chromium-browser may be a snap's launcher).
        binary = "/opt/google/chrome/chrome" if os.path.exists("/opt/google/chrome/chrome") else find_binary()
        self.assertTrue(binary and os.environ.get("OPENVIBE_NODE_EXE"), "CI's root step has Chrome and builds the Node")
        out = Out()
        plugin = Computer(platform="linux")
        rt = Runtime(plugin, infile=io.StringIO(""), outfile=out)
        rt.dispatch({"op": "hello", "config": {"level": "trusted", "notify": False, "desk_isolation": "sandbox", "desk_egress": "none",
                                                "browser_path": binary, "desk_dir": tempfile.mkdtemp(prefix="ov-desks-")}})
        self.assertEqual(out.msgs()[-1]["op"], "ready", out.msgs())
        n = [0]

        def cmd(kind, value):
            n[0] += 1
            cid = "r%d" % n[0]
            rt.dispatch({"op": "command", "id": cid, "kind": kind, "value": value, "deadline_ms": 60000})
            return out.wait_for(lambda m: m.get("id") == cid and m.get("op") in ("ack", "nack"), timeout=60)
        self.assertEqual(cmd("desk", {"action": "start", "desk": "desk-a", "kind": "browser", "width": 800, "height": 600})["op"], "ack")
        try:
            shot = cmd("screen", {"desk": "desk-a", "max_width": 400})
            b = plugin.desk_browsers.get("desk-a")
            self.assertEqual(shot["op"], "ack", "%r\nthe sandbox said: %s" % (shot, "\n".join(b.stderr_tail) if b else "(no browser)"))
            self.assertEqual(shot["data"]["format"], "jpeg")
            self.assertEqual(b.sandbox[1], "desk-sandbox")
            page = cmd("browser", {"action": "read", "desk": "desk-a"})
            self.assertEqual(page["op"], "ack", page)
            proc = b.proc
        finally:
            cmd("desk", {"action": "stop", "desk": "desk-a"})
        self.assertIsNotNone(proc.poll(), "the desk's sandbox ended with it")
        self.assertEqual([d for d in os.listdir(tempfile.gettempdir()) if d.startswith("openvibe-desk-")], [],
                         "the sandbox removed its root (SIGTERM, not killed)")
        plugin.stop()


if __name__ == "__main__":
    unittest.main()
