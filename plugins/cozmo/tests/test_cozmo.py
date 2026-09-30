import base64
import io
import json
import math
import os
import sys
import tempfile
import threading
import time
import unittest
import wave

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
sys.path.insert(0, os.path.dirname(HERE))
sys.path.insert(0, os.path.join(os.path.dirname(os.path.dirname(HERE)), "sdk"))

from PIL import Image  # noqa: E402

import fake_pycozmo as fpc  # noqa: E402
from openvibe_cozmo import Cozmo, render, tts  # noqa: E402
from openvibe_plugin import Runtime  # noqa: E402


class Clock:
    t = 10.0

    def __call__(self):
        return self.t


def write_wav(path, rate=16000, n=1600, channels=1):
    with wave.open(path, "wb") as w:
        w.setnchannels(channels)
        w.setsampwidth(2)
        w.setframerate(rate)
        frames = bytearray()
        for i in range(n):
            s = int(8000 * math.sin(i / 10.0))
            frames += s.to_bytes(2, "little", signed=True) * channels
        w.writeframes(bytes(frames))


class FakeTTS:
    engine = "fake"

    def __init__(self):
        self.texts = []
        self.threads = []

    def __call__(self, text, wav_path):
        self.texts.append(text)
        self.threads.append(threading.current_thread())
        write_wav(wav_path, rate=16000)  # like pico2wave: must be resampled to 22050


class Base(unittest.TestCase):
    fw_sig = None
    config = {}

    def setUp(self):
        self.out = io.StringIO()
        self.clock = Clock()
        self.clients = []
        self.tts = FakeTTS()
        self.frames = tempfile.mkdtemp()

        def factory(pc, config):
            c = fpc.Client(fw_sig=self.fw_sig, reachable=config.get("_reachable", True))
            self.clients.append(c)
            return c

        self.p = Cozmo(client_factory=factory, pycozmo_module=fpc.module, synthesizer=self.tts, clock=self.clock)
        self.p.log = lambda *a: None
        self.rt = Runtime(self.p, infile=io.StringIO(""), outfile=self.out, clock=self.clock)

    def tearDown(self):
        self.p.close()

    def hello(self, **config):
        cfg = {"frame_dir": self.frames}
        cfg.update(self.config)
        cfg.update(config)
        self.send({"op": "hello", "config": cfg})

    @property
    def cli(self):
        return self.clients[-1]

    def send(self, m):
        self.rt.feed_line(json.dumps(m))

    def msgs(self):
        return [json.loads(line) for line in self.out.getvalue().splitlines()]

    def last(self):
        return self.msgs()[-1]

    def cmd(self, kind, value, id="c", deadline_ms=300):
        self.send({"op": "command", "id": id, "kind": kind, "value": value, "deadline_ms": deadline_ms})
        return self.last()

    def events(self, name):
        return [m for m in self.msgs() if m.get("op") == "event" and m.get("name") == name]


class ConnectTest(Base):
    def test_ready_and_describe(self):
        self.hello()
        ms = self.msgs()
        self.assertEqual(ms[0]["op"], "describe")
        self.assertEqual(ms[0]["driver"], "cozmo")
        self.assertEqual(ms[0]["motion_kinds"], ["drive"])
        caps = ms[0]["capabilities"]
        self.assertTrue(caps["bridge"])
        self.assertEqual(caps["camera"], {"jpeg": True, "width": 320, "height": 240, "color": False, "fps": 10.0})
        self.assertEqual(caps["display"]["width"], 128)
        self.assertEqual(caps["display"]["height"], 32)
        self.assertIn("cube_lights", caps["actuator"]["names"])
        self.assertEqual(ms[-1], {"op": "ready"})
        self.assertEqual(self.cli.names()[:3], ["start", "connect", "wait_for_robot"])
        self.assertEqual(self.cli.calls_to("wait_for_robot")[0][2], {"timeout": 10.0})
        self.assertIn(("enable_camera", (True,), {"color": False}), [c[:3] for c in self.cli.calls])
        # The runtime stops everything right after setup.
        self.assertIn("stop_all_motors", self.cli.names())

    def test_probe_never_creates_client(self):
        self.send({"op": "hello", "config": {}, "probe": True})
        self.assertEqual(self.clients, [])
        self.assertEqual([m["op"] for m in self.msgs()], ["describe"])

    def test_describe_without_pycozmo(self):
        p = Cozmo(which=lambda name: None)
        caps = p.describe({})
        self.assertIsNone(caps["say"]["engine"])
        self.assertIsNone(p.pc)

    def test_unreachable_faults_and_cleans_up(self):
        self.hello(_reachable=False, connect_timeout_s=2)
        f = self.last()
        self.assertEqual(f["op"], "fault")
        self.assertEqual(f["fault_code"], "not_connected")
        self.assertIn("Wi-Fi", f["message"])
        self.assertEqual(self.cli.names()[-2:], ["disconnect", "stop"])
        self.assertIsNone(self.p.client)
        self.p.stop()  # safe while unconnected
        self.assertEqual(self.cmd("drive", {"throttle": 1})["fault_code"], "not_ready")

    def test_stop_is_safe_before_setup(self):
        self.p.stop()
        self.p.stop_motion()
        self.assertIsNone(self.p.poll())


