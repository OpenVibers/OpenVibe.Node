# Plugins

A plugin is a driver: a separate executable the Node core starts, speaking **JSON lines over stdin/stdout** (one JSON
object per line, UTF-8). Its stderr goes to the Node's log. The core never links driver code, so a driver can be written
in any language; the robot drivers are Python 3 because the kit libraries and PyCozmo are.

Bundled plugins: [`dryrun`](../plugins/dryrun), [`adeept_adr036`](../plugins/adeept_adr036/README.md),
[`cozmo`](../plugins/cozmo/README.md). Python plugins use the runtime in [`plugins/sdk`](../plugins/sdk) (package
`openvibe_plugin`), which implements everything in "Safety rules" below, so a driver only says how to move its
hardware.

## Configuration

`config.json` lists the plugins (see [install.md](install.md#configuration)):

```json
{"plugins": [
  {"name": "adeept_adr036", "config": {"backend": "real", "wheels": "mecanum"}},
  {"name": "my_arm", "command": ["/opt/arm/driver", "--serial", "/dev/ttyUSB0"], "env": {"ARM_DEBUG": "1"}}
]}
```

A bundled plugin needs only its `name`; the core runs `<state>/venv/bin/python -m openvibe_<name>`. Anything else needs
`command`. `config` is sent to the plugin in `hello`. The core adds `frame_dir` (where to write camera frames) unless
set, and the environment variable `OPENVIBE_NODE_STATE`.

## Lifecycle

1. The core starts the process and sends `hello`.
2. The plugin answers `describe` **before touching any hardware**.
3. Unless `hello.probe` is true, the plugin opens its hardware with every actuator stopped and sends `ready` (or
   `fault` if it cannot).
4. The core sends `heartbeat` every 250 ms and commands as they arrive; the plugin answers each command with `ack` or
   `nack` and sends `telemetry`, `event` and `video` messages whenever it has them.
5. On shutdown the core sends `stop` and closes stdin; the plugin stops everything and exits. After 2 s it is killed.

A plugin that exits is restarted with exponential backoff (1 s doubling to 30 s, reset after a minute of uptime). It
starts stopped; if the e-stop or the local kill switch is latched, the core sends `estop` right after `hello`.
A plugin that does not describe itself within 15 s, or stops reading stdin, is killed.

Probe mode (`hello` with `"probe": true`) is used by `openvibe-node pair` and `openvibe-node plugins`: the plugin
describes itself and waits for EOF without opening any hardware.

## Messages

### Core → plugin

| op          | fields                                                        | the plugin must                                        |
|-------------|---------------------------------------------------------------|--------------------------------------------------------|
| `hello`     | `config` {…}, `probe` bool                                    | answer `describe`; then set up (unless probe)          |
| `command`   | `id`, `kind`, `value`, `deadline_ms`                          | act, answer `ack` or `nack` with the same `id`         |
| `heartbeat` | `t` (core clock, ms)                                          | note the time                                          |
| `stop`      |                                                               | stop every actuator                                    |
| `estop`     |                                                               | stop every actuator and refuse motion until `resume`   |
| `resume`    |                                                               | accept motion again                                    |

`value` has already been validated and clamped to the owner's limits by the core ([protocol.md](protocol.md#commands)).
`deadline_ms` is relative to receipt and already capped.

### Plugin → core

| op          | fields                                                                                      |
|-------------|---------------------------------------------------------------------------------------------|
| `describe`  | `driver`, `version`, `protocol` (1), `capabilities` {…}, `motion_kinds` [kinds]             |
| `ready`     | — setup finished, every actuator stopped                                                    |
| `fault`     | `fault_code`, `message` — the plugin runs but cannot drive (commands get this code as nack) |
| `ack`       | `id`                                                                                        |
| `nack`      | `id`, `fault_code`, `message`                                                               |
| `telemetry` | `battery` {`volts`, `percent`}, `rssi`, `sensors` {…}, `faults` […]                         |
| `event`     | `name` (`cliff`, `picked_up`, `low_battery`, `heartbeat_lost`, `stop_failed`, …), any fields |
| `video`     | `format` (`jpeg`), `path`, optional `width`, `height`, `seq`                                |

For `video`, the plugin writes each frame to a file (write to a temporary name, then rename, and rotate a few names so
a slow reader never sees a partial file) and announces it with `{"op": "video", "format": "jpeg", "path": …}`
(`emit_video` in [`plugins/sdk`](../plugins/sdk)). The core reads the file at once and encodes it: with `ffmpeg` if
installed, otherwise with its built-in encoder at up to 5 frames per second. The core side of this contract is
`JPEGSource` in [`internal/video/sources.go`](../internal/video/sources.go#L298), selected when the plugin's
`describe` advertises `camera.jpeg: true` (`internal/node/node.go`).

## Safety rules (every plugin)

A plugin must stop every actuator itself — the core dying must never leave motors running:

- when its **stdin closes** (EOF), then exit;
- when a **motion command's deadline passes** without a newer command (`deadline_ms` after receipt);
- when it has **not heard a heartbeat for 1 s** (and refuse motion with `no_heartbeat` until heartbeats return);
- on `stop`, `estop` and a `halt` command;
- when its own sensors say so (Cozmo: cliff, pick-up).

`stop` must be idempotent and safe at any time, including before setup. Handlers must not block: slow work (speech
synthesis, file I/O) goes on a thread. Nothing but protocol lines may go to stdout; the Python runtime enforces this by
pointing file descriptor 1 at stderr and keeping a private copy for the protocol, because hardware libraries print
there.

## Capabilities

`describe.capabilities` is an open object; the core uses these keys to route commands, and Bot uses the rest to build
the panel (ADR-043 decision 7: names are open strings checked against a registry in Bot).

| key        | meaning                                                                                  | routes      |
|------------|------------------------------------------------------------------------------------------|-------------|
| `drive`    | `{"type": "differential" \| "mecanum", …}`                                               | `drive`     |
| `ptz`      | `{"pan": {"min", "max", …}, "tilt": {…}}`                                                | `ptz`       |
| `actuator` | `{"names": ["lift", …] \| ["any"], "ranges": {…}}`                                       | `actuator`  |
| `say`      | `{…}`                                                                                    | `say`       |
| `display`  | `{"width", "height", "text", "faces", "image"}`                                          | `display`   |
| `camera`   | `{"h264_command": [argv]}` (the core runs it), `{"jpeg": true}` (frames by `video`), or `{"source": "test_pattern"}` | video |
| `battery`, `sensors`, `lights`, `bridge` | informational                                              | —           |

## The bundled drivers

### `dryrun`

Accepts every kind, logs it, reports simulated battery and distance, and asks the core for the test-pattern camera.
`config.record: "<path>"` appends one JSON line per `setup`, `command` and `stop` — end-to-end tests (here and in
OpenVibe.Bot) read it to check what reached the "robot".

### `adeept_adr036` — Adeept 4WD Smart Car Kit for Raspberry Pi (on-board)

Motors on the Robot HAT's PCA9685 (I²C `0x5f`, 50 Hz): M1 front-left (15, 14), M2 rear-left (12, 13), M3 rear-right
(11, 10), M4 front-right (8, 9), all with direction −1, as in the kit's `Move.py`; the table is plugin config.
`wheels: "ordinary"` mixes `throttle`/`steer`; `"mecanum"` mixes `x`/`y`/`rotation` (FL = x − y − r, FR = x + y + r,
RL = x + y − r, RR = x − y + r, normalised). Pan/tilt servos on channels 0/1 (500–2400 µs, 180°) take `ptz`
`pan`/`tilt` or actuators `pan`/`tilt`. Other actuators: `buzzer` `{"note": "A4"}` / `{"hz": 440}` / `null` (at most 2 s
per tone), `lights` `{"r", "g", "b", "index"?}` (WS2812 over SPI 0.0). Telemetry: `distance_cm` (ultrasonic 23/24),
`line` {left, middle, right} (GPIO 22/27/17), battery from the ADS7830 at `0x48` (2 × 18650: 6.0–8.4 V). Camera:
`rpicam-vid` H.264, published by the core. `backend: "fake"` runs it on any machine. It never starts or exposes the
kit's stock server.

### `cozmo` — Anki/DDL Cozmo through PyCozmo (bridge)

The computer running the Node joins Cozmo's Wi-Fi on one interface and the internet on another. `drive` →
`drive_wheels` (mm/s, `max_wheel_mmps` default 150); `ptz` `tilt` and actuator `head` (−1..1 → −25°..44.5°); actuator
`lift` (0..1 → 32..92 mm); `backpack_lights` and `cube_lights` `{"r", "g", "b", "cube"?}`; `head_light` bool (infrared);
`say` through `espeak-ng` or `pico2wave` → 22 kHz mono PCM; `display` text, faces or a PNG on the 128 × 32 screen;
camera frames (320 × 240) as JPEG files. A cliff or pick-up stops the wheels inside the event callback and emits an
event; while at a cliff only backing away is allowed, while picked up nothing drives. Firmware other than PyCozmo's
supported version (2381, `allow_firmware`) is reported as `firmware_unsupported`.

## Writing a plugin in Python

```python
from openvibe_plugin import Fault, Plugin, clamp, run

class Arm(Plugin):
    driver, version = "my_arm", "0.1.0"
    motion_kinds = frozenset({"drive", "actuator"})   # stop these at their deadline

    def describe(self, config):
        return {"actuator": {"names": ["shoulder", "gripper"]}}

    def setup(self, config):
        self.bus = open_serial(config["port"])           # every actuator stopped when this returns

    def handle(self, cmd):
        if cmd.kind != "actuator":
            raise Fault("unsupported")
        self.bus.speed(cmd.value["name"], clamp(cmd.value.get("value"), -1, 1))

    def stop(self):
        if getattr(self, "bus", None):
            self.bus.stop_all()

if __name__ == "__main__":
    raise SystemExit(run(Arm()))
```

Test it with `openvibe_plugin.Runtime(plugin, infile, outfile, clock)` and a fake clock, as the bundled plugins do.

## Where OpenVibe.Actor attaches (later)

Actor's computer control (terminal, files, screen, keyboard and mouse, browser, the machine's own coding agents) will
be one more plugin, `computer`, on the same protocol; the core does not change:

- **Capabilities** it will describe: `terminal`, `files`, `screen`, `input`, `browser`, `agents`, each with what the
  machine allows (for example `{"screen": {"displays": 2}, "agents": {"names": ["claude-code"]}}`).
- **Command kinds** it will add: `exec`, `fs`, `screen`, `input`, `browser`, `agent`. The core routes by capability
  key, so new kinds need only a line in `protocol.Kinds` and `plugins.capabilityFor`, plus clamping rules in
  `safety.Limits.Clamp` if they carry magnitudes. None of them are motion kinds, so deadlines do not apply; they are
  not in `GuardedKinds`, so an e-stop does not block them unless Actor asks for that.
- **Control levels** (Observe, Ask, Trusted, Full control) are enforced twice: by Bot/Actor when it issues a command
  (the operator's `role`), and in the plugin, which reads the owner's local level from its `config` and answers
  `nack not_allowed` for anything above it. `Ask` makes the plugin hold the command and raise an `event` `approval`
  that the local UI answers; the command's reply waits (the core's reply timeout is max(deadline, 1 s), so Actor
  commands will carry a long `deadline_ms` and the cap will come from Actor's own profile).
- **Output larger than a line** (screenshots, file contents, terminal streams) uses the `video` pattern: the plugin
  writes a file and sends its path in an `event`, and the core uploads it; the upload path is to be designed with Actor.
- The local kill switch (`openvibe-node stop`) will also make the `computer` plugin drop to Observe.
