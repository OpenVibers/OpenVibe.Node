"""OpenVibe Node computer plugin: the machine's terminal, files, screen and input for its owner's agents (plan T14/T17).

Off unless its owner lists it in the Node's config. The owner sets the **control level** on the machine itself; a cloud
command can never raise it:

* ``observe`` (the default): capture the screen; list, stat and read files inside the roots.
* ``ask``: also run programs, write files inside the roots and use the mouse and keyboard, each only after the owner
  approves it on the machine
  (the tray, a notification's buttons, or ``openvibe-node approvals``, ``approve <id>``, ``deny <id>``); unanswered
  requests expire.
* ``trusted``: run programs, write files inside the roots and use the mouse and keyboard without asking.
* ``full``: as trusted, anywhere the account running the Node can reach.

The local kill switch (``openvibe-node stop``) and a remote e-stop drop the plugin to observe, kill what is running
and deny what waits; the core refuses acting commands while either is latched too. The plugin never runs as root.

Command kinds and their answers (the ack's ``data``):

* ``exec`` ``{"argv": [...], "cwd"?, "timeout_s"?, "stdin"?}`` → ``{exit_code, stdout, stderr, truncated, timed_out,
  duration_ms}``. A program and its arguments, never a shell line; output is cut at ``max_output_bytes``.
* ``fs`` ``{"op": "list"|"stat"|"read"|"write"|"mkdir", "path", …}``: ``read`` takes ``offset``/``length`` and answers
  ``content_b64``; ``write`` takes ``content_b64`` and ``mode`` (``create``, ``overwrite`` or ``append``).
* ``screen`` ``{"max_width"?}`` → ``{format, width, height, content_b64}`` (JPEG through Pillow when it is installed).
* ``screen_stream`` ``{"action": "start", "seconds"?, "fps"?}`` → ``{streaming, seconds, fps, tool}``, or ``{"action":
  "stop"}``: the screen shown live to the machine's owner. The plugin captures a JPEG frame ``fps`` times a second (at
  most 5) with the same tools as ``screen`` and hands each to the core as video (``stream: "screen"``), which publishes
  it to the owner's private stream; the core alone holds the WHIP URL and key. It runs at every level, like a
  screenshot, says so on the desktop when it starts (config ``notify``), and ends when its time is up (30 s to 30 min),
  on stop, and at once on the local kill switch or an e-stop. Config ``allow_screen_stream: false`` turns it off.
* ``input`` ``{"action", "x"?, "y"?, "width"?, "height"?, "dy"?, "text"?, "keys"?}`` → ``{done, action, x?, y?}``: move,
  click, double_click or right_click at a point, scroll by ``dy`` clicks, type ``text`` into whatever has focus, or press
  one key or chord (``keys``, for example ``ctrl+s``). ``x``/``y`` are in the space of a screen picture ``width`` ×
  ``height`` (the ``screen`` answer) and are scaled to the real screen. An acting command: refused at observe, approved
  one by one at ask. xdotool on X11, cliclick on macOS (no scroll there); none on Wayland yet. Config
  ``allow_input: false`` turns it off.
* ``browser`` ``{"action", "url"?, "selector"?, "text"?, "key"?, "max_width"?}``: a Chromium the agent drives, in a profile
  of its own (``browser_profile``), over a pipe (no port is opened). ``open`` a ``http(s)`` URL, ``click`` a CSS
  ``selector`` or the button or link whose ``text`` it shows, ``fill`` a field (``selector`` and ``text``), ``press`` a
  ``key`` (Enter, Tab, Escape, arrows …), ``back`` → ``{url, title, text, truncated, links}``; ``read`` the page the same
  way; ``screenshot`` → ``{format, width, height, content_b64}``; ``close``. Reading, screenshots and closing run at every
  level; the rest are acting commands. Below level full, ``open`` refuses an address on the machine's own network.
  Config ``allow_browser: false`` turns it off; ``browser_path``, ``browser_headless`` (default true).
* ``desk`` ``{"action": "start"|"stop"|"status"|"list", "desk"?, "width"?, "height"?}``: a private desk (``desk.py``): an Xvfb display with a
  window manager and a D-Bus session of its own, where the agent works without touching the owner's screen. ``screen``,
  ``screen_stream``, ``input`` and ``browser`` take ``"desk": "private"`` to act there (the browser shown on the desk, in a
  profile of its own, ``browser_profile`` + ``-desk``); ``main`` (the default) is the owner's screen. Several desks run at
  once (Node v0.19, Desks plan D1): ``"desk": "desk-<name>"`` names one (start it with ``desk`` and that name), each with
  its own display, bus, browser profile (``browser_profile`` + ``-<name>``), temp folder and downloads folder, so two
  agents working at once never see or drive each other's screen. ``max_desks`` (default: one per 2 GiB of memory, 1 to
  8) caps how many run at once; ``list`` reports the running ones. A desk no command used for ``desk_idle_s`` (15
  minutes by default) is stopped, so a desk an agent left behind (it crashed or restarted mid-task) ends by itself.
  ``"profile": "<name>"`` on start (Node v0.24, Desks plan D7 on one machine) keeps the desk's browser profile by
  name: the desk's browser starts from that profile (its cookies and site storage: sign in once, on any desk) and the
  desk saves it back when it stops (the last desk to stop wins), then removes its own copy. Caches are never kept, a
  profile larger than ``desk_profile_max_mb`` is not saved, and the kill switch saves nothing.
  ``"kind": "browser"`` on start (Node v0.23, Desks plan D2) makes a **browser desk**: a headless browser alone, with no
  display, about half the cost of a whole desk (``kind: desktop``, the default), on Linux or macOS. Its page is its
  screen: ``screen``, ``input`` (mouse, wheel, keys and text, sent to the page) and ``screen_stream`` (frames of the page)
  on it go through the browser's pipe, and ``browser`` drives that same page. A browser desk counts as half a desk
  against ``max_desks``. ``describe`` lists the kinds this machine runs (``desk.kinds``). Starting and stopping act
  (refused at observe, approved at ask), ``status`` runs at every level, and the kill switch stops the desk. Whole desks
  need Linux with Xvfb, browser desks a Chromium-family browser; config ``allow_desk: false`` turns both off.
* ``probe`` ``{"target", "timeout_s"?}`` → for ``http(s)://…`` one GET, no redirect followed: ``{status, content_type,
  etag, last_modified, body, truncated, ms}``; for ``tcp://host:port``: ``{open, ms, error?}``. Read-only, so it runs at
  every level, and only towards the machine's own network: every address the name resolves to must be private,
  loopback or link-local, and the connection goes to the address that was checked. Config ``probe: false`` turns it off.
"""

import base64
import contextlib
import http.client
import ipaddress
import os
import re
import socket
import ssl
import shlex
import shutil
import signal
import stat
import subprocess
import sys
import tempfile
import threading
import time
import urllib.parse

from openvibe_plugin import Deferred, Fault, Plugin

from .browser import KEY_CODES, Browser, BrowserError, find_binary
from .desk import MAX_DESKS_CAP, BrowserDesk, Desks, PrivateDesk, default_max_desks

LEVELS = ("observe", "ask", "trusted", "full")
RANK = {name: i for i, name in enumerate(LEVELS)}
READ_OPS = frozenset({"list", "stat", "read"})
WRITE_OPS = frozenset({"write", "mkdir"})
MAX_ARGS = 64
MAX_ARG_LEN = 4096
MAX_STDIN = 64 * 1024
MAX_LIST = 1000
MAX_RUNNING = 2
MAX_WAITING = 8
# An approval from the desktop (the notification's button or the macOS dialog) is not taken while the agent drives the
# mouse or keyboard, nor this many seconds after: an approved click must not land on another request's Approve button.
INPUT_GUARD_S = 2.0
# The core caps an ack's data at 512 KB of JSON; base64 grows by a third.
MAX_OUTPUT_CAP = 192 * 1024
MAX_READ_CAP = 256 * 1024
SCREEN_MAX_BYTES = 300 * 1024
# The live screen: frame width (Pillow resizes larger screens) and JPEG quality; the stream ends after this many
# capture failures in a row.
STREAM_WIDTH = 1600
STREAM_QUALITY = 70
STREAM_MAX_FAILURES = 5
INPUT_ACTIONS = frozenset({"move", "click", "double_click", "right_click", "scroll", "type", "key"})
BROWSER_READS = frozenset({"read", "screenshot", "close"})
BROWSER_ACTS = frozenset({"open", "click", "fill", "press", "back"})
MAX_URL = 2048
PROFILE_NAME = re.compile(r"^[a-z0-9][a-z0-9-]{0,23}$")
# What a kept profile leaves out: caches, crash reports, locks and the last session's tabs (a desk opens on a blank page).
PROFILE_SKIP = shutil.ignore_patterns("Cache", "Code Cache", "GPUCache", "GrShaderCache", "GraphiteDawnCache", "ShaderCache",
                                      "DawnCache", "DawnGraphiteCache", "DawnWebGPUCache", "CacheStorage", "ScriptCache",
                                      "component_crx_cache", "Crashpad", "BrowserMetrics*", "Safe Browsing",
                                      "optimization_guide_*", "Singleton*", "Sessions", "blob_storage", "*.tmp")
