"""OpenVibe Node bridge plugin for ROS 2 robots.

The Node runs on the robot's computer (or next to it) and this plugin turns Bot's commands into ROS 2 messages and the
robot's topics into telemetry and camera frames:

* ``drive`` → ``geometry_msgs/msg/Twist`` on ``cmd_vel`` (``throttle``/``steer``, or ``x``/``y``/``rotation`` on a
  holonomic base), scaled by ``max_linear`` m/s and ``max_angular`` rad/s. A zero Twist is published at every stop:
  the deadline of the last drive, a lost heartbeat, ``stop``, ``estop``, ``halt``, stdin closing, and before exit.
* ``say`` → ``std_msgs/msg/String`` on ``say_topic`` (when set).
* ``sensor_msgs/msg/BatteryState`` → battery percent and volts; ``sensor_msgs/msg/Range`` or the front arc of a
  ``sensor_msgs/msg/LaserScan`` → ``distance_cm``; ``nav_msgs/msg/Odometry`` → ``odom`` {x, y, yaw, speed}.
* ``sensor_msgs/msg/CompressedImage`` (JPEG) on ``image_topic`` → camera frames for the Node to publish.

Two ways to reach ROS 2 (``transport``):

* ``rclpy``: the plugin is a ROS 2 node itself. Needs a sourced ROS 2 environment (``rclpy`` importable).
* ``rosbridge``: JSON over WebSocket to a rosbridge server (``ros2 launch rosbridge_server
  rosbridge_websocket_launch.xml``), on this machine or the robot's. No ROS 2 libraries needed here.
* ``auto`` (default): ``rclpy`` when it imports, otherwise ``rosbridge``.

If ROS cannot be reached the plugin reports the ``ros_unreachable`` fault, refuses commands with it, and tries again
every ``retry_s`` seconds; when it reconnects it says ``ready`` again, which ends the fault.
"""

import base64
import importlib
import importlib.util
import json
import math
import os
import shutil
import tempfile
import threading
import time

from openvibe_plugin import Fault, Plugin, clamp

from .ws import WebSocket

FRAME_RING = 4

DEFAULTS = {
    "transport": "auto",                      # auto | rclpy | rosbridge
    "rosbridge_url": "ws://127.0.0.1:9090",
    "node_name": "openvibe_node",
    "cmd_vel": "/cmd_vel",
    "max_linear": 0.3,                        # m/s at full throttle
    "max_angular": 1.5,                       # rad/s at full steer
    "holonomic": False,                       # x/y/rotation drive (mecanum, omni) instead of throttle/steer
    "battery_topic": "/battery_state",
    "range_topic": "",                        # sensor_msgs/Range (an ultrasonic or ToF sensor)
    "scan_topic": "/scan",                    # sensor_msgs/LaserScan: the nearest return in the front arc
    "front_arc_deg": 30,
    "odom_topic": "/odom",
    "image_topic": "",                        # sensor_msgs/CompressedImage, JPEG
    "fps": 5,
    "say_topic": "",                          # std_msgs/String
    "retry_s": 3,
}

TWIST = "geometry_msgs/msg/Twist"


def _num(v, default=0.0):
    try:
        f = float(v)
    except (TypeError, ValueError):
        return default
    return f if f == f else default


# ── Transports ─────────────────────────────────────────────────────────────────────────────────────────────────────
class RosbridgeBus:
    """rosbridge v2 over WebSocket: advertise + publish, subscribe; incoming publishes go to the topic's callbacks."""

    def __init__(self, url, log, connect=WebSocket):
        self.log = log
        self.ws = connect(url)
        self.alive = True
        self.subs = {}
        self.advertised = set()
        self.thread = threading.Thread(target=self._read, name="rosbridge", daemon=True)
        self.thread.start()

    def _send(self, obj):
        self.ws.send_text(json.dumps(obj, separators=(",", ":")))

    def publish(self, topic, type_, msg):
        if topic not in self.advertised:
            self._send({"op": "advertise", "topic": topic, "type": type_})
            self.advertised.add(topic)
        self._send({"op": "publish", "topic": topic, "msg": msg})

    def subscribe(self, topic, type_, callback, throttle_ms=0):
        self.subs.setdefault(topic, []).append(callback)
        self._send({"op": "subscribe", "topic": topic, "type": type_, "throttle_rate": int(throttle_ms), "queue_length": 1})

    def _read(self):
        try:
            while True:
                text = self.ws.recv()
                if text is None:
                    break
                try:
                    m = json.loads(text)
                except ValueError:
                    continue
                if isinstance(m, dict) and m.get("op") == "publish":
                    for cb in self.subs.get(m.get("topic"), ()):
                        try:
                            cb(m.get("msg") or {})
                        except Exception as e:
                            self.log("ros2: %s callback failed: %r", m.get("topic"), e)
        finally:
            self.alive = False

    def close(self):
        self.alive = False
        self.ws.close()


