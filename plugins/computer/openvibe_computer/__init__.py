"""OpenVibe Node computer plugin: the machine's terminal, files and screen for its owner's agents (plan T14/T17).

Off unless its owner lists it in the Node's config. The owner sets the **control level** on the machine itself; a cloud
command can never raise it:

* ``observe`` (the default): capture the screen; list, stat and read files inside the roots.
* ``ask``: also run programs and write files inside the roots, each only after the owner approves it on the machine
  (``openvibe-node approvals``, ``approve <id>``, ``deny <id>``); unanswered requests expire.
* ``trusted``: run programs and write files inside the roots without asking.
* ``full``: as trusted, anywhere the account running the Node can reach.

The local kill switch (``openvibe-node stop``) and a remote e-stop drop the plugin to observe, kill what is running
and deny what waits; the core refuses acting commands while either is latched too. The plugin never runs as root.

Command kinds and their answers (the ack's ``data``):

* ``exec`` ``{"argv": [...], "cwd"?, "timeout_s"?, "stdin"?}`` → ``{exit_code, stdout, stderr, truncated, timed_out,
  duration_ms}``. A program and its arguments, never a shell line; output is cut at ``max_output_bytes``.
* ``fs`` ``{"op": "list"|"stat"|"read"|"write"|"mkdir", "path", …}``: ``read`` takes ``offset``/``length`` and answers
  ``content_b64``; ``write`` takes ``content_b64`` and ``mode`` (``create``, ``overwrite`` or ``append``).
* ``screen`` ``{"max_width"?}`` → ``{format, width, height, content_b64}`` (JPEG through Pillow when it is installed).
"""

import base64
import os
import shlex
import shutil
import stat
import subprocess
import sys
import tempfile
import threading
import time

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


def _int(v, name, lo, hi, default):
    if v is None:
        return default
    if isinstance(v, bool) or not isinstance(v, (int, float)) or v != v:
        raise Fault("bad_value", "%s must be a number" % name)
    return int(max(lo, min(hi, v)))


class Computer(Plugin):
    driver = "computer"
    version = "0.1.0"
    motion_kinds = frozenset()
    telemetry_interval_s = 2.0

    def __init__(self, which=shutil.which, popen=subprocess.Popen, clock=time.monotonic, env=None, platform=None):
        self.which = which
        self.popen = popen
        self.clock = clock
        self.env = os.environ if env is None else env
        self.platform = platform or sys.platform
        self.level = "observe"
        self.roots = []
        self.timeout_s = 30
        self.max_output = 64 * 1024
        self.max_read = 256 * 1024
        self.allow_exec = True
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
            self.log("waiting for approval %s: %s", cmd.id, summary)
            raise Deferred()
        self.log("%s %s: %s", cmd.kind, cmd.id, summary)
        return self._start(cmd, run) if threaded else run()

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
