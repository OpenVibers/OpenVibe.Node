import base64
import hashlib
import io
import json
import math
import os
import socket
import struct
import sys
import tempfile
import threading
import time

import pytest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.dirname(HERE))
sys.path.insert(0, os.path.join(os.path.dirname(os.path.dirname(HERE)), "sdk"))

from openvibe_plugin import Runtime  # noqa: E402
from openvibe_ros2 import DEFAULTS, RclpyBus, RosbridgeBus, Ros2, _fill, _to_dict  # noqa: E402
from openvibe_ros2.ws import GUID, WebSocket, WebSocketError  # noqa: E402

JPEG = b"\xff\xd8\xff\xe0" + b"\x00" * 64 + b"\xff\xd9"


class Clock:
    t = 10.0

    def __call__(self):
        return self.t


class FakeBus:
    """Records publishes and subscriptions; tests deliver topic messages with deliver()."""

    def __init__(self):
        self.alive = True
        self.published = []
        self.subs = {}
        self.closed = False

    def publish(self, topic, type_, msg):
        if not self.alive:
            raise OSError("closed")
        self.published.append((topic, type_, json.loads(json.dumps(msg))))

    def subscribe(self, topic, type_, callback, throttle_ms=0):
        self.subs[topic] = (type_, callback)

    def deliver(self, topic, msg):
        self.subs[topic][1](msg)

    def close(self):
        self.closed = True
        self.alive = False


class Harness:
    def __init__(self, fail=0, **config):
        self.clock = Clock()
        self.buses = []
        self.fail = fail

        def factory(cfg, log):
            if self.fail:
                self.fail -= 1
                raise OSError("connection refused")
            b = FakeBus()
            self.buses.append(b)
            return b

        self.p = Ros2(bus_factory=factory, clock=self.clock)
        self.p.log = lambda *a: None
        self.out = io.StringIO()
        self.rt = Runtime(self.p, infile=io.StringIO(""), outfile=self.out, clock=self.clock)
        self.frames = tempfile.mkdtemp()
        self.config = dict(config)

    def lines(self):
        return [json.loads(x) for x in self.out.getvalue().splitlines() if x.strip()]

    def ops(self, op):
        return [m for m in self.lines() if m.get("op") == op]

    def hello(self, probe=False):
        self.rt.dispatch({"op": "hello", "config": self.config, "probe": probe})

    def cmd(self, cid, kind, value, deadline_ms=300):
        self.rt.dispatch({"op": "heartbeat", "t": 0})
        self.rt.dispatch({"op": "command", "id": cid, "kind": kind, "value": value, "deadline_ms": deadline_ms})

    @property
    def bus(self):
        return self.buses[-1]

    def twists(self):
        return [m for (t, ty, m) in self.bus.published if ty == "geometry_msgs/msg/Twist"]


def test_describe_touches_nothing_and_names_what_is_configured():
    h = Harness(image_topic="/camera/image_raw/compressed", say_topic="/say", holonomic=True)
    h.hello(probe=True)
    d = h.ops("describe")[0]
    assert d["driver"] == "ros2" and d["motion_kinds"] == ["drive"]
    caps = d["capabilities"]
    assert caps["drive"]["type"] == "mecanum"
    assert caps["camera"] == {"jpeg": True} and caps["say"]["topic"] == "/say"
    assert caps["sensors"]["distance_cm"] and caps["bridge"]["kind"] == "ros2"
    assert h.buses == [] and not h.ops("ready"), "probe mode never connects"


def test_setup_subscribes_publishes_a_zero_twist_and_says_ready():
    h = Harness()
    h.hello()
    assert h.ops("ready")
    assert set(h.bus.subs) == {"/battery_state", "/scan", "/odom"}
    assert h.bus.subs["/scan"][0] == "sensor_msgs/msg/LaserScan"
    assert h.twists()[-1] == {"linear": {"x": 0.0, "y": 0.0, "z": 0.0}, "angular": {"x": 0.0, "y": 0.0, "z": 0.0}}


