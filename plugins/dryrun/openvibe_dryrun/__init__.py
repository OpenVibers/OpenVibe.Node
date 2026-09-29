"""Dry-run driver: a pretend differential robot with a pan-tilt head, a speaker and a display.

It accepts every command, logs it to stderr, keeps a simulated state (speeds, pan/tilt, battery) and reports it as
telemetry. The camera is the core's built-in test pattern. Used by CI, by people trying the Node on a laptop, and by
OpenVibe.Bot's end-to-end tests.
"""

import json
import time

from openvibe_plugin import Fault, Plugin, clamp

__version__ = "0.1.0"


class DryRun(Plugin):
    driver = "dryrun"
    version = __version__
    motion_kinds = frozenset({"drive"})

    def __init__(self):
        self.state = {"throttle": 0.0, "steer": 0.0, "pan": 0.0, "tilt": 0.0, "actuators": {}}
        self.battery = 100.0
        self.started = time.monotonic()
        self.log_commands = True

    def describe(self, config):
        return {
            "drive": {"type": "differential"},
            "ptz": {"pan": {"min": -1, "max": 1}, "tilt": {"min": -1, "max": 1}},
            "actuator": {"names": ["any"]},
            "say": {},
            "display": {"text": True},
            "battery": {},
            "sensors": ["distance_cm"],
            "camera": {"source": "test_pattern"},
        }

    def setup(self, config):
        self.log_commands = bool(config.get("log_commands", True))
        self.log("dryrun: ready (no hardware)")

    def handle(self, cmd):
        v = cmd.value if isinstance(cmd.value, dict) else {}
        if cmd.kind == "drive":
            if "x" in v or "y" in v:
                self.state["throttle"] = clamp(v.get("x", 0), -1, 1)
                self.state["steer"] = clamp(v.get("rotation", 0), -1, 1)
            else:
                self.state["throttle"] = clamp(v.get("throttle", 0), -1, 1)
                self.state["steer"] = clamp(v.get("steer", 0), -1, 1)
        elif cmd.kind == "ptz":
            if "pan" in v:
                self.state["pan"] = clamp(v["pan"], -1, 1)
            if "tilt" in v:
                self.state["tilt"] = clamp(v["tilt"], -1, 1)
        elif cmd.kind == "actuator":
            name = v.get("name")
            if not isinstance(name, str) or not name:
                raise Fault("bad_value", "actuator needs a name")
            self.state["actuators"][name] = v.get("value")
        elif cmd.kind in ("say", "display"):
            pass
        else:
            raise Fault("unsupported", "kind %s" % cmd.kind)
        if self.log_commands:
            self.log("dryrun: %s %s (deadline %d ms)", cmd.kind, json.dumps(v, sort_keys=True), cmd.deadline_ms)

    def stop(self):
        if self.state["throttle"] or self.state["steer"]:
            self.log("dryrun: stop")
        self.state["throttle"] = 0.0
        self.state["steer"] = 0.0

    def poll(self):
        self.battery = max(0.0, 100.0 - (time.monotonic() - self.started) / 360.0)
        return {
            "battery": {"volts": round(6.0 + 2.4 * self.battery / 100.0, 2), "percent": int(self.battery)},
            "sensors": {"distance_cm": 123.4, "moving": bool(self.state["throttle"] or self.state["steer"])},
        }
