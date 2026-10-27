"""The agent browser's egress guard (openvibe_computer/egress.py, Node v0.25.0): every connection is resolved, checked and
made to the address checked; a refused one gets 403. In a real browser, a page's own script cannot reach a refused
address. The plugin refuses the machine's own network below level full, and browser_egress: open turns the guard off."""
import http.server
import io
import os
import shutil
import socket
import socketserver
import sys
import tempfile
import threading
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
sys.path.insert(0, os.path.dirname(HERE))
sys.path.insert(0, os.path.join(os.path.dirname(os.path.dirname(HERE)), "sdk"))

from openvibe_computer import Computer  # noqa: E402
from openvibe_computer.browser import Browser, find_binary  # noqa: E402
from openvibe_computer.egress import EgressProxy, not_public  # noqa: E402
from openvibe_plugin import Fault, Runtime  # noqa: E402
from test_computer import Out  # noqa: E402


class Page(http.server.BaseHTTPRequestHandler):
    seen = []

    def do_GET(self):
        Page.seen.append((self.server.server_address[1], self.path, self.headers.get("Connection")))
        body = b"<!doctype html><title>Here</title><p>hello</p>"
        self.send_response(200)
        self.send_header("Content-Type", "text/html")
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Access-Control-Allow-Origin", "*")
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *a):
        pass


def serve():
    srv = socketserver.ThreadingTCPServer(("127.0.0.1", 0), Page)
    srv.daemon_threads = True
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    return srv


def ask(port, raw):
    s = socket.create_connection(("127.0.0.1", port), timeout=5)
    s.sendall(raw)
    data = b""
    while True:
        chunk = s.recv(65536)
        if not chunk:
            break
        data += chunk
        if b"\r\n\r\n" in data and raw.startswith(b"CONNECT"):
            break
    return s, data


class NotPublicTest(unittest.TestCase):
    def test_addresses(self):
        for ip in ("10.0.0.1", "172.16.5.4", "192.168.1.1", "127.0.0.1", "169.254.1.1", "100.64.0.1", "0.0.0.0",
                   "::1", "fe80::1", "fd00::1", "::ffff:192.168.1.1", "224.0.0.1", "not an address"):
            self.assertTrue(not_public(ip), ip)
        for ip in ("8.8.8.8", "93.184.215.14", "2606:4700::1111", "::ffff:8.8.8.8"):
            self.assertFalse(not_public(ip), ip)


class EgressProxyTest(unittest.TestCase):
    def setUp(self):
        self.ok, self.no = serve(), serve()
        self.addCleanup(self.ok.shutdown)
        self.addCleanup(self.no.shutdown)
        self.no_port = self.no.server_address[1]
        self.proxy = EgressProxy(blocked=lambda ip, port: port == self.no_port)
        self.port = self.proxy.start()
        self.addCleanup(self.proxy.stop)

    def test_connect_is_tunnelled_or_refused(self):
        s, head = ask(self.port, b"CONNECT 127.0.0.1:%d HTTP/1.1\r\nHost: x\r\n\r\n" % self.ok.server_address[1])
        self.assertTrue(head.startswith(b"HTTP/1.1 200"), head)
        s.sendall(b"GET /through HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")
        body = b""
        while True:
            chunk = s.recv(65536)
            if not chunk:
                break
            body += chunk
        self.assertIn(b"hello", body)
        s.close()
        s, head = ask(self.port, b"CONNECT 127.0.0.1:%d HTTP/1.1\r\nHost: x\r\n\r\n" % self.no_port)
        self.assertTrue(head.startswith(b"HTTP/1.1 403"), head)
        s.close()
        self.assertEqual(self.proxy.refused, 1)

    def test_plain_http_goes_on_in_origin_form(self):
        Page.seen = []
        s, _ = ask(self.port, b"GET http://127.0.0.1:%d/plain?x=1 HTTP/1.1\r\nHost: 127.0.0.1\r\nProxy-Connection: keep-alive\r\n\r\n" % self.ok.server_address[1])
        s.close()
        self.assertEqual(Page.seen[-1][1:], ("/plain?x=1", "close"), "origin form, one request per connection")
        s, data = ask(self.port, b"GET http://127.0.0.1:%d/ HTTP/1.1\r\nHost: 127.0.0.1\r\n\r\n" % self.no_port)
        s.close()
        self.assertTrue(data.startswith(b"HTTP/1.1 403"), data)

    def test_a_name_with_any_refused_address_is_refused(self):
        # One public and one private address: the name is refused, so no answer can pick the private one later.
        proxy = EgressProxy(blocked=lambda ip, port: not_public(ip), resolve=lambda host, port: ["93.184.215.14", "192.168.1.1"])
        port = proxy.start()
        self.addCleanup(proxy.stop)
        s, head = ask(port, b"CONNECT sneaky.example:443 HTTP/1.1\r\nHost: x\r\n\r\n")
        s.close()
        self.assertTrue(head.startswith(b"HTTP/1.1 403"), head)
        for bad in (b"CONNECT :443 HTTP/1.1\r\n\r\n", b"GET /relative HTTP/1.1\r\n\r\n", b"CONNECT a:99999 HTTP/1.1\r\n\r\n"):
            s, data = ask(port, bad)
            s.close()
            self.assertTrue(data.startswith(b"HTTP/1.1 400"), (bad, data))


