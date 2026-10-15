"""The computer plugin's browser: one Chromium the agent drives, in a profile of its own (plan T14, the browser kind).

Chromium is started on the first command with ``--remote-debugging-pipe``: it reads Chrome DevTools Protocol commands
from file descriptor 3 and writes answers and events to 4, each message JSON ended by a NUL byte. No port is opened,
so nothing else on the machine can drive it. The profile is a folder of its own (``browser_profile``, by default
``~/.local/share/openvibe-node/browser``): the agent never sees the owner's own browser sessions unless the owner points
that setting at one.

    Browser(binary, profile, headless).open(url) / read() / click(selector|text) / fill(selector, text) / press(key)
        / back() / screenshot(max_width) / close()

Every page answer is ``{url, title, text, links}``: the page's visible text (cut at ``MAX_TEXT``) and its first links.
"""

import base64
import json
import os
import subprocess
import threading
import time

MAX_TEXT = 20000
STARTUP_S = 45.0
MAX_LINKS = 60
KEY_CODES = {
    "Enter": (13, "\r"), "Tab": (9, "\t"), "Escape": (27, ""), "Backspace": (8, ""), "Delete": (46, ""),
    "ArrowUp": (38, ""), "ArrowDown": (40, ""), "ArrowLeft": (37, ""), "ArrowRight": (39, ""),
    "PageUp": (33, ""), "PageDown": (34, ""), "Home": (36, ""), "End": (35, ""), "Space": (32, " "),
}
CANDIDATES = ("chromium", "chromium-browser", "google-chrome", "google-chrome-stable", "microsoft-edge", "brave-browser")
MAC_APPS = ("/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
            "/Applications/Chromium.app/Contents/MacOS/Chromium",
            "/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
            "/Applications/Brave Browser.app/Contents/MacOS/Brave Browser")

SUMMARY_JS = """(() => {
  const text = (document.body ? document.body.innerText : '').replace(/\\n{3,}/g, '\\n\\n');
  const links = [...document.querySelectorAll('a[href]')]
    .filter((a) => /^https?:/.test(a.href))
    .slice(0, %d)
    .map((a) => ({ text: (a.innerText || a.getAttribute('aria-label') || '').trim().slice(0, 100), href: a.href }));
  return { url: location.href, title: document.title, text: text.slice(0, %d), truncated: text.length > %d, links };
})()""" % (MAX_LINKS, MAX_TEXT, MAX_TEXT)

CLICK_JS = """((selector, text) => {
  let el = null;
  if (selector) el = document.querySelector(selector);
  else {
    const want = text.trim().toLowerCase();
    const items = [...document.querySelectorAll('a, button, [role=button], input[type=submit], input[type=button], summary, label')];
    el = items.find((e) => (e.innerText || e.value || e.getAttribute('aria-label') || '').trim().toLowerCase() === want)
      || items.find((e) => (e.innerText || e.value || e.getAttribute('aria-label') || '').trim().toLowerCase().includes(want));
  }
  if (!el) return false;
  el.scrollIntoView({ block: 'center' });
  el.click();
  return true;
})(%s, %s)"""

FOCUS_JS = """((selector) => {
  const el = document.querySelector(selector);
  if (!el) return false;
  el.scrollIntoView({ block: 'center' });
  el.focus();
  if ('value' in el) { el.value = ''; el.dispatchEvent(new Event('input', { bubbles: true })); }
  else if (el.isContentEditable) el.textContent = '';
  return true;
})(%s)"""


class BrowserError(Exception):
    def __init__(self, code, message):
        super().__init__(message)
        self.code = code
        self.message = message


def find_binary(which, platform, configured=None):
    """The Chromium-family browser to drive: the configured path, else the first one on PATH (or a macOS app)."""
    if configured:
        return configured if os.path.isfile(configured) and os.access(configured, os.X_OK) else None
    for name in CANDIDATES:
        path = which(name)
        if path:
            return path
    if platform == "darwin":
        for path in MAC_APPS:
            if os.path.isfile(path):
                return path
    return None


