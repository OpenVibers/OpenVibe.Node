"""A stand-in for the parts of PyCozmo 0.8.0 the plugin uses, so tests run without it (or a robot).

Names, signatures and constants mirror pycozmo.Client, pycozmo.robot, pycozmo.event, pycozmo.lights and
pycozmo.protocol_encoder. Every Client call is recorded in ``calls`` as (name, args, kwargs, thread).
"""

import collections
import math
import threading
import types
import wave


class Angle:
    def __init__(self, radians=None, degrees=None):
        self.radians = radians if radians is not None else math.radians(degrees)

    @property
    def degrees(self):
        return math.degrees(self.radians)


class Distance:
    def __init__(self, mm):
        self.mm = mm


class Speed:
    def __init__(self, mmps):
        self.mmps = mmps


class LiftPosition:
    def __init__(self, height):
        self.height = height


class RobotStatusFlag:
    IS_PICKED_UP = 0x8
    CLIFF_DETECTED = 0x10000


robot = types.SimpleNamespace(
    MIN_HEAD_ANGLE=Angle(degrees=-25), MAX_HEAD_ANGLE=Angle(degrees=44.5),
    MIN_LIFT_HEIGHT=Distance(mm=32.0), MAX_LIFT_HEIGHT=Distance(mm=92.0),
    MAX_WHEEL_SPEED=Speed(mmps=200.0), RobotStatusFlag=RobotStatusFlag)


class Event:
    pass


class EvtCliffDetectedChange(Event):
    pass


class EvtRobotPickedUpChange(Event):
    pass


class EvtNewRawCameraImage(Event):
    pass


event = types.SimpleNamespace(EvtCliffDetectedChange=EvtCliffDetectedChange,
                              EvtRobotPickedUpChange=EvtRobotPickedUpChange,
                              EvtNewRawCameraImage=EvtNewRawCameraImage)


class Color:
    def __init__(self, int_color=None, rgb=None, name=None):
        self.rgb = rgb

    def to_int16(self):
        r, g, b = self.rgb
        return (r * 31 // 255) << 10 | (g * 31 // 255) << 5 | (b * 31 // 255)


lights = types.SimpleNamespace(Color=Color)


class Packet:
    def __init__(self, **kw):
        self.__dict__.update(kw)

    def __repr__(self):
        return "%s(%r)" % (type(self).__name__, self.__dict__)


class LightState(Packet):
    def __init__(self, on_color=0, off_color=0, on_frames=0, off_frames=0, transition_on_frames=0,
                 transition_off_frames=0, offset=0):
        super().__init__(on_color=on_color, off_color=off_color)


class CubeId(Packet):
    def __init__(self, object_id=0, rotation_period_frames=0):
        super().__init__(object_id=object_id, rotation_period_frames=rotation_period_frames)


class CubeLights(Packet):
    def __init__(self, states=()):
        assert len(states) == 4
        super().__init__(states=states)


class ObjectConnect(Packet):
    def __init__(self, factory_id=0, connect=False):
        super().__init__(factory_id=factory_id, connect=connect)


protocol_encoder = types.SimpleNamespace(LightState=LightState, CubeId=CubeId, CubeLights=CubeLights,
                                         ObjectConnect=ObjectConnect)
protocol_declaration = types.SimpleNamespace(FIRMWARE_VERSION=2381)


class ConnectionTimeout(Exception):
    pass


class Conn:
    def __init__(self, client):
        self.client = client
        self.sent = []

    def send(self, pkt):
        self.sent.append(pkt)


class ObjectType:
    def __init__(self, name):
        self.name = name


class FakeObject:
    def __init__(self, factory_id, object_type):
        self.factory_id = factory_id
        self.object_type = ObjectType(object_type)


class Client:
    """Records calls; ``reachable=False`` makes wait_for_robot time out."""

    def __init__(self, robot_addr=None, protocol_log_messages=None, auto_initialize=True, enable_animations=True,
                 enable_procedural_face=True, fw_sig=None, reachable=True):
        self.calls = []
        self.conn = Conn(self)
        self.reachable = reachable
        self._fw = fw_sig if fw_sig is not None else {"version": 2381, "build": "DEVELOPMENT"}
        self.robot_fw_sig = None
        self.dispatch_handlers = collections.defaultdict(list)
        self.head_angle = Angle(radians=robot.MIN_HEAD_ANGLE.radians)
        self.lift_position = LiftPosition(height=Distance(mm=32.0))
        self.battery_voltage = 0.0
        self.robot_status = 0
        self.robot_picked_up = False
        self.available_objects = {}
        self.connected_objects = {}
        self.played = []  # (path, frames, rate, channels, thread) for play_audio

    def _rec(self, name, *args, **kwargs):
        self.calls.append((name, args, kwargs, threading.current_thread()))

    def names(self):
        return [c[0] for c in self.calls]

    def calls_to(self, name):
        return [c for c in self.calls if c[0] == name]

    # Lifecycle.
    def start(self):
        self._rec("start")

    def stop(self):
        self._rec("stop")
        self.dispatch_handlers = collections.defaultdict(list)

    def connect(self):
        self._rec("connect")

    def disconnect(self):
        self._rec("disconnect")

    def wait_for_robot(self, timeout=5.0):
        self._rec("wait_for_robot", timeout=timeout)
        if not self.reachable:
            raise ConnectionTimeout("Failed to connect to Cozmo.")
        self.robot_fw_sig = self._fw

    # Events (pycozmo.event.Dispatcher).
    def add_handler(self, event, f, one_shot=False):
        self.dispatch_handlers[event].append(f)

    def dispatch(self, event, *args, **kwargs):
        for f in list(self.dispatch_handlers[event]):
            f(*args, **kwargs)

    # Motion.
    def drive_wheels(self, lwheel_speed, rwheel_speed, lwheel_acc=0.0, rwheel_acc=0.0, duration=None):
        self._rec("drive_wheels", lwheel_speed, rwheel_speed, lwheel_acc, rwheel_acc, duration=duration)

    def stop_all_motors(self):
        self._rec("stop_all_motors")

    def set_head_angle(self, angle, accel=10.0, max_speed=10.0, duration=0.0):
        self._rec("set_head_angle", angle)

    def set_lift_height(self, height, accel=10.0, max_speed=10.0, duration=0.0):
        self._rec("set_lift_height", height)

    # Lights, screen, sound, camera.
    def set_all_backpack_lights(self, light):
        self._rec("set_all_backpack_lights", light)

    def set_head_light(self, enable):
        self._rec("set_head_light", enable)

    def display_image(self, im, duration=None):
        if im.size != (128, 32) or im.mode != "1":  # what pycozmo.image_encoder.ImageEncoder enforces
            raise ValueError("bad image %s %s" % (im.size, im.mode))
        self._rec("display_image", im, duration=duration)

    def play_audio(self, fspec):
        with wave.open(fspec, "rb") as w:  # what pycozmo.audio.load_wav enforces
            if w.getsampwidth() != 2 or w.getframerate() not in (22050, 48000):
                raise ValueError("Invalid audio format")
            self.played.append((fspec, w.getnframes(), w.getframerate(), w.getnchannels(),
                                threading.current_thread()))
        self._rec("play_audio", fspec)

    def enable_camera(self, enable=True, color=False):
        self._rec("enable_camera", enable, color=color)

    def set_volume(self, level):
        self._rec("set_volume", level)


module = types.SimpleNamespace(Client=Client, robot=robot, event=event, lights=lights,
                               protocol_encoder=protocol_encoder, protocol_declaration=protocol_declaration)