def test_drive_scales_to_twist_with_ros_signs_and_stops_at_the_deadline():
    h = Harness(max_linear=0.5, max_angular=2.0)
    h.hello()
    h.cmd("c1", "drive", {"throttle": 0.5, "steer": 0.25})
    assert h.ops("ack")[-1]["id"] == "c1"
    t = h.twists()[-1]
    assert t["linear"]["x"] == pytest.approx(0.25) and t["angular"]["z"] == pytest.approx(-0.5), "steer right is clockwise"
    h.clock.t += 0.31
    h.rt.tick()
    assert h.twists()[-1]["linear"]["x"] == 0 and h.twists()[-1]["angular"]["z"] == 0
    assert "deadline" in h.rt.stop_reasons
    h.cmd("c2", "drive", {"throttle": 9, "steer": -9})
    t = h.twists()[-1]
    assert t["linear"]["x"] == pytest.approx(0.5) and t["angular"]["z"] == pytest.approx(2.0), "clamped to full scale"


def test_holonomic_drive_uses_x_forward_y_left_rotation_ccw():
    h = Harness(holonomic=True, max_linear=1.0, max_angular=1.0)
    h.hello()
    h.cmd("h1", "drive", {"x": 0.5, "y": 0.25, "rotation": -0.5})
    t = h.twists()[-1]
    assert (t["linear"]["x"], t["linear"]["y"], t["angular"]["z"]) == (0.5, 0.25, -0.5)


def test_estop_halt_heartbeat_and_eof_publish_zero_and_refuse_motion():
    h = Harness()
    h.hello()
    h.cmd("m1", "drive", {"throttle": 1})
    h.rt.dispatch({"op": "estop"})
    assert h.twists()[-1]["linear"]["x"] == 0
    h.cmd("m2", "drive", {"throttle": 1})
    assert h.ops("nack")[-1] == {"op": "nack", "id": "m2", "fault_code": "estopped"}
    h.rt.dispatch({"op": "resume"})
    h.cmd("m3", "drive", {"throttle": 1})
    h.clock.t += 1.5
    h.rt.tick()
    assert h.ops("event")[-1]["name"] == "heartbeat_lost" and h.twists()[-1]["linear"]["x"] == 0
    n = len(h.twists())
    h.rt.shutdown("eof")
    assert len(h.twists()) == n + 1 and h.twists()[-1]["linear"]["x"] == 0 and h.bus.closed


def test_say_publishes_a_string_only_when_configured():
    h = Harness(say_topic="/say")
    h.hello()
    h.cmd("s1", "say", {"text": "Hello from OpenVibe"})
    assert ("/say", "std_msgs/msg/String", {"data": "Hello from OpenVibe"}) in h.bus.published
    h2 = Harness()
    h2.hello()
    h2.cmd("s2", "say", {"text": "hi"})
    assert h2.ops("nack")[-1]["fault_code"] == "unsupported"


def test_telemetry_battery_scan_range_and_odom():
    h = Harness()
    h.hello()
    h.bus.deliver("/battery_state", {"percentage": 0.42, "voltage": 12.06})
    # 360 rays from -pi: the nearest return straight ahead (index 180) counts, a nearer one behind does not.
    ranges = [5.0] * 360
    ranges[180] = 0.8
    ranges[0] = 0.1
    ranges[181] = float("inf")
    h.bus.deliver("/scan", {"angle_min": -math.pi, "angle_increment": 2 * math.pi / 360, "range_min": 0.05, "range_max": 12, "ranges": ranges})
    yaw = 0.5
    h.bus.deliver("/odom", {"pose": {"pose": {"position": {"x": 1.234, "y": -2.0}, "orientation": {"x": 0, "y": 0, "z": math.sin(yaw / 2), "w": math.cos(yaw / 2)}}},
                            "twist": {"twist": {"linear": {"x": 0.3, "y": 0.4}}}})
    t = h.p.poll()
    assert t["battery"] == {"percent": 42.0, "volts": 12.06}
    assert t["sensors"]["distance_cm"] == 80.0
    assert t["sensors"]["odom"] == {"x": 1.234, "y": -2.0, "yaw": 0.5, "speed": 0.5}
    hr = Harness(range_topic="/sonar")
    hr.hello()
    assert "/sonar" in hr.bus.subs and "/scan" not in hr.bus.subs, "a Range topic replaces the scan"
    hr.bus.deliver("/sonar", {"range": 0.355})
    assert hr.p.poll()["sensors"]["distance_cm"] == 35.5


