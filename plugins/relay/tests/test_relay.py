import base64
import io
import json
import os
import socket
import sys
import time

import pytest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.dirname(HERE))
sys.path.insert(0, os.path.join(os.path.dirname(os.path.dirname(HERE)), "sdk"))

from openvibe_plugin import Fault, Runtime  # noqa: E402
from openvibe_relay import Relay, live_message  # noqa: E402


class Clock:
    now = 10.0

    def __call__(self):
        return self.now


def wait_for(predicate, timeout=3):
    end = time.monotonic() + timeout
    while time.monotonic() < end:
        if predicate():
            return
        time.sleep(0.01)
    assert predicate()


def start(config):
    p = Relay()
    out = io.StringIO()
    clock = Clock()
    rt = Runtime(p, infile=io.StringIO(""), outfile=out, clock=clock)
    rt.dispatch({"op": "hello", "config": config})
    assert json.loads(out.getvalue().splitlines()[-1]) == {"op": "ready"}
    return p, rt, out, clock


def command(rt, out, kind, value, cid="c", deadline=300):
    rt.dispatch({"op": "command", "id": cid, "kind": kind, "value": value, "deadline_ms": deadline})
    return json.loads(out.getvalue().splitlines()[-1])


def read_lines(path):
    try:
        with open(path) as f:
            return [json.loads(line) for line in f]
    except FileNotFoundError:
        return []


def child_config(path, restart=False):
    code = ("import sys,json,os\n"
            "p=sys.argv[1]\n"
            "for line in sys.stdin:\n"
            " with open(p,'a') as f: f.write(line)\n"
            " if len(open(p).readlines()) == 1 and len(sys.argv)>2: break\n")
    argv = [sys.executable, "-c", code, str(path)]
    if restart:
        argv.append("once")
    return {"output": "child", "argv": argv}


@pytest.mark.parametrize("kind,value,expected", [
    ("button", {"name": "forward"}, {"type": "command", "command": "forward"}),
    ("button", {"name": "forward", "state": "down"}, {"type": "key_down", "command": "forward"}),
    ("button", {"name": "forward", "state": "up"}, {"type": "key_up", "command": "forward"}),
    ("point", {"x": 0.123456, "y": 1}, {"type": "video_click", "x": 0.1235, "y": 1}),
])
def test_live_shapes(kind, value, expected):
    assert live_message(kind, value) == expected


def test_refuses_other_kinds_and_bad_values():
    with pytest.raises(Fault) as error:
        live_message("drive", {"throttle": 1})
    assert error.value.code == "unsupported"
    with pytest.raises(Fault) as error:
        live_message("point", {"x": 2, "y": 0})
    assert error.value.code == "bad_value"


def test_child_output_and_eof_release(tmp_path):
    path = tmp_path / "messages.jsonl"
    p, rt, out, _ = start(child_config(path))
    try:
        assert command(rt, out, "button", {"name": "lights"}) == {"op": "ack", "id": "c"}
        assert command(rt, out, "point", {"x": 0.25, "y": 0.75}) == {"op": "ack", "id": "c"}
        assert command(rt, out, "button", {"name": "forward", "state": "down"}) == {"op": "ack", "id": "c"}
        assert command(rt, out, "drive", {"throttle": 1})["fault_code"] == "unsupported"
        rt.shutdown("eof")
        wait_for(lambda: len(read_lines(path)) == 4)
        assert read_lines(path) == [
            {"type": "command", "command": "lights"},
            {"type": "video_click", "x": 0.25, "y": 0.75},
            {"type": "key_down", "command": "forward"},
            {"type": "key_up", "command": "forward"},
        ]
    finally:
        rt.shutdown()
        p.close()


def test_child_restarts_after_exit(tmp_path):
    path = tmp_path / "messages.jsonl"
    p, rt, out, _ = start(child_config(path, restart=True))
    try:
        first_pid = p.child.pid
        assert command(rt, out, "button", {"name": "first"})["op"] == "ack"
        wait_for(lambda: len(read_lines(path)) == 1)
        wait_for(lambda: p.child is not None and p.child.pid != first_pid and p.child.poll() is None)
        assert command(rt, out, "button", {"name": "second"})["op"] == "ack"
        wait_for(lambda: len(read_lines(path)) == 2)
        assert [x["command"] for x in read_lines(path)] == ["first", "second"]
    finally:
        rt.shutdown()


def test_missed_heartbeat_releases_hold(tmp_path):
    path = tmp_path / "messages.jsonl"
    p, rt, out, clock = start(child_config(path))
    try:
        assert command(rt, out, "button", {"name": "forward", "state": "down"}, deadline=5000)["op"] == "ack"
        wait_for(lambda: len(read_lines(path)) == 1)
        clock.now += 1.1
        rt.tick()
        wait_for(lambda: len(read_lines(path)) == 2)
        assert read_lines(path)[-1] == {"type": "key_up", "command": "forward"}
        assert command(rt, out, "button", {"name": "forward", "state": "down"})["fault_code"] == "no_heartbeat"
    finally:
        rt.shutdown()


def test_deadline_releases_hold(tmp_path):
    path = tmp_path / "messages.jsonl"
    p, rt, out, clock = start(child_config(path))
    try:
        assert command(rt, out, "button", {"name": "forward", "state": "down"})["op"] == "ack"
        wait_for(lambda: len(read_lines(path)) == 1)
        clock.now += 0.31
        rt.tick()
        wait_for(lambda: len(read_lines(path)) == 2)
        assert read_lines(path)[-1] == {"type": "key_up", "command": "forward"}
    finally:
        rt.shutdown()


def _recv_exact(sock, n):
    data = b""
    while len(data) < n:
        data += sock.recv(n - len(data))
    return data


def _recv_ws(sock):
    first, size = _recv_exact(sock, 2)
    assert first == 0x81
    if size == 126:
        size = int.from_bytes(_recv_exact(sock, 2), "big")
    return json.loads(_recv_exact(sock, size))


def test_websocket_broadcast():
    with socket.socket() as probe:
        try:
            probe.bind(("127.0.0.1", 0))
        except OSError as exc:
            pytest.skip("loopback bind unavailable: %s" % exc)
        port = probe.getsockname()[1]
    p, rt, out, _ = start({"output": "websocket", "port": port})
    clients = []
    try:
        for _ in range(2):
            sock = socket.create_connection(("127.0.0.1", port), timeout=2)
            sock.settimeout(2)
            key = base64.b64encode(os.urandom(16)).decode()
            sock.sendall(("GET /ws/control?mode=hardware HTTP/1.1\r\nHost: localhost\r\n"
                          "Upgrade: websocket\r\nConnection: Upgrade\r\n"
                          "Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: %s\r\n\r\n" % key).encode())
            response = b""
            while b"\r\n\r\n" not in response:
                response += sock.recv(1024)
            assert b"101 Switching Protocols" in response
            clients.append(sock)
        wait_for(lambda: len(p.server.clients) == 2)
        assert command(rt, out, "button", {"name": "horn"})["op"] == "ack"
        assert [_recv_ws(c) for c in clients] == [
            {"type": "command", "command": "horn"},
            {"type": "command", "command": "horn"},
        ]
        assert command(rt, out, "button", {"name": "forward", "state": "down"})["op"] == "ack"
        assert [_recv_ws(c)["type"] for c in clients] == ["key_down", "key_down"]
        rt.shutdown("eof")
        assert [_recv_ws(c) for c in clients] == [
            {"type": "key_up", "command": "forward"},
            {"type": "key_up", "command": "forward"},
        ]
    finally:
        rt.shutdown()
        for c in clients:
            c.close()
