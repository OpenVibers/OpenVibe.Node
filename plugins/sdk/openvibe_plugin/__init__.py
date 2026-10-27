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

__all__ = ["Plugin", "Fault", "Deferred", "Command", "StopInfo", "Runtime", "run", "clamp", "MOTION_KINDS",
           "HEARTBEAT_TIMEOUT_S", "DEFAULT_DEADLINE_MS", "PROTOCOL_VERSION", "STOP_CAUSES", "EMERGENCY_CAUSES",
           "RUNTIME_CAUSES", "SHARED_CLOCK", "SHARED_CLOCK_NAME", "TIMING_UNAVAILABLE", "__version__"]

# The Node release this runtime ships with (pyproject.toml carries the same).
__version__ = "0.30.0"
PROTOCOL_VERSION = 1
HEARTBEAT_TIMEOUT_S = 1.0
DEFAULT_DEADLINE_MS = 300
MAX_DEADLINE_MS = 5000
# Kinds that move nothing (a computer's exec, fs and screen) keep a long deadline: the plugin answers within it.
MAX_LONG_DEADLINE_MS = 600000
# Kinds that move something continuously and therefore stop at their deadline. A plugin may widen this set
# (Plugin.motion_kinds) for actuators it drives by speed rather than by position.
MOTION_KINDS = frozenset({"drive"})
# Kinds refused while an e-stop is latched or the heartbeat is lost. halt is always accepted.
GUARDED_KINDS = frozenset({"drive", "ptz", "actuator"})


def _monotonic_ns():
    return time.clock_gettime_ns(time.CLOCK_MONOTONIC)


