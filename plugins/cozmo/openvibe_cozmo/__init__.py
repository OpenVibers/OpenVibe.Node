"""Cozmo driver: a bridge plugin for Anki/Digital Dream Labs Cozmo through PyCozmo.

Cozmo runs closed firmware and is its own Wi-Fi access point (no internet). The Node runs on a computer next to it with
one network interface joined to Cozmo's Wi-Fi and another on the internet; this plugin talks to the robot with PyCozmo
(https://github.com/zayfod/pycozmo, MIT) over the first interface.

Command mapping:

* ``drive`` ``{"throttle": -1..1, "steer": -1..1}`` -> ``drive_wheels(left, right)`` in mm/s, where left = throttle +
  steer and right = throttle - steer (steer > 0 turns right), normalised so no wheel exceeds 1, times
  ``max_wheel_mmps``. ``duration`` is never passed (PyCozmo would sleep in it); the runtime's deadline stops the wheels.
* ``ptz`` ``{"tilt": -1..1}`` -> head angle, linear from MIN_HEAD_ANGLE (-25 deg) to MAX_HEAD_ANGLE (44.5 deg).
* ``actuator``: ``head`` (-1..1, as tilt), ``lift`` (0..1 = 32..92 mm), ``backpack_lights`` / ``cube_lights``
  (``{"r","g","b"}`` 0..255, or null for off), ``head_light`` (bool).
* ``say`` ``{"text"}`` -> espeak-ng or pico2wave on a worker thread, then ``play_audio``.
* ``display`` ``{"text"} | {"face"} | {"image_png_b64"}`` -> 128x32 1-bit image -> ``display_image``.

Safety beyond the runtime's (deadline, heartbeat, EOF, estop): PyCozmo reports cliff and pick-up on its own threads;
those callbacks stop the wheels themselves at once and then go through ``runtime.safe_stop``. While a cliff is
detected, drive commands that would turn either wheel forward are refused (backing away is allowed); while the robot is
picked up every drive command is refused. Firmware PyCozmo does not support latches a ``firmware_unsupported`` fault:
setup still succeeds so the Node stays up and reports it, and every command except ``halt`` is nacked.

PyCozmo is imported lazily in setup, so describe and probe work without it installed.
"""

import os
import queue
import shutil
import tempfile
import threading
import time

from openvibe_plugin import Fault, Plugin, clamp

from . import render, tts

__version__ = "0.1.0"

# Cozmo's single-cell LiPo: about 3.5 V empty, 4.2 V full (under light load).
BATTERY_EMPTY_V = 3.5
BATTERY_FULL_V = 4.2
FRAME_RING = 4  # camera frames rotate through this many files so a slow reader keeps the frame it was told about
SAY_QUEUE = 4


def _default_client_factory(pycozmo, config):
    addr = config.get("robot_addr")  # e.g. ["172.31.1.1", 5551]; None -> PyCozmo's default
    return pycozmo.Client(
        robot_addr=tuple(addr) if addr else None,
        auto_initialize=True,
        # The animation controller is what streams audio and screen images to the robot, so it must run.
        enable_animations=True,
        # PyCozmo's procedural face would overwrite our display images; opt in with config.
        enable_procedural_face=bool(config.get("procedural_face", False)),
    )