class DriveTest(Base):
    def setUp(self):
        super().setUp()
        self.hello()
        self.cli.calls.clear()

    def last_drive(self):
        return self.cli.calls_to("drive_wheels")[-1]

    def test_drive_maps_to_mmps_without_duration(self):
        for value, (l, r) in [
            ({"throttle": 1, "steer": 0}, (150, 150)),
            ({"throttle": -0.5, "steer": 0}, (-75, -75)),
            ({"throttle": 0, "steer": 1}, (150, -150)),       # steer > 0 turns right: left wheel forward
            ({"throttle": 1, "steer": 1}, (150, 0)),          # normalised: (2, 0) / 2
            ({"throttle": 0.5, "steer": 0.25}, (112.5, 37.5)),
            ({"throttle": 5, "steer": 0}, (150, 150)),        # clamped
        ]:
            self.assertEqual(self.cmd("drive", value)["op"], "ack", value)
            name, args, kwargs, _ = self.last_drive()
            self.assertAlmostEqual(args[0], l, msg=value)
            self.assertAlmostEqual(args[1], r, msg=value)
            self.assertLessEqual(len(args), 4)
            self.assertIsNone(kwargs.get("duration"), "duration must never be passed")

    def test_max_wheel_capped_by_hardware(self):
        p = Cozmo(client_factory=lambda pc, c: fpc.Client(), pycozmo_module=fpc.module, synthesizer=self.tts)
        p.log = lambda *a: None
        rt = Runtime(p, infile=io.StringIO(""), outfile=io.StringIO(), clock=self.clock)
        rt.feed_line(json.dumps({"op": "hello", "config": {"max_wheel_mmps": 500, "camera": False}}))
        self.assertEqual(p.wheel_speeds(1, 0), (200.0, 200.0))
        p.close()

    def test_bad_value(self):
        self.assertEqual(self.cmd("drive", {"throttle": "fast"})["fault_code"], "bad_value")
        self.assertEqual(self.cli.calls_to("drive_wheels"), [])

    def test_deadline_stops(self):
        self.cmd("drive", {"throttle": 1}, deadline_ms=300)
        self.clock.t += 0.2
        self.rt.tick()
        self.assertNotIn("stop_all_motors", self.cli.names())
        self.clock.t += 0.11
        self.rt.tick()
        self.assertEqual(self.last_drive()[1][:2], (0.0, 0.0))
        self.assertEqual(self.cli.names()[-1], "stop_all_motors")
        self.assertEqual(self.rt.stop_reasons[-1], "deadline")

    def test_heartbeat_loss_stops(self):
        self.cmd("drive", {"throttle": 1}, deadline_ms=5000)
        self.clock.t += 1.1
        self.rt.tick()
        self.assertEqual(self.rt.stop_reasons[-1], "heartbeat")
        self.assertEqual(self.cli.names()[-2:], ["drive_wheels", "stop_all_motors"])
        self.assertEqual(self.last_drive()[1][:2], (0.0, 0.0))
        self.assertEqual(self.cmd("drive", {"throttle": 1})["fault_code"], "no_heartbeat")

    def test_estop_stops_and_refuses(self):
        self.cmd("drive", {"throttle": 1})
        self.send({"op": "estop"})
        self.assertEqual(self.cli.names()[-1], "stop_all_motors")
        n = len(self.cli.calls_to("drive_wheels"))
        for kind, value in [("drive", {"throttle": 1}), ("ptz", {"tilt": 1}), ("actuator", {"name": "lift",
                                                                                         "value": 1})]:
            self.assertEqual(self.cmd(kind, value)["fault_code"], "estopped")
        self.assertEqual(len(self.cli.calls_to("drive_wheels")), n)
        self.assertEqual(self.cmd("halt", {}, id="h"), {"op": "ack", "id": "h"})
        self.send({"op": "resume"})
        self.assertEqual(self.cmd("drive", {"throttle": 1})["op"], "ack")

    def test_stop_failure_still_calls_stop_all_motors(self):
        def boom(*a, **k):
            raise OSError("socket gone")
        self.cli.drive_wheels = boom
        self.send({"op": "stop"})
        self.assertEqual(self.cli.names()[-1], "stop_all_motors")
        self.assertEqual(self.events("stop_failed")[0]["reason"], "stop")