def test_camera_frames_from_a_jpeg_compressed_image_rate_limited():
    h = Harness(image_topic="/cam/compressed", fps=2)
    h.config["frame_dir"] = h.frames
    h.hello()
    h.bus.deliver("/cam/compressed", {"format": "rgb8; jpeg compressed bgr8", "data": base64.b64encode(JPEG).decode()})
    v = h.ops("video")
    assert len(v) == 1 and v[0]["format"] == "jpeg"
    with open(v[0]["path"], "rb") as f:
        assert f.read() == JPEG
    h.bus.deliver("/cam/compressed", {"format": "jpeg", "data": JPEG})   # rclpy hands bytes
    assert len(h.ops("video")) == 1, "within 1/fps"
    h.clock.t += 0.6
    h.bus.deliver("/cam/compressed", {"format": "jpeg", "data": JPEG})
    assert len(h.ops("video")) == 2
    h.clock.t += 1
    h.bus.deliver("/cam/compressed", {"format": "png", "data": base64.b64encode(b"\x89PNG").decode()})
    assert len(h.ops("video")) == 2, "not JPEG: no frame"


def test_unreachable_ros_faults_and_the_runtime_sets_up_again_until_ready():
    h = Harness(fail=2, retry_s=3)
    h.hello()
    f = h.ops("fault")
    assert f and f[0]["fault_code"] == "ros_unreachable" and not h.ops("ready")
    h.cmd("x1", "drive", {"throttle": 1})
    assert h.ops("nack")[-1]["fault_code"] == "not_ready"
    h.clock.t += 3.1
    h.rt.tick()                     # second failure: still down, no second fault
    assert not h.ops("ready") and len(h.ops("fault")) == 1
    h.clock.t += 3.1
    h.rt.tick()                     # ROS is up
    assert h.ops("ready") and len(h.buses) == 1
    h.cmd("x2", "drive", {"throttle": 1})
    assert h.ops("ack")[-1]["id"] == "x2"
    # The link drops later: a fault, commands refused by the plugin, and ready again once it reconnects.
    h.bus.alive = False
    h.p.poll()
    assert h.ops("fault")[-1]["message"] == "the connection to ROS 2 was lost"
    h.cmd("x3", "drive", {"throttle": 1})
    assert h.ops("nack")[-1]["fault_code"] == "ros_unreachable"
    h.clock.t += 3.1
    h.p.poll()
    assert len(h.buses) == 2 and h.ops("ready")[-1] == {"op": "ready"} and len(h.ops("ready")) == 2
    assert h.twists()[-1]["linear"]["x"] == 0, "a zero Twist first on the new link"


def test_bad_transport_is_a_config_fault():
    h = Harness(transport="dds")
    h.hello()
    assert h.ops("fault")[0]["fault_code"] == "bad_config"


# ── rclpy helpers ─────────────────────────────────────────────────────────────────────────────────────────────────
class Vector3:
    def __init__(self):
        self.x = self.y = self.z = 0.0

    @staticmethod
    def get_fields_and_field_types():
        return {"x": "double", "y": "double", "z": "double"}


class Twist:
    def __init__(self):
        self.linear, self.angular = Vector3(), Vector3()

    @staticmethod
    def get_fields_and_field_types():
        return {"linear": "geometry_msgs/Vector3", "angular": "geometry_msgs/Vector3"}