MAX_SELECTOR = 500
POINT_ACTIONS = frozenset({"move", "click", "double_click", "right_click"})
MAX_TYPE = 2000
KEYS_OK = frozenset("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_+-")
MODIFIERS = {"ctrl": "ctrl", "control": "ctrl", "alt": "alt", "option": "alt", "shift": "shift", "cmd": "cmd",
             "command": "cmd", "super": "super", "meta": "super", "win": "super"}
# xdotool key names → cliclick's kp: names (macOS); a single character is typed with t:.
CLICLICK_KEYS = {"return": "return", "enter": "enter", "escape": "esc", "esc": "esc", "tab": "tab", "backspace": "delete",
                 "delete": "fwd-delete", "space": "space", "up": "arrow-up", "down": "arrow-down", "left": "arrow-left",
                 "right": "arrow-right", "home": "home", "end": "end", "page_up": "page-up", "prior": "page-up",
                 "page_down": "page-down", "next": "page-down"}



def _parse_notify_send(out):
    """notify-send --wait prints the name of the button pressed; nothing when the notification just closed."""
    lines = (out or "").strip().splitlines()
    last = lines[-1].strip() if lines else ""
    return True if last == "approve" else False if last == "deny" else None


def _parse_dialog(out):
    """osascript's display dialog: "button returned:Approve, gave up:false"; giving up answers nothing."""
    out = out or ""
    if "gave up:true" in out:
        return None
    return True if "button returned:Approve" in out else False if "button returned:Deny" in out else None

def _tree_size(path, ignore):
    """The bytes a copy of path with that ignore would hold."""
    total = 0
    for root, dirs, files in os.walk(path):
        skip = ignore(root, dirs + files)
        dirs[:] = [d for d in dirs if d not in skip]
        for f in files:
            if f not in skip:
                try:
                    total += os.lstat(os.path.join(root, f)).st_size
                except OSError:
                    pass
    return total


def _int(v, name, lo, hi, default):
    if v is None:
        return default
    if isinstance(v, bool) or not isinstance(v, (int, float)) or v != v:
        raise Fault("bad_value", "%s must be a number" % name)
    return int(max(lo, min(hi, v)))


