"""Relay Bot buttons and points to a local OpenVibe.Live hardware client."""

import base64
import hashlib
import json
import queue
import socket
import socketserver
import subprocess
import sys
import threading
import time

from openvibe_plugin import Fault, Plugin

__version__ = "0.1.0"
_GUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"


def live_message(kind, value):
    """Return the hardware-facing Live fields available in a Bot command.

    Live's hardware socket sends ``{type:'command', command}`` for a press, ``key_down`` / ``key_up`` for a hold and
    ``video_click`` with ``x`` / ``y`` (server/controls/control-server.js:360,475,527), and its example Pi bridge
    dispatches on exactly those names (control-bridge-example-cozmo.py:148-156). ``key_held`` / ``key_released`` are
    the names Live broadcasts to *viewers*, never to the hardware; using them here would break existing scripts.
    """
    if not isinstance(value, dict):
        raise Fault("bad_value", "value must be an object")
    if kind == "button":
        name = value.get("name")
        if not isinstance(name, str) or not name:
            raise Fault("bad_value", "button needs a name")
        if "state" not in value:
            return {"type": "command", "command": name}
        state = value["state"]
        if state == "down":
            return {"type": "key_down", "command": name}
        if state == "up":
            return {"type": "key_up", "command": name}
        raise Fault("bad_value", "button state must be down or up")
    if kind == "point":
        coords = {}
        for key in ("x", "y"):
            number = value.get(key)
            if isinstance(number, bool) or not isinstance(number, (int, float)) or not 0 <= number <= 1:
                raise Fault("bad_value", "point %s must be from 0 to 1" % key)
            coords[key] = round(number, 4)
        return {"type": "video_click", **coords}
    raise Fault("unsupported", "kind %s not supported" % kind)


def _read_exact(sock, size):
    result = bytearray()
    while len(result) < size:
        chunk = sock.recv(size - len(result))
        if not chunk:
            raise EOFError
        result.extend(chunk)
    return bytes(result)


class _WebSocketHandler(socketserver.BaseRequestHandler):
    def handle(self):
        sock = self.request
        sock.settimeout(1)
        try:
            request = bytearray()
            while b"\r\n\r\n" not in request and len(request) < 8192:
                request.extend(_read_exact(sock, 1))
            lines = request.decode("latin1").split("\r\n")
            headers = {}
            for line in lines[1:]:
                if ":" in line:
                    key, val = line.split(":", 1)
                    headers[key.lower()] = val.strip()
            key = headers.get("sec-websocket-key", "")
            if not lines[0].startswith("GET ") or headers.get("sec-websocket-version") != "13" or not key:
                return
            accept = base64.b64encode(hashlib.sha1((key + _GUID).encode("ascii")).digest()).decode("ascii")
            sock.sendall(("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\n"
                          "Connection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n" % accept).encode("ascii"))
            with self.server.clients_lock:
                self.server.clients[sock] = threading.Lock()
            while not self.server.closing.is_set():
                try:
                    first, second = _read_exact(sock, 2)
                except (TimeoutError, socket.timeout):
                    # socket.timeout is not TimeoutError before Python 3.10; a client that only receives (the
                    # normal Pi case) sits idle between commands, so this must not drop the connection.
                    continue
                opcode = first & 0x0f
                length = second & 0x7f
                if not second & 0x80 or opcode not in (0x1, 0x2, 0x8, 0x9, 0xA):
                    break
                if length == 126:
                    length = int.from_bytes(_read_exact(sock, 2), "big")
                elif length == 127:
                    length = int.from_bytes(_read_exact(sock, 8), "big")
                if length > 65536:
                    break
                mask = _read_exact(sock, 4)
                payload = _read_exact(sock, length)
                if opcode == 0x8:
                    break
                if opcode == 0x9:
                    payload = bytes(b ^ mask[i % 4] for i, b in enumerate(payload))
                    with self.server.clients_lock:
                        lock = self.server.clients.get(sock)
                    if lock:
                        with lock:
                            sock.sendall(_frame(0xA, payload))
        except (EOFError, OSError, ValueError, UnicodeError):
            pass
        finally:
            with self.server.clients_lock:
                self.server.clients.pop(sock, None)


def _frame(opcode, payload):
    size = len(payload)
    if size < 126:
        prefix = bytes((0x80 | opcode, size))
    elif size < 65536:
        prefix = bytes((0x80 | opcode, 126)) + size.to_bytes(2, "big")
    else:
        prefix = bytes((0x80 | opcode, 127)) + size.to_bytes(8, "big")
    return prefix + payload