def test_fill_and_to_dict_round_trip_a_nested_message():
    t = _fill(Twist(), {"linear": {"x": 0.2, "y": 0.0, "z": 0.0}, "angular": {"x": 0.0, "y": 0.0, "z": -1.0}})
    assert t.linear.x == 0.2 and t.angular.z == -1.0
    assert _to_dict(t) == {"linear": {"x": 0.2, "y": 0.0, "z": 0.0}, "angular": {"x": 0.0, "y": 0.0, "z": -1.0}}
    assert _to_dict(bytearray(b"\xff\xd8")) == b"\xff\xd8"


def test_rclpy_bus_with_a_fake_rclpy(monkeypatch):
    import types
    created = {"pubs": [], "subs": []}

    class Pub:
        def __init__(self, cls, topic):
            self.topic, self.sent = topic, []

        def publish(self, m):
            self.sent.append(m)

    class Node:
        def create_publisher(self, cls, topic, qos):
            p = Pub(cls, topic)
            created["pubs"].append(p)
            return p

        def create_subscription(self, cls, topic, cb, qos):
            created["subs"].append((cls, topic, cb))

        def destroy_node(self):
            created["destroyed"] = True

    class Executor:
        def __init__(self):
            self.stop = threading.Event()

        def add_node(self, n):
            pass

        def spin(self):
            self.stop.wait(5)

        def shutdown(self):
            self.stop.set()

    rclpy = types.SimpleNamespace(__name__="rclpy", ok=lambda: False, init=lambda args=None: created.setdefault("init", True),
                                  create_node=lambda name: Node(), executors=types.SimpleNamespace(SingleThreadedExecutor=Executor),
                                  try_shutdown=lambda: created.setdefault("shutdown", True))
    geometry = types.ModuleType("geometry_msgs.msg")
    geometry.Twist = Twist
    monkeypatch.setitem(sys.modules, "geometry_msgs", types.ModuleType("geometry_msgs"))
    monkeypatch.setitem(sys.modules, "geometry_msgs.msg", geometry)
    bus = RclpyBus("openvibe_node", lambda *a: None, rclpy=rclpy)
    bus.publish("/cmd_vel", "geometry_msgs/msg/Twist", {"linear": {"x": 0.1, "y": 0.0, "z": 0.0}, "angular": {"x": 0.0, "y": 0.0, "z": 0.3}})
    assert created["init"] and created["pubs"][0].topic == "/cmd_vel" and created["pubs"][0].sent[0].angular.z == 0.3
    got = []
    bus.subscribe("/cmd_vel_echo", "geometry_msgs/msg/Twist", got.append)
    created["subs"][0][2](_fill(Twist(), {"linear": {"x": 0.5}}))
    assert got == [{"linear": {"x": 0.5, "y": 0.0, "z": 0.0}, "angular": {"x": 0.0, "y": 0.0, "z": 0.0}}]
    bus.close()
    assert created["destroyed"] and created["shutdown"]


