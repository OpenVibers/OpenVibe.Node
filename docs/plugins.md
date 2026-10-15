# Plugins

A plugin is a driver: a separate executable the Node core starts, speaking **JSON lines over stdin/stdout** (one JSON
object per line, UTF-8). Its stderr goes to the Node's log. The core never links driver code, so a driver can be written
in any language; the robot drivers are Python 3 because the kit libraries and PyCozmo are.

Bundled plugins: [`dryrun`](../plugins/dryrun), [`adeept_adr036`](../plugins/adeept_adr036/README.md),
[`cozmo`](../plugins/cozmo/README.md), [`relay`](../plugins/relay), [`ros2`](../plugins/ros2/README.md). Python plugins use the runtime in [`plugins/sdk`](../plugins/sdk) (package
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
| `approval`  | `id`, `approved` bool                                         | the owner's answer to an `approval` event it raised    |
| `tray`      | `attached` bool, `ttl_s`                                      | (computer plugin only) the tray shows its approvals for `ttl_s` s unless told again: its own desktop prompts may stay quiet |

`value` has already been validated and clamped to the owner's limits by the core ([protocol.md](protocol.md#commands)).
`deadline_ms` is relative to receipt and already capped.

### Plugin → core

| op          | fields                                                                                      |
|-------------|---------------------------------------------------------------------------------------------|
| `describe`  | `driver`, `version`, `protocol` (1), `capabilities` {…}, `motion_kinds` [kinds]             |
| `ready`     | — setup finished, every actuator stopped                                                    |
| `fault`     | `fault_code`, `message` — the plugin runs but cannot drive (commands get this code as nack); `ready` again when it can (the robot came back) ends the fault |
| `ack`       | `id`                                                                                        |
| `nack`      | `id`, `fault_code`, `message`                                                               |
| `telemetry` | `battery` {`volts`, `percent`}, `rssi`, `sensors` {…}, `faults` […]                         |
| `event`     | `name` (`cliff`, `picked_up`, `low_battery`, `heartbeat_lost`, `stop_failed`, …), any fields |
| `video`     | `format` (`jpeg`), `path`, optional `width`, `height`, `seq`                                |
| `local_stop` | `reason` — the driver's own stop (a button, a sensor it trusts): latches the Node's local stop, see below |

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

**A driver's own stop.** A driver with a stop of its own (a stop button on the robot, a hotkey on a bridge computer,
a sensor it trusts) sends `local_stop` (Python: `self.local_stop("reason")`). The core stops every plugin at once and
latches its **local** stop with that reason, persisted across restarts; Bot shows the robot as stopped on the robot.
Only `openvibe-node resume` on that machine releases it: the owner's remote clear does not, and nothing a plugin sends
releases a latch. The Python runtime refuses motion from the moment it is called and ignores a `resume` that crosses it
on the wire. `openvibe-node stop` sets the same latch from a terminal.

