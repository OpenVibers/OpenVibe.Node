"""Hardware access for the Adeept ADR036, behind one small interface with two backends.

* ``RealHardware`` talks to the Raspberry Pi: PCA9685 (motors and servos) over I2C with Adafruit's libraries, gpiozero
  for the ultrasonic sensor, the line-tracking inputs and the buzzer, smbus2 for the ADS7830 battery ADC and spidev for
  the WS2812 LEDs. Every import happens inside a method, so this module imports on any machine.
* ``FakeHardware`` records every write (channel throttles, servo angles, buzzer tones, SPI transfers) and lets tests
  set the distance, the line inputs and the ADC values. When the environment variable ``OPENVIBE_FAKE_HW_LOG`` names a
  file, each write is also appended to it as one JSON line, so a test can watch a plugin running as a child process.

Both backends expose the same methods; the driver never imports a hardware library itself.
"""

import json
import os
import threading

DEVICE_TREE_MODEL = "/proc/device-tree/model"
FAKE_LOG_ENV = "OPENVIBE_FAKE_HW_LOG"


def is_raspberry_pi(path=None):
    """True if the device tree says this is a Raspberry Pi."""
    try:
        with open(path or DEVICE_TREE_MODEL, "rb") as f:
            return b"Raspberry Pi" in f.read()
    except OSError:
        return False


class HardwareError(Exception):
    """A backend could not be selected or opened."""


def make_hardware(backend, model_path=None):
    """Return the backend for the config value ``backend`` ("auto", "real" or "fake")."""
    if backend == "fake":
        return FakeHardware()
    if backend == "real":
        return RealHardware()
    if backend == "auto":
        if is_raspberry_pi(model_path):
            return RealHardware()
        raise HardwareError("not a Raspberry Pi; set backend=fake to try")
    raise HardwareError("unknown backend %r (use auto, real or fake)" % (backend,))


# Real backend.

class _RealBuzzer:
    def __init__(self, pin, octaves):
        from gpiozero import TonalBuzzer
        from gpiozero.tones import Tone
        self._tone = Tone
        self.dev = TonalBuzzer(pin, octaves=octaves)

    def play_hz(self, hz):
        # An int passed to play() would be read as a MIDI note, so always build the Tone from a frequency.
        self.dev.play(self._tone(frequency=float(hz)))

    def stop(self):
        self.dev.stop()

    def close(self):
        self.dev.close()


class _RealAdc:
    def __init__(self, bus, address):
        from smbus2 import SMBus
        self.bus = SMBus(bus)
        self.address = address

    def read(self, command):
        return self.bus.read_byte_data(self.address, command)

    def close(self):
        self.bus.close()


class _RealSpi:
    def __init__(self, bus, device, mode):
        import spidev
        self.dev = spidev.SpiDev()
        self.dev.open(bus, device)
        self.dev.mode = mode

    def xfer(self, data, speed_hz):
        self.dev.xfer(list(data), int(speed_hz))

    def close(self):
        self.dev.close()


class RealHardware:
    name = "real"

    def __init__(self):
        self.pwm = None
        self._closables = []

    def open_pwm(self, address, frequency):
        import board
        import busio
        from adafruit_pca9685 import PCA9685
        i2c = busio.I2C(board.SCL, board.SDA)
        self.pwm = PCA9685(i2c, address=address)
        self.pwm.frequency = frequency
        self._closables.append(self.pwm.deinit)

    def set_pwm_frequency(self, frequency):
        self.pwm.frequency = frequency

    def motor(self, name, in1, in2):
        from adafruit_motor import motor
        m = motor.DCMotor(self.pwm.channels[in1], self.pwm.channels[in2])
        m.decay_mode = motor.SLOW_DECAY
        return m  # .throttle = -1..1 (0 brakes)

    def servo(self, name, channel, min_pulse, max_pulse, actuation_range):
        from adafruit_motor import servo
        return servo.Servo(self.pwm.channels[channel], min_pulse=min_pulse, max_pulse=max_pulse,
                           actuation_range=actuation_range)  # .angle = 0..actuation_range

    def distance_sensor(self, echo, trigger, max_distance):
        from gpiozero import DistanceSensor
        d = DistanceSensor(echo=echo, trigger=trigger, max_distance=max_distance)
        self._closables.append(d.close)
        return d  # .distance in metres, 0..max_distance

    def input_pin(self, pin):
        from gpiozero import InputDevice
        d = InputDevice(pin)
        self._closables.append(d.close)
        return d  # .value 0/1

    def adc(self, bus, address):
        a = _RealAdc(bus, address)
        self._closables.append(a.close)
        return a

    def buzzer(self, pin, octaves):
        b = _RealBuzzer(pin, octaves)
        self._closables.append(b.close)
        return b

    def spi(self, bus, device, mode):
        s = _RealSpi(bus, device, mode)
        self._closables.append(s.close)
        return s

    def close(self):
        while self._closables:
            try:
                self._closables.pop()()
            except Exception:
                pass


# Fake backend.

class _FakeMotor:
    def __init__(self, hw, name, in1, in2):
        self.hw, self.name, self.channels = hw, name, (in1, in2)
        self.decay_mode = "slow"
        self._throttle = None

    @property
    def throttle(self):
        return self._throttle

    @throttle.setter
    def throttle(self, value):
        self._throttle = value
        self.hw.record(dev="motor", name=self.name, channels=list(self.channels), throttle=value)


