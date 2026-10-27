"""The agent browser's egress guard (Node v0.25.0): a forward proxy on 127.0.0.1 that every agent browser goes through.

The browser's ``open`` already refuses an address on the machine's own network below level full, but a page it opened
could still reach the network with its own scripts (fetch, an image, a WebSocket, a redirect): the owner's router, a
printer, another computer at home. Every connection the browser makes now comes here first. Its address is resolved,
checked (``blocked(ip, port)``: private, loopback, link-local and other non-public addresses, below level full), and
then connected to the exact address that was checked, so a name cannot resolve to a public address for the check and a
private one for the connection.

    proxy = EgressProxy(blocked=lambda ip, port: ...)  ;  port = proxy.start()  ;  ...  ;  proxy.stop()

HTTPS and WebSockets arrive as CONNECT and are tunnelled; plain HTTP arrives in absolute form and is sent on in origin
form with ``Connection: close`` (one request per connection, which the browser handles). The browser is started with
``--proxy-server``, ``--proxy-bypass-list=<-loopback>`` (so loopback goes through the guard too) and WebRTC limited to
proxied traffic (no UDP around it). QUIC is off whenever a proxy is set.
"""
from __future__ import annotations

import ipaddress
import select
import socket
import threading

MAX_HEAD = 64 * 1024
IDLE_S = 300.0
CONNECT_TIMEOUT_S = 15.0


def not_public(ip):
    """Whether an address is not on the public internet: private, loopback, link-local, multicast, reserved, ..."""
    try:
        a = ipaddress.ip_address(ip.split("%", 1)[0])
    except ValueError:
        return True
    if isinstance(a, ipaddress.IPv6Address) and a.ipv4_mapped:
        a = a.ipv4_mapped
    return not a.is_global or a.is_multicast


class EgressProxy:
    def __init__(self, blocked, resolve=None, log=None):
        self.blocked = blocked                      # (ip, port) -> True to refuse
        self.resolve = resolve or (lambda host, port: [ai[4][0] for ai in socket.getaddrinfo(host, port, type=socket.SOCK_STREAM)])
        self.log = log or (lambda *a: None)
        self.sock = None
        self.port = None
        self.mu = threading.Lock()
        self.refused = 0

    def start(self):
        with self.mu:
            if self.sock is not None:
                return self.port
            s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
            s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
            s.bind(("127.0.0.1", 0))
            s.listen(128)
            self.sock, self.port = s, s.getsockname()[1]
        threading.Thread(target=self._accept, args=(s,), name="computer-egress", daemon=True).start()
        return self.port

    def stop(self):
        with self.mu:
            s, self.sock, self.port = self.sock, None, None
        if s is not None:
            try:
                s.close()
            except OSError:
                pass

    def args(self):
        """The browser's arguments to go through this guard."""
        port = self.start()
        return ["--proxy-server=http://127.0.0.1:%d" % port, "--proxy-bypass-list=<-loopback>",
                "--force-webrtc-ip-handling-policy=disable_non_proxied_udp"]

    # ---- one connection ---------------------------------------------------------------------------------------------
    def _accept(self, s):
        while True:
            try:
                conn, _ = s.accept()
            except OSError:
                return
            threading.Thread(target=self._serve, args=(conn,), name="computer-egress-conn", daemon=True).start()

    def _serve(self, client):
        upstream = None
        try:
            client.settimeout(CONNECT_TIMEOUT_S)
            head = b""
            while b"\r\n\r\n" not in head:
                chunk = client.recv(8192)
                if not chunk:
                    return
                head += chunk
                if len(head) > MAX_HEAD:
                    return self._answer(client, 431, "Request Header Fields Too Large")
            raw_head, rest = head.split(b"\r\n\r\n", 1)
            lines = raw_head.decode("latin-1").split("\r\n")
            parts = lines[0].split(" ")
            if len(parts) != 3:
                return self._answer(client, 400, "Bad Request")
            method, target, version = parts
            if method == "CONNECT":
                host, port = self._split(target, 443)
            else:
                if not target.lower().startswith("http://"):
                    return self._answer(client, 400, "Bad Request")
                hostport, _, path = target[7:].partition("/")
                host, port = self._split(hostport, 80)
            if host is None:
                return self._answer(client, 400, "Bad Request")
            upstream = self._connect(host, port)
            if upstream is None:
                return self._answer(client, 403, "Forbidden by the agent's network guard")
            if method == "CONNECT":
                client.sendall(b"HTTP/1.1 200 Connection Established\r\n\r\n")
                if rest:
                    upstream.sendall(rest)
            else:
                headers = [h for h in lines[1:] if h and not h.lower().startswith(("proxy-connection:", "connection:", "keep-alive:"))]
                out = "%s /%s %s\r\n%s\r\nConnection: close\r\n\r\n" % (method, path, version, "\r\n".join(headers))
                upstream.sendall(out.encode("latin-1") + rest)
            self._relay(client, upstream)
        except (OSError, ValueError, UnicodeError):
            pass
        finally:
            for x in (client, upstream):
                if x is not None:
                    try:
                        x.close()
                    except OSError:
                        pass

    @staticmethod
    def _split(hostport, default):
        hostport = hostport.strip()
        if hostport.startswith("["):
            host, _, tail = hostport[1:].partition("]")
            port = tail[1:] if tail.startswith(":") else ""
        elif hostport.count(":") == 1:
            host, port = hostport.split(":")
        else:
            host, port = hostport, ""
        if not host:
            return None, None
        try:
            port = int(port) if port else default
        except ValueError:
            return None, None
        return (host, port) if 0 < port < 65536 else (None, None)

    def _connect(self, host, port):
        """A connection to the first address of host that is allowed, or None. The address checked is the one used."""
        try:
            addrs = self.resolve(host, port)
        except (OSError, UnicodeError):
            return None
        for ip in addrs:
            if self.blocked(ip, port):
                with self.mu:
                    self.refused += 1
                self.log("egress refused %s:%d (%s)", host, port, ip)
                return None   # one non-public address is enough to refuse the name
        for ip in addrs:
            try:
                return socket.create_connection((ip, port), timeout=CONNECT_TIMEOUT_S)
            except OSError:
                continue
        return None

    @staticmethod
    def _answer(client, code, text):
        body = text.encode()
        client.sendall(b"HTTP/1.1 %d %s\r\nContent-Type: text/plain\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s"
                       % (code, text.encode(), len(body), body))

    @staticmethod
    def _relay(a, b):
        a.settimeout(None)
        b.settimeout(None)
        socks = [a, b]
        while True:
            r, _, _ = select.select(socks, [], [], IDLE_S)
            if not r:
                return
            for s in r:
                data = s.recv(65536)
                if not data:
                    return
                (b if s is a else a).sendall(data)