class Computer(Plugin):
    driver = "computer"
    version = "0.12.0"
    motion_kinds = frozenset()
    telemetry_interval_s = 2.0

    def __init__(self, which=shutil.which, popen=subprocess.Popen, clock=time.monotonic, env=None, platform=None, screen_size=None,
                 browser_factory=None, resolve=None, desk=None, desk_factory=None):
        self.which = which
        self.popen = popen
        self.screen_size = screen_size or self._screen_size
        self.clock = clock
        self.env = os.environ if env is None else env
        self.platform = platform or sys.platform
        self.level = "observe"
        self.roots = []
        self.timeout_s = 30
        self.max_output = 64 * 1024
        self.max_read = 256 * 1024
        self.allow_exec = True
        self.allow_probe = True
        self.allow_input = True
        self.notify = True
        self.notify_actions = None  # whether notify-send shows buttons (libnotify 0.7.10+); found out on first use
        self.prompts = {}  # command id -> the notification or dialog process asking about it
        self.tray_until = 0.0  # the Node's tray shows the approvals until then (op "tray")
        self.input_busy = 0
        self.input_ended = None
        self.approval_timeout_s = 120
        self.pending = {}  # command id -> (run, expires_at, summary)
        self.procs = set()
        self.running = 0
        self.mu = threading.Lock()
        self.allow_browser = True
        self.browser_path = None
        self.browser_profile = None
        self.browser_headless = True
        self.browser_idle_s = 600
        self.browser = None
        self.allow_screen_stream = True
        self.stream = None  # the live screen: {"sid", "until", "fps", "tool", "dir", "stop": Event}
        self.stream_mu = threading.Lock()
        self.browser_mu = threading.Lock()
        self.browser_op_mu = threading.Lock()
        self.browser_factory = browser_factory or (lambda binary, profile, headless, env=None: Browser(binary, profile, headless, env=env))
        self.desk_factory = desk_factory or (lambda: PrivateDesk(which=which, popen=popen))
        self.desk = desk or self.desk_factory()   # the desk named private (the first one, as before v0.9.0)
        self.desks = Desks(self.desk_factory, private=self.desk, max_desks=default_max_desks(),
                           browser_factory=lambda: BrowserDesk(clock=time.time, base_env=self.env))
        self.allow_desk = True
        self.desk_browsers = {}    # desk name -> the agent's browser on that desk (its own profile, shown there)
        self.desk_used = {}        # desk name -> when a command last used it (clock), for desk_idle_s
        self.desk_profiles = {}    # desk name -> the kept profile it started with (Node v0.24.0)
        self.desk_restored = set() # desks whose browser started from their kept profile (so it is saved back on stop)
        self.profile_locks = {}    # profile name -> one copy in or out at a time
        self.desk_profile_max_mb = 256
        self.desk_idle_s = 900
        self.browser_locks = {}    # "main" or a desk name -> one action at a time in that browser
        self.resolve = resolve or (lambda host: [ai[4][0] for ai in socket.getaddrinfo(host, None)])

    # ---- configuration -------------------------------------------------------------------------------------------
    def _configure(self, config):
        level = str(config.get("level") or "observe").strip().lower()
        if level not in RANK:
            raise Fault("bad_value", "level is one of %s" % ", ".join(LEVELS))
        roots = config.get("roots")
        if roots is None:
            roots = [os.path.expanduser("~")]
        if not isinstance(roots, list) or not all(isinstance(r, str) and r.strip() for r in roots):
            raise Fault("bad_value", "roots is a list of folders")
        self.level = level
        self.roots = [os.path.realpath(os.path.expanduser(r.strip())) for r in roots]
        self.timeout_s = _int(config.get("timeout_s"), "timeout_s", 1, 600, 30)
        self.max_output = _int(config.get("max_output_bytes"), "max_output_bytes", 1024, MAX_OUTPUT_CAP, 64 * 1024)
        self.max_read = _int(config.get("max_read_bytes"), "max_read_bytes", 1024, MAX_READ_CAP, 256 * 1024)
        self.approval_timeout_s = _int(config.get("approval_timeout_s"), "approval_timeout_s", 10, 600, 120)
        self.allow_exec = config.get("allow_exec", True) is not False
        self.allow_probe = config.get("probe", True) is not False
        self.allow_input = config.get("allow_input", True) is not False
        self.notify = config.get("notify", True) is not False
        self.allow_browser = config.get("allow_browser", True) is not False
        self.allow_screen_stream = config.get("allow_screen_stream", True) is not False
        self.allow_desk = config.get("allow_desk", True) is not False
        self.desks.max_desks = _int(config.get("max_desks"), "max_desks", 1, MAX_DESKS_CAP, default_max_desks())
        self.desk_idle_s = _int(config.get("desk_idle_s"), "desk_idle_s", 60, 86400, 900)
        self.desk_profile_max_mb = _int(config.get("desk_profile_max_mb"), "desk_profile_max_mb", 16, 4096, 256)
        self.desks.base_dir = os.path.expanduser(config.get("desk_dir") or "~/.local/share/openvibe-node/desks")
        self.browser_path = config.get("browser_path") or None
        self.browser_profile = os.path.expanduser(config.get("browser_profile") or "~/.local/share/openvibe-node/browser")
        self.browser_headless = config.get("browser_headless", True) is not False
        self.browser_idle_s = _int(config.get("browser_idle_s"), "browser_idle_s", 60, 86400, 600)

    def describe(self, config):
        try:
            self._configure(config)
        except Fault:
            return {}
        caps = {"files": {"level": self.level, "roots": self.roots, "max_read_bytes": self.max_read}}
        if self.allow_exec:
            caps["terminal"] = {"level": self.level, "timeout_s": self.timeout_s, "max_output_bytes": self.max_output}
        tool = self._screen_tool()
        if tool:
            caps["screen"] = {"level": self.level, "tool": tool[0],
                              "stream": bool(self.allow_screen_stream and self._stream_capable(tool))}
        if self.allow_probe:
            caps["probe"] = {"lan_only": True, "schemes": ["http", "https", "tcp"]}
        itool = self._input_tool() if self.allow_input else None
        if itool:
            caps["input"] = {"level": self.level, "tool": itool, "scroll": itool == "xdotool"}
        binary = self._browser_binary()
        if binary:
            caps["browser"] = {"level": self.level, "browser": os.path.basename(binary), "headless": self.browser_headless}
        kinds = self._desk_kinds()
        if kinds:
            # A browser desk's screen and input are its page's own (the browser's pipe); a whole desk's need X tools.
            whole, page = "desktop" in kinds, "browser" in kinds
            caps["desk"] = {"level": self.level, "window_manager": self.desks.window_manager() if whole else None,
                            "input": page or (whole and bool(self.which("xdotool"))),
                            "screen": page or (whole and bool(self._x11_tool())),
                            "named": True, "max_desks": self.desks.max_desks, "kinds": kinds, "profiles": True}
        return caps

    def _desk_kinds(self):
        """The kinds of desk this machine runs: desktop (Linux with Xvfb) and browser (a Chromium-family browser)."""
        if not self.allow_desk:
            return []
        kinds = []
        if self.platform.startswith("linux") and self.desks.available():
            kinds.append("desktop")
        if self._browser_binary():
            kinds.append("browser")
        return kinds

    def setup(self, config):
        self._configure(config)
        if hasattr(os, "geteuid") and os.geteuid() == 0:
            raise Fault("not_allowed", "the computer plugin never runs as root: run the Node as your own account")

    # ---- safety ----------------------------------------------------------------------------------------------------
    def effective_level(self):
        """The level now: observe while a local stop or an e-stop is latched."""
        if self.runtime is not None and self.runtime.estopped:
            return "observe"
        return self.level

    def stop(self):
        """halt, stop, the local kill switch and an e-stop: kill what runs, deny what waits."""
        with self.mu:
            procs = list(self.procs)
            waiting = list(self.pending.keys())
            self.pending.clear()
        for p in procs:
            self._kill(p)
        self._close_browser()
        self._end_stream("stopped on the machine")
        self._close_desk_browsers()
        self.desks.stop_all()
        self._drop_desk_profiles()
        for cid in waiting:
            self._close_prompt(cid)
            self.emit_event("approval_done", id=cid, approved=False, reason="stopped")
            self.fail(cid, "local_stop", "stopped on the machine before it ran")

    def _kill(self, proc):
        try:
            if hasattr(os, "killpg"):
                os.killpg(proc.pid, 9)
            else:
                proc.kill()
        except (OSError, ProcessLookupError):
            pass

    def poll(self):
        now = self.clock()
        b = self.browser
        if b is not None and b.alive() and b.used_at and time.monotonic() - b.used_at > self.browser_idle_s:
            self._close_browser()
        # A desk no command used for desk_idle_s is stopped: an agent that crashed or restarted mid-task never ran its
        # own stop, and a desk left running would hold its share of the machine and its browser's sessions.
        for name in self.desks.running_names():
            if now - self.desk_used.get(name, now) > self.desk_idle_s:
                self._end_desk_browser(name)
                self.desks.stop(name)
                self.desk_used.pop(name, None)
                self.emit_event("desk_idle_stopped", desk=name, idle_s=self.desk_idle_s)
                self.log("desk %s stopped after %d s without a command", name, self.desk_idle_s)
            elif name not in self.desk_used:
                self.desk_used[name] = now
        expired = []
        with self.mu:
            for cid, (_, at, _) in list(self.pending.items()):
                if now >= at:
                    expired.append(cid)
                    del self.pending[cid]
            waiting, running = len(self.pending), self.running
        for cid in expired:
            self._close_prompt(cid)
            self.emit_event("approval_done", id=cid, approved=False, reason="expired")
            self.fail(cid, "not_allowed", "nobody answered on the machine in time")
        # Sensors are what the core forwards to Bot: the machines page shows the level and what waits on the machine.
        streaming = self.stream is not None
        return {"sensors": {"computer_level": self.effective_level(), "computer_waiting": waiting, "computer_running": running,
                            "computer_screen_stream": "live" if streaming else "off",
                            "computer_desks": len(self.desks.running_names())}}

    # ---- paths -----------------------------------------------------------------------------------------------------
    def _inside(self, path):
        return any(path == r or path.startswith(r.rstrip(os.sep) + os.sep) for r in self.roots)

    def _path(self, raw, level, create=False):
        """The real path a command names, inside the roots unless the level is full."""
        if not isinstance(raw, str) or not raw or len(raw) > MAX_ARG_LEN or "\x00" in raw:
            raise Fault("bad_value", "path is a non-empty string")
        path = os.path.expanduser(raw)
        if not os.path.isabs(path):
            raise Fault("bad_value", "path must be absolute")
        if create and not os.path.lexists(path):
            parent = os.path.realpath(os.path.dirname(path))
            real = os.path.join(parent, os.path.basename(path))
        else:
            real = os.path.realpath(path)
        if level != "full" and not self._inside(real):
            raise Fault("not_allowed", "%s is outside this machine's allowed folders" % real)
        return real

    # ---- commands --------------------------------------------------------------------------------------------------
    def handle(self, cmd):
        v = cmd.value
        if not isinstance(v, dict):
            raise Fault("bad_value", "value must be an object")
        level = self.effective_level()
        if cmd.kind == "desk":
            return self._desk_op(cmd, v, level)
        desk_env = self._desk_env(v) if cmd.kind in ("screen", "screen_stream", "input", "browser") else None
        desk_name = v.get("desk") if desk_env else None
        if desk_name:
            self.desk_used[desk_name] = self.clock()
        page = bool(desk_name) and self.desks.kind(desk_name) == "browser"   # a browser desk: its page is its screen
        if cmd.kind == "screen" and page:
            width = _int(v.get("max_width"), "max_width", 320, 3840, 1280)
            return self._start(cmd, lambda: self._page_shot(desk_name, width))
        if cmd.kind == "screen":
            tool = self._x11_tool() if desk_env else self._screen_tool()
            if not tool:
                raise Fault("unsupported", "no screen capture tool on this machine")
            width = _int(v.get("max_width"), "max_width", 320, 3840, 1280)
            return self._start(cmd, lambda: self._screen(tool, width, env=desk_env))
        if cmd.kind == "screen_stream":
            return self._screen_stream(v, env=desk_env, desk=desk_name, page=page)
        if cmd.kind == "fs":
            op = v.get("op")
            if op in READ_OPS:
                return self._fs_read_op(op, v, level)
            if op not in WRITE_OPS:
                raise Fault("bad_value", "op is list, stat, read, write or mkdir")
            run, summary = self._fs_write_op(op, v, level)
            return self._act(cmd, level, run, summary, threaded=False)
        if cmd.kind == "probe":
            if not self.allow_probe:
                raise Fault("unsupported", "this machine does not allow probes")
            target, timeout = self._probe_target(v)
            return self._start(cmd, lambda: self._probe(target, timeout))
        if cmd.kind == "input":
            if not self.allow_input:
                raise Fault("unsupported", "this machine does not allow mouse and keyboard input")
            if page:
                tool = "page"
            else:
                tool = ("xdotool" if self.which("xdotool") else None) if desk_env else self._input_tool()
            if not tool:
                raise Fault("unsupported", "no input tool on this machine (xdotool on X11, cliclick on macOS)")
            run, summary = self._input_op(tool, v, env=desk_env, desk=desk_name)
            return self._act(cmd, level, self._driving(run), summary, threaded=True)
        if cmd.kind == "browser":
            run, summary, acting = self._browser_op(v, level, env=desk_env, desk=desk_name)
            if acting:
                return self._act(cmd, level, run, summary, threaded=True)
            return self._start(cmd, run)
        if cmd.kind == "exec":
            if not self.allow_exec:
                raise Fault("unsupported", "this machine does not allow running programs")
            run, summary = self._exec_op(cmd, v, level)
            return self._act(cmd, level, run, summary, threaded=True)
        raise Fault("unsupported", "kind %s not supported" % cmd.kind)

    # ---- the private desk --------------------------------------------------------------------------------------------
    def _desk_env(self, v):
        """None for the owner's own screen (desk main, the default); a desk's environment for private or desk-<name>."""
        where = v.get("desk", "main")
        if where == "main":
            return None
        Desks.check(where)
        if not self.allow_desk:
            raise Fault("unsupported", "this machine does not allow a private desk (allow_desk is off)")
        return self.desks.env(where)

    def _desk_op(self, cmd, v, level):
        action = v.get("action")
        name = v.get("desk", "private")
        if action == "list":
            return self.desks.list()
        Desks.check(name)
        if action == "status":
            return self.desks.status(name)
        if action not in ("start", "stop"):
            raise Fault("bad_value", "action is start, stop, status or list")
        self.desk_used[name] = self.clock()
        if not self.allow_desk:
            raise Fault("unsupported", "this machine does not offer desks (allow_desk is off)")
        what = "a private desk" if name == "private" else "desk %s" % name
        if action == "start":
            kind = v.get("kind", "desktop")
            if kind not in ("desktop", "browser"):
                raise Fault("bad_value", "kind is desktop or browser")
            kinds = self._desk_kinds()
            if kind not in kinds:
                if kind == "browser":
                    raise Fault("unsupported", "a browser desk needs Chromium, Chrome, Edge or Brave on this machine")
                raise Fault("unsupported", "a whole desk needs Linux with Xvfb%s" % (": this machine runs browser desks (kind browser)" if kinds else ""))
            width = _int(v.get("width"), "width", 640, 3840, 1280)
            height = _int(v.get("height"), "height", 480, 2160, 800)
            profile = v.get("profile")
            if profile is not None and (not isinstance(profile, str) or not PROFILE_NAME.match(profile)):
                raise Fault("bad_value", "profile is a name: lower-case letters, digits and dashes, at most 24 characters")
            if profile is not None and name == "private":
                raise Fault("bad_value", "the private desk keeps its own profile: name a desk desk-<name> to start one from a kept profile")
            if kind == "browser":
                what = "a browser desk" if name == "private" else "browser desk %s" % name

            def start():
                st = self.desks.start(name, width, height, kind)
                if profile and self.desk_profiles.get(name) != profile:
                    self.desk_profiles[name] = profile
                    self.desk_restored.discard(name)
                return dict(st, profile=profile) if profile else st
            return self._act(cmd, level, start,
                             "start %s (%d×%d) where your agent works without touching your screen%s"
                             % (what, width, height, ", in your kept browser profile %s" % profile if profile else ""),
                             threaded=True)

        def stop():
            saved = self._end_desk_browser(name)
            out = self.desks.stop(name)
            return dict(out, profile_saved=saved) if saved is not None else out
        return self._act(cmd, level, stop, "stop %s (and close what runs on it)" % what, threaded=False)

    def _desk_browser(self, desk, binary=None, env=None):
        """The agent's browser on a desk, started on first use: shown on a whole desk's display, headless on a browser
        desk (its window the desk's size: its page is the desk's screen). Its own profile, downloads and temp folder."""
        with self.browser_mu:
            b = self.desk_browsers.get(desk)
            if b is not None and b.alive():
                return b
            binary = binary or self._browser_binary()
            if not binary:
                raise Fault("unsupported", "no browser on this machine (Chromium, Chrome, Edge or Brave), or this machine does not allow it")
            env = env or self.desks.env(desk)
            page = self.desks.kind(desk) == "browser"
            profile = self._desk_profile_dir(desk)
            self._restore_profile(desk)
            b = self.desk_browsers[desk] = self.browser_factory(binary, profile, page, env=env)
            b.download_dir = self.desks.dir(desk, "downloads")
            if page:
                st = self.desks.status(desk)
                b.window = b.page_size = (st.get("width") or 1280, st.get("height") or 800)
            return b

    def _desk_lock(self, desk):
        with self.browser_mu:
            return self.browser_locks.setdefault(desk, threading.Lock())

    def _on_page(self, desk, act):
        """act(browser) on a browser desk's page, one action at a time on that desk; a browser error is a fault."""
        b = self._desk_browser(desk)
        try:
            with self._desk_lock(desk):
                b.page()
                return act(b)
        except BrowserError as e:
            raise Fault(e.code, e.message)

    def _page_shot(self, desk, width):
        """A picture of a browser desk's page (not under its action lock: a look never waits for a click)."""
        b = self._desk_browser(desk)
        try:
            b.page()
            return b.screenshot(width)
        except BrowserError as e:
            raise Fault(e.code, e.message)

    def _desk_profile_dir(self, desk):
        return self.browser_profile + ("-desk" if desk == "private" else "-" + desk)

    def _kept_profile_dir(self, profile):
        return self.browser_profile + "-kept-" + profile

    def _profile_lock(self, profile):
        with self.mu:
            return self.profile_locks.setdefault(profile, threading.Lock())

    def _restore_profile(self, desk):
        """Before a desk's browser first starts in this run of the desk: its profile is a fresh copy of the kept one."""
        profile = self.desk_profiles.get(desk)
        if not profile or desk in self.desk_restored:
            return
        self.desk_restored.add(desk)
        own, kept = self._desk_profile_dir(desk), self._kept_profile_dir(profile)
        shutil.rmtree(own, ignore_errors=True)
        with self._profile_lock(profile):
            if os.path.isdir(kept):
                shutil.copytree(kept, own, symlinks=True, ignore=PROFILE_SKIP)
                os.chmod(own, 0o700)

    def _end_desk_browser(self, desk):
        """Close a desk's browser so it writes everything down, save its profile back when it started from a kept one
        (the last desk to stop wins), and remove the desk's own copy. → True when saved, False when not (too large or
        a failed copy), None for a desk without a kept profile."""
        b = self.desk_browsers.pop(desk, None)
        if b is not None:
            b.close(graceful=True)
        profile = self.desk_profiles.pop(desk, None)
        used = desk in self.desk_restored
        self.desk_restored.discard(desk)
        if not profile:
            return None
        own, kept = self._desk_profile_dir(desk), self._kept_profile_dir(profile)
        try:
            if not used or not os.path.isdir(own):
                return False
            size = _tree_size(own, PROFILE_SKIP)
            if size > self.desk_profile_max_mb * 1024 * 1024:
                self.emit_event("desk_profile_too_big", desk=desk, profile=profile, mb=size // (1024 * 1024))
                return False
            with self._profile_lock(profile):
                new, old = kept + ".new", kept + ".old"
                shutil.rmtree(new, ignore_errors=True)
                shutil.rmtree(old, ignore_errors=True)
                shutil.copytree(own, new, symlinks=True, ignore=PROFILE_SKIP)
                os.chmod(new, 0o700)
                if os.path.isdir(kept):
                    os.rename(kept, old)
                os.rename(new, kept)
                shutil.rmtree(old, ignore_errors=True)
            return True
        except OSError as e:
            self.log("desk %s: its profile %s was not saved: %s", desk, profile, e)
            return False
        finally:
            shutil.rmtree(own, ignore_errors=True)   # a copy for one run of the desk

    def _drop_desk_profiles(self):
        """The kill switch: nothing is saved, and the desks' own copies go."""
        for desk in list(self.desk_profiles):
            self.desk_profiles.pop(desk, None)
            self.desk_restored.discard(desk)
            shutil.rmtree(self._desk_profile_dir(desk), ignore_errors=True)

    def _close_desk_browser(self, name):
        b = self.desk_browsers.pop(name, None)
        if b is not None:
            b.close()

    def _close_desk_browsers(self):
        for name in list(self.desk_browsers):
            self._close_desk_browser(name)

    def _x11_tool(self):
        """An X11 capture tool for the private desk (it is always X11, whatever the owner's session is)."""
        if self.which("import"):
            return ("import", lambda out: ["import", "-window", "root", out])
        if self.which("scrot"):
            return ("scrot", lambda out: ["scrot", "-o", out])
        return None

    def _act(self, cmd, level, run, summary, threaded):
        if RANK[level] < RANK["ask"]:
            stopped = self.runtime is not None and self.runtime.estopped
            raise Fault("local_stop" if stopped else "not_allowed",
                        "stopped on the machine: observe only" if stopped else
                        "this machine allows observing only (its owner sets the level on the machine)")
        if level == "ask":
            with self.mu:
                if len(self.pending) >= MAX_WAITING:
                    raise Fault("not_ready", "%d requests already wait for approval on the machine" % MAX_WAITING)
                self.pending[cmd.id] = ((run, threaded), self.clock() + self.approval_timeout_s, summary)
            self.emit_event("approval", id=cmd.id, kind=cmd.kind, summary=summary, expires_s=self.approval_timeout_s)
            self._notify(cmd.id, summary)
            self.log("waiting for approval %s: %s", cmd.id, summary)
            raise Deferred()
        self.log("%s %s: %s", cmd.kind, cmd.id, summary)
        return self._start(cmd, run) if threaded else run()

    def _notify(self, cid, summary):
        """Tell the desktop that a request waits (config `notify`, on by default) when the Node runs in a graphical
        session and the Node's tray does not show it already (`openvibe-node tray`). On Linux the notification has
        Approve and Deny buttons when notify-send can show them (libnotify 0.7.10+); on macOS a dialog asks, with Deny
        as its default button. Best effort: it never blocks or fails a command."""
        if not self.notify or self.clock() < self.tray_until:
            return
        title = "OpenVibe: your agent asks"
        hint = "openvibe-node approve %s  (or deny %s)" % (cid, cid)
        parse = None
        if self.platform.startswith("linux") and (self.env.get("DISPLAY") or self.env.get("WAYLAND_DISPLAY")) and self.which("notify-send"):
            if self._notify_can_act():
                argv = ["notify-send", "--app-name=OpenVibe Node", "--urgency=critical", "--wait",
                        "--expire-time=%d" % (self.approval_timeout_s * 1000), "--action=approve=Approve",
                        "--action=deny=Deny", "--", title, summary[:200]]
                parse = _parse_notify_send
            else:
                argv = ["notify-send", "--app-name=OpenVibe Node", title, "%s\n%s" % (summary[:200], hint)]
        elif self.platform == "darwin" and self.which("osascript"):
            quote = lambda s: '"%s"' % s.replace("\\", "\\\\").replace('"', '\\"')
            argv = ["osascript", "-e", 'display dialog %s with title %s buttons {"Deny", "Approve"} default button "Deny" '
                    "giving up after %d with icon caution" % (quote(summary[:400]), quote(title), self.approval_timeout_s)]
            parse = _parse_dialog
        else:
            return
        try:
            if parse is None:
                self.popen(argv, stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, start_new_session=True)
                return
            proc = self.popen(argv, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
                              start_new_session=True, text=True)
        except OSError:
            return
        with self.mu:
            old = self.prompts.pop(cid, None)
            self.prompts[cid] = proc
        if old is not None:
            self._end_prompt(old)
        threading.Thread(target=self._await_prompt, args=(cid, proc, parse), name="computer-ask-%s" % cid, daemon=True).start()

    def _notify_can_act(self):
        """Whether this notify-send has --action and --wait (libnotify 0.7.10+), asked once."""
        if self.notify_actions is None:
            try:
                p = self.popen(["notify-send", "--help"], stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                               stderr=subprocess.STDOUT, text=True)
                out = p.communicate(timeout=3)[0] or ""
                self.notify_actions = "--action" in out and "--wait" in out
            except (OSError, subprocess.SubprocessError, AttributeError):
                self.notify_actions = False
        return self.notify_actions

    def _await_prompt(self, cid, proc, parse):
        try:
            out = proc.communicate()[0] or ""
        except (OSError, ValueError, AttributeError):
            out = ""
        with self.mu:
            if self.prompts.get(cid) is proc:
                del self.prompts[cid]
        answer = parse(out)
        if answer is not None:
            self._desktop_answer(cid, answer)

    def _desktop_answer(self, cid, approved):
        """The owner pressed Approve or Deny on the desktop. An approval that arrives while the agent drives the mouse or
        keyboard (or just after) may be the agent's own click: it is not taken, and the request is shown again."""
        with self.mu:
            item = self.pending.get(cid)
            driving = self.input_busy > 0 or (self.input_ended is not None and self.clock() - self.input_ended < INPUT_GUARD_S)
        if item is None:
            return
        if approved and driving:
            self.log("an approval of %s came from the desktop while the agent drove the mouse or keyboard: asking again", cid)
            self._notify(cid, item[2])
            return
        with (self.runtime.lock if self.runtime is not None else contextlib.nullcontext()):
            self.on_approval(cid, approved)

    def _close_prompt(self, cid):
        """Take down the notification or dialog of a request answered elsewhere, expired or stopped."""
        with self.mu:
            proc = self.prompts.pop(cid, None)
        if proc is not None:
            self._end_prompt(proc)

    def _end_prompt(self, proc):
        try:
            if proc.poll() is None:
                proc.send_signal(signal.SIGINT)  # notify-send --wait closes its notification on SIGINT
        except (OSError, AttributeError):
            pass

    def _driving(self, run):
        """An input command, counted while it runs so desktop approvals wait it out (INPUT_GUARD_S)."""
        def go():
            with self.mu:
                self.input_busy += 1
            try:
                return run()
            finally:
                with self.mu:
                    self.input_busy -= 1
                    self.input_ended = self.clock()
        return go

    def on_tray(self, attached, ttl_s):
        try:
            ttl = max(1.0, min(float(ttl_s or 15), 60.0))
        except (TypeError, ValueError):
            ttl = 15.0
        self.tray_until = self.clock() + ttl if attached else 0.0

    def on_approval(self, cid, approved):
        with self.mu:
            item = self.pending.pop(cid, None)
        if item is None:
            return
        self._close_prompt(cid)
        (run, threaded), _, summary = item
        self.emit_event("approval_done", id=cid, approved=bool(approved))
        if not approved:
            self.log("denied %s: %s", cid, summary)
            self.fail(cid, "not_allowed", "the owner denied it on the machine")
            return
        if self.effective_level() == "observe":
            self.fail(cid, "local_stop", "stopped on the machine before it ran")
            return
        self.log("approved %s: %s", cid, summary)
        if threaded:
            self._thread(cid, run)
            return
        try:
            self.complete(cid, run())
        except Fault as f:
            self.fail(cid, f.code, f.message)
        except Exception as e:
            self.fail(cid, "hardware", str(e))

    def _start(self, cmd, run):
        with self.mu:
            if self.running >= MAX_RUNNING:
                raise Fault("not_ready", "%d commands are already running on this machine" % MAX_RUNNING)
        self._thread(cmd.id, run)
        raise Deferred()

    def _thread(self, cid, run):
        with self.mu:
            self.running += 1

        def work():
            try:
                self.complete(cid, run())
            except Fault as f:
                self.fail(cid, f.code, f.message)
            except Exception as e:
                self.fail(cid, "hardware", str(e))
            finally:
                with self.mu:
                    self.running -= 1

        threading.Thread(target=work, name="computer-%s" % cid, daemon=True).start()

    # ---- exec ------------------------------------------------------------------------------------------------------
    def _exec_op(self, cmd, v, level):
        argv = v.get("argv")
        if not isinstance(argv, list) or not argv or len(argv) > MAX_ARGS or \
                not all(isinstance(a, str) and len(a) <= MAX_ARG_LEN and "\x00" not in a for a in argv) or not argv[0]:
            raise Fault("bad_value", "argv is a list of 1 to %d strings: a program and its arguments" % MAX_ARGS)
        cwd = self._path(v["cwd"], level) if v.get("cwd") is not None else (self.roots[0] if self.roots else None)
        if cwd is not None and not os.path.isdir(cwd):
            raise Fault("bad_value", "cwd %s is not a folder" % cwd)
        stdin = v.get("stdin")
        if stdin is not None and (not isinstance(stdin, str) or len(stdin.encode("utf-8")) > MAX_STDIN):
            raise Fault("bad_value", "stdin is a string of at most 64 KB")
        timeout = _int(v.get("timeout_s"), "timeout_s", 1, self.timeout_s, self.timeout_s)
        deadline = cmd.deadline

        def run():
            left = deadline - self.clock() - 1.0
            if left < 1:
                raise Fault("expired", "no time was left to run it")
            return self._exec(argv, cwd, stdin, min(timeout, left))

        return run, "run %s in %s" % (shlex.join(argv)[:300], cwd)

    def _exec(self, argv, cwd, stdin, timeout):
        started = self.clock()
        try:
            proc = self.popen(argv, cwd=cwd, stdin=subprocess.PIPE if stdin is not None else subprocess.DEVNULL,
                              stdout=subprocess.PIPE, stderr=subprocess.PIPE, start_new_session=True)
        except FileNotFoundError:
            raise Fault("bad_value", "no program %s on this machine" % argv[0])
        except PermissionError:
            raise Fault("not_allowed", "%s may not be run by the Node's account" % argv[0])
        with self.mu:
            self.procs.add(proc)
        cap = self.max_output
        got = {"out": [b"", 0], "err": [b"", 0]}

        def drain(stream, key):
            buf = got[key]
            while True:
                chunk = stream.read(65536)
                if not chunk:
                    return
                buf[1] += len(chunk)
                if len(buf[0]) < cap:
                    buf[0] += chunk[:cap - len(buf[0])]

        readers = [threading.Thread(target=drain, args=(proc.stdout, "out"), daemon=True),
                   threading.Thread(target=drain, args=(proc.stderr, "err"), daemon=True)]
        for t in readers:
            t.start()
        if stdin is not None:
            try:
                proc.stdin.write(stdin.encode("utf-8"))
                proc.stdin.close()
            except (BrokenPipeError, OSError):
                pass
        timed_out = False
        try:
            code = proc.wait(timeout=timeout)
        except subprocess.TimeoutExpired:
            timed_out = True
            self._kill(proc)
            code = proc.wait()
        finally:
            with self.mu:
                self.procs.discard(proc)
        for t in readers:
            t.join(2)
        for stream in (proc.stdout, proc.stderr, proc.stdin):
            if stream is not None:
                try:
                    stream.close()
                except OSError:
                    pass
        text = lambda b: b.decode("utf-8", "replace")
        return {"exit_code": code, "stdout": text(got["out"][0]), "stderr": text(got["err"][0]),
                "truncated": got["out"][1] > cap or got["err"][1] > cap, "timed_out": timed_out,
                "duration_ms": int((self.clock() - started) * 1000)}

    # ---- probe (the node carrier of OpenVibe.Watch) -----------------------------------------------------------------
    @staticmethod
    def _lan(addr):
        ip = ipaddress.ip_address(addr)
        if isinstance(ip, ipaddress.IPv6Address) and ip.ipv4_mapped:
            ip = ip.ipv4_mapped
        return ip.is_private or ip.is_loopback or ip.is_link_local

    def _probe_target(self, v):
        raw = v.get("target")
        if not isinstance(raw, str) or len(raw) > 2048:
            raise Fault("bad_value", "target is http(s)://host/path or tcp://host:port")
        u = urllib.parse.urlsplit(raw)
        if u.scheme not in ("http", "https", "tcp") or not u.hostname or u.username or u.password:
            raise Fault("bad_value", "target is http(s)://host/path or tcp://host:port, with no credentials")
        try:
            port = u.port or {"http": 80, "https": 443}.get(u.scheme)
        except ValueError:
            raise Fault("bad_value", "the port is not a number")
        if not port:
            raise Fault("bad_value", "a tcp target names its port")
        timeout = _int(v.get("timeout_s"), "timeout_s", 1, 30, 10)
        return {"scheme": u.scheme, "host": u.hostname, "port": port, "path": (u.path or "/") + ("?" + u.query if u.query else ""), "raw": raw}, timeout

    def _resolve_lan(self, host, port):
        """The first address of host, when every address it resolves to is on this machine's own network."""
        try:
            infos = socket.getaddrinfo(host, port, type=socket.SOCK_STREAM)
        except socket.gaierror as e:
            raise Fault("bad_value", "%s does not resolve: %s" % (host, e))
        addrs = [i[4][0] for i in infos]
        if not addrs or not all(self._lan(a) for a in addrs):
            raise Fault("not_allowed", "a probe reaches this machine's own network only (private, loopback or link-local addresses); %s is not" % host)
        return infos[0][0], addrs[0]

    def _probe(self, t, timeout):
        family, addr = self._resolve_lan(t["host"], t["port"])
        started = self.clock()
        ms = lambda: int((self.clock() - started) * 1000)
        if t["scheme"] == "tcp":
            try:
                with socket.socket(family, socket.SOCK_STREAM) as s:
                    s.settimeout(timeout)
                    s.connect((addr, t["port"]))
                return {"open": True, "ms": ms()}
            except OSError as e:
                return {"open": False, "ms": ms(), "error": str(e)[:200]}
        raw = socket.create_connection((addr, t["port"]), timeout=timeout)
        try:
            if t["scheme"] == "https":
                raw = ssl.create_default_context().wrap_socket(raw, server_hostname=t["host"])
            conn = http.client.HTTPConnection(t["host"], t["port"], timeout=timeout)
            conn.sock = raw
            conn.request("GET", t["path"], headers={"User-Agent": "OpenVibe-Node-probe/1", "Accept": "*/*", "Connection": "close"})
            res = conn.getresponse()
            cap = min(self.max_output, 64 * 1024)
            body = res.read(cap + 1)
            return {"status": res.status, "content_type": res.getheader("Content-Type"), "etag": res.getheader("ETag"),
                    "last_modified": res.getheader("Last-Modified"), "location": res.getheader("Location"),
                    "body": body[:cap].decode("utf-8", "replace"), "truncated": len(body) > cap, "ms": ms()}
        except ssl.SSLError as e:
            raise Fault("hardware", "TLS failed: %s" % str(e)[:200])
        except (OSError, http.client.HTTPException) as e:
            raise Fault("hardware", "the probe failed: %s" % str(e)[:200])
        finally:
            try:
                raw.close()
            except OSError:
                pass

    # ---- files -----------------------------------------------------------------------------------------------------
    @staticmethod
    def _kind(mode):
        if stat.S_ISDIR(mode):
            return "dir"
        if stat.S_ISREG(mode):
            return "file"
        if stat.S_ISLNK(mode):
            return "link"
        return "other"

    def _fs_read_op(self, op, v, level):
        path = self._path(v.get("path"), level)
        try:
            st = os.stat(path)
        except FileNotFoundError:
            raise Fault("bad_value", "%s does not exist" % path)
        except PermissionError:
            raise Fault("not_allowed", "the Node's account may not read %s" % path)
        if op == "stat":
            return {"path": path, "type": self._kind(st.st_mode), "size": st.st_size, "mtime": int(st.st_mtime),
                    "mode": oct(stat.S_IMODE(st.st_mode))}
        if op == "list":
            if not stat.S_ISDIR(st.st_mode):
                raise Fault("bad_value", "%s is not a folder" % path)
            entries, total = [], 0
            try:
                with os.scandir(path) as it:
                    for e in sorted(it, key=lambda e: e.name):
                        total += 1
                        if len(entries) >= MAX_LIST:
                            continue
                        try:
                            s = e.stat(follow_symlinks=False)
                            entries.append({"name": e.name, "type": self._kind(s.st_mode), "size": s.st_size,
                                            "mtime": int(s.st_mtime)})
                        except OSError:
                            entries.append({"name": e.name, "type": "other"})
            except PermissionError:
                raise Fault("not_allowed", "the Node's account may not list %s" % path)
            return {"path": path, "entries": entries, "total": total, "truncated": total > len(entries)}
        if not stat.S_ISREG(st.st_mode):
            raise Fault("bad_value", "%s is not a file" % path)
        offset = _int(v.get("offset"), "offset", 0, 2 ** 53, 0)
        length = _int(v.get("length"), "length", 0, self.max_read, self.max_read)
        try:
            with open(path, "rb") as f:
                f.seek(offset)
                data = f.read(length)
        except PermissionError:
            raise Fault("not_allowed", "the Node's account may not read %s" % path)
        return {"path": path, "size": st.st_size, "offset": offset, "length": len(data),
                "truncated": offset + len(data) < st.st_size, "content_b64": base64.b64encode(data).decode("ascii")}

    def _fs_write_op(self, op, v, level):
        path = self._path(v.get("path"), level, create=True)
        if op == "mkdir":
            def run():
                existed = os.path.isdir(path)
                try:
                    os.makedirs(path, exist_ok=True)
                except FileExistsError:
                    raise Fault("bad_value", "%s exists and is not a folder" % path)
                except PermissionError:
                    raise Fault("not_allowed", "the Node's account may not create %s" % path)
                return {"path": path, "created": not existed}
            return run, "make the folder %s" % path
        mode = v.get("mode") or "create"
        if mode not in ("create", "overwrite", "append"):
            raise Fault("bad_value", "mode is create, overwrite or append")
        raw = v.get("content_b64")
        if not isinstance(raw, str):
            raise Fault("bad_value", "content_b64 is base64 text")
        try:
            data = base64.b64decode(raw, validate=True)
        except (ValueError, TypeError):
            raise Fault("bad_value", "content_b64 is not base64")
        if len(data) > self.max_read:
            raise Fault("bad_value", "at most %d bytes per write" % self.max_read)

        def run():
            if os.path.isdir(path):
                raise Fault("bad_value", "%s is a folder" % path)
            if mode == "create" and os.path.lexists(path):
                raise Fault("bad_value", "%s exists (mode overwrite or append replaces or extends it)" % path)
            try:
                if mode == "append":
                    with open(path, "ab") as f:
                        f.write(data)
                else:
                    fd, tmp = tempfile.mkstemp(prefix=".openvibe-", dir=os.path.dirname(path))
                    try:
                        with os.fdopen(fd, "wb") as f:
                            f.write(data)
                        if os.path.exists(path):
                            shutil.copymode(path, tmp)
                        os.replace(tmp, path)
                    except BaseException:
                        if os.path.exists(tmp):
                            os.unlink(tmp)
                        raise
            except PermissionError:
                raise Fault("not_allowed", "the Node's account may not write %s" % path)
            return {"path": path, "written": len(data), "mode": mode}

        verb = {"create": "create", "overwrite": "overwrite", "append": "append to"}[mode]
        return run, "%s %s (%d bytes)" % (verb, path, len(data))

    # ---- input -----------------------------------------------------------------------------------------------------
    # ---- browser ---------------------------------------------------------------------------------------------------
    def _browser_binary(self):
        if not self.allow_browser or self.platform.startswith("win"):
            return None
        return find_binary(self.which, self.platform, self.browser_path)

    def _close_browser(self):
        # Never waits for an action in flight: closing kills the process, and the action fails at once.
        b, self.browser = self.browser, None
        if b is not None:
            b.close()

    def _local_host(self, host):
        """Does `host` name (or resolve to) an address on the machine's own network?"""
        try:
            addrs = [host] if self._is_ip(host) else self.resolve(host)
        except (OSError, UnicodeError):
            raise Fault("bad_value", "%s does not resolve" % host)
        return any(self._lan(a) for a in addrs) or not addrs

    @staticmethod
    def _is_ip(host):
        try:
            ipaddress.ip_address(host)
            return True
        except ValueError:
            return False

    def _browser_url(self, raw, level):
        if not isinstance(raw, str) or not raw or len(raw) > MAX_URL or any(c in raw for c in "\x00\r\n"):
            raise Fault("bad_value", "url is an http or https address")
        u = urllib.parse.urlsplit(raw)
        if u.scheme not in ("http", "https") or not u.hostname:
            raise Fault("bad_value", "url is an http or https address")
        if level != "full" and self._local_host(u.hostname.strip("[]")):
            raise Fault("not_allowed", "%s is on this machine's own network: only at level full" % u.hostname)
        return raw

    def _browser_op(self, v, level, env=None, desk=None):
        """(run, summary, acting) for one browser action; nothing is opened or clicked before run()."""
        binary = self._browser_binary()
        if not binary:
            raise Fault("unsupported", "no browser on this machine (Chromium, Chrome, Edge or Brave), or this machine does not allow it")
        action = v.get("action")
        if action not in BROWSER_READS and action not in BROWSER_ACTS:
            raise Fault("bad_value", "action is one of %s" % ", ".join(sorted(BROWSER_READS | BROWSER_ACTS)))
        selector, text, key, url = v.get("selector"), v.get("text"), v.get("key"), None
        if action == "open":
            url = self._browser_url(v.get("url"), level)
        if action in ("click", "fill") and selector is not None and (not isinstance(selector, str) or not selector or len(selector) > MAX_SELECTOR):
            raise Fault("bad_value", "selector is a CSS selector of at most %d characters" % MAX_SELECTOR)
        if action == "click" and selector is None and (not isinstance(text, str) or not text.strip() or len(text) > 200):
            raise Fault("bad_value", "click takes a selector, or the text of a button or link (at most 200 characters)")
        if action == "fill":
            if selector is None:
                raise Fault("bad_value", "fill takes the selector of the field")
            if not isinstance(text, str) or len(text) > MAX_TYPE or "\x00" in text:
                raise Fault("bad_value", "text is at most %d characters" % MAX_TYPE)
        if action == "press" and key not in KEY_CODES:
            raise Fault("bad_value", "key is one of %s" % ", ".join(sorted(KEY_CODES)))
        width = _int(v.get("max_width"), "max_width", 320, 2560, 1280)

        lock_key = desk if env else "main"

        def browser():
            if env:   # on a desk: its own browser (shown on a whole desk, headless on a browser desk)
                return self._desk_browser(desk, binary, env)
            with self.browser_mu:
                if self.browser is None or not self.browser.alive():
                    self.browser = self.browser_factory(binary, self.browser_profile, self.browser_headless)
                return self.browser

        def lock():
            with self.browser_mu:
                return self.browser_locks.setdefault(lock_key, threading.Lock())

        def run():
            if action == "close":
                self._close_desk_browser(desk) if env else self._close_browser()
                return {"done": True}
            b = browser()
            try:
                with lock():   # one action at a time in each browser; two desks' browsers act in parallel
                    if action == "open":
                        return b.open(url)
                    if action == "read":
                        return b.read()
                    if action == "screenshot":
                        return b.screenshot(width)
                    if action == "click":
                        return b.click(selector=selector, text=None if selector else text)
                    if action == "fill":
                        return b.fill(selector, text)
                    if action == "press":
                        return b.press(key)
                    return b.back()
            except BrowserError as e:
                raise Fault(e.code, e.message)

        if action == "open":
            summary = "open %s in the browser" % url[:300]
        elif action == "click":
            summary = "click %s in the browser" % (selector if selector else "the %r button or link" % text[:120])
        elif action == "fill":
            shown = text if len(text) <= 120 else text[:120] + "…"
            summary = "type %d characters into %s in the browser: %r" % (len(text), selector[:200], shown)
        elif action == "press":
            summary = "press %s in the browser" % key
        elif action == "back":
            summary = "go back in the browser"
        else:
            summary = "%s the browser" % action
        return run, summary, action in BROWSER_ACTS

    def _input_tool(self):
        """xdotool on an X11 session, cliclick on macOS, else None (Wayland has no general input tool yet)."""
        if self.platform == "darwin" and self.which("cliclick"):
            return "cliclick"
        if self.platform.startswith("linux") and self.env.get("DISPLAY") and not self.env.get("WAYLAND_DISPLAY") and self.which("xdotool"):
            return "xdotool"
        return None

    def _input_op(self, tool, v, env=None, desk=None):
        """(run, summary) for one input action; nothing touches the screen until run() (after approval at ask). With env,
        on a desk (its display; points scale to the desk's own size). tool "page": a browser desk's page, through the
        browser's pipe (points scale to the page's size)."""
        action = v.get("action")
        if action not in INPUT_ACTIONS:
            raise Fault("bad_value", "action is one of %s" % ", ".join(sorted(INPUT_ACTIONS)))
        point = None
        if action in POINT_ACTIONS or (action == "scroll" and v.get("x") is not None):
            x = _int(v.get("x"), "x", 0, 16384, None)
            y = _int(v.get("y"), "y", 0, 16384, None)
            if x is None or y is None:
                raise Fault("bad_value", "%s needs x and y" % action)
            w = _int(v.get("width"), "width", 1, 16384, None)
            h = _int(v.get("height"), "height", 1, 16384, None)
            if (w is None) != (h is None):
                raise Fault("bad_value", "width and height come together")
            point = (x, y, w, h)
        if action == "scroll":
            dy = _int(v.get("dy"), "dy", -20, 20, None)
            if not dy:
                raise Fault("bad_value", "scroll needs dy (clicks, -20 to 20, not 0)")
            if tool not in ("xdotool", "page"):
                raise Fault("unsupported", "scrolling needs xdotool (Linux X11)")
        if action == "type":
            text = v.get("text")
            if not isinstance(text, str) or not text or len(text) > MAX_TYPE or "\x00" in text:
                raise Fault("bad_value", "text is 1 to %d characters" % MAX_TYPE)
        if action == "key":
            keys = v.get("keys")
            if not isinstance(keys, str) or not keys or len(keys) > 64 or not set(keys) <= KEYS_OK or keys.startswith("-"):
                raise Fault("bad_value", "keys is one key or chord, for example Return or ctrl+s")

        def run():
            if tool == "page":
                return self._on_page(desk, lambda b: self._page_input(b, action, point, v))
            at = (self._desk_scale(point, desk) if env else self._scale(tool, point)) if point else None
            argv = self._input_argv(tool, action, at, v)
            proc = self.popen(argv, stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, start_new_session=True,
                              **({"env": env} if env else {}))
            try:
                _, err = proc.communicate(timeout=30)
            except subprocess.TimeoutExpired:
                self._kill(proc)
                raise Fault("hardware", "%s did not finish in 30 s" % tool)
            if proc.returncode != 0:
                raise Fault("hardware", "%s failed: %s" % (tool, (err or b"").decode("utf-8", "replace")[:200]))
            out = {"done": True, "action": action}
            if at:
                out["x"], out["y"] = at
            return out

        where = (" on the private desk" if desk == "private" else " on desk %s" % desk) if env else ""
        if action == "type":
            text = v["text"]
            shown = text if len(text) <= 120 else text[:120] + "…"
            summary = "type %d characters%s: %r" % (len(text), where, shown)
        elif action == "key":
            summary = "press %s%s" % (v["keys"], where)
        elif action == "scroll":
            summary = "scroll %s %d%s" % ("up" if v["dy"] < 0 else "down", abs(v["dy"]), where)
        else:
            x, y, w, h = point
            summary = "%s at %d, %d%s%s" % (action.replace("_", " "), x, y, " of a %d×%d picture of the screen" % (w, h) if w else "", where)
        return run, summary

    def _desk_scale(self, point, desk="private"):
        """A point in a picture of the desk, on the desk's own display (the desk the command names, at its size)."""
        x, y, w, h = point
        st = self.desks.status(desk or "private")
        if not w or not st.get("running") or not st.get("width"):
            return x, y
        return min(st["width"] - 1, round(x * st["width"] / w)), min(st["height"] - 1, round(y * st["height"] / h))

    @staticmethod
    def _page_input(b, action, point, v):
        """One input action on a browser desk's page. A point in a picture of the page scales to the page's own size;
        a scroll without a point is at the page's middle."""
        vw, vh = b.viewport()
        if point:
            x, y, w, h = point
            if w:
                x, y = round(x * vw / w), round(y * vh / h)
            x, y = min(max(0, x), vw - 1), min(max(0, y), vh - 1)
        else:
            x, y = vw // 2, vh // 2
        if action in POINT_ACTIONS:
            out = b.mouse(action, x, y)
        elif action == "scroll":
            out = b.wheel(x, y, v["dy"])
        elif action == "type":
            out = b.insert(v["text"])
        else:
            out = b.keys(v["keys"])
        out = dict(out, action=action)
        if point:
            out["x"], out["y"] = x, y
        return out

    def _screen_size(self, tool):
        """The screen's size in the units the input tool uses: pixels for xdotool, points for cliclick (a Retina
        screen's picture is twice as many pixels)."""
        if tool == "xdotool":
            out = subprocess.run(["xdotool", "getdisplaygeometry"], capture_output=True, text=True, timeout=10, env=self._tool_env())
            parts = out.stdout.split()
        else:
            out = subprocess.run(["osascript", "-e", "tell application \"Finder\" to get bounds of window of desktop"],
                                 capture_output=True, text=True, timeout=10)
            parts = [p.strip() for p in out.stdout.split(",")][2:4]
        if out.returncode != 0 or len(parts) != 2 or not all(p.isdigit() for p in parts):
            raise Fault("hardware", "could not read the screen size")
        return int(parts[0]), int(parts[1])

    def _scale(self, tool, point):
        x, y, w, h = point
        if not w:
            return x, y
        sw, sh = self.screen_size(tool)
        return min(sw - 1, round(x * sw / w)), min(sh - 1, round(y * sh / h))

    def _tool_env(self):
        env = dict(os.environ)
        env.update({k: v for k, v in self.env.items() if k in ("DISPLAY", "XAUTHORITY")})
        return env

    @staticmethod
    def _input_argv(tool, action, at, v):
        if tool == "xdotool":
            move = ["mousemove", "--sync", str(at[0]), str(at[1])] if at else []
            if action == "move":
                return ["xdotool"] + move
            if action in ("click", "double_click", "right_click"):
                button = "3" if action == "right_click" else "1"
                repeat = ["--repeat", "2"] if action == "double_click" else []
                return ["xdotool"] + move + ["click"] + repeat + [button]
            if action == "scroll":
                return ["xdotool"] + move + ["click", "--repeat", str(abs(v["dy"])), "4" if v["dy"] < 0 else "5"]
            if action == "type":
                return ["xdotool", "type", "--delay", "12", "--", v["text"]]
            return ["xdotool", "key", "--", v["keys"]]
        # cliclick (macOS): c: click, dc: double, rc: right, m: move, t: type, kd/ku: modifier down/up, kp: a named key.
        if action in POINT_ACTIONS:
            verb = {"move": "m", "click": "c", "double_click": "dc", "right_click": "rc"}[action]
            return ["cliclick", "%s:%d,%d" % (verb, at[0], at[1])]
        if action == "type":
            return ["cliclick", "t:" + v["text"]]
        parts = v["keys"].split("+")
        mods = [MODIFIERS.get(p.lower()) for p in parts[:-1]]
        if None in mods:
            raise Fault("bad_value", "unknown modifier in %s" % v["keys"])
        key = parts[-1]
        if len(key) == 1:
            press = ["t:" + key]
        elif key.lower() in CLICLICK_KEYS:
            press = ["kp:" + CLICLICK_KEYS[key.lower()]]
        elif key.lower().startswith("f") and key[1:].isdigit() and 1 <= int(key[1:]) <= 16:
            press = ["kp:" + key.lower()]
        else:
            raise Fault("unsupported", "the key %s is not supported on macOS" % key)
        mods = ["cmd" if m == "super" else m for m in mods]
        return ["cliclick"] + ["kd:" + m for m in mods] + press + ["ku:" + m for m in reversed(mods)]

    # ---- screen ----------------------------------------------------------------------------------------------------
    def _screen_tool(self):
        """(name, argv builder) for this machine's screen capture, or None."""
        if self.platform == "darwin" and self.which("screencapture"):
            return ("screencapture", lambda out: ["screencapture", "-x", "-t", "png", out])
        if self.platform.startswith("linux"):
            if self.env.get("WAYLAND_DISPLAY") and self.which("grim"):
                return ("grim", lambda out: ["grim", out])
            if self.env.get("DISPLAY"):
                if self.which("import"):
                    return ("import", lambda out: ["import", "-window", "root", out])
                if self.which("scrot"):
                    return ("scrot", lambda out: ["scrot", "-o", out])
                if self.which("gnome-screenshot"):
                    return ("gnome-screenshot", lambda out: ["gnome-screenshot", "-f", out])
        return None

    def _screen(self, tool, max_width, env=None):
        fd, out = tempfile.mkstemp(prefix="openvibe-screen-", suffix=".png")
        os.close(fd)
        try:
            proc = self.popen(tool[1](out), stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
                              stderr=subprocess.PIPE, start_new_session=True, **({"env": env} if env else {}))
            try:
                _, err = proc.communicate(timeout=15)
            except subprocess.TimeoutExpired:
                self._kill(proc)
                raise Fault("hardware", "the screen capture did not finish in 15 s")
            if proc.returncode != 0 or not os.path.getsize(out):
                raise Fault("hardware", "screen capture failed: %s" % (err or b"").decode("utf-8", "replace")[:200])
            return self._encode(out, max_width)
        finally:
            if os.path.exists(out):
                os.unlink(out)

    # ---- the live screen --------------------------------------------------------------------------------------------
    @staticmethod
    def _have_pillow():
        try:
            import PIL.Image  # noqa: F401
        except ImportError:
            return False
        return True

    @staticmethod
    def _native_jpeg(tool, path):
        """The capture tool writing JPEG itself (for machines without Pillow), or None when it only writes PNG."""
        name = tool[0]
        if name == "screencapture":
            return ["screencapture", "-x", "-t", "jpg", path]
        if name == "grim":
            return ["grim", "-t", "jpeg", "-q", str(STREAM_QUALITY), path]
        if name == "import":
            return ["import", "-silent", "-window", "root", "-quality", str(STREAM_QUALITY), path]
        if name == "scrot":
            return ["scrot", "-o", "-q", str(STREAM_QUALITY), path]
        return None

    def _stream_capable(self, tool):
        return self._have_pillow() or self._native_jpeg(tool, "x.jpg") is not None

    def _screen_stream(self, v, env=None, desk=None, page=False):
        action = v.get("action")
        if action == "stop":
            return {"streaming": False, "stopped": self._end_stream("stopped by its owner")}
        if action != "start":
            raise Fault("bad_value", "action is start or stop")
        if not self.allow_screen_stream:
            raise Fault("unsupported", "this machine does not show its screen live (allow_screen_stream is off)")
        if self.runtime is not None and self.runtime.estopped:
            raise Fault("local_stop", "stopped on the machine")
        tool = ("page", None) if page else (self._x11_tool() if env else self._screen_tool())
        if not tool:
            raise Fault("unsupported", "no screen capture tool on this machine")
        if not page and not self._stream_capable(tool):
            raise Fault("unsupported", "%s only writes PNG: install Pillow (pip install pillow) to show the screen live" % tool[0])
        seconds = _int(v.get("seconds"), "seconds", 30, 1800, 600)
        fps = _int(v.get("fps"), "fps", 1, 5, 2)
        self._end_stream("replaced")
        frames = tempfile.mkdtemp(prefix="openvibe-screen-stream-")  # 0700, this account only
        stream = {"sid": os.urandom(8).hex(), "until": self.clock() + seconds, "fps": fps, "tool": tool, "dir": frames,
                  "stop": threading.Event(), "env": env, "desk": desk}
        with self.stream_mu:
            self.stream = stream
        threading.Thread(target=self._stream_loop, args=(stream,), name="computer-screen-stream", daemon=True).start()
        if not env:   # the private desk is not the owner's screen: nothing of theirs goes live
            self._notify_stream(seconds)
        return {"streaming": True, "sid": stream["sid"], "seconds": seconds, "fps": fps, "tool": tool[0],
                "desk": (desk or "private") if env else "main"}

    def _end_stream(self, reason):
        """End the live screen, if one runs → whether one did."""
        with self.stream_mu:
            stream, self.stream = self.stream, None
        if stream is None:
            return False
        stream["stop"].set()
        stream["reason"] = reason
        return True

    def _stream_loop(self, stream):
        period = 1.0 / stream["fps"]
        n, failures, reason = 0, 0, "time is up"
        try:
            while not stream["stop"].is_set():
                started = self.clock()
                if started >= stream["until"]:
                    break
                path = os.path.join(stream["dir"], "frame-%d.jpg" % (n % 3))  # the core reads each one at once
                try:
                    self._stream_frame(stream["tool"], path, env=stream.get("env"), desk=stream.get("desk"))
                    n += 1
                    failures = 0
                    self.emit_video(path, stream="screen")
                except Fault as f:
                    failures += 1
                    if failures >= STREAM_MAX_FAILURES:
                        reason = "the screen capture failed: %s" % f.message
                        break
                stream["stop"].wait(max(0.0, period - (self.clock() - started)))
        finally:
            shutil.rmtree(stream["dir"], ignore_errors=True)
            with self.stream_mu:
                if self.stream is stream:
                    self.stream = None
            # The core ends the stream with the same id (a newer one that replaced it carries its own).
            self.emit_event("screen_stream_ended", sid=stream["sid"], reason=stream.get("reason") or reason, frames=n)

    def _stream_frame(self, tool, path, env=None, desk=None):
        """One JPEG frame of the screen at path, at most STREAM_WIDTH wide when Pillow can resize it. On a browser desk,
        a picture of its page from the browser itself (JPEG and scaled there: no capture tool, no Pillow)."""
        if tool[0] == "page":
            # Not under the desk's action lock: the live view keeps moving while an action waits for a page to load.
            try:
                self._desk_browser(desk).frame(path, STREAM_WIDTH, STREAM_QUALITY)
            except BrowserError as e:
                raise Fault(e.code, e.message)
            return
        if self._have_pillow():
            fd, png = tempfile.mkstemp(prefix="openvibe-screen-", suffix=".png")
            os.close(fd)
            try:
                self._capture(tool[1](png), env=env)
                from PIL import Image
                with Image.open(png) as im:
                    im = im.convert("RGB")
                    if im.width > STREAM_WIDTH:
                        im = im.resize((STREAM_WIDTH, max(1, round(im.height * STREAM_WIDTH / im.width))))
                    im.save(path + ".part", "JPEG", quality=STREAM_QUALITY)
                os.replace(path + ".part", path)
            finally:
                if os.path.exists(png):
                    os.unlink(png)
            return
        self._capture(self._native_jpeg(tool, path + ".part.jpg"), env=env)
        os.replace(path + ".part.jpg", path)

    def _capture(self, argv, env=None):
        proc = self.popen(argv, stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE,
                          start_new_session=True, **({"env": env} if env else {}))
        try:
            _, err = proc.communicate(timeout=10)
        except subprocess.TimeoutExpired:
            self._kill(proc)
            raise Fault("hardware", "the screen capture did not finish in 10 s")
        if proc.returncode != 0:
            raise Fault("hardware", (err or b"").decode("utf-8", "replace")[:200] or "the capture tool failed")

    def _notify_stream(self, seconds):
        """Say on the desktop that the screen is live, and how to end it (config `notify`). Best effort."""
        if not self.notify:
            return
        title = "Your screen is live on OpenVibe"
        body = "Shown to your own account for %d min. Run `openvibe-node stop` to end it now." % max(1, seconds // 60)
        argv = None
        if self.platform.startswith("linux") and (self.env.get("DISPLAY") or self.env.get("WAYLAND_DISPLAY")) and self.which("notify-send"):
            argv = ["notify-send", "--app-name=OpenVibe Node", "--", title, body]
        elif self.platform == "darwin" and self.which("osascript"):
            quote = lambda s: '"%s"' % s.replace("\\", "\\\\").replace('"', '\\"')
            argv = ["osascript", "-e", "display notification %s with title %s" % (quote(body), quote(title))]
        if argv is None:
            return
        try:
            self.popen(argv, stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                       start_new_session=True)
        except OSError:
            pass

    @staticmethod
    def _encode(path, max_width):
        try:
            from PIL import Image
        except ImportError:
            with open(path, "rb") as f:
                png = f.read()
            if len(png) > SCREEN_MAX_BYTES:
                raise Fault("unsupported", "install Pillow (pip install pillow) to send screens larger than 300 KB")
            return {"format": "png", "content_b64": base64.b64encode(png).decode("ascii")}
        import io
        with Image.open(path) as im:
            im = im.convert("RGB")
            width = min(max_width, im.width)
            quality = 75
            while True:
                height = max(1, round(im.height * width / im.width))
                frame = im.resize((width, height)) if width != im.width else im
                buf = io.BytesIO()
                frame.save(buf, "JPEG", quality=quality, optimize=True)
                if buf.tell() <= SCREEN_MAX_BYTES or width <= 320:
                    break
                if quality > 50:
                    quality -= 15
                else:
                    width = max(320, int(width * 0.75))
        return {"format": "jpeg", "width": width, "height": height,
                "content_b64": base64.b64encode(buf.getvalue()).decode("ascii")}