def _to_dict(m):
    """A ROS 2 message as plain data (fields by name; byte arrays as bytes)."""
    if hasattr(m, "get_fields_and_field_types"):
        return {k: _to_dict(getattr(m, k)) for k in m.get_fields_and_field_types()}
    if isinstance(m, (bytes, bytearray)):
        return bytes(m)
    if hasattr(m, "tobytes") and getattr(m, "typecode", None) in ("B", "b"):   # array.array('B')
        return m.tobytes()
    if isinstance(m, (list, tuple)) or (hasattr(m, "__iter__") and hasattr(m, "typecode")):
        return [_to_dict(x) for x in m]
    return m


def _fill(m, d):
    for k, v in d.items():
        if isinstance(v, dict):
            _fill(getattr(m, k), v)
        else:
            setattr(m, k, v)
    return m


class RclpyBus:
    """The plugin as a ROS 2 node: publishers and subscriptions on its own executor thread."""

    def __init__(self, node_name, log, rclpy=None):
        self.log = log
        self.rclpy = rclpy or importlib.import_module("rclpy")
        if not self.rclpy.ok():
            self.rclpy.init(args=None)
        self.node = self.rclpy.create_node(node_name)
        executors = importlib.import_module(self.rclpy.__name__ + ".executors") if not hasattr(self.rclpy, "executors") else self.rclpy.executors
        self.executor = executors.SingleThreadedExecutor()
        self.executor.add_node(self.node)
        self.pubs = {}
        self.alive = True
        self.thread = threading.Thread(target=self._spin, name="rclpy", daemon=True)
        self.thread.start()

    def _spin(self):
        try:
            self.executor.spin()
        except Exception as e:
            self.log("ros2: executor stopped: %r", e)
        finally:
            self.alive = False

    @staticmethod
    def _cls(type_):
        pkg, kind, name = type_.split("/")
        return getattr(importlib.import_module("%s.%s" % (pkg, kind)), name)

    def publish(self, topic, type_, msg):
        pub = self.pubs.get(topic)
        if pub is None:
            pub = self.pubs[topic] = self.node.create_publisher(self._cls(type_), topic, 10)
        pub.publish(_fill(self._cls(type_)(), msg))

    def subscribe(self, topic, type_, callback, throttle_ms=0):
        self.node.create_subscription(self._cls(type_), topic, lambda m: callback(_to_dict(m)), 1)

    def close(self):
        self.alive = False
        try:
            self.executor.shutdown()
            self.node.destroy_node()
        except Exception:
            pass
        try:
            self.rclpy.try_shutdown()
        except Exception:
            pass


def default_bus(cfg, log):
    t = cfg["transport"]
    if t == "rclpy" or (t == "auto" and importlib.util.find_spec("rclpy") is not None):
        return RclpyBus(cfg["node_name"], log)
    return RosbridgeBus(cfg["rosbridge_url"], log)