class _WebSocketServer(socketserver.ThreadingTCPServer):
    allow_reuse_address = True
    daemon_threads = True

    def __init__(self, port):
        self.clients = {}
        self.clients_lock = threading.Lock()
        self.closing = threading.Event()
        super().__init__(("127.0.0.1", port), _WebSocketHandler)

    def broadcast(self, message):
        frame = _frame(0x1, json.dumps(message, separators=(",", ":")).encode("utf-8"))
        with self.clients_lock:
            clients = list(self.clients.items())
        for sock, lock in clients:
            try:
                with lock:
                    sock.sendall(frame)
            except OSError:
                with self.clients_lock:
                    self.clients.pop(sock, None)

    def close(self):
        self.closing.set()
        self.shutdown()
        self.server_close()
        with self.clients_lock:
            clients = list(self.clients)
            self.clients.clear()
        for sock in clients:
            try:
                sock.shutdown(2)
            except OSError:
                pass


class Relay(Plugin):
    driver = "relay"
    version = __version__
    # A held button must be released when its command deadline expires.
    motion_kinds = frozenset({"button"})

    def __init__(self):
        self.held = set()
        self.held_lock = threading.Lock()
        self.server = None
        self.server_thread = None
        self.child = None
        self.worker = None
        self.queue = queue.Queue(maxsize=256)
        self.closing = threading.Event()
        self.argv = None

    def describe(self, config):
        return {"button": {}, "point": {}}

    def setup(self, config):
        output = config.get("output", "websocket")
        if output == "websocket":
            port = config.get("port", 8765)
            if isinstance(port, bool) or not isinstance(port, int) or not 1 <= port <= 65535:
                raise Fault("bad_value", "port must be from 1 to 65535")
            try:
                self.server = _WebSocketServer(port)
            except OSError as exc:
                raise Fault("not_connected", str(exc)) from exc
            self.server_thread = threading.Thread(target=self.server.serve_forever, daemon=True)
            self.server_thread.start()
        elif output == "child":
            self.argv = config.get("argv")
            if not isinstance(self.argv, list) or not self.argv or not all(isinstance(a, str) and a for a in self.argv):
                raise Fault("bad_value", "child output needs a nonempty argv array")
            self._start_child()
            self.worker = threading.Thread(target=self._run_child, daemon=True)
            self.worker.start()
        else:
            raise Fault("bad_value", "output must be websocket or child")

    def _start_child(self):
        try:
            self.child = subprocess.Popen(self.argv, stdin=subprocess.PIPE, stdout=sys.stderr, stderr=sys.stderr)
        except OSError as exc:
            raise Fault("not_connected", str(exc)) from exc

    def _run_child(self):
        delay = 0.1
        while not self.closing.is_set():
            if self.child.poll() is not None:
                self.log("relay child exited (%s); restarting", self.child.returncode)
                try:
                    self.child.stdin.close()
                except OSError:
                    pass
                # Do not replay commands buffered while the child was absent.
                while True:
                    try:
                        self.queue.get_nowait()
                    except queue.Empty:
                        break
                if self.closing.wait(delay):
                    break
                try:
                    self._start_child()
                    delay = min(delay * 2, 5.0)
                except Fault as exc:
                    self.log("relay child restart failed: %s", exc)
                continue
            try:
                message = self.queue.get(timeout=0.05)
            except queue.Empty:
                continue
            try:
                self.child.stdin.write((json.dumps(message, separators=(",", ":")) + "\n").encode("utf-8"))
                self.child.stdin.flush()
            except (BrokenPipeError, OSError, ValueError):
                pass

    def _send(self, message):
        if self.server is not None:
            self.server.broadcast(message)
        elif self.child is not None and self.child.poll() is None and not self.closing.is_set():
            try:
                self.queue.put_nowait(message)
            except queue.Full as exc:
                raise Fault("not_connected", "relay child is not reading") from exc
        else:
            raise Fault("not_connected", "relay output is unavailable")

    def handle(self, cmd):
        message = live_message(cmd.kind, cmd.value)
        if cmd.kind == "button" and "state" in cmd.value:
            name = cmd.value["name"]
            with self.held_lock:
                if cmd.value["state"] == "down":
                    self._send(message)
                    self.held.add(name)
                else:
                    self._send(message)
                    self.held.discard(name)
        else:
            self._send(message)

    def stop_motion(self):
        self.stop()

    def stop(self):
        with self.held_lock:
            names = sorted(self.held)
            self.held.clear()
            for name in names:
                try:
                    self._send({"type": "key_up", "command": name})
                except Fault:
                    pass

    def close(self):
        if self.server is not None:
            self.server.close()
            self.server = None
        if self.worker is not None:
            # Give queued safety releases a brief chance to reach the child.
            end = time.monotonic() + 0.5
            while not self.queue.empty() and time.monotonic() < end:
                time.sleep(0.01)
            self.closing.set()
            self.worker.join(timeout=0.5)
            if self.child is not None:
                try:
                    self.child.stdin.close()
                except OSError:
                    pass
                try:
                    self.child.wait(timeout=0.5)
                except subprocess.TimeoutExpired:
                    self.child.terminate()
                    try:
                        self.child.wait(timeout=0.5)
                    except subprocess.TimeoutExpired:
                        self.child.kill()
                        self.child.wait()
