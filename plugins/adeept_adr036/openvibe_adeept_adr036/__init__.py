"""Driver for the Adeept 4WD Smart Car Kit for Raspberry Pi (ADR036), ordinary or mecanum wheels.

Hardware (from Adeept's own Move.py / Voltage.py, reimplemented here; none of the kit's code is imported):

* four DC motors on a PCA9685 at I2C 0x5f, 50 Hz, driven as adafruit_motor DCMotors (slow decay);
* pan/tilt servos on the same PCA9685, channels 0 and 1;
* HC-SR04 ultrasonic sensor (echo 24, trigger 23), three line-tracking inputs (22, 27, 17);
* ADS7830 ADC at I2C 0x48 reading the 2x18650 pack through a 3k/1k divider;
* a tonal buzzer on GPIO 18 and WS2812 LEDs driven over SPI 0.0.

The camera is published by the core (``rpicam-vid``); this plugin only describes the command. The kit's stock server
(0.0.0.0:8888 with admin/123456, MJPEG on :5000) must stay disabled: it is never started or exposed from here.

All hardware access goes through ``hardware.py`` so the driver runs against a fake in tests (config backend "fake").
"""

import copy
import collections
import statistics
import time

from openvibe_plugin import Fault, Plugin, clamp

from . import hardware

__version__ = "0.1.0"

MOTOR_NAMES = ("front_left", "rear_left", "rear_right", "front_right")

DEFAULT_CONFIG = {
    "backend": "auto",  # auto (real on a Raspberry Pi, fault elsewhere) | real | fake
    "i2c_address": 0x5F,  # PCA9685
    "pwm_frequency": 50,
    "reassert_frequency": True,  # the kit re-sets the PCA9685 frequency before each motor update
    "wheels": "ordinary",  # ordinary (differential) | mecanum
    "max_throttle": 1.0,
    # Kit M1..M4. direction -1: the kit drives forward with throttle = -speed on every motor.
    "motors": {
        "front_left": {"channels": [15, 14], "direction": -1},  # M1
        "rear_left": {"channels": [12, 13], "direction": -1},  # M2
        "rear_right": {"channels": [11, 10], "direction": -1},  # M3
        "front_right": {"channels": [8, 9], "direction": -1},  # M4
    },
    "servo_pulse": {"min_pulse": 500, "max_pulse": 2400, "actuation_range": 180},
    "servos": {
        "pan": {"channel": 0, "min_deg": 10, "max_deg": 170, "center_deg": 90, "invert": False},
        "tilt": {"channel": 1, "min_deg": 30, "max_deg": 150, "center_deg": 90, "invert": False},
    },
    "ultrasonic": {"echo": 24, "trigger": 23, "max_distance_m": 2.0},
    "line": {"left": 22, "middle": 27, "right": 17, "invert": False},
    "battery": {"i2c_bus": 1, "address": 0x48, "channel": 0, "adc_vref": 5.2, "r_top": 3000, "r_bottom": 1000,
                "full_volts": 8.4, "warning_volts": 6.0, "median_window": 5},
    "buzzer": {"pin": 18, "octaves": 1, "max_seconds": 2.0},
    "lights": {"count": 8, "spi_bus": 0, "spi_device": 0, "order": "GRB", "speed_hz": 6400000},
    "camera": {"width": 640, "height": 480, "framerate": 20},
}


def deep_merge(base, override):
    """Return base with override merged in; nested dicts merge, everything else replaces."""
    out = copy.deepcopy(base)
    for k, v in (override or {}).items():
        if isinstance(v, dict) and isinstance(out.get(k), dict):
            out[k] = deep_merge(out[k], v)
        else:
            out[k] = copy.deepcopy(v)
    return out


# Pure helpers (tested directly).

def normalise(*ws):
    """Scale wheel speeds down together so that max |w| <= 1."""
    m = max(1.0, max(abs(w) for w in ws))
    return tuple(w / m for w in ws)


def mix_differential(throttle, steer):
    """throttle forward +, steer + turns right. Returns speeds by wheel name (+ = forward)."""
    left, right = normalise(throttle + steer, throttle - steer)
    return {"front_left": left, "rear_left": left, "rear_right": right, "front_right": right}