def _qpc_ns(ticks, freq):
    """A QueryPerformanceCounter reading in nanoseconds, rounded down: whole seconds and the remainder apart, exactly as
    Node computes it (internal/plugins/clock.go qpcNS), so the two values compare exactly."""
    return (ticks // freq) * 1000000000 + (ticks % freq) * 1000000000 // freq


def _qpc_clock():
    """A reader of the raw QueryPerformanceCounter in ns (Windows), or None. Raw, through kernel32: Python's own
    time.monotonic is GetTickCount64 before 3.13 and perf_counter's reference point is not specified, so neither is
    relied on to match Node."""
    try:
        import ctypes
        k32 = ctypes.WinDLL("kernel32")
        f = ctypes.c_int64()
        if not k32.QueryPerformanceFrequency(ctypes.byref(f)) or f.value <= 0:
            return None
        freq = f.value
    except (AttributeError, OSError, ImportError):
        return None

    def read():
        t = ctypes.c_int64()
        if not k32.QueryPerformanceCounter(ctypes.byref(t)):
            raise OSError("QueryPerformanceCounter failed")
        return _qpc_ns(t.value, freq)
    return read


def _shared_clock():
    """(name, reader) of the clock this process shares with the core, as hello names it (Node 0.30), or (None, None).
    CLOCK_MONOTONIC on Unix; the raw QueryPerformanceCounter on Windows: one clock for every process on the machine,
    which a step of the wall clock does not move. A command's expires_at_mono_ns is an instant on it (Node 0.18)."""
    if sys.platform == "win32":
        read = _qpc_clock()
        return ("qpc", read) if read else (None, None)
    if hasattr(time, "clock_gettime_ns") and hasattr(time, "CLOCK_MONOTONIC"):
        return "clock_monotonic", _monotonic_ns
    return None, None


SHARED_CLOCK_NAME, SHARED_CLOCK = _shared_clock()

# The fault a strict-timing runtime answers a motion command with when its original expiry cannot be checked.
TIMING_UNAVAILABLE = "timing_unavailable"


def _truthy(v):
    return str(v or "").strip().lower() in ("1", "true", "yes", "on")


class Fault(Exception):
    """Raised by a driver to answer a command with a nack."""

    def __init__(self, code, message=""):
        super().__init__(message or code)
        self.code = code
        self.message = message


class Deferred(Exception):
    """Raised by handle() to answer later, with Plugin.complete(cmd.id, data) or Plugin.fail(cmd.id, code, message):
    slow work done on a thread of the plugin's own, or a command held for its owner's approval on the machine. The core
    waits for the answer until the command's deadline_ms (computer kinds carry long ones)."""


def _ack(cid, data=None):
    out = {"op": "ack", "id": cid}
    if data is not None:
        out["data"] = data
    return out


class Command:
    """operator is who sent it, as Bot's gate saw them: {"subject": "usr_…" or None, "role": "owner" | "operator" |
    "queue" | …}, or None when the core passed none. turn_epoch is the robot's control epoch Bot decided it under (an
    int, or None from older Bots): it rises when a queue turn ends, is left or is paused for an owner takeover, and the
    core already refuses a command older than the newest halt's, so a driver may also keep the newest it saw and refuse
    anything older (defence in depth).

    provenance is where it came from, unchanged from Bot (a dict, or None from older Nodes): "session" (the link
    session it arrived on; a new one on every connection), "seq" (the frame's number on that session, strictly
    increasing with gaps), "issued_at_ms" (Bot's clock when it sent the command and minted its id) and "expires_at_ms"
    (the absolute deadline on Bot's clock, motion kinds only), and "clock_uncertainty_ms" (Node v0.17.0: how far the
    core's estimate of Bot's clock may trail it, the connection's fastest round trip; the deadline was already checked
    against estimate + this, and a driver may refuse motion above a cap of its own). Bot sends a command once and never
    again; the runtime already refuses one whose seq is not above the last on its session (plugins.md, "Provenance").

    deadline_ms is the whole milliseconds the command had left when its line was read, and deadline the instant on the
    runtime's clock it expires: the core counts down to the moment it wrote the line, and the runtime takes off the time
    the line then sat in the pipe (expires_at_mono_ns, Node 0.18), so no delay on the way gives a command more time."""
    __slots__ = ("id", "kind", "value", "deadline_ms", "deadline", "target", "operator", "turn_epoch", "provenance")

    def __init__(self, id, kind, value, deadline_ms, now, target=None, operator=None, turn_epoch=None, provenance=None):
        self.id = id
        self.kind = kind
        self.value = value if value is not None else {}
        self.deadline_ms = int(deadline_ms)
        self.deadline = now + deadline_ms / 1000.0
        self.target = target
        self.operator = operator if isinstance(operator, dict) else None
        self.turn_epoch = turn_epoch if isinstance(turn_epoch, int) and not isinstance(turn_epoch, bool) and turn_epoch > 0 else None
        self.provenance = _provenance(provenance)

    def __repr__(self):
        return "Command(id=%r, kind=%r, value=%r, deadline_ms=%r)" % (self.id, self.kind, self.value, self.deadline_ms)


def _left_ms(expires_ns, shared_at):
    """Milliseconds from shared_at to expires_ns (both on the shared clock), or None when either is missing."""
    if shared_at is None or not isinstance(expires_ns, int) or isinstance(expires_ns, bool):
        return None
    return (expires_ns - shared_at) / 1e6


def _provenance(p):
    """The core's provenance dict with only well-typed fields, or None."""
    if not isinstance(p, dict):
        return None
    out = {}
    if isinstance(p.get("session"), str) and p["session"]:
        out["session"] = p["session"]
    for k in ("seq", "issued_at_ms", "expires_at_ms"):
        v = p.get(k)
        if isinstance(v, int) and not isinstance(v, bool) and v > 0:
            out[k] = v
    u = p.get("clock_uncertainty_ms")
    if isinstance(u, int) and not isinstance(u, bool) and u >= 0:
        out["clock_uncertainty_ms"] = u
    return out or None


# Why a driver is stopped (StopInfo.cause). From the core: an ordinary stop revokes remote motion and latches nothing;
# an emergency latches the platform's stop until its owner clears it.
STOP_CAUSES = ("halt", "link_lost", "shutdown", "timeout")
EMERGENCY_CAUSES = ("remote", "local", "driver", "latched")
# From the runtime itself: a motion command's own deadline, the core going quiet, the handler failing, stdin closing.
RUNTIME_CAUSES = ("setup", "deadline", "heartbeat", "handler_error", "eof")


class StopInfo:
    """Why the plugin is told to stop: what on_stop() and on_resume() receive (Node v0.17.0; plugins.md, "Why a plugin
    is stopped").

    cause, from the core: "halt" (Bot ended the controlling turn: a release, a handoff to the next viewer, a timeout or
    an unbind; turn_epoch is that halt's epoch), "link_lost" (the link to Bot dropped), "shutdown" (the Node is
    stopping), "timeout" (a command got no answer within its window). From the runtime: "setup" (the stop every setup
    ends with, so all actuators start stopped), "deadline" (a motion command's
    own deadline passed; motion_only is True: stop what moves on its own), "heartbeat" (the core went quiet),
    "handler_error", "eof". An older core sends no cause: "stop".
    emergency is True for the platform's e-stop, with cause "remote" (a robot owner or staff), "local"
    (`openvibe-node stop` on the device), "driver" (a driver's own local stop) or "latched" (the stop was already
    latched when this process or the Node started: replayed is True and first_cause is the original cause when the core
    knows it); an older core sends "estop". reason is the latch's reason text, when there is one.
    latched is whether the platform's stop latch is set as this is delivered. It is cleared only by its owner, which
    on_resume() reports with cause "cleared": information, never a re-arm.
    halt_epoch is the newest halt epoch the runtime knows; it survives a restart of this process (the core's hello)."""
    __slots__ = ("cause", "emergency", "latched", "reason", "turn_epoch", "halt_epoch", "replayed", "first_cause",
                 "motion_only")

    def __init__(self, cause, emergency=False, latched=False, reason=None, turn_epoch=None, halt_epoch=0,
                 replayed=False, first_cause=None, motion_only=False):
        self.cause = cause
        self.emergency = emergency
        self.latched = latched
        self.reason = reason
        self.turn_epoch = turn_epoch
        self.halt_epoch = halt_epoch
        self.replayed = replayed
        self.first_cause = first_cause
        self.motion_only = motion_only

    def __repr__(self):
        return "StopInfo(cause=%r, emergency=%r, latched=%r, turn_epoch=%r, halt_epoch=%r, replayed=%r)" % (
            self.cause, self.emergency, self.latched, self.turn_epoch, self.halt_epoch, self.replayed)


def _text(v, limit=200):
    return str(v)[:limit] if isinstance(v, str) and v else None


def _epoch(v):
    return v if isinstance(v, int) and not isinstance(v, bool) and v > 0 else None


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
    # Seconds between attempts to set up again after setup raised (the hardware or the robot's computer was not up
    # yet). None: a failed setup stays failed until the core restarts the plugin.
    retry_setup_s = None
    # Strict timing (Node 0.30): a motion command whose original expiry cannot be checked on the clock shared with the
    # core (none on this machine, a failed read, a different clock, or a core that sent none) is refused before handle()
    # with timing_unavailable, never run on deadline_ms alone. The deployment can also require it: OPENVIBE_STRICT_TIMING=1
    # in the plugin's environment, or strict_timing in the Node's plugin configuration (hello).
    strict_timing = False

    runtime = None  # set by Runtime

    def describe(self, config):
        """Return the capabilities dict. Called before setup and in probe mode; must not touch hardware."""
        return {}

    def setup(self, config):
        """Open the hardware. Every actuator must be stopped when this returns."""

    def handle(self, cmd):
        """Execute a command. Return normally for ack (a dict returned is the ack's data: what a computer command
        answers), raise Fault for nack, raise Deferred to answer later with complete() or fail()."""
        raise Fault("unsupported", "kind %s not supported" % cmd.kind)

    def stop_motion(self):
        """Stop what moves on its own (wheels). Called at a motion deadline. Defaults to stop()."""
        self.stop()

    def stop(self):
        """Stop every actuator. Must be idempotent and safe to call at any time, including before setup."""

    def on_stop(self, info):
        """Stop, knowing why: info is a StopInfo (its cause, whether it is an emergency, the halt epoch, the latch).
        Every stop the runtime makes calls this. The default calls stop_motion() for a motion deadline and stop() for
        everything else, so a plugin that overrides only those behaves as before. Override it to tell an ordinary stop
        (revoke remote motion; latch nothing of your own) from an emergency (info.emergency). Like stop() it must stop
        every actuator it is asked to, be idempotent, and is reported, not retried, if it raises."""
        if info.motion_only:
            self.stop_motion()
        else:
            self.stop()

    def on_resume(self, info):
        """The platform's stop latch was cleared by its owner (info.cause "cleared"). Information only: never re-arm
        hardware, clear a physical latch or restore a remote grant here. Commands are accepted again by the runtime and
        each still passes every check. The default does nothing."""

    def poll(self):
        """Return a telemetry dict (battery, rssi, sensors) or None. Called every telemetry_interval_s."""
        return None

    def close(self):
        """Release the hardware. stop() has already been called."""

    def on_approval(self, cid, approved):
        """The owner answered, on the machine (`openvibe-node approve|deny <id>`), an approval this plugin asked for
        with emit_event("approval", id=…). Runs on the runtime's main thread."""

    def on_voice(self, on, call_word):
        """Vibe by voice (contracts 0.214.0): the owner lets Vibe answer their voice on this machine (on) and chose
        call_word, or stopped it. Sent to plugins whose describe lists the `voice` capability, after every config from
        Bot and when the plugin emits the voice_ready event. A voice plugin listens for the call word only while on."""

    def on_voice_answer(self, cid, state, text, url):
        """Vibe's answer to a voice_ask this plugin emitted (emit_event("voice_ask", id=…, text=…)): state is working,
        answered or refused; text is what to say, url the Vibe chat that keeps it."""

    def on_tray(self, attached, ttl_s):
        """The Node's tray (`openvibe-node tray`, in the owner's graphical session) is showing this plugin's approvals,
        for the next ttl_s seconds unless told again. A plugin that notifies the desktop itself can stay quiet then.
        Sent only to the plugin that handles computer kinds."""

    # Helpers for drivers.
    def complete(self, cid, data=None):
        """Answer a deferred command with ack (and its data). Safe from any thread."""
        if self.runtime is not None:
            self.runtime.send(_ack(cid, data))

    def fail(self, cid, code, message=""):
        """Answer a deferred command with nack. Safe from any thread."""
        if self.runtime is not None:
            out = {"op": "nack", "id": cid, "fault_code": code}
            if message:
                out["message"] = str(message)[:500]
            self.runtime.send(out)

    def emit_event(self, name, **fields):
        if self.runtime is not None:
            self.runtime.send(dict(op="event", name=name, **fields))

    def emit_video(self, path, format="jpeg", **fields):
        if self.runtime is not None:
            self.runtime.send(dict(op="video", format=format, path=path, **fields))

    def emit_telemetry(self, **fields):
        if self.runtime is not None:
            self.runtime.send(dict(op="telemetry", **fields))

    def local_stop(self, reason=""):
        """Latch the Node's local stop: the driver's own stop button, or a sensor it trusts. Every actuator stops now,
        the panel shows the robot as stopped on the robot, and only ``openvibe-node resume`` on this machine releases
        it (the owner's remote clear does not). Safe to call from any thread."""
        if self.runtime is not None:
            self.runtime.local_stop(reason)

    def log(self, msg, *args):
        sys.stderr.write((msg % args if args else msg) + "\n")
        sys.stderr.flush()


class Runtime:
    """Drives one Plugin from a line-oriented input and output. Tests construct it with in-memory streams."""

    def __init__(self, plugin, infile=None, outfile=None, clock=time.monotonic,
                 heartbeat_timeout_s=HEARTBEAT_TIMEOUT_S, shared_clock=SHARED_CLOCK, shared_clock_name=SHARED_CLOCK_NAME):
        self.plugin = plugin
        plugin.runtime = self
        self.infile = infile if infile is not None else sys.stdin
        self.outfile = outfile if outfile is not None else sys.stdout
        self.clock = clock
        # The clock shared with the core (SHARED_CLOCK, ns), read with clock when each line arrives; None: not shared.
        self.shared_clock = shared_clock
        self.shared_clock_name = shared_clock_name if shared_clock is not None else None
        # The clock the core named in hello (Node 0.30), and whether it is ours: expires_at_mono_ns is trusted only then.
        self.core_clock = None
        self.clock_agrees = True
        self.strict_timing = bool(getattr(plugin, "strict_timing", False)) or _truthy(os.environ.get("OPENVIBE_STRICT_TIMING"))
        self.heartbeat_timeout_s = heartbeat_timeout_s
        self.lock = threading.RLock()
        self.out_lock = threading.Lock()
        self.inbox = queue.Queue()
        self.config = {}
        self.probe = False
        self.ready = False
        self.estopped = False
        # True from a local_stop until the core's estop confirms it: a resume crossing it on the wire is ignored.
        self.local_stop_pending = False
        self.motion_deadline = None
        self.last_heartbeat = None
        self.heartbeat_lost = False
        self.next_poll = 0.0
        self.setup_retry_at = None
        self.closed = False
        self.stop_reasons = []  # for tests: why stop() was called, in order
        self.stop_infos = []  # and the StopInfo each on_stop() got
        # The newest turn_epoch a halt carried ("stop" with turn_epoch); a command from an older turn is refused.
        self.halt_epoch = 0
        # The link session of the last command handled, and its seq: a command on that session whose seq is not above
        # it is a replay (Bot never sends one; this holds even if something did).
        self.last_session = None
        self.last_seq = 0

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
    def safe_stop(self, reason, motion_only=False, info=None):
        """Stop through the plugin's on_stop with why (info; built from reason when not given). reason is what
        stop_reasons records: the op or the runtime's own cause."""
        if info is None:
            info = StopInfo(reason, latched=self.estopped, halt_epoch=self.halt_epoch, motion_only=motion_only)
        with self.lock:
            self.stop_reasons.append(reason)
            self.stop_infos.append(info)
            self.motion_deadline = None
            try:
                self.plugin.on_stop(info)
            except Exception as e:  # a failing stop is the worst case; report it loudly
                self.plugin.log("stop failed (%s): %r", reason, e)
                self.send({"op": "event", "name": "stop_failed", "reason": reason, "error": str(e)})

    def local_stop(self, reason=""):
        with self.lock:
            self.estopped = True
            self.local_stop_pending = True
        self.safe_stop("local_stop", info=StopInfo("driver", emergency=True, latched=True, reason=_text(reason, 120),
                                                   halt_epoch=self.halt_epoch))
        self.send({"op": "local_stop", "reason": str(reason or "")[:120]})

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
        if not self.ready and self.setup_retry_at is not None and now >= self.setup_retry_at and not self.closed:
            self.setup_retry_at = None
            self._setup(retrying=True)
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
    def _timing_gap(self, msg, shared_at):
        """Why a motion command's original expiry cannot be checked, in words."""
        if self.shared_clock is None:
            return "this machine has no clock this runtime shares with the core"
        if not self.clock_agrees:
            return "the core's clock (%s) is not this runtime's (%s)" % (self.core_clock, self.shared_clock_name)
        if not isinstance(msg.get("expires_at_mono_ns"), int) or isinstance(msg.get("expires_at_mono_ns"), bool):
            return "the core sent no expiry on the shared clock (expires_at_mono_ns)"
        if shared_at is None:
            return "the shared clock could not be read when the command arrived"
        return "the command's expiry could not be checked"

    def shared_now(self):
        """The shared clock now (ns), or None."""
        if self.shared_clock is None:
            return None
        try:
            return self.shared_clock()
        except (OSError, ValueError):
            return None

    def dispatch(self, msg, at=None, shared_at=None):
        """Handle one message. at is when the line arrived (defaults to now): deadlines and heartbeats count from
        arrival, so a backlog after a slow handler cannot make stale commands or heartbeats look fresh. shared_at is
        the shared clock at that moment (read with at when at defaults): a command's expires_at_mono_ns is measured
        against it, so time in the pipe before arrival counts too."""
        if at is None:
            at = self.clock()
            shared_at = self.shared_now()
        op = msg.get("op")
        if op == "hello":
            self.on_hello(msg)
        elif op == "heartbeat":
            self.last_heartbeat = max(self.last_heartbeat or at, at)
            if self.heartbeat_lost:
                self.heartbeat_lost = False
        elif op == "command":
            self.on_command(msg, at, shared_at)
        elif op == "stop":
            epoch = _epoch(msg.get("turn_epoch"))
            if epoch and epoch > self.halt_epoch:
                self.halt_epoch = epoch
            cause = _text(msg.get("cause"), 40) or "stop"
            self.safe_stop("stop", info=StopInfo(cause, latched=self.estopped, turn_epoch=epoch,
                                                 halt_epoch=self.halt_epoch))
        elif op == "estop":
            self.estopped = True
            self.local_stop_pending = False
            self.safe_stop("estop", info=StopInfo(_text(msg.get("cause"), 40) or "estop", emergency=True, latched=True,
                                                  reason=_text(msg.get("reason")), halt_epoch=self.halt_epoch,
                                                  replayed=msg.get("replayed") is True,
                                                  first_cause=_text(msg.get("first_cause"), 40)))
        elif op == "approval":
            try:
                with self.lock:
                    self.plugin.on_approval(msg.get("id"), msg.get("approved") is True)
            except Exception as e:
                self.plugin.log("approval handler failed: %r", e)
        elif op == "tray":
            try:
                self.plugin.on_tray(msg.get("attached") is True, msg.get("ttl_s"))
            except Exception as e:
                self.plugin.log("tray handler failed: %r", e)
        elif op == "voice":
            try:
                self.plugin.on_voice(msg.get("on") is True, str(msg.get("call_word") or ""))
            except Exception as e:
                self.plugin.log("voice handler failed: %r", e)
        elif op == "voice_answer":
            try:
                self.plugin.on_voice_answer(str(msg.get("id") or ""), str(msg.get("state") or ""), str(msg.get("text") or ""), str(msg.get("url") or ""))
            except Exception as e:
                self.plugin.log("voice answer handler failed: %r", e)
        elif op == "resume":
            if self.local_stop_pending:
                self.plugin.log("resume ignored: the local stop has not reached the core yet")
            else:
                self.estopped = False
                try:
                    with self.lock:
                        self.plugin.on_resume(StopInfo(_text(msg.get("cause"), 40) or "cleared",
                                                       halt_epoch=self.halt_epoch))
                except Exception as e:
                    self.plugin.log("resume handler failed: %r", e)
        else:
            self.plugin.log("unknown op %r", op)

    def on_hello(self, msg):
        self.config = msg.get("config") or {}
        self.probe = bool(msg.get("probe"))
        # The clock the core put expires_at_mono_ns on. A different one than ours (or ours missing) is never compared
        # with: the field then counts as absent, and strict timing refuses motion.
        core_clock = msg.get("shared_clock")
        self.core_clock = core_clock if isinstance(core_clock, str) else None
        self.clock_agrees = self.core_clock is None or self.core_clock == self.shared_clock_name
        if msg.get("strict_timing") is True:
            self.strict_timing = True
        # The newest halt epoch the core has seen (Node v0.17.0): a restarted process keeps refusing older turns.
        epoch = _epoch(msg.get("halt_epoch"))
        if epoch and epoch > self.halt_epoch:
            self.halt_epoch = epoch
        try:
            caps = self.plugin.describe(self.config)
        except Exception as e:
            caps = {}
            self.plugin.log("describe failed: %r", e)
        describe = {"op": "describe", "driver": self.plugin.driver, "version": self.plugin.version,
                    "protocol": PROTOCOL_VERSION, "capabilities": caps, "motion_kinds": sorted(self.plugin.motion_kinds)}
        if self.strict_timing:
            # The core refuses too, before writing: a motion command never leaves it without its original expiry.
            describe["timing"] = "strict"
        self.send(describe)
        if self.probe:
            return
        self._setup()

    def _setup(self, retrying=False):
        """Set the plugin up; ready on success, else a fault (sent once, not on every retry) and, when the plugin
        sets retry_setup_s, another attempt that long from now. A later success sends ready, which ends the fault."""
        try:
            with self.lock:
                self.plugin.setup(self.config)
                self.plugin.on_stop(StopInfo("setup", latched=self.estopped, halt_epoch=self.halt_epoch))
            self.ready = True
            self.last_heartbeat = self.clock()
            self.send({"op": "ready"})
            return
        except Fault as f:
            fault = {"op": "fault", "fault_code": f.code, "message": f.message}
        except Exception as e:
            fault = {"op": "fault", "fault_code": "hardware", "message": str(e)}
        if not retrying:
            self.send(fault)
        if self.plugin.retry_setup_s:
            self.setup_retry_at = self.clock() + float(self.plugin.retry_setup_s)

    def on_command(self, msg, now=None, shared_at=None):
        cid = msg.get("id")
        kind = msg.get("kind")
        if now is None:
            now = self.clock()
            shared_at = self.shared_now()
        try:
            dl = int(msg.get("deadline_ms") or DEFAULT_DEADLINE_MS)
        except (TypeError, ValueError):
            dl = DEFAULT_DEADLINE_MS
        moves = kind in GUARDED_KINDS or kind in self.plugin.motion_kinds
        dl = max(1, min(dl, MAX_DEADLINE_MS if moves else MAX_LONG_DEADLINE_MS))
        # What the command had left when its line arrived, on the clock shared with the core (Node 0.18): deadline_ms
        # was counted down to the moment the core wrote the line, so this takes off the time it then sat in the pipe.
        # Only on the clock the core named (hello, Node 0.30): another clock's instant is never compared with ours.
        left = _left_ms(msg.get("expires_at_mono_ns"), shared_at if self.clock_agrees else None) if moves else None
        if left is not None and left >= 1:
            dl = min(dl, left)
        cmd = Command(cid, kind, msg.get("value"), dl, now, msg.get("target"), msg.get("operator"), msg.get("turn_epoch"),
                      msg.get("provenance"))
        if kind == "halt":
            if cmd.turn_epoch and cmd.turn_epoch > self.halt_epoch:
                self.halt_epoch = cmd.turn_epoch
            self.safe_stop("halt", info=StopInfo("halt", latched=self.estopped, turn_epoch=cmd.turn_epoch,
                                                 halt_epoch=self.halt_epoch))
            self.send({"op": "ack", "id": cid})
            return
        if cmd.turn_epoch and cmd.turn_epoch < self.halt_epoch:
            self.send({"op": "nack", "id": cid, "fault_code": "expired",
                       "message": "from a turn that ended: epoch %d is older than the newest halt's %d" % (cmd.turn_epoch, self.halt_epoch)})
            return
        prov = cmd.provenance or {}
        if prov.get("session") and prov.get("seq"):
            if prov["session"] == self.last_session and prov["seq"] <= self.last_seq:
                self.send({"op": "nack", "id": cid, "fault_code": "expired",
                           "message": "a replay: seq %d is not above %d on this session" % (prov["seq"], self.last_seq)})
                return
            self.last_session, self.last_seq = prov["session"], prov["seq"]
        # Strict timing: a motion command whose original expiry cannot be checked here never reaches the driver.
        if moves and self.strict_timing and left is None:
            self.send({"op": "nack", "id": cid, "fault_code": TIMING_UNAVAILABLE,
                       "message": "strict timing: " + self._timing_gap(msg, shared_at)})
            return
        # A motion command whose time ran out in the pipe, before its line was read, never reaches the driver.
        if left is not None and left < 1:
            self.send({"op": "nack", "id": cid, "fault_code": "expired",
                       "message": "its deadline passed before the runtime read it (%d ms late, counted on the clock it "
                                  "shares with the core)" % max(0, round(-left))})
            return
        # Nor does one whose time ran out while it waited behind a slow handler (its deadline counts from when its line
        # arrived): the runtime holds nothing past its deadline.
        if moves and self.clock() >= cmd.deadline:
            self.send({"op": "nack", "id": cid, "fault_code": "expired",
                       "message": "its deadline passed while it waited behind earlier input (%d ms after it arrived)"
                       % round((self.clock() - now) * 1000)})
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
                data = self.plugin.handle(cmd)
                if kind in self.plugin.motion_kinds:
                    self.motion_deadline = cmd.deadline
            self.send(_ack(cid, data))
        except Deferred:
            return
        except Fault as f:
            out = {"op": "nack", "id": cid, "fault_code": f.code}
            if f.message:
                out["message"] = f.message
            self.send(out)
        except Exception as e:
            self.safe_stop("handler_error")
            self.send({"op": "nack", "id": cid, "fault_code": "hardware", "message": str(e)})

    def feed_line(self, line, at=None, shared_at=None):
        line = line.strip()
        if not line:
            return
        try:
            msg = json.loads(line)
        except ValueError:
            self.plugin.log("bad json from core: %r", line[:200])
            return
        if isinstance(msg, dict):
            self.dispatch(msg, at, shared_at)

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
                # The runtime's clock first, then the shared one: the time left can only come out short.
                at = self.clock()
                self.inbox.put((line, at, self.shared_now()))
        except Exception:
            pass
        self.inbox.put(None)

    def run(self):
        t = threading.Thread(target=self._reader, name="stdin", daemon=True)
        t.start()
        try:
            while True:
                try:
                    item = self.inbox.get(timeout=0.02)
                except queue.Empty:
                    item = ("", None, None)
                if item is None:
                    break
                line, at, shared_at = item
                if line:
                    self.feed_line(line, at, shared_at)
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