class HazardTest(Base):
    def setUp(self):
        super().setUp()
        self.hello()

    def fire(self, event, state):
        t = threading.Thread(target=self.cli.dispatch, args=(event, self.cli, state), name="pycozmo-conn")
        t.start()
        t.join(2)

    def test_cliff_stops_from_other_thread_and_blocks_forward(self):
        self.cmd("drive", {"throttle": 1}, deadline_ms=5000)
        self.cli.calls.clear()
        self.fire(fpc.EvtCliffDetectedChange, True)
        # First two calls happen on the event thread, before the runtime lock.
        first = self.cli.calls[:2]
        self.assertEqual([c[0] for c in first], ["drive_wheels", "stop_all_motors"])
        self.assertEqual(first[0][1][:2], (0.0, 0.0))
        self.assertEqual(first[0][3].name, "pycozmo-conn")
        self.assertIn("cliff", self.rt.stop_reasons)
        self.assertIsNone(self.rt.motion_deadline)
        self.assertEqual(self.events("cliff")[0]["state"], True)

        self.assertEqual(self.cmd("drive", {"throttle": 0.5})["fault_code"], "cliff")
        self.assertEqual(self.cmd("drive", {"throttle": 0, "steer": 0.5})["fault_code"], "cliff")
        n = len(self.cli.calls_to("drive_wheels"))
        self.assertEqual(self.cmd("drive", {"throttle": -0.5})["op"], "ack")
        self.assertEqual(self.cli.calls_to("drive_wheels")[n][1][:2], (-75.0, -75.0))

        self.fire(fpc.EvtCliffDetectedChange, False)
        self.assertEqual(len(self.events("cliff_cleared")), 1)
        self.assertEqual(self.cmd("drive", {"throttle": 0.5})["op"], "ack")

    def test_picked_up_stops_and_refuses_drive(self):
        self.cmd("drive", {"throttle": 1}, deadline_ms=5000)
        self.cli.calls.clear()
        self.fire(fpc.EvtRobotPickedUpChange, True)
        self.assertEqual([c[0] for c in self.cli.calls[:2]], ["drive_wheels", "stop_all_motors"])
        self.assertEqual(self.cli.calls[0][1][:2], (0.0, 0.0))
        self.assertIn("picked_up", self.rt.stop_reasons)
        self.assertEqual(len(self.events("picked_up")), 1)
        self.assertEqual(self.cmd("drive", {"throttle": 1})["fault_code"], "picked_up")
        self.assertEqual(self.cmd("drive", {"throttle": -1})["fault_code"], "picked_up")
        self.assertEqual(self.cmd("ptz", {"tilt": 0})["op"], "ack")
        self.fire(fpc.EvtRobotPickedUpChange, False)
        self.assertEqual(self.cmd("drive", {"throttle": 1})["op"], "ack")

    def test_callback_never_raises_into_pycozmo(self):
        def boom(*a, **k):
            raise OSError("gone")
        self.cli.drive_wheels = boom
        self.cli.stop_all_motors = boom
        self.p._on_cliff(self.cli, True)  # must not raise
        self.assertTrue(self.p.cliff)


