import http.server
import io
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
from openvibe_computer.browser import Browser, BrowserError, find_binary  # noqa: E402
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


class ReceiptTest(unittest.TestCase):
    def test_what_an_action_changed(self):
        from openvibe_computer.browser import receipt
        before = {"url": "https://a/", "text": "One\nTwo\n\nThree"}
        self.assertEqual(receipt(before, {"url": "https://a/", "text": "One\nTwo\nThree"}), {"url_changed": False, "appeared": [], "gone": 0})
        self.assertEqual(receipt(before, {"url": "https://a/#x", "text": "One\nFour\nThree\n" + "\n".join("n%d" % i for i in range(20))}),
                         {"url_changed": True, "appeared": ["Four"] + ["n%d" % i for i in range(7)], "gone": 1})
        self.assertIsNone(receipt(None, {"url": "x", "text": ""}), "no receipt without a before")
        long = receipt(before, {"url": "https://a/", "text": "x" * 500})
        self.assertEqual(len(long["appeared"][0]), 160)


class PageKeysTest(unittest.TestCase):
    """A browser desk's keys (Desks plan D2): xdotool's names, pressed on the page as the browser's own key events."""

    def browser(self):
        b = Browser("chromium", "/nonexistent")
        self.sent = []
        b.call = lambda method, params=None, **kw: self.sent.append(params) or {}
        b.start = lambda: None
        b._eval = lambda expression: {"url": "https://a/", "text": ""}
        b._settle = lambda *a: None
        return b

    def pressed(self):
        return [(p["type"], p["key"], p["modifiers"], p.get("text")) for p in self.sent]

    def test_keys_and_chords(self):
        b = self.browser()
        b.keys("ctrl+a")
        self.assertEqual(self.pressed(), [("rawKeyDown", "Control", 2, None), ("rawKeyDown", "a", 2, None),
                                          ("keyUp", "a", 2, None), ("keyUp", "Control", 0, None)], "a chord types nothing")
        self.sent.clear()
        b.keys("Return")
        self.assertEqual(self.pressed(), [("keyDown", "Enter", 0, "\r"), ("keyUp", "Enter", 0, None)])
        self.sent.clear()
        b.keys("shift+a")
        self.assertEqual(self.pressed()[1], ("keyDown", "A", 8, "A"))
        self.sent.clear()
        b.keys("Page_Down")
        self.assertEqual(self.pressed()[0][:2], ("rawKeyDown", "PageDown"))
        for bad in ("hyper+a", "Menu"):
            with self.assertRaises(BrowserError):
                b.keys(bad)