class Cozmo(Plugin):
    driver = "cozmo"
    version = __version__
    motion_kinds = frozenset({"drive"})
    telemetry_interval_s = 0.5

    def __init__(self, client_factory=None, pycozmo_module=None, synthesizer=None, clock=time.monotonic,
                 which=shutil.which):
        """All arguments are for tests: ``client_factory(pycozmo, config)`` returns a Client-like object,
        ``pycozmo_module`` replaces ``import pycozmo``, ``synthesizer(text, wav_path)`` replaces the TTS binary
        lookup, ``clock`` drives the camera throttle."""
        self.client_factory = client_factory or _default_client_factory
        self.pc = pycozmo_module
        self.synthesizer = synthesizer
        self.clock = clock
        self.which = which
        self.cfg = {}
        self.client = None  # set only once connected; stop() is a no-op until then
        self.fault = None  # (code, message) latched at connect, e.g. firmware_unsupported
        self.cliff = False
        self.picked_up = False
        self.low_battery = False
        self.limits = {}
        self.frame_dir = None
        self._own_frame_dir = False
        self._frame_lock = threading.Lock()
        self._next_frame = 0.0
        self._frame_seq = 0
        self._say_queue = None
        self._say_thread = None
        self._work_dir = None
        self._cubes_requested = set()
        self._camera_on = False

    # Description (no hardware, no pycozmo).
    def describe(self, config):
        synth = self.synthesizer or tts.find_synthesizer(config, self.which)
        return {
            "drive": {"type": "differential", "max_mmps": self._max_wheel_mmps(config, 200.0)},
            "ptz": {"tilt": {"min": -1, "max": 1, "maps_to": "head"}},
            "actuator": {"names": ["head", "lift", "backpack_lights", "head_light", "cube_lights"],
                         "ranges": {"head": [-1, 1], "lift": [0, 1]}},
            "say": {"max_chars": int(config.get("max_say_chars", 200)),
                    "engine": getattr(synth, "engine", "custom") if synth else None},
            "display": {"width": render.WIDTH, "height": render.HEIGHT, "text": True, "faces": list(render.FACES),
                        "image": True},
            "camera": {"jpeg": True, "width": 320, "height": 240,
                       "color": bool(config.get("color", False)), "fps": self._fps(config)},
            "battery": {},
            "sensors": ["cliff", "picked_up", "head_deg", "lift_mm"],
            "bridge": True,
        }

    @staticmethod
    def _max_wheel_mmps(config, hw_max):
        return max(0.0, min(float(config.get("max_wheel_mmps", 150.0)), hw_max))

    @staticmethod
    def _fps(config):
        return max(0.1, min(float(config.get("fps", 10)), 30.0))

    # Setup and teardown.
    def setup(self, config):
        self.cfg = config
        if self.pc is None:
            try:
                import pycozmo  # noqa: F401 (lazy: describe/probe must work without it)
            except ImportError:
                raise Fault("unsupported", "PyCozmo is not installed (pip install 'openvibe-cozmo[robot]')")
            self.pc = pycozmo
        pc = self.pc
        r = pc.robot
        self.limits = {
            "head_min": r.MIN_HEAD_ANGLE.radians, "head_max": r.MAX_HEAD_ANGLE.radians,
            "lift_min": r.MIN_LIFT_HEIGHT.mm, "lift_max": r.MAX_LIFT_HEIGHT.mm,
            "wheel_mmps": self._max_wheel_mmps(config, r.MAX_WHEEL_SPEED.mmps),
            "accel": float(config.get("wheel_accel_mmps2", 0.0)),  # 0 = firmware default
        }

        cli = self.client_factory(pc, config)
        timeout = float(config.get("connect_timeout_s", 10))
        try:
            cli.start()
            cli.connect()
            cli.wait_for_robot(timeout=timeout)
        except Exception as e:
            self._teardown(cli)
            raise Fault("not_connected",
                        "could not reach Cozmo within %.0f s (%s). Join this computer to Cozmo's Wi-Fi (the network "
                        "named 'Cozmo_...'; the password is shown on Cozmo's face when you raise and lower the lift) "
                        "and check nothing else (the Cozmo app) is connected to it." % (timeout, e))
        self.client = cli
        self._check_firmware(cli, config)

        # Safety callbacks come first and are registered even when the firmware is unsupported.
        ev = pc.event
        cli.add_handler(ev.EvtCliffDetectedChange, self._on_cliff)
        cli.add_handler(ev.EvtRobotPickedUpChange, self._on_picked_up)
        self.picked_up = bool(getattr(cli, "robot_picked_up", False))
        cliff_flag = getattr(getattr(r, "RobotStatusFlag", None), "CLIFF_DETECTED", 0)
        self.cliff = bool(cliff_flag and (getattr(cli, "robot_status", 0) & cliff_flag))
        if self.fault:
            return

        self._start_speaker(config)
        if config.get("camera", True):
            self._setup_frame_dir(config)
            cli.add_handler(ev.EvtNewRawCameraImage, self._on_camera_image)
            cli.enable_camera(True, color=bool(config.get("color", False)))
            self._camera_on = True
        if "volume" in config:
            cli.set_volume(int(clamp(config["volume"], 0, 1) * 65535))
        self.log("cozmo: connected, firmware %s", (cli.robot_fw_sig or {}).get("version"))

    def _check_firmware(self, cli, config):
        sig = cli.robot_fw_sig or {}
        supported = self.pc.protocol_declaration.FIRMWARE_VERSION
        allowed = config.get("allow_firmware") or [supported]
        try:
            allowed = [int(v) for v in allowed]
        except (TypeError, ValueError):
            allowed = [supported]
        version = sig.get("version")
        build = sig.get("build")
        if build == "FACTORY":
            msg = ("Cozmo is running factory/recovery firmware (version %s); PyCozmo supports %s. Update the robot "
                   "with the Cozmo app, then reconnect." % (version, supported))
        elif version not in allowed:
            msg = ("Cozmo firmware %s is not supported (supported: %s). Update the robot with the Cozmo app, or add "
                   "the version to allow_firmware at your own risk." % (version, ", ".join(str(v) for v in allowed)))
        else:
            return
        self.fault = ("firmware_unsupported", msg)
        self.log("cozmo: %s", msg)
        self.emit_event("firmware_unsupported", version=version, build=build, supported=allowed, message=msg)

    def _setup_frame_dir(self, config):
        d = config.get("frame_dir")
        if d:
            os.makedirs(d, exist_ok=True)
            self.frame_dir, self._own_frame_dir = d, False
        else:
            self.frame_dir, self._own_frame_dir = tempfile.mkdtemp(prefix="openvibe-cozmo-frames-"), True

    def close(self):
        cli, self.client = self.client, None
        if self._say_queue is not None:
            self._say_queue.put(None)
            if self._say_thread is not None:
                self._say_thread.join(timeout=2)
            self._say_queue = self._say_thread = None
        if cli is not None:
            if self._camera_on:
                self._camera_on = False
                try:
                    cli.enable_camera(False)
                except Exception:
                    pass
            self._teardown(cli)
        for d, own in ((self.frame_dir, self._own_frame_dir), (self._work_dir, True)):
            if d and own:
                shutil.rmtree(d, ignore_errors=True)
        self._work_dir = None

    def _teardown(self, cli):
        for name in ("disconnect", "stop"):
            try:
                getattr(cli, name)()
            except Exception as e:
                self.log("cozmo: %s failed: %r", name, e)

    # Stopping. Called under the runtime lock from the main thread or from a PyCozmo thread via safe_stop.
    def _halt_wheels(self, cli):
        cli.drive_wheels(0.0, 0.0, 0.0, 0.0)

    def stop_motion(self):
        self._stop(motion_only=True)

    def stop(self):
        self._stop(motion_only=False)

    def _stop(self, motion_only):
        cli = self.client
        if cli is None:
            return
        err = None
        # Both steps always run; the first failure is re-raised so the runtime reports stop_failed.
        try:
            self._halt_wheels(cli)
        except Exception as e:
            err = e
        try:
            cli.stop_all_motors()  # wheels, head and lift
        except Exception as e:
            err = err or e
        if err is not None:
            raise err

    # Commands.
    def handle(self, cmd):
        if self.fault:
            raise Fault(*self.fault)
        if self.client is None:
            raise Fault("not_connected", "Cozmo is not connected")
        v = cmd.value if isinstance(cmd.value, dict) else {}
        k = cmd.kind
        if k == "drive":
            self._drive(v)
        elif k == "ptz":
            if "pan" in v and v["pan"] not in (0, None):
                raise Fault("unsupported", "Cozmo has no pan axis; turn with drive")
            if "tilt" in v:
                self._head(v["tilt"])
        elif k == "actuator":
            self._actuator(v)
        elif k == "say":
            self._say(v)
        elif k == "display":
            self._display(v)
        else:
            raise Fault("unsupported", "kind %s" % k)

    def wheel_speeds(self, throttle, steer):
        """(left, right) in mm/s for a throttle/steer pair; public for tests and docs."""
        t = clamp(throttle, -1, 1)
        s = clamp(steer, -1, 1)
        left, right = t + s, t - s
        m = max(1.0, abs(left), abs(right))
        vmax = self.limits.get("wheel_mmps", 150.0)
        return left / m * vmax, right / m * vmax

    def _drive(self, v):
        if "throttle" in v or "steer" in v:
            left, right = self.wheel_speeds(v.get("throttle", 0), v.get("steer", 0))
        else:  # the dryrun's holonomic form; Cozmo ignores y
            left, right = self.wheel_speeds(v.get("x", 0), v.get("rotation", 0))
        if self.picked_up:
            raise Fault("picked_up", "Cozmo is picked up; put it down on a flat surface")
        if self.cliff and (left > 0 or right > 0):
            raise Fault("cliff", "cliff detected; only backing away is allowed")
        acc = self.limits.get("accel", 0.0)
        # Never pass duration: PyCozmo sleeps in drive_wheels when it is set.
        self.client.drive_wheels(left, right, acc, acc)

    def head_radians(self, value):
        lo, hi = self.limits["head_min"], self.limits["head_max"]
        return lo + (clamp(value, -1, 1) + 1.0) / 2.0 * (hi - lo)

    def lift_mm(self, value):
        lo, hi = self.limits["lift_min"], self.limits["lift_max"]
        return lo + clamp(value, 0, 1) * (hi - lo)

    def _head(self, value):
        self.client.set_head_angle(self.head_radians(value))

    def _light_state(self, value):
        pc = self.pc
        if value is None or value is False:
            rgb = (0, 0, 0)
        elif isinstance(value, dict):
            rgb = tuple(int(clamp(value.get(c, 0), 0, 255)) for c in ("r", "g", "b"))
        elif isinstance(value, (list, tuple)) and len(value) == 3:
            rgb = tuple(int(clamp(c, 0, 255)) for c in value)
        else:
            raise Fault("bad_value", "light value must be {r,g,b} (0..255) or null")
        c = pc.lights.Color(rgb=rgb)
        return pc.protocol_encoder.LightState(on_color=c.to_int16(), off_color=c.to_int16())

    def _actuator(self, v):
        name = v.get("name")
        value = v.get("value")
        cli = self.client
        if name == "head":
            self._head(value)
        elif name == "lift":
            cli.set_lift_height(self.lift_mm(value))
        elif name == "backpack_lights":
            cli.set_all_backpack_lights(self._light_state(value))
        elif name == "head_light":
            if not isinstance(value, bool):
                raise Fault("bad_value", "head_light value must be true or false")
            cli.set_head_light(value)
        elif name == "cube_lights":
            ls = self._light_state(value)
            cubes = sorted(dict(cli.connected_objects))
            if v.get("cube") is not None:
                try:
                    want = int(v["cube"])
                except (TypeError, ValueError):
                    raise Fault("bad_value", "cube must be an object id")
                if want not in cubes:
                    raise Fault("unavailable", "cube %s is not connected (connected: %s)" % (want, cubes))
                cubes = [want]
            if not cubes:
                raise Fault("unavailable", "no cube connected")
            pe = self.pc.protocol_encoder
            for oid in cubes:
                # CubeId selects the cube the following CubeLights applies to.
                cli.conn.send(pe.CubeId(object_id=oid))
                cli.conn.send(pe.CubeLights(states=(ls, ls, ls, ls)))
        else:
            raise Fault("bad_value", "unknown actuator %r" % (name,))

    # Speech: synthesis and playback on a worker thread; handle() only queues.
    def _start_speaker(self, config):
        if self.synthesizer is None:
            self.synthesizer = tts.find_synthesizer(config, self.which)
        self._work_dir = tempfile.mkdtemp(prefix="openvibe-cozmo-tts-")
        self._say_queue = queue.Queue(maxsize=SAY_QUEUE)
        self._say_thread = threading.Thread(target=self._speaker, name="cozmo-say", daemon=True)
        self._say_thread.start()

    def _say(self, v):
        if self.synthesizer is None:
            raise Fault("unsupported", "no espeak-ng or pico2wave on this computer (apt install espeak-ng)")
        text = tts.clean_text(v.get("text", ""), int(self.cfg.get("max_say_chars", 200)))
        if not text:
            raise Fault("bad_value", "say needs text")
        try:
            self._say_queue.put_nowait(text)
        except queue.Full:
            raise Fault("busy", "speech queue full")

    def _speaker(self):
        n = 0
        while True:
            text = self._say_queue.get()
            if text is None:
                return
            n += 1
            raw = os.path.join(self._work_dir, "say-%d-raw.wav" % n)
            out = os.path.join(self._work_dir, "say-%d.wav" % n)
            try:
                self.synthesizer(text, raw)
                tts.to_cozmo_wav(raw, out)
                cli = self.client
                if cli is not None:
                    cli.play_audio(out)  # loads the whole file into packets before returning
            except Exception as e:
                self.log("cozmo: say failed: %r", e)
                self.emit_event("say_failed", error=str(e))
            finally:
                for p in (raw, out):
                    try:
                        os.remove(p)
                    except OSError:
                        pass

    def _display(self, v):
        try:
            if "image_png_b64" in v:
                img = render.render_png_b64(v["image_png_b64"])
            elif "face" in v:
                img = render.render_face(v["face"])
            elif "text" in v:
                img = render.render_text(str(v["text"])[:200])
            else:
                raise Fault("bad_value", "display needs text, face or image_png_b64")
        except ValueError as e:
            raise Fault("bad_value", str(e))
        self.client.display_image(img)  # no duration: PyCozmo would sleep

    # PyCozmo callbacks (its threads). They must never raise: an exception would kill PyCozmo's dispatch thread.
    def _on_cliff(self, cli, state):
        self._on_hazard("cliff", "cliff", state)

    def _on_picked_up(self, cli, state):
        self._on_hazard("picked_up", "picked_up", state)

    def _on_hazard(self, attr, name, state):
        state = bool(state)
        setattr(self, attr, state)
        if not state:
            self.emit_event(name + "_cleared")
            return
        # Stop the wheels right here, before waiting for the runtime lock that the main thread may hold.
        c = self.client
        if c is not None:
            try:
                self._halt_wheels(c)
                c.stop_all_motors()
            except Exception as e:
                self.log("cozmo: %s stop failed: %r", name, e)
        self.emit_event(name, state=True)
        try:
            if self.runtime is not None:
                self.runtime.safe_stop(name)
        except Exception as e:
            self.log("cozmo: safe_stop(%s) failed: %r", name, e)

    def _on_camera_image(self, cli, image):
        try:
            now = self.clock()
            with self._frame_lock:
                if now < self._next_frame or self.frame_dir is None:
                    return
                self._next_frame = now + 1.0 / self._fps(self.cfg)
                self._frame_seq += 1
                seq = self._frame_seq
            path = os.path.join(self.frame_dir, "frame-%d.jpg" % (seq % FRAME_RING))
            tmp = path + ".tmp"
            image.convert("RGB").save(tmp, "JPEG", quality=int(self.cfg.get("jpeg_quality", 70)))
            os.replace(tmp, path)  # readers never see a partial file
            self.emit_video(path, format="jpeg", width=image.width, height=image.height, seq=seq)
        except Exception as e:
            self.log("cozmo: camera frame failed: %r", e)

    # Telemetry.
    def poll(self):
        cli = self.client
        faults = [self.fault[0]] if self.fault else []
        if cli is None:
            return {"faults": faults} if faults else None
        self._connect_cubes(cli)
        t = {"sensors": {"cliff": self.cliff, "picked_up": self.picked_up,
                         "cubes": len(getattr(cli, "connected_objects", {}) or {})}}
        try:
            t["sensors"]["head_deg"] = round(cli.head_angle.degrees, 1)
            t["sensors"]["lift_mm"] = round(cli.lift_position.height.mm, 1)
        except AttributeError:
            pass
        v = float(getattr(cli, "battery_voltage", 0.0) or 0.0)
        if v > 0:  # 0 until the first RobotState arrives
            pct = (v - BATTERY_EMPTY_V) / (BATTERY_FULL_V - BATTERY_EMPTY_V) * 100.0
            t["battery"] = {"volts": round(v, 2), "percent": int(max(0.0, min(100.0, pct)))}
            threshold = float(self.cfg.get("low_battery_v", 3.6))
            if v < threshold and not self.low_battery:
                self.low_battery = True
                self.emit_event("low_battery", volts=round(v, 2), threshold=threshold)
            elif v > threshold + 0.1:  # hysteresis so a sagging cell does not flap
                self.low_battery = False
        if self.low_battery:
            faults.append("low_battery")
        t["faults"] = faults
        return t

    def _connect_cubes(self, cli):
        """PyCozmo lists cubes it hears (available_objects) but does not connect them; ask once per cube."""
        if self.fault or not self.cfg.get("connect_cubes", True):
            return
        try:
            for fid, obj in list(cli.available_objects.items()):
                if fid in self._cubes_requested:
                    continue
                kind = getattr(obj.object_type, "name", str(obj.object_type))
                if kind.startswith("Block_LIGHTCUBE"):
                    self._cubes_requested.add(fid)
                    cli.conn.send(self.pc.protocol_encoder.ObjectConnect(factory_id=fid, connect=True))
        except Exception as e:
            self.log("cozmo: cube connect failed: %r", e)