class FirmwareTest(Base):
    fw_sig = {"version": 2315, "build": "DEVELOPMENT"}

    def test_mismatch_nacks_everything_but_halt(self):
        self.hello()
        self.assertEqual(self.last(), {"op": "ready"})  # the Node stays up and reports the fault
        ev = self.events("firmware_unsupported")[0]
        self.assertEqual(ev["version"], 2315)
        self.assertEqual(ev["supported"], [2381])
        for kind, value in [("drive", {"throttle": 1}), ("ptz", {"tilt": 1}), ("say", {"text": "hi"}),
                            ("display", {"text": "hi"}), ("actuator", {"name": "lift", "value": 1})]:
            r = self.cmd(kind, value)
            self.assertEqual(r["fault_code"], "firmware_unsupported", kind)
            self.assertIn("2315", r["message"])
            self.assertIn("2381", r["message"])
        self.assertEqual(self.cli.calls_to("drive_wheels")[-1][1][:2], (0.0, 0.0))  # only stops were sent
        self.assertEqual(self.cmd("halt", {}, id="h"), {"op": "ack", "id": "h"})
        self.assertNotIn("enable_camera", self.cli.names())
        self.clock.t += 1
        self.send({"op": "heartbeat"})
        self.rt.tick()
        self.assertEqual(self.last()["faults"], ["firmware_unsupported"])

    def test_allow_firmware(self):
        self.hello(allow_firmware=[2315, 2381])
        self.assertEqual(self.events("firmware_unsupported"), [])
        self.assertEqual(self.cmd("drive", {"throttle": 1})["op"], "ack")


class FactoryFirmwareTest(Base):
    fw_sig = {"version": 2381, "build": "FACTORY"}

    def test_factory_build(self):
        self.hello()
        r = self.cmd("drive", {"throttle": 1})
        self.assertEqual(r["fault_code"], "firmware_unsupported")
        self.assertIn("factory", r["message"])


class HeadLiftLightsTest(Base):
    def setUp(self):
        super().setUp()
        self.hello()

    def test_head_tilt_bounds(self):
        for tilt, deg in [(-1, -25.0), (1, 44.5), (0, 9.75), (-3, -25.0), (3, 44.5)]:
            self.assertEqual(self.cmd("ptz", {"tilt": tilt})["op"], "ack")
            self.assertAlmostEqual(self.cli.calls_to("set_head_angle")[-1][1][0], math.radians(deg))
        self.cmd("actuator", {"name": "head", "value": 1})
        self.assertAlmostEqual(self.cli.calls_to("set_head_angle")[-1][1][0], math.radians(44.5))
        self.assertEqual(self.cmd("ptz", {"pan": 0.5})["fault_code"], "unsupported")

    def test_lift_bounds(self):
        for value, mm in [(0, 32.0), (1, 92.0), (0.5, 62.0), (-1, 32.0), (2, 92.0)]:
            self.assertEqual(self.cmd("actuator", {"name": "lift", "value": value})["op"], "ack")
            self.assertAlmostEqual(self.cli.calls_to("set_lift_height")[-1][1][0], mm)

    def test_lights(self):
        self.assertEqual(self.cmd("actuator", {"name": "backpack_lights", "value": {"r": 255, "g": 0,
                                                                                    "b": 0}})["op"], "ack")
        ls = self.cli.calls_to("set_all_backpack_lights")[-1][1][0]
        self.assertEqual((ls.on_color, ls.off_color), (31 << 10, 31 << 10))
        self.assertEqual(self.cmd("actuator", {"name": "head_light", "value": True})["op"], "ack")
        self.assertEqual(self.cli.calls_to("set_head_light")[-1][1], (True,))
        self.assertEqual(self.cmd("actuator", {"name": "head_light", "value": "on"})["fault_code"], "bad_value")
        self.assertEqual(self.cmd("actuator", {"name": "wings", "value": 1})["fault_code"], "bad_value")

    def test_cube_lights(self):
        self.assertEqual(self.cmd("actuator", {"name": "cube_lights", "value": {"g": 255}})["fault_code"],
                         "unavailable")
        self.cli.connected_objects = {1: {}, 2: {}}
        self.cli.conn.sent.clear()
        self.assertEqual(self.cmd("actuator", {"name": "cube_lights", "value": {"g": 255}})["op"], "ack")
        sent = self.cli.conn.sent
        self.assertEqual([type(p).__name__ for p in sent], ["CubeId", "CubeLights"] * 2)
        self.assertEqual([p.object_id for p in sent[::2]], [1, 2])
        self.assertEqual(sent[1].states[0].on_color, 31 << 5)
        sent.clear()
        self.assertEqual(self.cmd("actuator", {"name": "cube_lights", "value": None, "cube": 2})["op"], "ack")
        self.assertEqual(sent[0].object_id, 2)
        self.assertEqual(self.cmd("actuator", {"name": "cube_lights", "value": {}, "cube": 9})["fault_code"],
                         "unavailable")

    def test_cubes_connected_once(self):
        self.cli.available_objects = {0xAB: fpc.FakeObject(0xAB, "Block_LIGHTCUBE1"),
                                      0xCD: fpc.FakeObject(0xCD, "Charger_Basic")}
        self.cli.conn.sent.clear()
        self.p.poll()
        self.p.poll()
        self.assertEqual([(type(p).__name__, p.factory_id) for p in self.cli.conn.sent], [("ObjectConnect", 0xAB)])