class PluginEgressTest(unittest.TestCase):
    def boot(self, **config):
        out = Out()
        plugin = Computer(env={}, platform="linux", which=lambda n: "/usr/bin/" + n)
        rt = Runtime(plugin, infile=io.StringIO(""), outfile=out)
        rt.dispatch({"op": "hello", "config": {"level": "trusted", "roots": [tempfile.gettempdir()], "notify": False, **config}})
        self.addCleanup(plugin.egress.stop)
        return plugin, out

    def test_the_machines_own_network_is_refused_below_full(self):
        plugin, out = self.boot()
        self.assertEqual(out.msgs()[0]["capabilities"]["browser"]["egress"], "public")
        self.assertTrue(plugin._egress_blocked("192.168.1.1", 80))
        self.assertTrue(plugin._egress_blocked("127.0.0.1", 631))
        self.assertFalse(plugin._egress_blocked("93.184.215.14", 443))
        args = plugin._browser_args()
        self.assertTrue(args[0].startswith("--proxy-server=http://127.0.0.1:"))
        self.assertIn("--proxy-bypass-list=<-loopback>", args)
        plugin.level = "full"
        self.assertFalse(plugin._egress_blocked("192.168.1.1", 80), "full reaches the local network, as open does")

    def test_open_turns_it_off_and_a_bad_value_is_refused(self):
        plugin, _ = self.boot(browser_egress="open")
        self.assertFalse(plugin._egress_blocked("192.168.1.1", 80))
        self.assertEqual(plugin._browser_args(), [])
        with self.assertRaises(Fault):
            plugin._configure({"level": "trusted", "browser_egress": "sometimes"})


@unittest.skipUnless(find_binary(shutil.which, sys.platform), "no Chromium-family browser on this machine")
class RealEgressTest(unittest.TestCase):
    def test_a_pages_script_cannot_reach_a_refused_address(self):
        ok, no = serve(), serve()
        self.addCleanup(ok.shutdown)
        self.addCleanup(no.shutdown)
        no_port = no.server_address[1]
        proxy = EgressProxy(blocked=lambda ip, port: port == no_port)
        self.addCleanup(proxy.stop)
        profile = tempfile.mkdtemp(prefix="ov-egress-")
        self.addCleanup(lambda: shutil.rmtree(profile, ignore_errors=True))
        extra = (["--no-sandbox"] if os.environ.get("CI") else []) + proxy.args()
        b = Browser(find_binary(shutil.which, sys.platform), profile, extra_args=extra)
        self.addCleanup(b.close)
        self.assertEqual(b.open("http://127.0.0.1:%d/" % ok.server_address[1])["title"], "Here", "an allowed address loads")
        got = b._eval("fetch('http://127.0.0.1:%d/probe').then((r) => 'reached ' + r.status).catch(() => 'refused')" % no_port)
        self.assertEqual(got, "refused", "the page's own script never reaches it")
        got = b._eval("fetch('http://127.0.0.1:%d/again').then((r) => 'reached ' + r.status).catch(() => 'refused')" % ok.server_address[1])
        self.assertEqual(got, "reached 200")


if __name__ == "__main__":
    unittest.main()