def mix_mecanum(x, y, r):
    """x forward +, y left +, r counter-clockwise +. Returns speeds by wheel name (+ = forward)."""
    fl, fr, rl, rr = normalise(x - y - r, x + y + r, x + y - r, x - y + r)
    return {"front_left": fl, "rear_left": rl, "rear_right": rr, "front_right": fr}


def servo_angle(v, spec):
    """Map a normalised position -1..1 onto min..center..max degrees (0 is the center)."""
    v = clamp(v, -1.0, 1.0)
    if spec.get("invert"):
        v = -v
    lo, c, hi = float(spec["min_deg"]), float(spec["center_deg"]), float(spec["max_deg"])
    return c + v * (hi - c) if v >= 0 else c + v * (c - lo)


def ads7830_command(chn):
    """ADS7830 single-ended command byte for a channel (as in the kit's ADS7830 code)."""
    return 0x84 | (((chn << 2 | chn >> 1) & 0x07) << 4)


def adc_to_volts(adc, cfg):
    ratio = float(cfg["r_bottom"]) / (float(cfg["r_top"]) + float(cfg["r_bottom"]))
    return adc / 255.0 * float(cfg["adc_vref"]) / ratio


def battery_percent(volts, cfg):
    lo, hi = float(cfg["warning_volts"]), float(cfg["full_volts"])
    return max(0.0, min(100.0, (volts - lo) / (hi - lo) * 100.0))


def ws2812_encode(colors, order="GRB"):
    """SPI bytes for WS2812 LEDs: 8 SPI bits per LED bit at 6.4 MHz, MSB first; a 1 is 0xF8, a 0 is 0x80.
    ``colors`` is a list of (r, g, b)."""
    tx = []
    for r, g, b in colors:
        c = {"R": r, "G": g, "B": b}
        for ch in order:
            d = int(c[ch]) & 0xFF
            byte = [0] * 8
            for ibit in range(8):
                byte[7 - ibit] = ((d >> ibit) & 1) * 0x78 + 0x80
            tx.extend(byte)
    return tx


_NOTE_SEMITONES = {"C": 0, "D": 2, "E": 4, "F": 5, "G": 7, "A": 9, "B": 11}


def note_to_hz(note):
    """'A4' -> 440.0; accepts sharps (#) and flats (b)."""
    if not isinstance(note, str) or len(note) < 2 or note[0].upper() not in _NOTE_SEMITONES:
        raise Fault("bad_value", "bad note %r" % (note,))
    s = _NOTE_SEMITONES[note[0].upper()]
    rest = note[1:]
    if rest[:1] == "#":
        s, rest = s + 1, rest[1:]
    elif rest[:1] == "b":
        s, rest = s - 1, rest[1:]
    try:
        octave = int(rest)
    except ValueError:
        raise Fault("bad_value", "bad note %r" % (note,))
    midi = 12 * (octave + 1) + s
    return 440.0 * 2 ** ((midi - 69) / 12.0)