A plugin whose setup can fail only because something is not up yet (the robot's computer still booting) sets
`retry_setup_s` (Python): the runtime sends the fault once, sets up again that often, and sends `ready` when it works,
which ends the fault. Without it, a failed setup stays failed until the core restarts the plugin.

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
| `button`   | `{…}`                                                                                    | `button`    |
| `point`    | `{…}`                                                                                    | `point`     |
| `camera`   | `{"h264_command": [argv]}` (the core runs it), `{"jpeg": true}` (frames by `video`), or `{"source": "test_pattern"}` | video |
| `battery`, `sensors`, `lights`, `bridge` | informational                                              | —           |

For the Adeept 4WD Smart Car's on-Pi pass — wiring, pairing, the panel checklist and camera troubleshooting — see
[hardware-adeept.md](hardware-adeept.md). For Cozmo's bridge pass — the two interfaces, pairing, firmware and the panel
checklist — see [hardware-cozmo.md](hardware-cozmo.md).

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

### `ros2` — ROS 2 robots (rclpy or rosbridge)

`drive` → `geometry_msgs/msg/Twist` on `cmd_vel` (`throttle`/`steer`, or `x`/`y`/`rotation` with `holonomic`), scaled by
`max_linear` m/s and `max_angular` rad/s, and a zero Twist at every stop; `say` → `std_msgs/msg/String` on `say_topic`.
Telemetry from `sensor_msgs/msg/BatteryState`, `sensor_msgs/msg/Range` or the front arc of a `sensor_msgs/msg/LaserScan`
(`distance_cm`) and `nav_msgs/msg/Odometry`; camera frames from a JPEG `sensor_msgs/msg/CompressedImage`. It reaches ROS 2
as a node of its own (`rclpy`, in a sourced ROS 2 environment) or through a rosbridge WebSocket server (no ROS 2
libraries where the Node runs; the installer's `--driver ros2` sets this up). When ROS is not up yet, or the link drops,
it reports `ros_unreachable` and tries again every `retry_s` seconds (`retry_setup_s` in the Python runtime). Topics,
speeds and setup: [plugins/ros2/README.md](../plugins/ros2/README.md).

### `relay` — existing OpenVibe.Live hardware clients

Use this when the owner already has a local script receiving Live stream controls. In `config.json`, replace the
`dryrun` plugin entry with one of these (the release bundle and installer include `relay`):

```json
{"name": "relay", "config": {"output": "child", "argv": ["python3", "/home/pi/controls.py", "--stdin"]}}
```

The child receives one JSON object per line on **its stdin**. `argv` is passed directly to the OS, never a shell
command. If it exits, the relay restarts it with backoff. On Node shutdown, the relay closes its stdin and stops
the process. A script that already uses Live's WebSocket protocol may instead use:

```json
{"name": "relay", "config": {"output": "websocket", "port": 8765}}
```

It listens only on `127.0.0.1` (default port 8765) and broadcasts to every connected client. The one-line change for
an existing Pi script is its Live socket URL — for Live's own example
[`control-bridge-example-cozmo.py`](https://github.com/OpenVibers/OpenVibe.Live/blob/main/control-bridge-example-cozmo.py)
that is line 27, `WS_URL = "wss://openvibe.live/ws/control?mode=hardware&stream_key=YOUR_STREAM_KEY"` →
`ws://127.0.0.1:8765/ws/control?mode=hardware`; no stream key is needed locally, and the relay ignores the path and
query. The WebSocket server uses only Python's standard library. Clients may send nothing; text messages are ignored
and protocol pings receive pongs.

Both outputs carry the hardware-facing Live fields exactly as Live's hardware socket sends them
(`server/controls/control-server.js:360,475,527`, dispatched by `control-bridge-example-cozmo.py:148-156`):
`{"type":"command","command":"forward"}` for a press, `{"type":"key_down","command":"forward"}` /
`{"type":"key_up","command":"forward"}` for a hold, and `{"type":"video_click","x":0.25,"y":0.75}` for a point
(rounded to four decimal places, as Live does). The Bot button `name` is the old Live button's **command string**,
not its label. Live's `key_held` / `key_released` names are viewer status broadcasts on its control socket, never the
hardware messages; the hardware socket, and so this relay, sends `key_down` / `key_up`. The old `control_id`,
`from_user` and `timestamp` fields are unavailable in Bot's Node → plugin command; Live's example bridge reads
`from_user` only for logging and defaults it, so hardware clients treat all three as optional. The SDK releases all
held buttons on deadline, missed heartbeat, stop, e-stop, halt and stdin close.

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

### `computer` — the machine's terminal, files and screen ([computer.md](computer.md))

Actor's computer control (plan T14/T17), on the same protocol. Capabilities `terminal`, `files` and `screen`, each
with the owner's control level (`observe`, `ask`, `trusted`, `full`) and folders; command kinds `exec`, `fs` and
`screen` (not motion kinds: long deadlines, no clamping, not in `GuardedKinds`; the core refuses `exec` and file writes
while a stop is latched). Its `ack` carries the answer in `data`. The level is enforced in the plugin from its local
config, never from a command; `ask` holds a command (`Deferred` in the SDK), raises an `approval` event and answers
once the owner runs `openvibe-node approve|deny <id>`, which the core forwards to the plugin as `{"op": "approval", "id",
"approved"}`. The local kill switch drops it to `observe`. `input` (mouse and keyboard) and `browser` are kinds on the
same plugin; the machine's own coding agents come later. While `openvibe-node tray` runs, the core sends the plugin
`{"op": "tray", "attached": true, "ttl_s": 15}` every few seconds (and `attached: false` when it quits), and the plugin
leaves the desktop prompts to the tray.

**SDK additions it uses:** `handle()` may return a dict (the ack's `data`) or raise `Deferred` and answer later with
`complete(id, data)` / `fail(id, code, message)` from any thread; `on_approval(id, approved)`; kinds that move nothing
keep their full `deadline_ms` (motion kinds stay capped at 5 s).

## Community plugins

A community robot's profile (`community.<name>`, submitted on openvibe.bot and reviewed by an OpenVibe admin) names
the plugin its robots run in `mapping.plugin`. The Node never downloads a plugin: its owner installs it from the
source the profile's page links to and lists it in the config's `plugins`, as with any plugin.

Before installing one, check that the profile is the one that was reviewed:

```sh
openvibe-node profile verify community.ballbot            # Bot from the config, else https://openvibe.bot
openvibe-node profile verify community.ballbot --key-id a7a0f02462c68119   # accept only this signing key
```

The Node fetches the profile (`GET /api/v1/profiles/:id`, with the `signature` Bot made on approval) and Bot's key
(`GET /api/v1/profiles/signing-key`). It checks that the key's id is the first 16 hex digits of the SHA-256 of its SPKI
DER and that the signature names that key. Then it verifies Ed25519 over the profile's canonical JSON: every object's
keys sorted by UTF-16 code units, no whitespace, strings and numbers as JavaScript's `JSON.stringify` writes them
(`internal/profilesig`; its test fixture was signed by Bot's own code). The check fails, with "do not install its
driver", when:

- the profile has no signature;
- the signature does not match;
- the key is not the pinned one.

`pair` runs the same check when the robot uses a community profile. It also checks that the profile Bot sent the Node
is the signed one in the catalogue. A failure is a warning: pairing goes on, and the plugin stays uninstalled until
the owner installs it.
