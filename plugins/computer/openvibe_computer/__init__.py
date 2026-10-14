"""OpenVibe Node computer plugin: the machine's terminal, files, screen and input for its owner's agents (plan T14/T17).

Off unless its owner lists it in the Node's config. The owner sets the **control level** on the machine itself; a cloud
command can never raise it:

* ``observe`` (the default): capture the screen; list, stat and read files inside the roots.
* ``ask``: also run programs, write files inside the roots and use the mouse and keyboard, each only after the owner
  approves it on the machine
  (``openvibe-node approvals``, ``approve <id>``, ``deny <id>``); unanswered requests expire.
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
* ``input`` ``{"action", "x"?, "y"?, "width"?, "height"?, "dy"?, "text"?, "keys"?}`` → ``{done, action, x?, y?}``: move,
  click, double_click or right_click at a point, scroll by ``dy`` clicks, type ``text`` into whatever has focus, or press
  one key or chord (``keys``, for example ``ctrl+s``). ``x``/``y`` are in the space of a screen picture ``width`` ×
  ``height`` (the ``screen`` answer) and are scaled to the real screen. An acting command: refused at observe, approved
  one by one at ask. xdotool on X11, cliclick on macOS (no scroll there); none on Wayland yet. Config
  ``allow_input: false`` turns it off.
* ``probe`` ``{"target", "timeout_s"?}`` → for ``http(s)://…`` one GET, no redirect followed: ``{status, content_type,
  etag, last_modified, body, truncated, ms}``; for ``tcp://host:port``: ``{open, ms, error?}``. Read-only, so it runs at
  every level, and only towards the machine's own network: every address the name resolves to must be private,
  loopback or link-local, and the connection goes to the address that was checked. Config ``probe: false`` turns it off.
"""

import base64
import http.client
import ipaddress
import os
import socket
import ssl
import shlex
import shutil
import stat
import subprocess
import sys
import tempfile
import threading
import time
import urllib.parse

from openvibe_plugin import Deferred, Fault, Plugin

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
# The core caps an ack's data at 512 KB of JSON; base64 grows by a third.
MAX_OUTPUT_CAP = 192 * 1024
MAX_READ_CAP = 256 * 1024
SCREEN_MAX_BYTES = 300 * 1024
INPUT_ACTIONS = frozenset({"move", "click", "double_click", "right_click", "scroll", "type", "key"})
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


def _int(v, name, lo, hi, default):
    if v is None:
        return default
    if isinstance(v, bool) or not isinstance(v, (int, float)) or v != v:
        raise Fault("bad_value", "%s must be a number" % name)
    return int(max(lo, min(hi, v)))


class Computer(Plugin):
    driver = "computer"
    version = "0.4.0"
    motion_kinds = frozenset()
    telemetry_interval_s = 2.0

    def __init__(self, which=shutil.which, popen=subprocess.Popen, clock=time.monotonic, env=None, platform=None, screen_size=None):
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
        self.approval_timeout_s = 120
        self.pending = {}  # command id -> (run, expires_at, summary)
        self.procs = set()
        self.running = 0
        self.mu = threading.Lock()

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
            caps["screen"] = {"level": self.level, "tool": tool[0]}
        if self.allow_probe:
            caps["probe"] = {"lan_only": True, "schemes": ["http", "https", "tcp"]}
        itool = self._input_tool() if self.allow_input else None
        if itool:
            caps["input"] = {"level": self.level, "tool": itool, "scroll": itool == "xdotool"}
        return caps

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
        for cid in waiting:
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
        expired = []
        with self.mu:
            for cid, (_, at, _) in list(self.pending.items()):
                if now >= at:
                    expired.append(cid)
                    del self.pending[cid]
            waiting, running = len(self.pending), self.running
        for cid in expired:
            self.emit_event("approval_done", id=cid, approved=False, reason="expired")
            self.fail(cid, "not_allowed", "nobody answered on the machine in time")
        # Sensors are what the core forwards to Bot: the machines page shows the level and what waits on the machine.
        return {"sensors": {"computer_level": self.effective_level(), "computer_waiting": waiting, "computer_running": running}}

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
        if cmd.kind == "screen":
            tool = self._screen_tool()
            if not tool:
                raise Fault("unsupported", "no screen capture tool on this machine")
            width = _int(v.get("max_width"), "max_width", 320, 3840, 1280)
            return self._start(cmd, lambda: self._screen(tool, width))
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
            tool = self._input_tool()
            if not tool:
                raise Fault("unsupported", "no input tool on this machine (xdotool on X11, cliclick on macOS)")
            run, summary = self._input_op(tool, v)
            return self._act(cmd, level, run, summary, threaded=True)
        if cmd.kind == "exec":
            if not self.allow_exec:
                raise Fault("unsupported", "this machine does not allow running programs")
            run, summary = self._exec_op(cmd, v, level)
            return self._act(cmd, level, run, summary, threaded=True)
        raise Fault("unsupported", "kind %s not supported" % cmd.kind)

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
        """A desktop notification that a request waits (config `notify`, on by default), when the Node runs in a
        graphical session: notify-send on Linux, osascript on macOS. Best effort: it never blocks or fails a command."""
        if not self.notify:
            return
        title = "OpenVibe: your agent asks"
        body = "%s\nopenvibe-node approve %s  (or deny %s)" % (summary[:200], cid, cid)
        if self.platform.startswith("linux") and (self.env.get("DISPLAY") or self.env.get("WAYLAND_DISPLAY")) and self.which("notify-send"):
            argv = ["notify-send", "--app-name=OpenVibe Node", title, body]
        elif self.platform == "darwin" and self.which("osascript"):
            quote = lambda s: '"%s"' % s.replace("\\", "\\\\").replace('"', '\\"')
            argv = ["osascript", "-e", "display notification %s with title %s" % (quote(body), quote(title))]
        else:
            return
        try:
            self.popen(argv, stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, start_new_session=True)
        except OSError:
            pass

    def on_approval(self, cid, approved):
        with self.mu:
            item = self.pending.pop(cid, None)
        if item is None:
            return
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
    def _input_tool(self):
        """xdotool on an X11 session, cliclick on macOS, else None (Wayland has no general input tool yet)."""
        if self.platform == "darwin" and self.which("cliclick"):
            return "cliclick"
        if self.platform.startswith("linux") and self.env.get("DISPLAY") and not self.env.get("WAYLAND_DISPLAY") and self.which("xdotool"):
            return "xdotool"
        return None

    def _input_op(self, tool, v):
        """(run, summary) for one input action; nothing touches the screen until run() (after approval at ask)."""
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
            if tool != "xdotool":
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
            at = self._scale(tool, point) if point else None
            argv = self._input_argv(tool, action, at, v)
            proc = self.popen(argv, stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, start_new_session=True)
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

        if action == "type":
            text = v["text"]
            shown = text if len(text) <= 120 else text[:120] + "…"
            summary = "type %d characters: %r" % (len(text), shown)
        elif action == "key":
            summary = "press %s" % v["keys"]
        elif action == "scroll":
            summary = "scroll %s %d" % ("up" if v["dy"] < 0 else "down", abs(v["dy"]))
        else:
            x, y, w, h = point
            summary = "%s at %d, %d%s" % (action.replace("_", " "), x, y, " of a %d×%d picture of the screen" % (w, h) if w else "")
        return run, summary

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

    def _screen(self, tool, max_width):
        fd, out = tempfile.mkstemp(prefix="openvibe-screen-", suffix=".png")
        os.close(fd)
        try:
            proc = self.popen(tool[1](out), stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
                              stderr=subprocess.PIPE, start_new_session=True)
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