# ── The plugin ─────────────────────────────────────────────────────────────────────────────────────────────────────
class Ros2(Plugin):
    driver, version = "ros2", "0.1.0"
    motion_kinds = frozenset({"drive"})
    retry_setup_s = DEFAULTS["retry_s"]   # ROS not up yet at start: the runtime sets up again (setup below)

    def __init__(self, bus_factory=default_bus, clock=time.monotonic):
        self.bus_factory = bus_factory
        self.clock = clock
        self.cfg = dict(DEFAULTS)
        self.bus = None
        self.faulted = False
        self.next_retry = 0.0
        self.lock = threading.Lock()
        self.battery = None
        self.distance_cm = None
        self.odom = None
        self.frame_dir = None
        self._own_frame_dir = False
        self.frame_seq = 0
        self.next_frame = 0.0
        self._warned = set()

    @staticmethod
    def config_of(config):
        cfg = dict(DEFAULTS)
        cfg.update({k: v for k, v in (config or {}).items() if k in DEFAULTS or k == "frame_dir"})
        if cfg["transport"] not in ("auto", "rclpy", "rosbridge"):
            raise Fault("bad_config", "transport must be auto, rclpy or rosbridge")
        cfg["max_linear"] = clamp(cfg["max_linear"], 0.0, 5.0)
        cfg["max_angular"] = clamp(cfg["max_angular"], 0.0, 10.0)
        cfg["fps"] = clamp(cfg["fps"], 0.2, 15.0)
        return cfg

    def describe(self, config):
        cfg = self.config_of(config)
        caps = {
            "drive": {"type": "mecanum" if cfg["holonomic"] else "differential", "max_linear_mps": cfg["max_linear"], "max_angular_rps": cfg["max_angular"]},
            "bridge": {"kind": "ros2", "transport": cfg["transport"], "cmd_vel": cfg["cmd_vel"]},
        }
        if cfg["battery_topic"]:
            caps["battery"] = {"topic": cfg["battery_topic"]}
        if cfg["range_topic"] or cfg["scan_topic"] or cfg["odom_topic"]:
            caps["sensors"] = {"distance_cm": bool(cfg["range_topic"] or cfg["scan_topic"]), "odom": bool(cfg["odom_topic"])}
        if cfg["image_topic"]:
            caps["camera"] = {"jpeg": True}
        if cfg["say_topic"]:
            caps["say"] = {"topic": cfg["say_topic"]}
        return caps

    # Setup and reconnecting.
    def setup(self, config):
        self.cfg = self.config_of(config)
        self.retry_setup_s = max(1.0, _num(self.cfg["retry_s"], 3))
        if self.frame_dir:
            self.close()
        if self.cfg["image_topic"]:
            d = self.cfg.get("frame_dir")
            if d:
                os.makedirs(d, exist_ok=True)
                self.frame_dir, self._own_frame_dir = d, False
            else:
                self.frame_dir, self._own_frame_dir = tempfile.mkdtemp(prefix="openvibe-ros2-frames-"), True
        self._connect()

    def _connect(self):
        try:
            bus = self.bus_factory(self.cfg, self.log)
        except Fault:
            raise
        except Exception as e:
            self.next_retry = self.clock() + _num(self.cfg["retry_s"], 3)
            raise Fault("ros_unreachable", "cannot reach ROS 2 (%s): %s" % (self.cfg["transport"], e))
        c = self.cfg
        if c["battery_topic"]:
            bus.subscribe(c["battery_topic"], "sensor_msgs/msg/BatteryState", self._on_battery, 1000)
        if c["range_topic"]:
            bus.subscribe(c["range_topic"], "sensor_msgs/msg/Range", self._on_range, 100)
        elif c["scan_topic"]:
            bus.subscribe(c["scan_topic"], "sensor_msgs/msg/LaserScan", self._on_scan, 200)
        if c["odom_topic"]:
            bus.subscribe(c["odom_topic"], "nav_msgs/msg/Odometry", self._on_odom, 200)
        if c["image_topic"]:
            bus.subscribe(c["image_topic"], "sensor_msgs/msg/CompressedImage", self._on_image, int(1000 / c["fps"]))
        self.bus = bus
        self.faulted = False

    def _bus_alive(self):
        return self.bus is not None and self.bus.alive

    def poll(self):
        if not self._bus_alive():
            if not self.faulted:
                self.faulted = True
                self.next_retry = self.clock() + _num(self.cfg["retry_s"], 3)
                if self.runtime is not None:
                    self.runtime.send({"op": "fault", "fault_code": "ros_unreachable", "message": "the connection to ROS 2 was lost"})
            elif self.clock() >= self.next_retry:
                try:
                    self._connect()
                    self.publish_zero()
                    if self.runtime is not None:
                        self.runtime.send({"op": "ready"})
                except Fault:
                    self.next_retry = self.clock() + _num(self.cfg["retry_s"], 3)
            return {"faults": ["ros_unreachable"]}
        out = {"sensors": {}}
        with self.lock:
            if self.battery:
                out["battery"] = dict(self.battery)
            if self.distance_cm is not None:
                out["sensors"]["distance_cm"] = self.distance_cm
            if self.odom:
                out["sensors"]["odom"] = dict(self.odom)
        return out

    # Commands.
    def twist(self, lx=0.0, ly=0.0, az=0.0):
        return {"linear": {"x": float(lx), "y": float(ly), "z": 0.0}, "angular": {"x": 0.0, "y": 0.0, "z": float(az)}}

    def publish_zero(self):
        if self._bus_alive():
            self.bus.publish(self.cfg["cmd_vel"], TWIST, self.twist())

    def handle(self, cmd):
        if not self._bus_alive():
            raise Fault("ros_unreachable", "not connected to ROS 2")
        v = cmd.value if isinstance(cmd.value, dict) else {}
        c = self.cfg
        if cmd.kind == "drive":
            if c["holonomic"]:
                # Bot's x is forward, y is left, rotation is counter-clockwise: ROS's own axes (REP 103).
                t = self.twist(clamp(v.get("x", 0), -1, 1) * c["max_linear"], clamp(v.get("y", 0), -1, 1) * c["max_linear"],
                               clamp(v.get("rotation", 0), -1, 1) * c["max_angular"])
            else:
                # Bot's steer is positive to the right; ROS's angular z is positive counter-clockwise (to the left).
                t = self.twist(clamp(v.get("throttle", 0), -1, 1) * c["max_linear"], 0.0, -clamp(v.get("steer", 0), -1, 1) * c["max_angular"])
            self.bus.publish(c["cmd_vel"], TWIST, t)
            return
        if cmd.kind == "say" and c["say_topic"]:
            text = str(v.get("text", ""))[:500]
            if not text:
                raise Fault("bad_value", "say needs text")
            self.bus.publish(c["say_topic"], "std_msgs/msg/String", {"data": text})
            return
        raise Fault("unsupported", "kind %s not supported" % cmd.kind)

    def stop_motion(self):
        self.stop()

    def stop(self):
        try:
            self.publish_zero()
        except Exception as e:
            self.log("ros2: could not publish the zero Twist: %r", e)

    def close(self):
        bus, self.bus = self.bus, None
        if bus is not None:
            try:
                bus.close()
            except Exception:
                pass
        if self.frame_dir and self._own_frame_dir:
            shutil.rmtree(self.frame_dir, ignore_errors=True)
        self.frame_dir = None

    # Topics.
    def _on_battery(self, m):
        pct = _num(m.get("percentage"), float("nan"))
        volts = _num(m.get("voltage"), float("nan"))
        b = {}
        if pct == pct and pct >= 0:
            b["percent"] = round(pct * 100 if pct <= 1.0 else pct, 1)
        if volts == volts and volts > 0:
            b["volts"] = round(volts, 2)
        with self.lock:
            self.battery = b or None

    def _on_range(self, m):
        r = _num(m.get("range"), float("nan"))
        with self.lock:
            self.distance_cm = round(r * 100, 1) if r == r and math.isfinite(r) and r >= 0 else None

    def _on_scan(self, m):
        ranges = m.get("ranges") or []
        a0, inc = _num(m.get("angle_min")), _num(m.get("angle_increment"))
        lo, hi = _num(m.get("range_min"), 0.0), _num(m.get("range_max"), float("inf"))
        half = math.radians(_num(self.cfg["front_arc_deg"], 30)) / 2
        best = None
        for i, r in enumerate(ranges):
            r = _num(r, float("nan"))
            if not (r == r) or not math.isfinite(r) or r < lo or r > hi:
                continue
            a = math.atan2(math.sin(a0 + i * inc), math.cos(a0 + i * inc))   # wrapped to -pi..pi, 0 is straight ahead
            if abs(a) <= half and (best is None or r < best):
                best = r
        with self.lock:
            self.distance_cm = round(best * 100, 1) if best is not None else None

    def _on_odom(self, m):
        pose = ((m.get("pose") or {}).get("pose") or {})
        p, q = pose.get("position") or {}, pose.get("orientation") or {}
        qx, qy, qz, qw = (_num(q.get(k)) for k in ("x", "y", "z", "w"))
        yaw = math.atan2(2 * (qw * qz + qx * qy), 1 - 2 * (qy * qy + qz * qz))
        lin = (((m.get("twist") or {}).get("twist") or {}).get("linear") or {})
        with self.lock:
            self.odom = {"x": round(_num(p.get("x")), 3), "y": round(_num(p.get("y")), 3), "yaw": round(yaw, 3),
                         "speed": round(math.hypot(_num(lin.get("x")), _num(lin.get("y"))), 3)}

    def _on_image(self, m):
        fmt = str(m.get("format") or "").lower()
        if "jpeg" not in fmt and "jpg" not in fmt:
            if "format" not in self._warned:
                self._warned.add("format")
                self.log("ros2: %s is %r, not JPEG: no camera frames (use a JPEG CompressedImage topic)", self.cfg["image_topic"], fmt)
            return
        now = self.clock()
        if now < self.next_frame or self.frame_dir is None:
            return
        self.next_frame = now + 1.0 / self.cfg["fps"]
        data = m.get("data")
        if isinstance(data, str):
            try:
                data = base64.b64decode(data)
            except ValueError:
                return
        if not isinstance(data, (bytes, bytearray)) or len(data) < 4 or data[:2] != b"\xff\xd8":
            return
        self.frame_seq += 1
        path = os.path.join(self.frame_dir, "frame-%d.jpg" % (self.frame_seq % FRAME_RING))
        tmp = path + ".tmp"
        with open(tmp, "wb") as f:
            f.write(data)
        os.replace(tmp, path)   # readers never see a partial file
        self.emit_video(path, format="jpeg", seq=self.frame_seq)
