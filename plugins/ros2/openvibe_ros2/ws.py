"""A small WebSocket client (RFC 6455) on the standard library, enough for rosbridge's JSON protocol.

Text frames only, client-masked, with ping/pong, close and continuation frames. ``ws://`` and ``wss://`` (TLS with the
system's trust store and hostname checks). A message larger than ``max_message`` closes the connection.
"""

import base64
import hashlib
import os
import socket
import ssl
import struct
import threading
import urllib.parse

GUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"


class WebSocketError(Exception):
    pass


class WebSocket:
    def __init__(self, url, timeout=5.0, max_message=16 * 1024 * 1024):
        u = urllib.parse.urlsplit(url)
        if u.scheme not in ("ws", "wss"):
            raise WebSocketError("the rosbridge URL must be ws:// or wss://, not %r" % (url,))
        host = u.hostname or "127.0.0.1"
        port = u.port or (443 if u.scheme == "wss" else 80)
        path = (u.path or "/") + ("?" + u.query if u.query else "")
        self.max_message = max_message
        self._send_lock = threading.Lock()
        self._closed = False
        sock = socket.create_connection((host, port), timeout=timeout)
        try:
            if u.scheme == "wss":
                sock = ssl.create_default_context().wrap_socket(sock, server_hostname=host)
            key = base64.b64encode(os.urandom(16)).decode("ascii")
            hostport = host if port in (80, 443) else "%s:%d" % (host, port)
            if ":" in host and not host.startswith("["):
                hostport = "[%s]:%d" % (host, port)
            sock.sendall(("GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"
                          "Sec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n" % (path, hostport, key)).encode("ascii"))
            raw = b""
            while b"\r\n\r\n" not in raw:
                chunk = sock.recv(4096)
                if not chunk:
                    raise WebSocketError("the server closed the connection during the handshake")
                raw += chunk
                if len(raw) > 16384:
                    raise WebSocketError("the handshake answer is too long")
            head, self._buf = raw.split(b"\r\n\r\n", 1)
            lines = head.decode("latin-1").split("\r\n")
            if len(lines[0].split()) < 2 or lines[0].split()[1] != "101":
                raise WebSocketError("the server did not switch to WebSocket: %s" % lines[0][:80])
            headers = {}
            for line in lines[1:]:
                k, _, v = line.partition(":")
                headers[k.strip().lower()] = v.strip()
            want = base64.b64encode(hashlib.sha1((key + GUID).encode("ascii")).digest()).decode("ascii")
            if headers.get("sec-websocket-accept") != want:
                raise WebSocketError("the server's handshake answer does not match")
            sock.settimeout(None)
        except Exception:
            sock.close()
            raise
        self.sock = sock

    # Frames.
    def _send_frame(self, opcode, payload):
        header = bytearray([0x80 | opcode])
        n = len(payload)
        if n < 126:
            header.append(0x80 | n)
        elif n < 65536:
            header.append(0x80 | 126)
            header += struct.pack(">H", n)
        else:
            header.append(0x80 | 127)
            header += struct.pack(">Q", n)
        mask = os.urandom(4)
        header += mask
        body = bytes(b ^ mask[i % 4] for i, b in enumerate(payload)) if n < 4096 else _mask_fast(payload, mask)
        with self._send_lock:
            self.sock.sendall(bytes(header) + body)

    def send_text(self, text):
        if self._closed:
            raise WebSocketError("closed")
        self._send_frame(0x1, text.encode("utf-8"))

    def _read_exact(self, n):
        while len(self._buf) < n:
            chunk = self.sock.recv(max(65536, n - len(self._buf)))
            if not chunk:
                raise WebSocketError("the connection closed")
            self._buf += chunk
        out, self._buf = self._buf[:n], self._buf[n:]
        return out

    def recv(self):
        """The next text message, or None when the connection closes."""
        parts = []
        size = 0
        try:
            while True:
                b1, b2 = self._read_exact(2)
                fin, opcode = b1 & 0x80, b1 & 0x0F
                n = b2 & 0x7F
                if n == 126:
                    n = struct.unpack(">H", self._read_exact(2))[0]
                elif n == 127:
                    n = struct.unpack(">Q", self._read_exact(8))[0]
                mask = self._read_exact(4) if b2 & 0x80 else None
                if size + n > self.max_message:
                    raise WebSocketError("a message is larger than %d bytes" % self.max_message)
                payload = self._read_exact(n)
                if mask:
                    payload = _mask_fast(payload, mask)
                if opcode == 0x8:
                    self.close()
                    return None
                if opcode == 0x9:
                    self._send_frame(0xA, payload)
                    continue
                if opcode == 0xA:
                    continue
                if opcode in (0x1, 0x2, 0x0):
                    parts.append(payload)
                    size += n
                    if fin:
                        return b"".join(parts).decode("utf-8", "replace")
        except (OSError, WebSocketError, ValueError):
            self.close()
            return None

    def close(self):
        if self._closed:
            return
        self._closed = True
        try:
            self._send_frame(0x8, b"")
        except Exception:
            pass
        try:
            self.sock.close()
        except Exception:
            pass


def _mask_fast(payload, mask):
    n = len(payload)
    if not n:
        return b""
    key = int.from_bytes((mask * (n // 4 + 1))[:n], "big")
    return (int.from_bytes(payload, "big") ^ key).to_bytes(n, "big")