class SayTest(Base):
    def test_say_plays_off_thread(self):
        self.hello()
        self.assertEqual(self.cmd("say", {"text": "hello  there\n" + "x" * 500})["op"], "ack")
        deadline = time.time() + 5
        while not self.cli.played and time.time() < deadline:
            time.sleep(0.01)
        self.assertEqual(len(self.cli.played), 1)
        path, frames, rate, channels, thread = self.cli.played[0]
        self.assertEqual((rate, channels), (22050, 1))
        self.assertAlmostEqual(frames, 1600 * 22050 / 16000, delta=2)
        self.assertIsNot(thread, threading.main_thread())
        self.assertIsNot(self.tts.threads[0], threading.main_thread())
        self.assertTrue(self.tts.texts[0].startswith("hello there x"))
        self.assertEqual(len(self.tts.texts[0]), 200)

    def test_empty_text(self):
        self.hello()
        self.assertEqual(self.cmd("say", {"text": "  "})["fault_code"], "bad_value")

    def test_missing_tts_is_unsupported(self):
        p = Cozmo(client_factory=lambda pc, c: fpc.Client(), pycozmo_module=fpc.module, which=lambda n: None)
        p.log = lambda *a: None
        out = io.StringIO()
        rt = Runtime(p, infile=io.StringIO(""), outfile=out, clock=self.clock)
        rt.feed_line(json.dumps({"op": "hello", "config": {"camera": False}}))
        rt.feed_line(json.dumps({"op": "command", "id": "s", "kind": "say", "value": {"text": "hi"}}))
        r = json.loads(out.getvalue().splitlines()[-1])
        self.assertEqual(r["fault_code"], "unsupported")
        self.assertIn("espeak-ng", r["message"])
        p.close()

    def test_find_synthesizer_prefers_espeak(self):
        self.assertEqual(tts.find_synthesizer({}, lambda n: "/usr/bin/" + n).engine, "espeak-ng")
        self.assertEqual(tts.find_synthesizer({}, lambda n: "/usr/bin/pico2wave" if n == "pico2wave" else None)
                         .engine, "pico2wave")
        self.assertIsNone(tts.find_synthesizer({}, lambda n: None))

    def test_resample_stereo_48k(self):
        d = tempfile.mkdtemp()
        src, dst = os.path.join(d, "a.wav"), os.path.join(d, "b.wav")
        write_wav(src, rate=48000, n=4800, channels=2)
        tts.to_cozmo_wav(src, dst)
        with wave.open(dst) as w:
            self.assertEqual((w.getframerate(), w.getnchannels(), w.getsampwidth()), (22050, 1, 2))
            self.assertAlmostEqual(w.getnframes(), 2205, delta=1)

    def test_clean_text(self):
        self.assertEqual(tts.clean_text("--help me\x00now", 200), "help me now")


class DisplayTest(Base):
    def setUp(self):
        super().setUp()
        self.hello()

    def shown(self):
        name, args, kwargs, _ = self.cli.calls_to("display_image")[-1]
        self.assertIsNone(kwargs.get("duration"))
        return args[0]

    def test_text(self):
        self.assertEqual(self.cmd("display", {"text": "Hello Cozmo"})["op"], "ack")
        img = self.shown()
        self.assertEqual((img.size, img.mode), ((128, 32), "1"))
        self.assertGreater(img.convert("L").histogram()[255], 20)

    def test_long_text_fits(self):
        img = render.render_text("the quick brown fox jumps over the lazy dog " * 10)
        self.assertEqual((img.size, img.mode), ((128, 32), "1"))

    def test_faces(self):
        for face in render.FACES:
            self.assertEqual(self.cmd("display", {"face": face})["op"], "ack", face)
            self.assertEqual(self.shown().size, (128, 32))
        self.assertEqual(self.cmd("display", {"face": "smug"})["fault_code"], "bad_value")

    def test_png(self):
        src = Image.new("L", (64, 64), 255)
        buf = io.BytesIO()
        src.save(buf, "PNG")
        b64 = base64.b64encode(buf.getvalue()).decode()
        self.assertEqual(self.cmd("display", {"image_png_b64": b64})["op"], "ack")
        img = self.shown()
        self.assertEqual((img.size, img.mode), ((128, 32), "1"))
        self.assertTrue(img.getpixel((64, 16)))
        self.assertFalse(img.getpixel((0, 16)))  # padded
        self.assertEqual(self.cmd("display", {"image_png_b64": "bm90IGFuIGltYWdl"})["fault_code"], "bad_value")
        self.assertEqual(self.cmd("display", {})["fault_code"], "bad_value")


