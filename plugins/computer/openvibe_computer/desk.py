"""The private desk: a display of the machine's own where an agent works without touching its owner's screen.

An Xvfb display with a window manager (when one is installed) and a D-Bus session of its own. The session matters:
apps already open on the owner's desktop (most browsers and GNOME apps) are single-instance over D-Bus, and without a
separate bus "open it on the private desk" would quietly open a window on the owner's screen instead. The owner watches
it through the plugin's live screen (``screen_stream`` with ``desk: private``) and takes over by driving it the same way.

Every process the desk starts is its own process group, kept by handle and ended by handle (never by name), so stopping
the desk ends exactly what it started. Nothing here touches the owner's display; its address is never read.

Brought in from the owner's desktop-mcp (``desks.py``), plan track K step 2 (Vibe's desk on Node).
"""
from __future__ import annotations

import os
import re
import shutil
import tempfile
import signal
import subprocess
import threading
import time

from openvibe_plugin import Fault

WINDOW_MANAGERS = ("metacity", "openbox", "xfwm4", "fluxbox")
DISPLAYS = range(90, 100)
START_TIMEOUT_S = 8.0


class PrivateDesk:
    kind = "desktop"

    def __init__(self, which=shutil.which, popen=subprocess.Popen, clock=time.time, sleep=time.sleep,
                 exists=os.path.exists, base_env=None, displays=DISPLAYS):
        self.which = which
        self.popen = popen
        self.clock = clock
        self.sleep = sleep
        self.exists = exists
        self.base_env = dict(os.environ if base_env is None else base_env)
        self.displays = displays
        self.mu = threading.Lock()
        self.procs = []          # (name, Popen) in start order: Xvfb, then the bus, then the window manager
        self.display = None
        self.width = None
        self.height = None
        self.since = None
        self.bus = None
        self.wm = None

    # ---- what this machine has -------------------------------------------------------------------------------------
    def available(self):
        return bool(self.which("Xvfb"))

    def window_manager(self):
        for name in WINDOW_MANAGERS:
            if self.which(name):
                return name
        return None

    def running(self):
        with self.mu:
            return self._running()

    def _running(self):
        return bool(self.procs) and self.procs[0][1].poll() is None

    # ---- start, stop, status --------------------------------------------------------------------------------------
    def status(self):
        with self.mu:
            if not self._running():
                return {"running": False, "available": self.available()}
            return {"running": True, "display": ":%d" % self.display, "width": self.width, "height": self.height,
                    "window_manager": self.wm, "since": int(self.since)}

    def start(self, width=1280, height=800):
        with self.mu:
            if self._running():
                return self._status_locked()
            self._end_locked()
            if not self.available():
                raise Fault("unsupported", "a private desk needs Xvfb on this machine (Debian/Ubuntu: apt install xvfb)")
            n = self._free_display()
            xvfb = self.popen(["Xvfb", ":%d" % n, "-screen", "0", "%dx%dx24" % (width, height), "-nolisten", "tcp",
                               "-noreset"], stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                              start_new_session=True)
            self.procs = [("Xvfb", xvfb)]
            if not self._wait_for(lambda: self.exists("/tmp/.X11-unix/X%d" % n) or xvfb.poll() is not None):
                self._end_locked()
                raise Fault("hardware", "the private desk did not start in %d s" % START_TIMEOUT_S)
            if xvfb.poll() is not None:
                self._end_locked()
                raise Fault("hardware", "Xvfb ended as it started (display :%d)" % n)
            self.display, self.width, self.height, self.since = n, width, height, self.clock()
            self.bus = self._start_bus()
            self.wm = self.window_manager()
            if self.wm:
                self.procs.append((self.wm, self.popen([self.wm], env=self._env_locked(), stdin=subprocess.DEVNULL,
                                                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                                                       start_new_session=True)))
            return self._status_locked()

    def stop(self):
        with self.mu:
            was = self._running()
            self._end_locked()
            return {"running": False, "stopped": was}

    def env(self):
        """The environment for a tool or app that should run on the desk (its display and its own bus)."""
        with self.mu:
            if not self._running():
                raise Fault("not_ready", "start the private desk first (desk start)")
            return self._env_locked()

    # ---- internals ------------------------------------------------------------------------------------------------
    def _status_locked(self):
        return {"running": True, "display": ":%d" % self.display, "width": self.width, "height": self.height,
                "window_manager": self.wm, "since": int(self.since)}

    def _env_locked(self):
        env = {k: v for k, v in self.base_env.items() if k not in ("WAYLAND_DISPLAY", "DBUS_SESSION_BUS_ADDRESS", "XAUTHORITY")}
        env["DISPLAY"] = ":%d" % self.display
        if self.bus:
            env["DBUS_SESSION_BUS_ADDRESS"] = self.bus
        return env

    def _free_display(self):
        for n in self.displays:
            if not self.exists("/tmp/.X%d-lock" % n) and not self.exists("/tmp/.X11-unix/X%d" % n):
                return n
        raise Fault("not_ready", "no free display number for a private desk (:%d to :%d are taken)" % (self.displays[0], self.displays[-1]))

    def _wait_for(self, ready):
        end = self.clock() + START_TIMEOUT_S
        while self.clock() < end:
            if ready():
                return True
            self.sleep(0.05)
        return ready()

    def _start_bus(self):
        """A D-Bus session of the desk's own; its address, or None where there is no dbus-daemon (apps then share none)."""
        if not self.which("dbus-daemon"):
            return None
        env = self._env_locked()
        proc = self.popen(["dbus-daemon", "--session", "--nofork", "--print-address=1"], env=env, stdin=subprocess.DEVNULL,
                          stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, start_new_session=True, text=True)
        self.procs.append(("dbus-daemon", proc))
        got = []
        reader = threading.Thread(target=lambda: got.append(proc.stdout.readline().strip()), daemon=True)
        reader.start()
        reader.join(3.0)
        address = got[0] if got and got[0].startswith(("unix:", "tcp:")) else None
        return address

    def _end_locked(self):
        for _, proc in reversed(self.procs):
            self._kill(proc)
        self.procs = []
        self.display = self.width = self.height = self.since = self.bus = self.wm = None

    @staticmethod
    def _kill(proc):
        if proc.poll() is not None:
            return
        try:
            if hasattr(os, "killpg"):
                os.killpg(proc.pid, signal.SIGTERM)
            else:
                proc.terminate()
            proc.wait(timeout=3)
        except Exception:  # noqa: BLE001 - still running or already gone: make sure it is gone
            try:
                if hasattr(os, "killpg"):
                    os.killpg(proc.pid, signal.SIGKILL)
                else:
                    proc.kill()
            except Exception:  # noqa: BLE001
                pass


