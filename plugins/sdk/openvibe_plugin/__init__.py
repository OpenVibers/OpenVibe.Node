"""OpenVibe Node plugin runtime.

A plugin is an executable the Node core starts. It reads JSON lines from stdin and writes JSON lines to stdout (one
object per line; logs go to stderr). This module implements the parts every driver must get right, so a driver only
says how to move its hardware:

* describe first, touch hardware only after ``hello`` (and never in probe mode);
* stop every actuator when stdin closes, when a motion command's deadline passes without a newer command, when no
  heartbeat has arrived from the core for ``HEARTBEAT_TIMEOUT_S``, and on ``stop`` / ``estop``;
* keep an e-stop latched until the core sends ``resume``.

See docs/plugins.md for the wire format.
"""

import json
import os
import queue
import sys
import threading
import time

__all__ = ["Plugin", "Fault", "Command", "Runtime", "run", "clamp", "MOTION_KINDS", "HEARTBEAT_TIMEOUT_S",
           "DEFAULT_DEADLINE_MS", "PROTOCOL_VERSION"]

PROTOCOL_VERSION = 1
HEARTBEAT_TIMEOUT_S = 1.0
DEFAULT_DEADLINE_MS = 300
MAX_DEADLINE_MS = 5000
# Kinds that move something continuously and therefore stop at their deadline. A plugin may widen this set
# (Plugin.motion_kinds) for actuators it drives by speed rather than by position.
MOTION_KINDS = frozenset({"drive"})
# Kinds refused while an e-stop is latched or the heartbeat is lost. halt is always accepted.
GUARDED_KINDS = frozenset({"drive", "ptz", "actuator"})


class Fault(Exception):
    """Raised by a driver to answer a command with a nack."""

    def __init__(self, code, message=""):
        super().__init__(message or code)
        self.code = code
        self.message = message


class Command:
    __slots__ = ("id", "kind", "value", "deadline_ms", "deadline", "target")

    def __init__(self, id, kind, value, deadline_ms, now, target=None):
        self.id = id
        self.kind = kind
        self.value = value if value is not None else {}
        self.deadline_ms = deadline_ms
        self.deadline = now + deadline_ms / 1000.0
        self.target = target

    def __repr__(self):
        return "Command(id=%r, kind=%r, value=%r, deadline_ms=%r)" % (self.id, self.kind, self.value, self.deadline_ms)


def clamp(v, lo, hi):
    try:
        v = float(v)
    except (TypeError, ValueError):
        raise Fault("bad_value", "not a number: %r" % (v,))
    if v != v:  # NaN
        raise Fault("bad_value", "NaN")
    return max(lo, min(hi, v))


class Plugin:
    """Base class for drivers. Override the methods you need.

    All methods run on the runtime's main thread, except that ``stop`` may also be called from ``emit_event`` callers
    (it takes the runtime lock). Methods must not block for long: do slow work (speech synthesis, file I/O) on a
    thread of your own.
    """

    driver = "plugin"
    version = "0.0.0"
    motion_kinds = MOTION_KINDS
    telemetry_interval_s = 0.5

    runtime = None  # set by Runtime

    def describe(self, config):
        """Return the capabilities dict. Called before setup and in probe mode; must not touch hardware."""
        return {}

    def setup(self, config):
        """Open the hardware. Every actuator must be stopped when this returns."""

    def handle(self, cmd):
        """Execute a command. Return normally for ack, raise Fault for nack."""
        raise Fault("unsupported", "kind %s not supported" % cmd.kind)

    def stop_motion(self):
        """Stop what moves on its own (wheels). Called at a motion deadline. Defaults to stop()."""
        self.stop()

    def stop(self):
        """Stop every actuator. Must be idempotent and safe to call at any time, including before setup."""

    def poll(self):
        """Return a telemetry dict (battery, rssi, sensors) or None. Called every telemetry_interval_s."""
        return None

    def close(self):
        """Release the hardware. stop() has already been called."""

    # Helpers for drivers.
    def emit_event(self, name, **fields):
        if self.runtime is not None:
            self.runtime.send(dict(op="event", name=name, **fields))

    def emit_video(self, path, format="jpeg", **fields):
        if self.runtime is not None:
            self.runtime.send(dict(op="video", format=format, path=path, **fields))

    def emit_telemetry(self, **fields):
        if self.runtime is not None:
            self.runtime.send(dict(op="telemetry", **fields))

    def log(self, msg, *args):
        sys.stderr.write((msg % args if args else msg) + "\n")
        sys.stderr.flush()