class CameraTest(Base):
    def test_throttle_and_jpeg(self):
        self.hello(fps=10)
        img = Image.new("RGB", (320, 240), (200, 10, 10))
        for _ in range(3):  # same instant: only the first passes
            self.cli.dispatch(fpc.EvtNewRawCameraImage, self.cli, img)
        self.clock.t += 0.05
        self.cli.dispatch(fpc.EvtNewRawCameraImage, self.cli, img)
        self.clock.t += 0.06
        self.cli.dispatch(fpc.EvtNewRawCameraImage, self.cli, img)
        vids = [m for m in self.msgs() if m.get("op") == "video"]
        self.assertEqual([v["seq"] for v in vids], [1, 2])
        v = vids[0]
        self.assertEqual((v["format"], v["width"], v["height"]), ("jpeg", 320, 240))
        self.assertTrue(v["path"].startswith(self.frames))
        with open(v["path"], "rb") as f:
            self.assertEqual(f.read(2), b"\xff\xd8")
        self.assertEqual(Image.open(v["path"]).size, (320, 240))
        self.assertEqual([n for n in os.listdir(self.frames) if n.endswith(".tmp")], [])

    def test_camera_disabled(self):
        self.hello(camera=False)
        self.assertNotIn("enable_camera", self.cli.names())


class TelemetryTest(Base):
    def test_poll_and_low_battery_once(self):
        self.hello()
        self.cli.battery_voltage = 3.85
        self.cli.head_angle = fpc.Angle(degrees=10)
        self.cli.lift_position = fpc.LiftPosition(fpc.Distance(50.0))
        t = self.p.poll()
        self.assertEqual(t["battery"], {"volts": 3.85, "percent": 50})
        self.assertEqual(t["sensors"], {"cliff": False, "picked_up": False, "cubes": 0, "head_deg": 10.0,
                                        "lift_mm": 50.0})
        self.assertEqual(t["faults"], [])
        self.cli.battery_voltage = 3.55
        self.p.poll()
        t = self.p.poll()
        self.assertEqual(len(self.events("low_battery")), 1)
        self.assertEqual(t["faults"], ["low_battery"])

    def test_no_battery_before_first_state(self):
        self.hello()
        self.assertNotIn("battery", self.p.poll())


class EOFTest(unittest.TestCase):
    def test_run_stops_and_disconnects_at_eof(self):
        clients = []

        def factory(pc, config):
            clients.append(fpc.Client())
            return clients[-1]

        p = Cozmo(client_factory=factory, pycozmo_module=fpc.module, synthesizer=FakeTTS())
        p.log = lambda *a: None
        lines = [{"op": "hello", "config": {"camera": False}},
                 {"op": "command", "id": "d", "kind": "drive", "value": {"throttle": 1}, "deadline_ms": 5000}]
        infile = io.StringIO("".join(json.dumps(m) + "\n" for m in lines))
        out = io.StringIO()
        rt = Runtime(p, infile=infile, outfile=out)
        self.assertEqual(rt.run(), 0)
        names = clients[0].names()
        self.assertEqual(rt.stop_reasons[-1], "eof")
        i = names.index("drive_wheels", names.index("stop_all_motors") + 1)  # the drive after setup's stop
        self.assertEqual(clients[0].calls[i][1][:2], (150.0, 150.0))
        self.assertEqual(names[-4:], ["drive_wheels", "stop_all_motors", "disconnect", "stop"])
        self.assertEqual(clients[0].calls[-4][1][:2], (0.0, 0.0))
        self.assertIsNone(p.client)


if __name__ == "__main__":
    unittest.main()
