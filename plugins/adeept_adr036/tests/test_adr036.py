import io
import json
import os
import subprocess
import sys
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
PLUGIN_DIR = os.path.dirname(HERE)
SDK_DIR = os.path.join(os.path.dirname(PLUGIN_DIR), "sdk")
sys.path.insert(0, PLUGIN_DIR)
sys.path.insert(0, SDK_DIR)

from openvibe_adeept_adr036 import (ADR036, adc_to_volts, ads7830_command, DEFAULT_CONFIG, mix_mecanum,  # noqa: E402
                                    note_to_hz, servo_angle, ws2812_encode)
from openvibe_adeept_adr036 import hardware  # noqa: E402
from openvibe_plugin import Fault, Runtime  # noqa: E402

FORWARD = {"front_left": 1, "rear_left": 1, "rear_right": 1, "front_right": 1}


class Clock:
    t = 10.0

    def __call__(self):
        return self.t


class Base(unittest.TestCase):
    config = {"backend": "fake"}
    fail = ()

    def setUp(self):
        self.out = io.StringIO()
        self.clock = Clock()
        self.p = ADR036()
        self.p.log = lambda *a: None
        fail = set(self.fail)

        def factory(backend):
            hw = hardware.make_hardware(backend)
            hw.fail = fail
            return hw
        self.p.hardware_factory = factory
        self.rt = Runtime(self.p, infile=io.StringIO(""), outfile=self.out, clock=self.clock)
        self.send({"op": "hello", "config": self.config})
        self.hw = self.p.hw

    def send(self, m):
        self.rt.feed_line(json.dumps(m))

    def msgs(self):
        return [json.loads(line) for line in self.out.getvalue().splitlines()]

    def last(self):
        return self.msgs()[-1]

    def cmd(self, kind, value, id="c", deadline_ms=300):
        self.send({"op": "command", "id": id, "kind": kind, "value": value, "deadline_ms": deadline_ms})
        return self.last()

    def advance(self, dt, heartbeat=True):
        self.clock.t += dt
        if heartbeat:
            self.send({"op": "heartbeat", "t": 1})
        self.rt.tick()

    def speeds(self):
        """Wheel speeds in the kit's sense (+ = forward): throttle * direction(-1)."""
        return {k: -v for k, v in self.hw.throttles().items()}

    def assertStopped(self):
        self.assertEqual(self.hw.throttles(), {"front_left": 0, "rear_left": 0, "rear_right": 0, "front_right": 0})


class WiringTest(Base):
    def test_ready(self):
        self.assertEqual(self.last(), {"op": "ready"})

    def test_pca9685_and_motor_channels(self):
        self.assertEqual(self.hw.pwm["address"], 0x5F)
        self.assertEqual(self.hw.writes[0], {"dev": "pwm", "address": 0x5F, "frequency": 50})
        ch = {name: m.channels for name, m in self.hw.motors.items()}
        self.assertEqual(ch, {"front_left": (15, 14),  # M1
                              "rear_left": (12, 13),  # M2
                              "rear_right": (11, 10),  # M3
                              "front_right": (8, 9)})  # M4
        self.assertStopped()

    def test_servos(self):
        for name, channel in (("pan", 0), ("tilt", 1)):
            s = self.hw.servos[name]
            self.assertEqual((s.channel, s.min_pulse, s.max_pulse, s.actuation_range), (channel, 500, 2400, 180))
            self.assertEqual(s.angle, 90)  # centred at setup

    def test_sensors_buzzer_spi(self):
        d = self.hw.distance
        self.assertEqual((d.echo, d.trigger, d.max_distance), (24, 23, 2.0))
        self.assertEqual(self.hw.inputs_opened, [22, 27, 17])
        self.assertEqual(self.hw.buzzer_dev.pin, 18)
        s = self.hw.spi_dev
        self.assertEqual((s.bus, s.device, s.mode), (0, 0, 0))
        self.assertEqual(self.hw.adc_dev.address, 0x48)
        self.assertEqual(self.hw.adc_dev.bus, 1)
        self.assertEqual(ads7830_command(0), 0x84)
        self.advance(0.1)
        self.assertEqual(self.hw.adc_reads[-1], (0x48, 0x84))

    def test_describe(self):
        d = self.msgs()[0]
        self.assertEqual(d["op"], "describe")
        self.assertEqual(d["driver"], "adeept_adr036")
        self.assertEqual(d["motion_kinds"], ["drive"])
        caps = d["capabilities"]
        self.assertEqual(caps["drive"]["type"], "differential")
        self.assertEqual(caps["sensors"], ["distance_cm", "line"])
        self.assertEqual(caps["lights"]["count"], 8)
        self.assertEqual(caps["camera"]["h264_command"][:3], ["rpicam-vid", "-t", "0"])
        self.assertIn("640", caps["camera"]["h264_command"])
        self.assertFalse(caps["camera"]["jpeg"])