class Browser:
    def __init__(self, binary, profile, headless=True, timeout_s=15.0, popen=subprocess.Popen, extra_args=()):
        self.binary = binary
        self.extra_args = list(extra_args)
        self.stderr_tail = []
        self.profile = profile
        self.headless = headless
        self.timeout_s = timeout_s
        self.popen = popen
        self.proc = None
        self.out = None
        self.inp = None
        self.next_id = 0
        self.waiting = {}          # message id -> [event, result]
        self.events = []           # (method, params) seen since the last mark
        self.mu = threading.Lock()
        self.session = None
        self.used_at = 0.0

    # ---- the process and the pipe ------------------------------------------------------------------------------------
    def alive(self):
        return self.proc is not None and self.proc.poll() is None

    def start(self):
        if self.alive():
            return
        os.makedirs(self.profile, mode=0o700, exist_ok=True)
        to_chrome_r, to_chrome_w = os.pipe()
        from_chrome_r, from_chrome_w = os.pipe()
        argv = [self.binary, "--remote-debugging-pipe", "--user-data-dir=%s" % self.profile, "--no-first-run",
                "--no-default-browser-check", "--disable-background-networking", "--disable-sync",
                "--disable-component-update", "--password-store=basic", "--window-size=1280,900", "about:blank"]
        if self.headless:
            argv.insert(1, "--headless=new")
        argv[1:1] = self.extra_args

        def fds():
            # In the child: Chromium reads commands on 3 and writes on 4. Both ends move above 10 first, so placing one
            # never closes the other; 3 and 4 are in pass_fds, so the close that follows this function keeps them.
            import fcntl
            r = fcntl.fcntl(to_chrome_r, fcntl.F_DUPFD, 10)
            w = fcntl.fcntl(from_chrome_w, fcntl.F_DUPFD, 10)
            os.dup2(r, 3)
            os.dup2(w, 4)

        self.proc = self.popen(argv, stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE,
                               pass_fds=(3, 4), preexec_fn=fds, start_new_session=True)
        threading.Thread(target=self._stderr, args=(self.proc.stderr,), name="computer-browser-err", daemon=True).start()
        os.close(to_chrome_r)
        os.close(from_chrome_w)
        self.out = os.fdopen(to_chrome_w, "wb", buffering=0)
        self.inp = os.fdopen(from_chrome_r, "rb", buffering=0)
        threading.Thread(target=self._reader, name="computer-browser", daemon=True).start()
        # A first start on a fresh profile can take a while on a slow machine: the first command gets a longer window.
        target = self.call("Target.createTarget", {"url": "about:blank"}, session=False, timeout_s=max(self.timeout_s, STARTUP_S))["targetId"]
        self.session = self.call("Target.attachToTarget", {"targetId": target, "flatten": True}, session=False)["sessionId"]
        self.call("Page.enable")
        self.call("Runtime.enable")

    def _stderr(self, stream):
        """Keep the browser's last lines of stderr: why it closed, when it does."""
        for line in iter(stream.readline, b""):
            self.stderr_tail = (self.stderr_tail + [line.decode("utf-8", "replace").strip()])[-5:]

    def _reader(self):
        buf = b""
        while True:
            try:
                chunk = self.inp.read(65536)
            except (OSError, ValueError):
                chunk = b""
            if not chunk:
                break
            buf += chunk
            while b"\0" in buf:
                raw, buf = buf.split(b"\0", 1)
                try:
                    msg = json.loads(raw.decode("utf-8"))
                except ValueError:
                    continue
                if "id" in msg:
                    with self.mu:
                        slot = self.waiting.get(msg["id"])
                    if slot is not None:
                        slot[1] = msg
                        slot[0].set()
                elif "method" in msg:
                    with self.mu:
                        self.events.append(msg["method"])
                        if len(self.events) > 500:
                            del self.events[:250]
        with self.mu:
            for slot in self.waiting.values():
                slot[0].set()

    def call(self, method, params=None, session=True, timeout_s=None):
        with self.mu:
            self.next_id += 1
            mid = self.next_id
            slot = [threading.Event(), None]
            self.waiting[mid] = slot
        msg = {"id": mid, "method": method, "params": params or {}}
        if session and self.session:
            msg["sessionId"] = self.session
        try:
            self.out.write(json.dumps(msg).encode("utf-8") + b"\0")
        except (OSError, ValueError, AttributeError):
            raise BrowserError("hardware", "the browser is not running")
        ok = slot[0].wait(timeout_s or self.timeout_s)
        with self.mu:
            self.waiting.pop(mid, None)
        if not ok:
            raise BrowserError("expired", "the browser did not answer %s in time" % method)
        if slot[1] is None:
            why = " | ".join(x for x in self.stderr_tail if x)[-400:]
            raise BrowserError("hardware", "the browser closed" + (": %s" % why if why else ""))
        if "error" in slot[1]:
            raise BrowserError("bad_value", "%s: %s" % (method, slot[1]["error"].get("message", "refused")))
        return slot[1].get("result", {})

    def _mark(self):
        with self.mu:
            self.events = []

    def _wait_for(self, methods, timeout_s):
        end = time.monotonic() + timeout_s
        while time.monotonic() < end:
            with self.mu:
                if any(m in methods for m in self.events):
                    return True
            time.sleep(0.05)
        return False

    def _eval(self, expression):
        r = self.call("Runtime.evaluate", {"expression": expression, "returnByValue": True, "awaitPromise": True})
        if r.get("exceptionDetails"):
            raise BrowserError("bad_value", "the page refused: %s" % r["exceptionDetails"].get("text", "an exception"))
        return r.get("result", {}).get("value")

    def _settle(self, navigated_timeout=2.0):
        """After an action: if it started a navigation, wait for the load; then a short pause for scripts."""
        if self._wait_for(("Page.frameStartedLoading", "Page.frameNavigated"), navigated_timeout):
            self._wait_for(("Page.loadEventFired",), self.timeout_s)
        time.sleep(0.3)

    # ---- the actions -----------------------------------------------------------------------------------------------
    def summary(self):
        self.used_at = time.monotonic()
        return self._eval(SUMMARY_JS)

    def open(self, url):
        self.start()
        self._mark()
        r = self.call("Page.navigate", {"url": url})
        if r.get("errorText"):
            raise BrowserError("hardware", "could not open %s: %s" % (url, r["errorText"]))
        self._wait_for(("Page.loadEventFired",), self.timeout_s)
        time.sleep(0.3)
        return self.summary()

    def read(self):
        self._need()
        return self.summary()

    def click(self, selector=None, text=None):
        self._need()
        self._mark()
        if not self._eval(CLICK_JS % (json.dumps(selector), json.dumps(text or ""))):
            raise BrowserError("bad_value", "nothing on the page matches %s" % (selector or repr(text)))
        self._settle()
        return self.summary()

    def fill(self, selector, text):
        self._need()
        if not self._eval(FOCUS_JS % json.dumps(selector)):
            raise BrowserError("bad_value", "nothing on the page matches %s" % selector)
        self.call("Input.insertText", {"text": text})
        return {"done": True, "selector": selector, "chars": len(text)}

    def press(self, key):
        self._need()
        code, text = KEY_CODES[key]
        self._mark()
        base = {"key": key, "code": key if key != "Space" else "Space", "windowsVirtualKeyCode": code, "nativeVirtualKeyCode": code}
        self.call("Input.dispatchKeyEvent", dict(base, type="keyDown", **({"text": text} if text else {})))
        self.call("Input.dispatchKeyEvent", dict(base, type="keyUp"))
        self._settle()
        return self.summary()

    def back(self):
        self._need()
        self._mark()
        self._eval("history.back()")
        self._settle()
        return self.summary()

    def screenshot(self, max_width=1280):
        self._need()
        self.used_at = time.monotonic()
        r = self.call("Page.captureScreenshot", {"format": "jpeg", "quality": 70})
        data = base64.b64decode(r["data"])
        metrics = self.call("Page.getLayoutMetrics")
        view = metrics.get("cssVisualViewport") or metrics.get("visualViewport") or {}
        width, height = int(view.get("clientWidth", 1280)), int(view.get("clientHeight", 900))
        if width > max_width:
            try:
                from PIL import Image
                import io
                img = Image.open(io.BytesIO(data))
                ratio = max_width / float(img.width)
                img = img.resize((max_width, max(1, int(img.height * ratio))))
                out = io.BytesIO()
                img.convert("RGB").save(out, "JPEG", quality=70)
                data, width, height = out.getvalue(), img.width, img.height
            except Exception:
                pass
        return {"format": "jpeg", "width": width, "height": height, "content_b64": base64.b64encode(data).decode("ascii")}

    def _need(self):
        if not self.alive() or not self.session:
            raise BrowserError("not_ready", "no page is open: open a URL first")

    def close(self):
        proc, self.proc, self.session = self.proc, None, None
        for f in (self.out, self.inp):
            try:
                if f:
                    f.close()
            except OSError:
                pass
        self.out = self.inp = None
        if proc is not None and proc.poll() is None:
            try:
                if hasattr(os, "killpg"):
                    os.killpg(proc.pid, 9)
                else:
                    proc.kill()
            except (OSError, ProcessLookupError):
                pass
        if proc is not None:
            try:
                proc.wait(timeout=5)
            except Exception:
                pass
            if proc.stderr:
                try:
                    proc.stderr.close()
                except OSError:
                    pass
        return {"done": True, "closed": proc is not None}