class _FakeServo:
    def __init__(self, hw, name, channel, min_pulse, max_pulse, actuation_range):
        self.hw, self.name, self.channel = hw, name, channel
        self.min_pulse, self.max_pulse, self.actuation_range = min_pulse, max_pulse, actuation_range
        self._angle = None

    @property
    def angle(self):
        return self._angle

    @angle.setter
    def angle(self, value):
        if value is not None and not 0 <= value <= self.actuation_range:
            raise ValueError("angle out of range")  # as adafruit_motor.servo does
        self._angle = value
        self.hw.record(dev="servo", name=self.name, channel=self.channel, angle=value)


class _FakeDistance:
    def __init__(self, hw, echo, trigger, max_distance):
        self.hw, self.echo, self.trigger, self.max_distance = hw, echo, trigger, max_distance

    @property
    def distance(self):
        return min(self.hw.distance_m, self.max_distance)


class _FakeInput:
    def __init__(self, hw, pin):
        self.hw, self.pin = hw, pin

    @property
    def value(self):
        return int(bool(self.hw.inputs.get(self.pin, 0)))


class _FakeAdc:
    def __init__(self, hw, bus, address):
        self.hw, self.bus, self.address = hw, bus, address

    def read(self, command):
        self.hw.adc_reads.append((self.address, command))
        return self.hw.adc_values.get(command, 0)


class _FakeBuzzer:
    def __init__(self, hw, pin, octaves):
        self.hw, self.pin, self.octaves = hw, pin, octaves

    def play_hz(self, hz):
        self.hw.record(dev="buzzer", pin=self.pin, hz=hz)

    def stop(self):
        self.hw.record(dev="buzzer", pin=self.pin, hz=None)


class _FakeSpi:
    def __init__(self, hw, bus, device, mode):
        self.hw, self.bus, self.device, self.mode = hw, bus, device, mode

    def xfer(self, data, speed_hz):
        self.hw.record(dev="spi", bus=self.bus, device=self.device, speed_hz=int(speed_hz), data=list(data))


class FakeHardware:
    """Records what the driver does. Tests read ``writes``/``motors``/``servos``... and set ``distance_m``,
    ``inputs[pin]`` and ``adc_values[command]``. Put a component name in ``fail`` ("pwm", "distance", "input",
    "adc", "buzzer", "spi") to make opening it raise."""

    name = "fake"

    def __init__(self, log_path=None):
        self.log_path = log_path if log_path is not None else os.environ.get(FAKE_LOG_ENV)
        self._lock = threading.Lock()
        self.writes = []
        self.fail = set()
        self.pwm = None  # {"address":..., "frequency":...}
        self.motors = {}  # name -> _FakeMotor
        self.servos = {}
        self.distance = None
        self.inputs_opened = []
        self.adc_dev = None
        self.buzzer_dev = None
        self.spi_dev = None
        self.closed = False
        # Inputs a test controls.
        self.distance_m = 2.0
        self.inputs = {}
        self.adc_values = {}
        self.adc_reads = []

    def record(self, **entry):
        with self._lock:
            self.writes.append(entry)
            if self.log_path:
                with open(self.log_path, "a") as f:
                    f.write(json.dumps(entry) + "\n")

    def _check(self, what):
        if what in self.fail:
            raise OSError("fake %s failure" % what)

    def open_pwm(self, address, frequency):
        self._check("pwm")
        self.pwm = {"address": address, "frequency": frequency}
        self.record(dev="pwm", address=address, frequency=frequency)

    def set_pwm_frequency(self, frequency):
        self.pwm["frequency"] = frequency
        self.record(dev="pwm", address=self.pwm["address"], frequency=frequency)

    def motor(self, name, in1, in2):
        m = self.motors[name] = _FakeMotor(self, name, in1, in2)
        return m

    def servo(self, name, channel, min_pulse, max_pulse, actuation_range):
        s = self.servos[name] = _FakeServo(self, name, channel, min_pulse, max_pulse, actuation_range)
        return s

    def distance_sensor(self, echo, trigger, max_distance):
        self._check("distance")
        self.distance = _FakeDistance(self, echo, trigger, max_distance)
        return self.distance

    def input_pin(self, pin):
        self._check("input")
        self.inputs_opened.append(pin)
        return _FakeInput(self, pin)

    def adc(self, bus, address):
        self._check("adc")
        self.adc_dev = _FakeAdc(self, bus, address)
        return self.adc_dev

    def buzzer(self, pin, octaves):
        self._check("buzzer")
        self.buzzer_dev = _FakeBuzzer(self, pin, octaves)
        return self.buzzer_dev

    def spi(self, bus, device, mode):
        self._check("spi")
        self.spi_dev = _FakeSpi(self, bus, device, mode)
        return self.spi_dev

    def close(self):
        self.closed = True
        self.record(dev="close")

    # Helpers for tests.
    def throttles(self):
        return {name: m.throttle for name, m in self.motors.items()}

    def last(self, dev):
        for w in reversed(self.writes):
            if w.get("dev") == dev:
                return w
        return None