class ADR036(Plugin):
    driver = "adeept_adr036"
    version = __version__
    motion_kinds = frozenset({"drive"})
    # The loop polls often so the buzzer's max duration is enforced within ~0.1 s; telemetry itself goes out at 2 Hz.
    telemetry_interval_s = 0.1
    telemetry_period_s = 0.5

    def __init__(self):
        self.hardware_factory = hardware.make_hardware  # tests replace this
        self.cfg = copy.deepcopy(DEFAULT_CONFIG)
        self.hw = None
        self.motors = {}  # name -> (motor, direction)
        self.servos = {}  # name -> (servo, spec)
        self.distance = None
        self.line = {}
        self.adc = None
        self.buzzer = None
        self.buzzer_until = None
        self.spi = None
        self.leds = []
        self.faults = {}  # component -> error text
        self.volts_window = collections.deque(maxlen=5)
        self.low_battery_sent = False
        self.next_telemetry = 0.0

    def now(self):
        return self.runtime.clock() if self.runtime is not None else time.monotonic()

    # Capabilities.
    def describe(self, config):
        cfg = deep_merge(DEFAULT_CONFIG, config)
        cam = cfg["camera"]
        ptz = {}
        for name in ("pan", "tilt"):
            s = cfg["servos"][name]
            ptz[name] = {"min": -1, "max": 1, "min_deg": s["min_deg"], "max_deg": s["max_deg"],
                         "center_deg": s["center_deg"]}
        return {
            "drive": {"type": "mecanum" if cfg["wheels"] == "mecanum" else "differential"},
            "ptz": ptz,
            "actuator": {"names": ["pan", "tilt", "buzzer", "lights"]},
            "sensors": ["distance_cm", "line"],
            "battery": {"full_volts": cfg["battery"]["full_volts"],
                        "warning_volts": cfg["battery"]["warning_volts"]},
            "lights": {"count": cfg["lights"]["count"]},
            "camera": {
                "h264_command": ["rpicam-vid", "-t", "0", "-n", "--codec", "h264", "--inline",
                                 "--width", str(cam["width"]), "--height", str(cam["height"]),
                                 "--framerate", str(cam["framerate"]), "-o", "-"],
                "jpeg": False,
            },
        }

    # Setup.
    def setup(self, config):
        cfg = self.cfg = deep_merge(DEFAULT_CONFIG, config)
        if cfg["wheels"] not in ("ordinary", "mecanum"):
            raise Fault("bad_config", "wheels must be ordinary or mecanum")
        for name in MOTOR_NAMES:
            if name not in cfg["motors"]:
                raise Fault("bad_config", "motors.%s missing" % name)
        try:
            self.hw = self.hardware_factory(cfg["backend"])
        except hardware.HardwareError as e:
            raise Fault("hardware", str(e))
        if self.hw.name == "fake":
            self.log("adeept_adr036: FAKE hardware backend, nothing will move")

        # PCA9685, motors and servos: without them there is no robot, so any failure is fatal.
        try:
            self.hw.open_pwm(int(cfg["i2c_address"]), int(cfg["pwm_frequency"]))
            for name in MOTOR_NAMES:
                m = cfg["motors"][name]
                in1, in2 = m["channels"]
                self.motors[name] = (self.hw.motor(name, int(in1), int(in2)), float(m.get("direction", -1)))
            self.stop_motion()
            sp = cfg["servo_pulse"]
            for name in ("pan", "tilt"):
                s = cfg["servos"][name]
                self.servos[name] = (self.hw.servo(name, int(s["channel"]), int(sp["min_pulse"]),
                                                   int(sp["max_pulse"]), int(sp["actuation_range"])), s)
                self.servos[name][0].angle = float(s["center_deg"])
        except Exception as e:
            self.motors, self.servos = {}, {}
            self._release()
            if isinstance(e, ImportError):
                raise Fault("hardware", "missing library: %s (pip install 'openvibe-adeept-adr036[pi]')" % e)
            raise Fault("hardware", "PCA9685 at 0x%02x: %s" % (int(cfg["i2c_address"]), e))

        # Sensors and extras: a failure is reported in telemetry and driving keeps working.
        us = cfg["ultrasonic"]
        self._optional("ultrasonic", lambda: setattr(self, "distance", self.hw.distance_sensor(
            echo=int(us["echo"]), trigger=int(us["trigger"]), max_distance=float(us["max_distance_m"]))))

        def open_line():
            self.line = {k: self.hw.input_pin(int(cfg["line"][k])) for k in ("left", "middle", "right")}
        self._optional("line", open_line)
        b = cfg["battery"]
        self.volts_window = collections.deque(maxlen=max(1, int(b["median_window"])))
        self._optional("battery", lambda: setattr(self, "adc", self.hw.adc(int(b["i2c_bus"]), int(b["address"]))))
        self._optional("buzzer", lambda: setattr(self, "buzzer", self.hw.buzzer(
            int(cfg["buzzer"]["pin"]), int(cfg["buzzer"]["octaves"]))))
        lc = cfg["lights"]
        self.leds = [(0, 0, 0)] * int(lc["count"])

        def open_lights():
            self.spi = self.hw.spi(int(lc["spi_bus"]), int(lc["spi_device"]), 0)
            self._show_leds()
        self._optional("lights", open_lights)
        self.log("adeept_adr036: ready (%s wheels, %s backend, faults: %s)", cfg["wheels"], self.hw.name,
                 ", ".join(sorted(self.faults)) or "none")

    def _optional(self, component, fn):
        try:
            fn()
        except Exception as e:
            self.faults[component] = "init failed: %s" % e
            self.log("adeept_adr036: %s unavailable: %s", component, e)

    # Commands.
    def handle(self, cmd):
        v = cmd.value if isinstance(cmd.value, dict) else {}
        if cmd.kind == "drive":
            self._drive(v)
        elif cmd.kind == "ptz":
            if "pan" not in v and "tilt" not in v:
                raise Fault("bad_value", "ptz needs pan and/or tilt")
            for name in ("pan", "tilt"):
                if name in v:
                    self._servo(name, v[name])
        elif cmd.kind == "actuator":
            name = v.get("name")
            if name in ("pan", "tilt"):
                self._servo(name, v.get("value"))
            elif name == "buzzer":
                self._buzz(v.get("value"))
            elif name == "lights":
                self._lights(v.get("value"))
            else:
                raise Fault("unsupported", "no actuator %r" % (name,))
        else:
            raise Fault("unsupported", "kind %s not supported" % cmd.kind)

    def _drive(self, v):
        mt = clamp(self.cfg["max_throttle"], 0.0, 1.0)
        if self.cfg["wheels"] == "mecanum":
            if any(k in v for k in ("x", "y", "rotation")):
                x, y, r = (clamp(v.get(k, 0), -1, 1) for k in ("x", "y", "rotation"))
            else:  # throttle/steer: steer + is a right (clockwise) turn, rotation + is counter-clockwise
                x, y, r = clamp(v.get("throttle", 0), -1, 1), 0.0, -clamp(v.get("steer", 0), -1, 1)
            speeds = mix_mecanum(x, y, r)
        else:
            if "throttle" in v or "steer" in v:
                t, s = clamp(v.get("throttle", 0), -1, 1), clamp(v.get("steer", 0), -1, 1)
            else:  # x/rotation on a differential base; y (strafe) cannot be done and is ignored
                t, s = clamp(v.get("x", 0), -1, 1), -clamp(v.get("rotation", 0), -1, 1)
            speeds = mix_differential(t, s)
        if self.cfg["reassert_frequency"]:
            self.hw.set_pwm_frequency(int(self.cfg["pwm_frequency"]))
        for name, (m, direction) in self.motors.items():
            m.throttle = max(-1.0, min(1.0, direction * speeds[name] * mt))

    def _servo(self, name, value):
        servo, spec = self.servos[name]
        servo.angle = servo_angle(value, spec)

    def _buzz(self, value):
        if self.buzzer is None:
            raise Fault("hardware", "buzzer unavailable: %s" % self.faults.get("buzzer", "not set up"))
        if not value:  # None, 0, {} -> off
            self._buzzer_off()
            return
        if not isinstance(value, dict):
            raise Fault("bad_value", "buzzer value is {note} or {hz} or null")
        if "note" in value:
            hz = note_to_hz(value["note"])
        elif "hz" in value:
            hz = clamp(value["hz"], 0, 1e6)
        else:
            raise Fault("bad_value", "buzzer value is {note} or {hz} or null")
        if hz == 0:
            self._buzzer_off()
            return
        # gpiozero's TonalBuzzer plays mid_tone (A4) +/- octaves only.
        octaves = int(self.cfg["buzzer"]["octaves"])
        lo, hi = 440.0 / 2 ** octaves, 440.0 * 2 ** octaves
        if not lo - 0.01 <= hz <= hi + 0.01:
            raise Fault("bad_value", "tone %.1f Hz outside %.0f..%.0f Hz" % (hz, lo, hi))
        self.buzzer.play_hz(round(max(lo, min(hi, hz)), 2))
        self.buzzer_until = self.now() + float(self.cfg["buzzer"]["max_seconds"])

    def _buzzer_off(self):
        self.buzzer_until = None
        if self.buzzer is not None:
            self.buzzer.stop()

    def _lights(self, value):
        if self.spi is None:
            raise Fault("hardware", "lights unavailable: %s" % self.faults.get("lights", "not set up"))
        if not value:
            rgb, index = (0, 0, 0), None
        elif isinstance(value, dict):
            rgb = tuple(int(clamp(value.get(k, 0), 0, 255)) for k in ("r", "g", "b"))
            index = value.get("index")
        else:
            raise Fault("bad_value", "lights value is {r,g,b[,index]} or null")
        if index is None:
            self.leds = [rgb] * len(self.leds)
        else:
            if isinstance(index, bool) or not isinstance(index, int) or not 0 <= index < len(self.leds):
                raise Fault("bad_value", "index must be 0..%d" % (len(self.leds) - 1))
            self.leds[index] = rgb
        self._show_leds()

    def _show_leds(self):
        lc = self.cfg["lights"]
        self.spi.xfer(ws2812_encode(self.leds, lc["order"]), int(lc["speed_hz"]))

    # Stopping.
    def stop_motion(self):
        """Wheels to throttle 0 (brake). Every motor is tried even if one write fails."""
        errors = []
        for name, (m, _) in self.motors.items():
            try:
                m.throttle = 0
            except Exception as e:
                errors.append("%s: %s" % (name, e))
        if errors:
            raise RuntimeError("motor stop failed: " + "; ".join(errors))

    def stop(self):
        """Wheels stop, buzzer off. Servos hold their position. Safe before setup and when repeated."""
        err = None
        try:
            self.stop_motion()
        except Exception as e:
            err = e
        try:
            self._buzzer_off()
        except Exception as e:
            err = err or e
        if err is not None:
            raise err

    # Telemetry.
    def poll(self):
        now = self.now()
        if self.buzzer_until is not None and now >= self.buzzer_until:
            try:
                self._buzzer_off()
            except Exception as e:
                self.log("adeept_adr036: buzzer off failed: %r", e)
        if now < self.next_telemetry:
            return None
        self.next_telemetry = now + self.telemetry_period_s
        out = {}
        sensors = {}
        if self.distance is not None:
            sensors["distance_cm"] = self._read("ultrasonic", lambda: round(self.distance.distance * 100.0, 1))
        if self.line:
            inv = bool(self.cfg["line"].get("invert"))
            sensors["line"] = self._read("line", lambda: {k: bool(d.value) != inv for k, d in self.line.items()})
        if sensors:
            out["sensors"] = sensors
        if self.adc is not None:
            battery = self._read("battery", self._battery)
            if battery is not None:
                out["battery"] = battery
        out["faults"] = [{"component": k, "error": v} for k, v in sorted(self.faults.items())]
        return out

    def _read(self, component, fn):
        try:
            value = fn()
        except Exception as e:
            self.faults[component] = "read failed: %s" % e
            return None
        if self.faults.get(component, "").startswith("read failed"):
            del self.faults[component]
        return value

    def _battery(self):
        b = self.cfg["battery"]
        adc = self.adc.read(ads7830_command(int(b["channel"])))
        self.volts_window.append(adc_to_volts(adc, b))
        volts = statistics.median(self.volts_window)
        warn = float(b["warning_volts"])
        low = volts < warn
        if low and not self.low_battery_sent:
            self.low_battery_sent = True
            self.emit_event("low_battery", volts=round(volts, 2), warning_volts=warn)
        elif volts > warn + 0.3:  # re-arm after a charge
            self.low_battery_sent = False
        return {"volts": round(volts, 2), "percent": int(round(battery_percent(volts, b))), "low": low}

    def close(self):
        try:
            if self.spi is not None:
                self.leds = [(0, 0, 0)] * len(self.leds)
                self._show_leds()
        except Exception as e:
            self.log("adeept_adr036: lights off failed: %r", e)
        self._release()

    def _release(self):
        if self.hw is not None:
            try:
                self.hw.close()
            except Exception as e:
                self.log("adeept_adr036: close failed: %r", e)