class DifferentialTest(Base):
    def test_forward_is_negative_throttle(self):
        self.assertEqual(self.cmd("drive", {"throttle": 0.5, "steer": 0})["op"], "ack")
        for name, t in self.hw.throttles().items():
            self.assertLess(t, 0, name)
            self.assertAlmostEqual(t, -0.5)
        self.assertEqual(self.hw.writes[-5], {"dev": "pwm", "address": 0x5F, "frequency": 50})  # re-asserted

    def test_rotate_left_matches_kit(self):
        # Kit 'rotate-left': M1, M2 backward; M3, M4 forward.
        self.cmd("drive", {"throttle": 0, "steer": -1})
        self.assertEqual(self.speeds(), {"front_left": -1, "rear_left": -1, "rear_right": 1, "front_right": 1})

    def test_steer_right_normalised(self):
        self.cmd("drive", {"throttle": 1, "steer": 1})
        self.assertEqual(self.speeds(), {"front_left": 1, "rear_left": 1, "rear_right": 0, "front_right": 0})

    def test_bad_value_and_clamp(self):
        self.assertEqual(self.cmd("drive", {"throttle": "fast"})["fault_code"], "bad_value")
        self.cmd("drive", {"throttle": 5})
        self.assertEqual(self.speeds(), FORWARD)

    def test_unsupported(self):
        for kind, value in (("say", {"text": "hi"}), ("display", {"text": "hi"}),
                            ("actuator", {"name": "lift", "value": 1})):
            self.assertEqual(self.cmd(kind, value)["fault_code"], "unsupported", kind)


class MaxThrottleTest(Base):
    config = {"backend": "fake", "max_throttle": 0.5}

    def test_scaled(self):
        self.cmd("drive", {"throttle": 1})
        self.assertEqual(set(self.speeds().values()), {0.5})


class MecanumTest(Base):
    config = {"backend": "fake", "wheels": "mecanum"}

    def test_kit_cases(self):
        # (value, kit M1 front_left, M2 rear_left, M3 rear_right, M4 front_right) from Server_MecanumWheels/Move.py.
        cases = [
            ({"x": 1}, (1, 1, 1, 1)),  # forward
            ({"y": 1}, (-1, 1, -1, 1)),  # strafe left: M1 back, M2 fwd, M3 back, M4 fwd
            ({"y": -1}, (1, -1, 1, -1)),  # strafe right
            ({"x": 1, "y": 1}, (0, 1, 0, 1)),  # forward-left: M1 0, M2 fwd, M3 0, M4 fwd
            ({"x": 1, "y": -1}, (1, 0, 1, 0)),  # forward-right
            ({"rotation": 1}, (-1, -1, 1, 1)),  # rotate-left: M1, M2 back, M3, M4 fwd
        ]
        for value, want in cases:
            self.assertEqual(self.cmd("drive", value)["op"], "ack")
            got = self.speeds()
            self.assertEqual((got["front_left"], got["rear_left"], got["rear_right"], got["front_right"]),
                             want, value)
        self.assertEqual(self.msgs()[0]["capabilities"]["drive"]["type"], "mecanum")

    def test_pure_mix_normalised(self):
        w = mix_mecanum(1, 1, 1)
        self.assertEqual(max(abs(x) for x in w.values()), 1.0)

    def test_throttle_steer_on_mecanum(self):
        self.cmd("drive", {"throttle": 0, "steer": -1})  # left turn == counter-clockwise rotation
        self.assertEqual(self.speeds(), {"front_left": -1, "rear_left": -1, "rear_right": 1, "front_right": 1})