class Runtime:
    """Drives one Plugin from a line-oriented input and output. Tests construct it with in-memory streams."""

    def __init__(self, plugin, infile=None, outfile=None, clock=time.monotonic,
                 heartbeat_timeout_s=HEARTBEAT_TIMEOUT_S):
        self.plugin = plugin
        plugin.runtime = self
        self.infile = infile if infile is not None else sys.stdin
        self.outfile = outfile if outfile is not None else sys.stdout
        self.clock = clock
        self.heartbeat_timeout_s = heartbeat_timeout_s
        self.lock = threading.RLock()
        self.out_lock = threading.Lock()
        self.inbox = queue.Queue()
        self.config = {}
        self.probe = False
        self.ready = False
        self.estopped = False
        self.motion_deadline = None
        self.last_heartbeat = None
        self.heartbeat_lost = False
        self.next_poll = 0.0
        self.closed = False
        self.stop_reasons = []  # for tests: why stop() was called, in order

    # Output.
    def send(self, obj):
        line = json.dumps(obj, separators=(",", ":"), default=str) + "\n"
        with self.out_lock:
            try:
                self.outfile.write(line)
                self.outfile.flush()
            except (BrokenPipeError, ValueError, OSError):
                pass

    # Safety.
    def safe_stop(self, reason, motion_only=False):
        with self.lock:
            self.stop_reasons.append(reason)
            self.motion_deadline = None
            try:
                if motion_only:
                    self.plugin.stop_motion()
                else:
                    self.plugin.stop()
            except Exception as e:  # a failing stop is the worst case; report it loudly
                self.plugin.log("stop failed (%s): %r", reason, e)
                self.send({"op": "event", "name": "stop_failed", "reason": reason, "error": str(e)})

    def tick(self):
        """Run time-based safety checks. Called by the loop at least every 20 ms."""
        now = self.clock()
        if self.motion_deadline is not None and now >= self.motion_deadline:
            self.safe_stop("deadline", motion_only=True)
        if self.ready and self.last_heartbeat is not None and not self.heartbeat_lost \
                and now - self.last_heartbeat > self.heartbeat_timeout_s:
            self.heartbeat_lost = True
            self.safe_stop("heartbeat")
            self.send({"op": "event", "name": "heartbeat_lost"})
        if self.ready and now >= self.next_poll:
            self.next_poll = now + self.plugin.telemetry_interval_s
            try:
                t = self.plugin.poll()
            except Exception as e:
                t = None
                self.plugin.log("poll failed: %r", e)
            if t:
                self.send(dict(op="telemetry", **t))

    # Input.
    def dispatch(self, msg):
        op = msg.get("op")
        if op == "hello":
            self.on_hello(msg)
        elif op == "heartbeat":
            self.last_heartbeat = self.clock()
            if self.heartbeat_lost:
                self.heartbeat_lost = False
        elif op == "command":
            self.on_command(msg)
        elif op == "stop":
            self.safe_stop("stop")
        elif op == "estop":
            self.estopped = True
            self.safe_stop("estop")
        elif op == "resume":
            self.estopped = False
        else:
            self.plugin.log("unknown op %r", op)

    def on_hello(self, msg):
        self.config = msg.get("config") or {}
        self.probe = bool(msg.get("probe"))
        try:
            caps = self.plugin.describe(self.config)
        except Exception as e:
            caps = {}
            self.plugin.log("describe failed: %r", e)
        self.send({"op": "describe", "driver": self.plugin.driver, "version": self.plugin.version,
                   "protocol": PROTOCOL_VERSION, "capabilities": caps,
                   "motion_kinds": sorted(self.plugin.motion_kinds)})
        if self.probe:
            return
        try:
            with self.lock:
                self.plugin.setup(self.config)
                self.plugin.stop()
            self.ready = True
            self.last_heartbeat = self.clock()
            self.send({"op": "ready"})
        except Fault as f:
            self.send({"op": "fault", "fault_code": f.code, "message": f.message})
        except Exception as e:
            self.send({"op": "fault", "fault_code": "hardware", "message": str(e)})

    def on_command(self, msg):
        cid = msg.get("id")
        kind = msg.get("kind")
        now = self.clock()
        try:
            dl = int(msg.get("deadline_ms") or DEFAULT_DEADLINE_MS)
        except (TypeError, ValueError):
            dl = DEFAULT_DEADLINE_MS
        dl = max(1, min(dl, MAX_DEADLINE_MS))
        cmd = Command(cid, kind, msg.get("value"), dl, now, msg.get("target"))
        if kind == "halt":
            self.safe_stop("halt")
            self.send({"op": "ack", "id": cid})
            return
        if not self.ready:
            self.send({"op": "nack", "id": cid, "fault_code": "not_ready"})
            return
        if kind in GUARDED_KINDS or kind in self.plugin.motion_kinds:
            if self.estopped:
                self.send({"op": "nack", "id": cid, "fault_code": "estopped"})
                return
            if self.heartbeat_lost:
                self.send({"op": "nack", "id": cid, "fault_code": "no_heartbeat"})
                return
        try:
            with self.lock:
                self.plugin.handle(cmd)
                if kind in self.plugin.motion_kinds:
                    self.motion_deadline = cmd.deadline
            self.send({"op": "ack", "id": cid})
        except Fault as f:
            out = {"op": "nack", "id": cid, "fault_code": f.code}
            if f.message:
                out["message"] = f.message
            self.send(out)
        except Exception as e:
            self.safe_stop("handler_error")
            self.send({"op": "nack", "id": cid, "fault_code": "hardware", "message": str(e)})

    def feed_line(self, line):
        line = line.strip()
        if not line:
            return
        try:
            msg = json.loads(line)
        except ValueError:
            self.plugin.log("bad json from core: %r", line[:200])
            return
        if isinstance(msg, dict):
            self.dispatch(msg)

    def shutdown(self, reason="eof"):
        if self.closed:
            return
        self.closed = True
        self.safe_stop(reason)
        try:
            self.plugin.close()
        except Exception as e:
            self.plugin.log("close failed: %r", e)

    # Loop.
    def _reader(self):
        try:
            for line in self.infile:
                self.inbox.put(line)
        except Exception:
            pass
        self.inbox.put(None)

    def run(self):
        t = threading.Thread(target=self._reader, name="stdin", daemon=True)
        t.start()
        try:
            while True:
                try:
                    line = self.inbox.get(timeout=0.02)
                except queue.Empty:
                    line = ""
                if line is None:
                    break
                if line:
                    self.feed_line(line)
                self.tick()
        finally:
            self.shutdown("eof")
        return 0


def protocol_stdout():
    """Reserve stdout for the protocol.

    Hardware libraries print to stdout (Adafruit Blinka warns about the board there), which would corrupt the JSON
    lines. Keep a private duplicate of file descriptor 1 for the protocol, then point descriptor 1 and sys.stdout at
    stderr, so every other print, from Python or from C, lands in the Node's log instead.
    """
    sys.stdout.flush()
    proto = os.fdopen(os.dup(1), "w", buffering=1, encoding="utf-8")
    os.dup2(2, 1)
    sys.stdout = sys.stderr
    return proto


def run(plugin):
    """Entry point for a plugin's __main__."""
    return Runtime(plugin, outfile=protocol_stdout()).run()