# What a browser desk leaves out of the machine's environment: the owner's display, session bus and X cookie, so
# nothing started for it can reach the owner's screen or their desktop apps.
OWNER_SESSION = ("DISPLAY", "WAYLAND_DISPLAY", "DBUS_SESSION_BUS_ADDRESS", "XAUTHORITY")


class BrowserDesk:
    """A browser desk (Desks plan D2): a browser alone, with no display, on Linux or macOS.

    Nothing runs here. The plugin starts the desk's headless browser (its own profile, downloads and temp folder) on
    the desk's first command and closes it when the desk stops. Its screen, mouse and keyboard and its live view are
    the page's own, over the browser's debugging pipe. The desk is its size, its start time and an environment
    without the owner's session (OWNER_SESSION). It costs about half of a whole desk: no display, no window manager,
    nothing drawn on a screen.
    """
    kind = "browser"

    def __init__(self, clock=time.time, base_env=None):
        self.clock = clock
        self.base_env = dict(os.environ if base_env is None else base_env)
        self.mu = threading.Lock()
        self.on = False
        self.width = self.height = self.since = None

    def available(self):
        return True   # the plugin checks for a browser before it starts one

    def window_manager(self):
        return None

    def running(self):
        return self.on

    def status(self):
        with self.mu:
            if not self.on:
                return {"running": False, "available": True}
            return {"running": True, "width": self.width, "height": self.height, "since": int(self.since)}

    def start(self, width=1280, height=800):
        with self.mu:
            if not self.on:
                self.on, self.width, self.height, self.since = True, width, height, self.clock()
        return self.status()

    def stop(self):
        with self.mu:
            was, self.on = self.on, False
            self.width = self.height = self.since = None
        return {"running": False, "stopped": was}

    def env(self):
        with self.mu:
            if not self.on:
                raise Fault("not_ready", "start the desk first (desk start)")
            return {k: v for k, v in self.base_env.items() if k not in OWNER_SESSION}


# A browser desk counts as half a desk against max_desks (a whole desk counts 2 of 2 * max_desks).
DESK_COST = {"desktop": 2, "browser": 1}


def kind_of(d):
    """A desk's kind; a desk object without one is a whole desk (desktop)."""
    return getattr(d, "kind", "desktop")


# A desk's name: `private` (the first desk, as before) or `desk-<name>` (Desks plan D1: several desks on one machine,
# one per agent task). Lower case, digits and dashes, so a name is also a safe path part.
DESK_NAME = re.compile(r"^(private|desk-[a-z0-9][a-z0-9-]{0,23})$")
MAX_DESKS_CAP = len(DISPLAYS)