class SafetyTest(Base):
    def test_deadline_stop(self):
        self.cmd("drive", {"throttle": 1}, deadline_ms=300)
        self.assertEqual(self.speeds(), FORWARD)
        self.advance(0.29)
        self.assertEqual(self.speeds(), FORWARD)
        self.advance(0.02)
        self.assertStopped()
        self.assertIn("deadline", self.rt.stop_reasons)

    def test_heartbeat_loss(self):
        self.cmd("drive", {"throttle": 1}, deadline_ms=5000)
        self.advance(1.05, heartbeat=False)
        self.assertStopped()
        self.assertTrue(any(m.get("name") == "heartbeat_lost" for m in self.msgs()))
        self.assertEqual(self.cmd("drive", {"throttle": 1})["fault_code"], "no_heartbeat")
        self.assertStopped()

    def test_estop_until_resume(self):
        self.cmd("drive", {"throttle": 1}, deadline_ms=5000)
        self.send({"op": "estop"})
        self.assertStopped()
        self.assertEqual(self.cmd("drive", {"throttle": 1})["fault_code"], "estopped")
        self.assertEqual(self.cmd("actuator", {"name": "buzzer", "value": {"hz": 440}})["fault_code"], "estopped")
        self.assertStopped()
        self.send({"op": "resume"})
        self.assertEqual(self.cmd("drive", {"throttle": 1})["op"], "ack")
        self.assertEqual(self.speeds(), FORWARD)

    def test_halt_and_stop_kill_buzzer(self):
        self.cmd("drive", {"throttle": 1}, deadline_ms=5000)
        self.cmd("actuator", {"name": "buzzer", "value": {"note": "A4"}})
        self.assertEqual(self.hw.last("buzzer")["hz"], 440.0)
        self.assertEqual(self.cmd("halt", {}), {"op": "ack", "id": "c"})
        self.assertStopped()
        self.assertIsNone(self.hw.last("buzzer")["hz"])

    def test_deadline_keeps_buzzer(self):
        self.cmd("actuator", {"name": "buzzer", "value": {"hz": 523.25}})
        self.cmd("drive", {"throttle": 1})
        self.advance(0.4)
        self.assertStopped()
        self.assertEqual(self.hw.last("buzzer")["hz"], 523.25)

    def test_buzzer_max_duration(self):
        self.cmd("actuator", {"name": "buzzer", "value": {"hz": 440}})
        self.advance(1.9)
        self.assertEqual(self.hw.last("buzzer")["hz"], 440)
        self.advance(0.15)
        self.assertIsNone(self.hw.last("buzzer")["hz"])

    def test_buzzer_values(self):
        self.cmd("actuator", {"name": "buzzer", "value": {"hz": 440}})
        self.cmd("actuator", {"name": "buzzer", "value": None})
        self.assertIsNone(self.hw.last("buzzer")["hz"])
        self.assertEqual(self.cmd("actuator", {"name": "buzzer", "value": {"hz": 5000}})["fault_code"], "bad_value")
        self.assertEqual(self.cmd("actuator", {"name": "buzzer", "value": {"note": "H2"}})["fault_code"],
                         "bad_value")
        self.assertAlmostEqual(note_to_hz("C5"), 523.25, places=2)
        self.assertAlmostEqual(note_to_hz("Bb4"), note_to_hz("A#4"))

    def test_stop_before_setup_and_idempotent(self):
        p = ADR036()
        p.stop()
        p.stop()
        p.close()
        self.p.stop()
        self.p.stop()
        self.assertStopped()


class ServoTest(Base):
    def test_mapping(self):
        pan, tilt = DEFAULT_CONFIG["servos"]["pan"], DEFAULT_CONFIG["servos"]["tilt"]
        self.assertEqual(servo_angle(0, pan), 90)
        self.assertEqual(servo_angle(1, pan), 170)
        self.assertEqual(servo_angle(-1, pan), 10)
        self.assertEqual(servo_angle(5, pan), 170)
        self.assertEqual(servo_angle(-5, tilt), 30)
        self.assertEqual(servo_angle(0.5, tilt), 120)
        self.assertRaises(Fault, servo_angle, "up", pan)

    def test_ptz_and_actuator(self):
        self.assertEqual(self.cmd("ptz", {"pan": 1, "tilt": -1})["op"], "ack")
        self.assertEqual((self.hw.servos["pan"].angle, self.hw.servos["tilt"].angle), (170, 30))
        self.cmd("actuator", {"name": "pan", "value": -1})
        self.assertEqual(self.hw.servos["pan"].angle, 10)
        self.cmd("actuator", {"name": "tilt", "value": 0})
        self.assertEqual(self.hw.servos["tilt"].angle, 90)
        self.assertEqual(self.cmd("ptz", {})["fault_code"], "bad_value")

    def test_servos_hold_on_stop(self):
        self.cmd("ptz", {"pan": 1})
        self.send({"op": "stop"})
        self.assertEqual(self.hw.servos["pan"].angle, 170)


