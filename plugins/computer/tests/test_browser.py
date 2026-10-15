import http.server
import os
import shutil
import socketserver
import sys
import threading
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.dirname(HERE))
sys.path.insert(0, os.path.join(os.path.dirname(os.path.dirname(HERE)), "sdk"))
sys.path.insert(0, HERE)

from openvibe_computer import Computer  # noqa: E402
from openvibe_computer.browser import Browser, find_binary  # noqa: E402
from openvibe_plugin import Runtime  # noqa: E402
from test_computer import Out, reply  # noqa: E402


class FakeBrowser:
    """What the plugin asks of a browser, recorded; pages are made up."""

    def __init__(self, binary, profile, headless):
        self.binary, self.profile, self.headless = binary, profile, headless
        self.calls = []
        self.open_ = True
        self.used_at = 0

    def alive(self):
        return self.open_

    def _page(self, url="https://example.org/"):
        return {"url": url, "title": "Example", "text": "Example page", "truncated": False, "links": []}

    def open(self, url):
        self.calls.append(("open", url))
        return self._page(url)

    def read(self):
        self.calls.append(("read",))
        return self._page()

    def click(self, selector=None, text=None):
        self.calls.append(("click", selector, text))
        return self._page()

    def fill(self, selector, text):
        self.calls.append(("fill", selector, text))
        return {"done": True, "selector": selector, "chars": len(text)}

    def press(self, key):
        self.calls.append(("press", key))
        return self._page()

    def back(self):
        self.calls.append(("back",))
        return self._page()

    def screenshot(self, max_width):
        self.calls.append(("screenshot", max_width))
        return {"format": "jpeg", "width": max_width, "height": 10, "content_b64": ""}

    def close(self):
        self.open_ = False
        self.calls.append(("close",))
        return {"done": True}


PUBLIC = {"example.org": ["93.184.215.14"], "router.lan": ["192.168.1.1"], "sneaky.example": ["93.184.215.14", "10.0.0.5"]}


class BrowserKindTest(unittest.TestCase):
    def boot(self, level, **config):
        self.made = []

        def factory(binary, profile, headless):
            b = FakeBrowser(binary, profile, headless)
            self.made.append(b)
            return b

        self.out = Out()
        self.plugin = Computer(env={}, platform="linux", which=lambda n: "/usr/bin/chromium" if n == "chromium" else None,
                               browser_factory=factory, resolve=lambda h: PUBLIC.get(h, []))
        self.rt = Runtime(self.plugin, infile=__import__("io").StringIO(""), outfile=self.out)
        self.rt.dispatch({"op": "hello", "config": {"level": level, "roots": [HERE], **config}})
        self.n = 0
        return self.out.msgs()

    def cmd(self, value):
        self.n += 1
        cid = "b%d" % self.n
        self.rt.dispatch({"op": "command", "id": cid, "kind": "browser", "value": value, "deadline_ms": 30000})
        return cid

    def test_described_with_its_level(self):
        caps = self.boot("ask")[0]["capabilities"]
        self.assertEqual(caps["browser"], {"level": "ask", "browser": "chromium", "headless": True})
        caps = self.boot("ask", allow_browser=False)[0]["capabilities"]
        self.assertNotIn("browser", caps)

    def test_reading_runs_at_observe_and_acting_does_not(self):
        self.boot("observe")
        r = reply(self.out, self.cmd({"action": "open", "url": "https://example.org/"}))
        self.assertEqual((r["op"], r["fault_code"]), ("nack", "not_allowed"))
        self.assertEqual(self.made, [], "nothing is started for a refused command")
        r = reply(self.out, self.cmd({"action": "read"}))
        self.assertEqual(r["op"], "ack", "reading the agent's page is observing")
        self.assertEqual(self.made[0].calls, [("read",)])

    def test_ask_waits_for_the_owner_and_shows_what_is_typed(self):
        self.boot("ask")
        cid = self.cmd({"action": "fill", "selector": "#q", "text": "hello"})
        ev = self.out.wait_for(lambda m: m.get("op") == "event" and m.get("name") == "approval")
        self.assertEqual((ev["kind"], ev["summary"]), ("browser", "type 5 characters into #q in the browser: 'hello'"))
        self.assertEqual(self.made, [], "nothing happens before the owner approves")
        self.plugin.on_approval(cid, True)
        self.assertEqual(reply(self.out, cid)["data"], {"done": True, "selector": "#q", "chars": 5})
        cid = self.cmd({"action": "click", "text": "Sign in"})
        ev = self.out.wait_for(lambda m: m.get("op") == "event" and m.get("name") == "approval" and m.get("id") == cid)
        self.assertEqual(ev["summary"], "click the 'Sign in' button or link in the browser")
        self.plugin.on_approval(cid, False)
        self.assertEqual(reply(self.out, cid)["fault_code"], "not_allowed")

    def test_trusted_drives_it_and_the_profile_is_its_own(self):
        self.boot("trusted")
        self.assertEqual(reply(self.out, self.cmd({"action": "open", "url": "https://example.org/a"}))["data"]["url"], "https://example.org/a")
        reply(self.out, self.cmd({"action": "click", "selector": "a.next"}))
        reply(self.out, self.cmd({"action": "press", "key": "Enter"}))
        reply(self.out, self.cmd({"action": "back"}))
        reply(self.out, self.cmd({"action": "screenshot", "max_width": 640}))
        b = self.made[0]
        self.assertEqual(len(self.made), 1, "one browser for the session")
        self.assertEqual(b.calls, [("open", "https://example.org/a"), ("click", "a.next", None), ("press", "Enter"), ("back",), ("screenshot", 640)])
        self.assertTrue(b.profile.endswith(os.path.join("openvibe-node", "browser")), b.profile)

    def test_addresses_and_values_it_refuses(self):
        self.boot("trusted")
        for value, code in [
            ({"action": "open", "url": "file:///etc/passwd"}, "bad_value"),
            ({"action": "open", "url": "javascript:alert(1)"}, "bad_value"),
            ({"action": "open", "url": "http://192.168.1.1/"}, "not_allowed"),
            ({"action": "open", "url": "http://router.lan/"}, "not_allowed"),
            ({"action": "open", "url": "https://sneaky.example/"}, "not_allowed"),
            ({"action": "open", "url": "http://[::1]:8080/"}, "not_allowed"),
            ({"action": "press", "key": "ctrl+alt+Delete"}, "bad_value"),
            ({"action": "fill", "text": "x"}, "bad_value"),
            ({"action": "click"}, "bad_value"),
            ({"action": "eval", "script": "1"}, "bad_value"),
        ]:
            r = reply(self.out, self.cmd(value))
            self.assertEqual((r["op"], r["fault_code"]), ("nack", code), value)
        self.boot("full")
        self.assertEqual(reply(self.out, self.cmd({"action": "open", "url": "http://router.lan/"}))["op"], "ack", "full reaches the local network")

    def test_the_kill_switch_closes_it(self):
        self.boot("trusted")
        reply(self.out, self.cmd({"action": "open", "url": "https://example.org/"}))
        self.plugin.stop()
        self.assertFalse(self.made[0].alive())
        self.assertEqual(reply(self.out, self.cmd({"action": "close"}))["data"], {"done": True})