def default_max_desks(meminfo="/proc/meminfo"):
    """How many desks this machine runs at once by default: one per 2 GiB of memory, 1 to 8 (a desk with a browser
    open uses about 0.5 to 1.5 GiB)."""
    try:
        with open(meminfo) as f:
            for line in f:
                if line.startswith("MemTotal:"):
                    return max(1, min(8, int(line.split()[1]) // (2 * 1024 * 1024)))
    except (OSError, ValueError, IndexError):
        pass
    return 2


class Desks:
    """The machine's desks by name: each a PrivateDesk (kind desktop: a display, a bus and, in the plugin, a browser
    shown there) or a BrowserDesk (kind browser: a headless browser alone).

    Two desks never share a display, a bus, a browser profile or a temp folder, so two agents working at once cannot
    see or drive each other's screen. At most `max_desks` whole desks run at once (a browser desk counts as half);
    starting one more is refused, never queued.
    Starts are serialized, so two desks starting together cannot pick the same display number.
    """

    def __init__(self, factory, private=None, max_desks=2, base_dir=None, browser_factory=None):
        self.factory = factory
        self.browser_factory = browser_factory or BrowserDesk
        self.max_desks = max_desks
        self.base_dir = base_dir
        self.mu = threading.Lock()
        self.desks = {}
        self.tmpdirs = {}   # desk name -> its own temp folder (short: under the system's temp folder)
        if private is not None:
            self.desks["private"] = private

    @staticmethod
    def check(name):
        if not isinstance(name, str) or not DESK_NAME.match(name):
            raise Fault("bad_value", "desk is main, private or desk-<name> (lower-case letters, digits and dashes, at most 29 characters)")
        return name

    def kind(self, name):
        """The kind of desk that name is now (desktop or browser), or None when there is no such desk."""
        with self.mu:
            d = self.desks.get(name)
        return kind_of(d) if d is not None else None

    def available(self):
        d = self.desks.get("private") or self.factory()
        return d.available()

    def window_manager(self):
        d = self.desks.get("private") or self.factory()
        return d.window_manager()

    def running_names(self):
        with self.mu:
            return [n for n, d in self.desks.items() if d.running()]

    def start(self, name, width=1280, height=800, kind="desktop"):
        self.check(name)
        if kind not in DESK_COST:
            raise Fault("bad_value", "kind is desktop or browser")
        if name == "private" and kind != "desktop":
            raise Fault("bad_value", "private is a whole desk: name a browser desk desk-<name>")
        with self.mu:
            d = self.desks.get(name)
            if d is not None and d.running() and kind_of(d) != kind:
                raise Fault("not_ready", "desk %s already runs as a %s desk: stop it first" % (name, kind_of(d)))
            if d is None or kind_of(d) != kind:
                d = self.desks[name] = self.factory() if kind == "desktop" else self.browser_factory()
            if not d.running():
                busy = sorted((n, kind_of(other)) for n, other in self.desks.items() if n != name and other.running())
                if sum(DESK_COST[k] for _, k in busy) + DESK_COST[kind] > 2 * self.max_desks:
                    raise Fault("not_ready", "this machine already runs %s, its limit (max_desks %d; a browser desk counts "
                                "as half): stop one first" % (", ".join("%s (%s)" % b for b in busy), self.max_desks))
            st = d.start(width, height)
        return dict(st, desk=name, kind=kind_of(d))

    def stop(self, name):
        self.check(name)
        with self.mu:
            d = self.desks.get(name)
        if d is None:
            return {"running": False, "stopped": False, "desk": name}
        out = dict(d.stop(), desk=name)
        self._drop_tmp(name)
        return out

    def stop_all(self):
        with self.mu:
            desks = list(self.desks.items())
        stopped = [n for n, d in desks if d.stop().get("stopped")]
        for n, _ in desks:
            self._drop_tmp(n)
        return stopped

    def status(self, name):
        self.check(name)
        with self.mu:
            d = self.desks.get(name)
        if d is None:
            return {"running": False, "available": self.available(), "desk": name}
        return dict(d.status(), desk=name, kind=kind_of(d))

    def list(self):
        with self.mu:
            desks = sorted(self.desks.items())
        running = [dict(d.status(), desk=n, kind=kind_of(d)) for n, d in desks if d.running()]
        return {"desks": running, "max_desks": self.max_desks, "available": self.available()}

    def env(self, name):
        """The environment for a tool or app on that desk: its display and bus, and a temp folder of its own."""
        self.check(name)
        with self.mu:
            d = self.desks.get(name)
        if d is None:
            raise Fault("not_ready", "start the desk first (desk start, desk %s)" % name)
        env = d.env()
        # A temp folder of the desk's own, made once per run of the desk (0700) and removed when it stops. It sits in
        # the system's temp folder, not under desk_dir: Chrome puts its SingletonSocket in TMPDIR, and a Unix socket's
        # path must fit 108 bytes (a desk_dir path on a desk host, /var/lib/…/desks/<name>/tmp/.com.google.Chrome.…,
        # did not, and Chrome closed as it started).
        with self.mu:
            tmp = self.tmpdirs.get(name)
            if tmp is None or not os.path.isdir(tmp):
                tmp = self.tmpdirs[name] = tempfile.mkdtemp(prefix="ov-%s-" % name)
        env["TMPDIR"] = tmp
        return env

    def _drop_tmp(self, name):
        with self.mu:
            tmp = self.tmpdirs.pop(name, None)
        if tmp:
            shutil.rmtree(tmp, ignore_errors=True)

    def dir(self, name, part):
        """A folder of the desk's own (downloads, …), 0700, under base_dir; None without one."""
        if not self.base_dir:
            return None
        path = os.path.join(self.base_dir, self.check(name), part)
        os.makedirs(path, mode=0o700, exist_ok=True)
        return path