class Handler(http.server.SimpleHTTPRequestHandler):
    PAGES = {
        "/": b"<!doctype html><title>Home</title><h1>Hello</h1><a href='/two'>Second page</a>"
             b"<form action='/two'><input id='q' name='q'><button>Search</button></form>",
        "/two": b"<!doctype html><title>Two</title><p>Arrived</p>",
        # Node v0.24.0: a page that opens another (a sign-in popup), and one that sets a cookie.
        "/pop": b"<!doctype html><title>Pop</title><button id='open' onclick=\"window.open('/two', 'signin', 'popup')\">Sign in</button>",
        "/cookie": b"<!doctype html><title>Cookie</title><script>if (!document.cookie.includes('kept=yes')) document.cookie = 'kept=yes; max-age=3600; path=/';</script>",
        "/session": b"<!doctype html><title>Session</title><script>if (!document.cookie.includes('sid=')) document.cookie = 'sid=s1; path=/';</script>",
        # Desks plan D3: a page whose button changes the page without going anywhere, and things that are not elements.
        "/app": b"<!doctype html><title>App</title><p>Ready</p><button id='go' aria-label='Add a row'>+</button>"
                b"<input type='email' placeholder='Email'><input type='checkbox' checked><a href='/two'>Second page</a>"
                b"<button style='display:none'>Hidden</button><div id='rows'></div>"
                b"<script>document.getElementById('go').onclick = () => { document.getElementById('rows').insertAdjacentHTML('beforeend', '<p>Row added</p>'); document.querySelector('p').remove(); };</script>",
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
        # Desks plan D3: the page's interactive elements as refs, and a receipt for each action.
        app = b.open(base + "/app")
        refs = {line.split(" ", 2)[2].split('"')[1]: line.split(" ", 1)[0] for line in app["elements"]}
        self.assertEqual(set(refs), {"Add a row", "Email", "", "Second page"}, app["elements"])
        self.assertTrue(any(l.endswith(' "Email" (email)') for l in app["elements"]), "an input says its type")
        self.assertTrue(any(" checkbox " in l and l.endswith(" checked") for l in app["elements"]), "a checkbox says it is checked")
        self.assertFalse(any("Hidden" in l for l in app["elements"]), "a hidden element is not offered")
        done = b.click(selector='[data-ov-ref="%s"]' % refs["Add a row"])
        self.assertEqual(done["receipt"], {"url_changed": False, "appeared": ["Row added"], "gone": 1})
        again = b.read()
        self.assertIn('%s button "Add a row"' % refs["Add a row"], again["elements"], "the same element keeps its ref")
        away = b.click(selector='[data-ov-ref="%s"]' % refs["Second page"])
        self.assertTrue(away["receipt"]["url_changed"])
        self.assertEqual(b.back()["receipt"]["url_changed"], True)
        self.assertTrue(shot["content_b64"])
        b.close()
        self.assertFalse(b.alive())

    def test_a_popup_is_followed_and_a_cookie_kept(self):
        # Node v0.24.0: a page this page opens comes to the front (the agent and a person watching see it), the page
        # it came from when it closes; and a browser closed gracefully keeps the cookie it was given.
        srv = socketserver.TCPServer(("127.0.0.1", 0), Handler)
        threading.Thread(target=srv.serve_forever, daemon=True).start()
        self.addCleanup(srv.shutdown)
        base = "http://127.0.0.1:%d" % srv.server_address[1]
        profile = __import__("tempfile").mkdtemp(prefix="ov-browser-")
        self.addCleanup(lambda: shutil.rmtree(profile, ignore_errors=True))
        extra = ["--no-sandbox"] if os.environ.get("CI") else []
        b = Browser(find_binary(shutil.which, sys.platform), profile, extra_args=extra)
        self.addCleanup(b.close)
        self.assertEqual(b.open(base + "/pop")["title"], "Pop")
        opened = b.click(selector="#open")
        self.assertEqual((opened["title"], opened["receipt"]["page"]), ("Two", "opened"), opened.get("receipt"))
        b._eval("window.close()")
        back = b.read()
        self.assertEqual(back["title"], "Pop", "the page it came from is in front again")
        # A cookie survives a graceful close.
        b.open(base + "/cookie")
        b.close(graceful=True)
        again = Browser(find_binary(shutil.which, sys.platform), profile, extra_args=extra)
        self.addCleanup(again.close)
        again.open(base + "/cookie")
        self.assertIn("kept=yes", again._eval("document.cookie"))
        # A session cookie (no expiry) ends with the browser; it is kept and put back by hand (Node v0.24.1).
        again.open(base + "/session")
        session = again.session_cookies()
        self.assertEqual([c["name"] for c in session], ["sid"])
        again.close(graceful=True)
        third = Browser(find_binary(shutil.which, sys.platform), profile, extra_args=extra)
        self.addCleanup(third.close)
        third.after_start = lambda: third.restore_cookies(session)
        third.open(base + "/two")
        self.assertIn("sid=s1", third._eval("document.cookie"), "the session sign-in is back")

    def test_a_browser_desk_its_page_is_the_screen(self):
        # Desks plan D2 in a real browser: no display; the page is the desk's screen, mouse and keyboard, and live view.
        srv = socketserver.TCPServer(("127.0.0.1", 0), Handler)
        threading.Thread(target=srv.serve_forever, daemon=True).start()
        self.addCleanup(srv.shutdown)
        base = "http://127.0.0.1:%d" % srv.server_address[1]
        tmp = __import__("tempfile").mkdtemp(prefix="ov-bdesk-")
        self.addCleanup(lambda: shutil.rmtree(tmp, ignore_errors=True))
        extra = ["--no-sandbox"] if os.environ.get("CI") else []
        out = Out()
        plugin = Computer(platform=sys.platform)
        plugin.browser_factory = lambda binary, profile, headless, env=None: Browser(binary, profile, headless, env=env, extra_args=extra)
        rt = Runtime(plugin, infile=io.StringIO(""), outfile=out)
        # Level full: the test page is on 127.0.0.1, which only full may open.
        rt.dispatch({"op": "hello", "config": {"level": "full", "roots": [tmp], "notify": False, "desk_dir": os.path.join(tmp, "desks"),
                                                "browser_profile": os.path.join(tmp, "profile")}})
        self.addCleanup(plugin.stop)
        n = [0]

        def cmd(kind, value):
            n[0] += 1
            rt.dispatch({"op": "command", "id": "b%d" % n[0], "kind": kind, "value": dict(value, desk="desk-t"), "deadline_ms": 60000})
            cid = "b%d" % n[0]
            r = out.wait_for(lambda m: m.get("id") == cid and m.get("op") in ("ack", "nack"), timeout=60)   # a first start is slow
            self.assertEqual(r["op"], "ack", r)
            return r["data"]
        self.assertEqual(cmd("desk", {"action": "start", "kind": "browser", "width": 1000, "height": 600})["kind"], "browser")
        cmd("browser", {"action": "open", "url": base + "/app"})
        shot = cmd("screen", {"max_width": 500})
        self.assertEqual((shot["format"], shot["width"]), ("jpeg", 500), "a picture of the 1000-wide page, at most 500 wide")
        b = plugin.desk_browsers["desk-t"]
        self.assertEqual(b.viewport(), (1000, 600), "the page is the desk's size")
        # Click the button where it is in the 500-wide picture: the page's own click, with what it changed.
        r = b._eval("(() => { const r = document.getElementById('go').getBoundingClientRect(); return [r.x + r.width / 2, r.y + r.height / 2]; })()")
        x, y = round(r[0] * shot["width"] / 1000), round(r[1] * shot["height"] / 600)
        clicked = cmd("input", {"action": "click", "x": x, "y": y, "width": shot["width"], "height": shot["height"]})
        self.assertEqual(clicked["receipt"]["appeared"], ["Row added"], clicked)
        # Type into the field it focused, select all, delete.
        b._eval("document.querySelector('input[type=email]').focus()")
        cmd("input", {"action": "type", "text": "a@b.co"})
        self.assertEqual(b._eval("document.querySelector('input[type=email]').value"), "a@b.co")
        cmd("input", {"action": "key", "keys": "ctrl+a"})
        cmd("input", {"action": "key", "keys": "BackSpace"})
        self.assertEqual(b._eval("document.querySelector('input[type=email]').value"), "")
        cmd("input", {"action": "scroll", "dy": 2})
        # A frame of the live view: a JPEG from the browser itself.
        frame = os.path.join(tmp, "frame.jpg")
        plugin._stream_frame(("page", None), frame, desk="desk-t")
        with open(frame, "rb") as f:
            self.assertEqual(f.read(2), b"\xff\xd8")
        cmd("desk", {"action": "stop"})
        self.assertFalse(b.alive())


if __name__ == "__main__":
    unittest.main()