# ── The WebSocket client and rosbridge, against a small local server ──────────────────────────────────────────────
class TinyServer:
    """A one-connection WebSocket server: records text frames it receives; send() pushes a text frame."""

    def __init__(self, bad_accept=False):
        self.srv = socket.socket()
        self.srv.bind(("127.0.0.1", 0))
        self.srv.listen(1)
        self.port = self.srv.getsockname()[1]
        self.got = []
        self.conn = None
        self.bad_accept = bad_accept
        self.ready = threading.Event()
        threading.Thread(target=self._serve, daemon=True).start()

    def _serve(self):
        conn, _ = self.srv.accept()
        req = b""
        while b"\r\n\r\n" not in req:
            req += conn.recv(4096)
        key = [l.split(b":", 1)[1].strip() for l in req.split(b"\r\n") if l.lower().startswith(b"sec-websocket-key")][0]
        accept = base64.b64encode(hashlib.sha1(key + GUID.encode()).digest())
        if self.bad_accept:
            accept = b"nope"
        conn.sendall(b"HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + accept + b"\r\n\r\n")
        self.conn = conn
        self.ready.set()
        buf = b""
        while True:
            try:
                chunk = conn.recv(65536)
            except OSError:
                return
            if not chunk:
                return
            buf += chunk
            while len(buf) >= 2:
                n = buf[1] & 0x7F
                off = 2
                if n == 126:
                    n = struct.unpack(">H", buf[2:4])[0]
                    off = 4
                elif n == 127:
                    n = struct.unpack(">Q", buf[2:10])[0]
                    off = 10
                if len(buf) < off + 4 + n:
                    break
                mask = buf[off:off + 4]
                payload = bytes(b ^ mask[i % 4] for i, b in enumerate(buf[off + 4:off + 4 + n]))
                op = buf[0] & 0x0F
                buf = buf[off + 4 + n:]
                if op == 0x1:
                    self.got.append(json.loads(payload))
                elif op == 0x8:
                    return

    def send(self, obj, opcode=0x1):
        data = json.dumps(obj).encode() if opcode == 0x1 else obj
        n = len(data)
        head = bytes([0x80 | opcode]) + (bytes([n]) if n < 126 else bytes([126]) + struct.pack(">H", n) if n < 65536 else bytes([127]) + struct.pack(">Q", n))
        self.conn.sendall(head + data)

    def wait_for(self, pred, timeout=3):
        end = time.time() + timeout
        while time.time() < end:
            if any(pred(m) for m in self.got):
                return True
            time.sleep(0.01)
        return False


def test_rosbridge_bus_advertises_publishes_subscribes_and_dispatches():
    s = TinyServer()
    bus = RosbridgeBus("ws://127.0.0.1:%d" % s.port, lambda *a: None)
    s.ready.wait(2)
    got = []
    bus.subscribe("/battery_state", "sensor_msgs/msg/BatteryState", got.append, 1000)
    bus.publish("/cmd_vel", "geometry_msgs/msg/Twist", {"linear": {"x": 0.1}})
    bus.publish("/cmd_vel", "geometry_msgs/msg/Twist", {"linear": {"x": 0.0}})
    assert s.wait_for(lambda m: m.get("op") == "publish" and m["msg"]["linear"]["x"] == 0.0)
    ops = [(m["op"], m["topic"]) for m in s.got]
    assert ops == [("subscribe", "/battery_state"), ("advertise", "/cmd_vel"), ("publish", "/cmd_vel"), ("publish", "/cmd_vel")], "advertised once"
    assert s.got[0]["type"] == "sensor_msgs/msg/BatteryState" and s.got[0]["throttle_rate"] == 1000
    s.send(b"", opcode=0x9)   # a ping is answered and skipped
    big = "x" * 70000          # a 64-bit-length frame
    s.send({"op": "publish", "topic": "/battery_state", "msg": {"percentage": 0.9, "note": big}})
    end = time.time() + 3
    while not got and time.time() < end:
        time.sleep(0.01)
    assert got and got[0]["percentage"] == 0.9 and len(got[0]["note"]) == 70000
    s.conn.shutdown(socket.SHUT_RDWR)   # FIN even while the server thread is in recv
    s.conn.close()
    end = time.time() + 3
    while bus.alive and time.time() < end:
        time.sleep(0.01)
    assert not bus.alive, "a closed connection marks the bus down"
    bus.close()


def test_websocket_refuses_a_bad_handshake_and_other_schemes():
    s = TinyServer(bad_accept=True)
    with pytest.raises(WebSocketError):
        WebSocket("ws://127.0.0.1:%d/" % s.port)
    with pytest.raises(WebSocketError):
        WebSocket("http://127.0.0.1:9090")


def test_defaults_document_every_config_key():
    readme = open(os.path.join(os.path.dirname(HERE), "README.md")).read()
    for k in DEFAULTS:
        assert "`%s`" % k in readme, k
