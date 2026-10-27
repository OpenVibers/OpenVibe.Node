"""The computer plugin's browser: one Chromium the agent drives, in a profile of its own (plan T14, the browser kind).

Chromium is started on the first command with ``--remote-debugging-pipe``: it reads Chrome DevTools Protocol commands
from file descriptor 3 and writes answers and events to 4, each message JSON ended by a NUL byte. No port is opened,
so nothing else on the machine can drive it. The profile is a folder of its own (``browser_profile``, by default
``~/.local/share/openvibe-node/browser``): the agent never sees the owner's own browser sessions unless the owner points
that setting at one.

    Browser(binary, profile, headless).open(url) / read() / click(selector|text) / fill(selector, text) / press(key)
        / back() / screenshot(max_width) / close()

On a browser desk (Desks plan D2: a browser alone, no display) the page is also the desk's screen: ``mouse``,
``wheel``, ``insert`` and ``keys`` are its mouse and keyboard, and ``frame`` writes one picture of it for the live view.

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
MAX_ELEMENTS = 120
MAX_APPEARED = 8
KEY_CODES = {
    "Enter": (13, "\r"), "Tab": (9, "\t"), "Escape": (27, ""), "Backspace": (8, ""), "Delete": (46, ""),
    "ArrowUp": (38, ""), "ArrowDown": (40, ""), "ArrowLeft": (37, ""), "ArrowRight": (39, ""),
    "PageUp": (33, ""), "PageDown": (34, ""), "Home": (36, ""), "End": (35, ""), "Space": (32, " "),
}
# xdotool's key names (what `input` takes) → the browser's keys: (key, code, Windows key code, text).
PAGE_KEYS = {
    "return": ("Enter", "Enter", 13, "\r"), "enter": ("Enter", "Enter", 13, "\r"), "kp_enter": ("Enter", "NumpadEnter", 13, "\r"),
    "tab": ("Tab", "Tab", 9, ""), "escape": ("Escape", "Escape", 27, ""), "esc": ("Escape", "Escape", 27, ""),
    "backspace": ("Backspace", "Backspace", 8, ""), "delete": ("Delete", "Delete", 46, ""), "space": (" ", "Space", 32, " "),
    "up": ("ArrowUp", "ArrowUp", 38, ""), "down": ("ArrowDown", "ArrowDown", 40, ""), "left": ("ArrowLeft", "ArrowLeft", 37, ""),
    "right": ("ArrowRight", "ArrowRight", 39, ""), "home": ("Home", "Home", 36, ""), "end": ("End", "End", 35, ""),
    "page_up": ("PageUp", "PageUp", 33, ""), "prior": ("PageUp", "PageUp", 33, ""), "page_down": ("PageDown", "PageDown", 34, ""),
    "next": ("PageDown", "PageDown", 34, ""), "insert": ("Insert", "Insert", 45, ""),
}
# Modifiers: the bit CDP's `modifiers` takes, and the key event that holds it down.
PAGE_MODIFIERS = {
    "alt": (1, ("Alt", "AltLeft", 18)), "option": (1, ("Alt", "AltLeft", 18)),
    "ctrl": (2, ("Control", "ControlLeft", 17)), "control": (2, ("Control", "ControlLeft", 17)),
    "super": (4, ("Meta", "MetaLeft", 91)), "meta": (4, ("Meta", "MetaLeft", 91)), "cmd": (4, ("Meta", "MetaLeft", 91)),
    "command": (4, ("Meta", "MetaLeft", 91)), "win": (4, ("Meta", "MetaLeft", 91)),
    "shift": (8, ("Shift", "ShiftLeft", 16)),
}


def page_key(name, shift=False):
    """(key, code, Windows key code, text) for one xdotool key name on a page, or None."""
    low = name.lower()
    if low in PAGE_KEYS:
        return PAGE_KEYS[low]
    if len(name) == 1 and name.isalpha():
        ch = name.upper() if shift else name.lower()
        return (ch, "Key" + name.upper(), ord(name.upper()), ch)
    if len(name) == 1 and name.isdigit():
        return (name, "Digit" + name, ord(name), name)
    if low.startswith("f") and low[1:].isdigit() and 1 <= int(low[1:]) <= 12:
        return ("F" + low[1:], "F" + low[1:], 111 + int(low[1:]), "")
    return None


CANDIDATES = ("chromium", "chromium-browser", "google-chrome", "google-chrome-stable", "microsoft-edge", "brave-browser")
MAC_APPS = ("/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
            "/Applications/Chromium.app/Contents/MacOS/Chromium",
            "/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
            "/Applications/Brave Browser.app/Contents/MacOS/Brave Browser")

# The page as text, its links, and its interactive elements as refs (Desks plan D3: act on refs from the page's own
# structure before ever reaching for a screenshot). An element keeps its ref (data-ov-ref) for as long as it stays on
# the page, so a ref from one read still names the same element after the page changed around it. Each element is one
# compact line, `e7 button "Sign in"`, which costs a model a fraction of a screenshot.
SUMMARY_JS = """(() => {
  const text = (document.body ? document.body.innerText : '').replace(/\\n{3,}/g, '\\n\\n');
  const links = [...document.querySelectorAll('a[href]')]
    .filter((a) => /^https?:/.test(a.href))
    .slice(0, %d)
    .map((a) => ({ text: (a.innerText || a.getAttribute('aria-label') || '').trim().slice(0, 100), href: a.href }));
  const SEL = 'a[href], button, input:not([type=hidden]), select, textarea, summary, [contenteditable=""], [contenteditable=true],'
    + ' [role=button], [role=link], [role=checkbox], [role=radio], [role=tab], [role=menuitem], [role=switch], [role=option], [role=combobox]';
  const shown = (el) => { const r = el.getClientRects(); if (!r.length) return false; const cs = getComputedStyle(el); return cs.visibility !== 'hidden' && cs.display !== 'none'; };
  const roleOf = (el) => el.getAttribute('role') || ({ A: 'link', BUTTON: 'button', SELECT: 'select', TEXTAREA: 'textbox', SUMMARY: 'button' })[el.tagName]
    || (el.tagName === 'INPUT' ? ({ checkbox: 'checkbox', radio: 'radio', submit: 'button', button: 'button', reset: 'button', range: 'slider', file: 'file' })[el.type] || 'textbox' : 'textbox');
  const labelOf = (el) => (el.labels && el.labels.length ? el.labels[0].innerText : '');
  const valueName = (el) => (['checkbox', 'radio'].includes(el.type) ? '' : el.value);
  const nameOf = (el) => (el.getAttribute('aria-label') || labelOf(el) || el.innerText || valueName(el) || el.getAttribute('placeholder') || el.getAttribute('title') || el.getAttribute('alt') || el.getAttribute('name') || '').replace(/\\s+/g, ' ').trim().slice(0, 80);
  window.__ovRef = window.__ovRef || 0;
  const elements = [];
  for (const el of document.querySelectorAll(SEL)) {
    if (elements.length >= %d) break;
    if (!shown(el)) continue;
    if (!el.dataset.ovRef) el.dataset.ovRef = 'e' + (++window.__ovRef);
    const role = roleOf(el);
    let line = el.dataset.ovRef + ' ' + role + ' ' + JSON.stringify(nameOf(el));
    if (role === 'textbox' && el.tagName === 'INPUT' && el.type && el.type !== 'text') line += ' (' + el.type + ')';
    if (el.checked) line += ' checked';
    if (el.disabled) line += ' disabled';
    if (role === 'textbox' && el.value) line += ' value=' + JSON.stringify(String(el.value).slice(0, 40));
    elements.push(line);
  }
  return { url: location.href, title: document.title, text: text.slice(0, %d), truncated: text.length > %d, links, elements };
})()""" % (MAX_LINKS, MAX_ELEMENTS, MAX_TEXT, MAX_TEXT)

# What the page says before an action, to tell what the action changed (its receipt).
BEFORE_JS = """(() => ({ url: location.href, text: (document.body ? document.body.innerText : '').slice(0, 60000) }))()"""


def receipt(before, after):
    """What an action changed (Desks plan D3), from the page before and after it: whether it went to another address,
    the first lines that appeared (MAX_APPEARED), and how many lines went away. An agent reads at once whether its click
    did anything, without another read or a screenshot."""
    if not isinstance(before, dict):
        return None
    old = [l.strip() for l in str(before.get("text") or "").splitlines() if l.strip()]
    new = [l.strip() for l in str(after.get("text") or "").splitlines() if l.strip()]
    old_set, new_set = set(old), set(new)
    appeared = [l[:160] for l in new if l not in old_set][:MAX_APPEARED]
    gone = len([l for l in old_set if l not in new_set])
    return {"url_changed": before.get("url") != after.get("url"), "appeared": appeared, "gone": gone}

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
    def __init__(self, binary, profile, headless=True, timeout_s=15.0, popen=subprocess.Popen, extra_args=(), env=None):
        self.binary = binary
        self.env = env   # the private desk's display and bus, or None for this process's own
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
        self.download_dir = None   # a desk's own downloads folder (set before the first action), else the browser's default
        self.window = (1280, 900)  # the window's size
        self.page_size = None      # on a browser desk, the page's exact size (the desk's: its page is its screen)
        self.write_mu = threading.Lock()   # one message at a time on the pipe: actions and the live view share it
        self.start_mu = threading.Lock()

    # ---- the process and the pipe ------------------------------------------------------------------------------------
    def alive(self):
        return self.proc is not None and self.proc.poll() is None

    def start(self):
        # One start at a time: an action and a browser desk's live view may both find the browser closed at once.
        with self.start_mu:
            if not self.alive():
                self._start()

    def _start(self):
        os.makedirs(self.profile, mode=0o700, exist_ok=True)
        to_chrome_r, to_chrome_w = os.pipe()
        from_chrome_r, from_chrome_w = os.pipe()
        argv = [self.binary, "--remote-debugging-pipe", "--user-data-dir=%s" % self.profile, "--no-first-run",
                "--no-default-browser-check", "--disable-background-networking", "--disable-sync",
                "--disable-component-update", "--password-store=basic", "--window-size=%d,%d" % self.window, "about:blank"]
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
                               pass_fds=(3, 4), preexec_fn=fds, start_new_session=True, **({"env": self.env} if self.env else {}))
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
        if self.page_size:
            self.call("Emulation.setDeviceMetricsOverride", {"width": self.page_size[0], "height": self.page_size[1],
                                                              "deviceScaleFactor": 1, "mobile": False})
        if self.download_dir:
            self.call("Browser.setDownloadBehavior", {"behavior": "allow", "downloadPath": self.download_dir}, session=False)

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
            with self.write_mu:
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

    def _before(self):
        try:
            return self._eval(BEFORE_JS)
        except BrowserError:
            return None

    def _with_receipt(self, before):
        after = self.summary()
        r = receipt(before, after)
        if r is not None:
            after["receipt"] = r
        return after

    def click(self, selector=None, text=None):
        self._need()
        before = self._before()
        self._mark()
        if not self._eval(CLICK_JS % (json.dumps(selector), json.dumps(text or ""))):
            raise BrowserError("bad_value", "nothing on the page matches %s" % (selector or repr(text)))
        self._settle()
        return self._with_receipt(before)

    def fill(self, selector, text):
        self._need()
        if not self._eval(FOCUS_JS % json.dumps(selector)):
            raise BrowserError("bad_value", "nothing on the page matches %s" % selector)
        self.call("Input.insertText", {"text": text})
        return {"done": True, "selector": selector, "chars": len(text)}

    def press(self, key):
        self._need()
        code, text = KEY_CODES[key]
        before = self._before()
        self._mark()
        base = {"key": key, "code": key if key != "Space" else "Space", "windowsVirtualKeyCode": code, "nativeVirtualKeyCode": code}
        self.call("Input.dispatchKeyEvent", dict(base, type="keyDown", **({"text": text} if text else {})))
        self.call("Input.dispatchKeyEvent", dict(base, type="keyUp"))
        self._settle()
        return self._with_receipt(before)

    def back(self):
        self._need()
        before = self._before()
        self._mark()
        self._eval("history.back()")
        self._settle()
        return self._with_receipt(before)

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

    # ---- a browser desk: the page is the screen ---------------------------------------------------------------------
    def page(self):
        """Make sure the browser runs (a blank page until one is opened): a browser desk always has a screen."""
        self.start()
        self.used_at = time.monotonic()

    def viewport(self):
        """The page's size in CSS pixels, which mouse events use."""
        metrics = self.call("Page.getLayoutMetrics")
        view = metrics.get("cssVisualViewport") or metrics.get("visualViewport") or {}
        return int(view.get("clientWidth") or self.window[0]), int(view.get("clientHeight") or self.window[1])

    def mouse(self, action, x, y):
        """move, click, double_click or right_click at (x, y) on the page, with what it changed (a receipt) for a click."""
        self.page()
        before = self._before() if action != "move" else None
        self._mark()
        self.call("Input.dispatchMouseEvent", {"type": "mouseMoved", "x": x, "y": y})
        if action == "move":
            return {"done": True}
        button = "right" if action == "right_click" else "left"
        for n in range(1, (2 if action == "double_click" else 1) + 1):
            self.call("Input.dispatchMouseEvent", {"type": "mousePressed", "x": x, "y": y, "button": button, "clickCount": n})
            self.call("Input.dispatchMouseEvent", {"type": "mouseReleased", "x": x, "y": y, "button": button, "clickCount": n})
        self._settle(1.0)
        return self._receipt_only(before)

    def wheel(self, x, y, dy):
        """Scroll at (x, y): dy clicks of the wheel (negative is up), about 100 pixels each."""
        self.page()
        self.call("Input.dispatchMouseEvent", {"type": "mouseWheel", "x": x, "y": y, "deltaX": 0, "deltaY": dy * 100})
        time.sleep(0.2)
        return {"done": True}

    def insert(self, text):
        """Type text where the page's focus is."""
        self.page()
        self.call("Input.insertText", {"text": text})
        return {"done": True}

    def keys(self, chord):
        """One key or chord in xdotool's names (Return, ctrl+a, shift+Tab, F5), pressed on the page."""
        parts = chord.split("+")
        mods = [PAGE_MODIFIERS.get(p.lower()) for p in parts[:-1]]
        if None in mods:
            raise BrowserError("bad_value", "unknown modifier in %s" % chord)
        bits = sum(m[0] for m in mods)
        key = page_key(parts[-1], shift=bool(bits & 8))
        if key is None:
            raise BrowserError("unsupported", "the key %s is not supported on a browser desk" % parts[-1])
        self.page()
        before = self._before()
        self._mark()
        held = 0
        for bit, (k, code, vk) in mods:
            held |= bit
            self.call("Input.dispatchKeyEvent", {"type": "rawKeyDown", "key": k, "code": code, "windowsVirtualKeyCode": vk, "modifiers": held})
        k, code, vk, text = key
        typed = text if text and not bits & 7 else ""   # with ctrl, alt or meta held a key types nothing
        down = {"type": "keyDown" if typed else "rawKeyDown", "key": k, "code": code, "windowsVirtualKeyCode": vk, "modifiers": bits}
        if typed:
            down["text"] = down["unmodifiedText"] = typed
        self.call("Input.dispatchKeyEvent", down)
        self.call("Input.dispatchKeyEvent", {"type": "keyUp", "key": k, "code": code, "windowsVirtualKeyCode": vk, "modifiers": bits})
        for bit, (mk, mcode, mvk) in reversed(mods):
            held &= ~bit
            self.call("Input.dispatchKeyEvent", {"type": "keyUp", "key": mk, "code": mcode, "windowsVirtualKeyCode": mvk, "modifiers": held})
        self._settle(1.0)
        return self._receipt_only(before)

    def _receipt_only(self, before):
        out = {"done": True}
        try:
            r = receipt(before, self._eval(BEFORE_JS))
        except BrowserError:
            r = None
        if r is not None:
            out["receipt"] = r
        return out

    def frame(self, path, max_width, quality):
        """One JPEG picture of the page at path (written whole, then moved into place), at most max_width wide."""
        self.page()
        width, height = self.viewport()
        params = {"format": "jpeg", "quality": quality}
        if width > max_width:
            params["clip"] = {"x": 0, "y": 0, "width": width, "height": height, "scale": max_width / float(width)}
        data = base64.b64decode(self.call("Page.captureScreenshot", params)["data"])
        with open(path + ".part", "wb") as f:
            f.write(data)
        os.replace(path + ".part", path)

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