class Handler(http.server.SimpleHTTPRequestHandler):
    PAGES = {
        "/": b"<!doctype html><title>Home</title><h1>Hello</h1><a href='/two'>Second page</a>"
             b"<form action='/two'><input id='q' name='q'><button>Search</button></form>",
        "/two": b"<!doctype html><title>Two</title><p>Arrived</p>",
    }

    def do_GET(self):
        body = self.PAGES.get(self.path.split("?")[0], b"not found")
        self.send_response(200)
        self.send_header("Content-Type", "text/html")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *a):
        pass


@unittest.skipUnless(find_binary(shutil.which, sys.platform), "no Chromium-family browser on this machine")
class RealBrowserTest(unittest.TestCase):
    def test_a_real_browser_over_the_pipe(self):
        srv = socketserver.TCPServer(("127.0.0.1", 0), Handler)
        threading.Thread(target=srv.serve_forever, daemon=True).start()
        self.addCleanup(srv.shutdown)
        base = "http://127.0.0.1:%d" % srv.server_address[1]
        profile = __import__("tempfile").mkdtemp(prefix="ov-browser-")
        self.addCleanup(lambda: shutil.rmtree(profile, ignore_errors=True))
        # CI runners (Ubuntu 24.04) refuse the unprivileged user namespaces Chrome's sandbox needs; a person's machine does
        # not, and the plugin never turns the sandbox off.
        b = Browser(find_binary(shutil.which, sys.platform), profile, extra_args=["--no-sandbox"] if os.environ.get("CI") else [])
        self.addCleanup(b.close)
        page = b.open(base + "/")
        self.assertEqual((page["title"], [l["text"] for l in page["links"]]), ("Home", ["Second page"]))
        self.assertEqual(b.click(text="Second page")["title"], "Two")
        self.assertEqual(b.back()["title"], "Home")
        self.assertEqual(b.fill("#q", "two words")["chars"], 9)
        self.assertTrue(b.press("Enter")["url"].endswith("/two?q=two+words"))
        shot = b.screenshot(640)
        self.assertEqual((shot["format"], shot["width"] <= 1280), ("jpeg", True))
        self.assertTrue(shot["content_b64"])
        b.close()
        self.assertFalse(b.alive())


if __name__ == "__main__":
    unittest.main()