class BatteryTest(Base):
    def test_conversion(self):
        b = DEFAULT_CONFIG["battery"]
        self.assertAlmostEqual(adc_to_volts(200, b), 200 / 255 * 5.2 / 0.25)
        self.assertAlmostEqual(adc_to_volts(255, b), 20.8)
        self.hw.adc_values[0x84] = 91  # 91/255*5.2*4 = 7.42 V
        self.advance(0.1)
        t = self.last()
        self.assertEqual(t["op"], "telemetry")
        self.assertEqual(t["battery"], {"volts": 7.42, "percent": 59, "low": False})

    def test_low_battery_once_with_median(self):
        self.hw.adc_values[0x84] = 91
        self.advance(0.1)
        self.hw.adc_values[0x84] = 70  # 5.71 V
        for _ in range(8):
            self.advance(0.5)
        events = [m for m in self.msgs() if m.get("op") == "event" and m["name"] == "low_battery"]
        self.assertEqual(len(events), 1)
        self.assertEqual(events[0]["volts"], 5.71)
        tel = [m for m in self.msgs() if m.get("op") == "telemetry"]
        self.assertEqual(tel[1]["battery"]["volts"], 6.57)  # median of 7.42 and 5.71: one bad reading is smoothed
        self.assertEqual(tel[-1]["battery"]["percent"], 0)

    def test_sensors_telemetry(self):
        self.hw.distance_m = 0.345
        self.hw.inputs[27] = 1
        self.advance(0.1)
        t = self.last()
        self.assertEqual(t["sensors"], {"distance_cm": 34.5, "line": {"left": False, "middle": True, "right": False}})
        self.assertEqual(t["faults"], [])


class SensorFailureTest(Base):
    fail = ("distance", "adc", "spi")

    def test_drive_still_works(self):
        self.assertEqual(self.msgs()[1], {"op": "ready"})
        self.assertEqual(self.cmd("drive", {"throttle": 1})["op"], "ack")
        self.assertEqual(self.speeds(), FORWARD)
        self.assertEqual(self.cmd("actuator", {"name": "lights", "value": {"r": 1}})["fault_code"], "hardware")
        self.advance(0.1)
        t = self.last()
        self.assertEqual([f["component"] for f in t["faults"]], ["battery", "lights", "ultrasonic"])
        self.assertIn("line", t["sensors"])


class PcaFailureTest(Base):
    fail = ("pwm",)

    def test_fault(self):
        m = self.last()
        self.assertEqual((m["op"], m["fault_code"]), ("fault", "hardware"))
        self.assertIn("0x5f", m["message"])
        self.assertEqual(self.cmd("drive", {"throttle": 1})["fault_code"], "not_ready")


class LightsTest(Base):
    def test_encoding(self):
        # Pure red: GRB order -> G=0x00, R=0xFF, B=0x00; bit 1 = 0xF8, bit 0 = 0x80, MSB first.
        self.assertEqual(ws2812_encode([(255, 0, 0)]), [0x80] * 8 + [0xF8] * 8 + [0x80] * 8)
        # 0xA5 in green: 1010 0101.
        self.assertEqual(ws2812_encode([(0, 0xA5, 0)])[:8], [0xF8, 0x80, 0xF8, 0x80, 0x80, 0xF8, 0x80, 0xF8])

    def test_command(self):
        self.assertEqual(self.hw.last("spi")["data"], [0x80] * 8 * 24)  # all off at setup
        self.cmd("actuator", {"name": "lights", "value": {"r": 0, "g": 0, "b": 255, "index": 2}})
        w = self.hw.last("spi")
        self.assertEqual(w["speed_hz"], 6400000)
        self.assertEqual(len(w["data"]), 8 * 24)
        self.assertEqual(w["data"][2 * 24:3 * 24], [0x80] * 16 + [0xF8] * 8)
        self.assertEqual(w["data"][:24], [0x80] * 24)
        self.assertEqual(self.cmd("actuator", {"name": "lights", "value": {"r": 1, "index": 8}})["fault_code"],
                         "bad_value")


