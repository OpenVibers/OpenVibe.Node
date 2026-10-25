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
import shutil
import signal
import subprocess
import threading
import time

from openvibe_plugin import Fault

WINDOW_MANAGERS = ("metacity", "openbox", "xfwm4", "fluxbox")
DISPLAYS = range(90, 100)
START_TIMEOUT_S = 8.0


class PrivateDesk:
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