class ProbeTest(unittest.TestCase):
    def test_probe_touches_no_hardware(self):
        p = ADR036()
        p.log = lambda *a: None

        def boom(backend):
            raise AssertionError("hardware opened in probe mode")
        p.hardware_factory = boom
        out = io.StringIO()
        rt = Runtime(p, infile=io.StringIO(""), outfile=out, clock=Clock())
        rt.feed_line(json.dumps({"op": "hello", "probe": True, "config": {"backend": "real", "wheels": "mecanum"}}))
        msgs = [json.loads(line) for line in out.getvalue().splitlines()]
        self.assertEqual([m["op"] for m in msgs], ["describe"])
        self.assertEqual(msgs[0]["capabilities"]["drive"]["type"], "mecanum")
        rt.shutdown()
        self.assertIsNone(p.hw)


class AutoBackendTest(unittest.TestCase):
    def test_auto_off_pi_faults(self):
        with tempfile.NamedTemporaryFile("wb", delete=False) as f:
            f.write(b"Generic x86 PC\0")
        try:
            with self.assertRaises(hardware.HardwareError) as e:
                hardware.make_hardware("auto", model_path=f.name)
            self.assertIn("backend=fake", str(e.exception))
            self.assertFalse(hardware.is_raspberry_pi("/nonexistent"))
            with open(f.name, "wb") as g:
                g.write(b"Raspberry Pi 5 Model B Rev 1.0\0")
            self.assertTrue(hardware.is_raspberry_pi(f.name))
        finally:
            os.unlink(f.name)

    def test_auto_setup_fault_on_this_machine(self):
        if hardware.is_raspberry_pi():
            self.skipTest("running on a Raspberry Pi")
        p = ADR036()
        p.log = lambda *a: None
        out = io.StringIO()
        rt = Runtime(p, infile=io.StringIO(""), outfile=out, clock=Clock())
        rt.feed_line(json.dumps({"op": "hello", "config": {}}))
        m = json.loads(out.getvalue().splitlines()[-1])
        self.assertEqual((m["op"], m["fault_code"]), ("fault", "hardware"))
        self.assertIn("not a Raspberry Pi", m["message"])


class ChildProcessTest(unittest.TestCase):
    def test_stdin_eof_stops_motors(self):
        with tempfile.TemporaryDirectory() as d:
            log = os.path.join(d, "hw.jsonl")
            env = dict(os.environ, PYTHONPATH=os.pathsep.join([SDK_DIR, PLUGIN_DIR]))
            env[hardware.FAKE_LOG_ENV] = log
            inp = "\n".join(json.dumps(m) for m in [
                {"op": "hello", "config": {"backend": "fake"}},
                {"op": "command", "id": "d1", "kind": "drive", "value": {"throttle": 0.8}, "deadline_ms": 5000},
            ]) + "\n"
            p = subprocess.run([sys.executable, "-m", "openvibe_adeept_adr036"], input=inp, env=env, cwd=d,
                               capture_output=True, text=True, timeout=20)
            self.assertEqual(p.returncode, 0, p.stderr)
            out = [json.loads(line) for line in p.stdout.splitlines()]
            self.assertIn({"op": "ack", "id": "d1"}, out)
            with open(log) as f:
                writes = [json.loads(line) for line in f]
            motor = [w for w in writes if w["dev"] == "motor"]
            self.assertIn(-0.8, [w["throttle"] for w in motor])
            last = {}
            for w in motor:
                last[tuple(w["channels"])] = w["throttle"]
            self.assertEqual(last, {(15, 14): 0, (12, 13): 0, (11, 10): 0, (8, 9): 0})
            self.assertEqual(writes[-1]["dev"], "close")


if __name__ == "__main__":
    unittest.main()
