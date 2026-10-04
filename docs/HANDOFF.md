# Handoff: build the OpenVibe Node (cloud session brief)

You are starting the OpenVibe Node from an empty repository. Read this whole file and `docs/ADR-043-bot-devices-and-control.md`
first; they are binding. Work on a branch `node-core`, commit in small steps with clear messages, push, and open a pull
request when the core and both robot plugins pass their tests. Do not deploy anything; you have no access to OpenVibe's
servers and do not need it.

## What the Node is

One service people install on any computer, server, Raspberry Pi or phone. It pairs with OpenVibe once, connects
**out** (no open ports), and lets OpenVibe products use the device: first **OpenVibe.Bot** (robots: drive, cameras,
sensors, with safety), later **OpenVibe.Actor** (computer control for AI agents: terminal, files, screen, keyboard and
mouse, the machine's own coding agents). One install serves robots and computers. Owner decisions (2026-09-29): Bot is an
open control panel for robots; the owner's first robots are an **Adeept 4WD Smart Car Kit for Raspberry Pi (ADR036)**
and a **Cozmo**.

## Architecture (decided)

- **Core: Go**, one static binary per platform (linux amd64/arm64/armv7, darwin amd64/arm64, windows amd64). Module path
  `github.com/OpenVibers/OpenVibe.Node`. Libraries: `github.com/pion/webrtc/v4` (WHIP publishing), `nhooyr.io/websocket`
  or `gorilla/websocket`, `github.com/kardianos/service` (systemd / launchd / Windows service). Keep dependencies few.
- **Drivers are plugins**: separate executables the core starts, speaking **JSON lines over stdin/stdout** (one object per
  line). Robot plugins are **Python 3** (the kit libraries and PyCozmo are Python). A plugin must stop every actuator
  itself when its stdin closes, when a command's deadline passes, or when it has not heard a heartbeat from the core for
  1 s — the core dying must never leave motors running.
- **Layout**: `cmd/openvibe-node/` (CLI), `internal/{config,credentials,link,protocol,safety,plugins,video,service}/`,
  `plugins/{dryrun,adeept_adr036,cozmo}/` (Python packages with their own tests), `install/install.sh`,
  `.github/workflows/ci.yml`, `docs/{protocol.md,plugins.md,install.md}`.

## The CLI

`openvibe-node pair <CODE>` (redeems a one-time pairing code, stores the credential), `openvibe-node run` (foreground),
`openvibe-node install` / `uninstall` (as a service), `openvibe-node status`, `openvibe-node stop` (**the local kill
switch**: stops every plugin's actuators immediately and holds them stopped until `openvibe-node resume`), `openvibe-node
plugins` (list, with capabilities). Config in `/etc/openvibe-node/` (or the platform's equivalent); the credential file is
mode 0600 and its contents never appear in logs, errors or `status`.

## Talking to OpenVibe.Bot (ADR-043 decisions 1, 2, 4, 5, 6)

- Pairing: `POST https://openvibe.bot/api/v1/pair` with `{code, agent_version, device_kind: "onboard"|"bridge",
  drivers: [...], capabilities: {...}}` → `{device_id, credential, publish_key, profile}`. Store credential and publish key.
- Control link: `wss://openvibe.bot/device`, credential in the `Authorization: Bearer` header of the upgrade (never in the
  URL). Every frame is JSON with `v` (1), `seq` (per-direction counter), `ts` (ms). Server→device: `hello`, `config`
  (limits, heartbeat_ms, deadman_ms), `command` (`id`, `kind` drive|actuator|ptz|say|display|halt, `value`, `deadline_ms`,
  `operator`, `role`), `estop` (latched), `heartbeat_ack`. Device→server: `status` (agent version, drivers, faults,
  estop_latched), `telemetry` (≤ 2 Hz: battery, rssi, sensors), `ack` / `nack` (`id`, `fault_code`), `heartbeat` (1 s),
  `estop_state`. A repeated command `id` is answered with the first result and never executed twice. Commands are never
  queued while offline and never replayed after a reconnect. Reconnect with exponential backoff and jitter.
- The service side (OpenVibe.Bot) is being built at the same time; its `docs/protocol.md` will be the exact wire format.
  Keep every wire detail inside `internal/protocol/` so aligning later is one package. Build a **fake Bot server** in Go
  test code that speaks this protocol and drive all link tests against it.

## Safety (ADR-043 decision 6; not optional)

- Every motion command carries a deadline (`deadline_ms`: an absolute instant on the server's clock, 300 ms after Bot sent it,
  capped by the profile's `max_command_ms`); the core nacks `expired` a command that arrives after it and gives the plugin
  only the time left, and the plugin stops at that deadline unless a newer command arrived. Held controls are re-sent by the operator every 150 ms.
- Stop all actuators on: 2 missed heartbeats, link loss, core shutdown, plugin crash (the core restarts it with backoff
  and it starts stopped), `openvibe-node stop`, an `estop`.
- The e-stop is latched locally (persisted across restarts) and only a server `estop` `latched:false` from the owner (or the local
  `resume`) clears it. Owner limits (`max_speed`, `max_turn`) clamp every value in the core before it reaches a plugin.
- Never run or expose a kit's stock control server (the Adeept kit ships one on 0.0.0.0:8888 with the fixed login
  `admin:123456` and an MJPEG stream on :5000 — the Node replaces it; `install.sh` disables it if it finds it enabled).

## Plugin protocol (write `docs/plugins.md`)

Core→plugin: `{"op":"hello","config":{...}}`, `{"op":"command","id":..,"kind":..,"value":..,"deadline_ms":..}`,
`{"op":"heartbeat","t":..}`, `{"op":"stop"}`, `{"op":"estop"}`. Plugin→core: `{"op":"describe","capabilities":{...},
"driver":"...","version":"..."}`, `{"op":"ack"|"nack","id":..,"fault_code":..}`, `{"op":"telemetry",...}`,
`{"op":"event","name":"cliff"|"picked_up"|"low_battery"|...}`, `{"op":"video","format":"jpeg","path":...}` for plugins
that produce camera frames.

## Plugin 1: `adeept_adr036` (the owner's first robot)

Raspberry Pi OS Bookworm, Python 3, `adafruit-circuitpython-pca9685`, `adafruit-circuitpython-motor`, `gpiozero`,
`spidev`, `smbus2`. From the kit's own code (Adeept ADR036, 2025–2026):
- **Motors**: PCA9685 at I²C **0x5f**, frequency 50; DC motors M1 = channels (15, 14), M2 = (12, 13), M3 = (11, 10),
  M4 = (8, 9) via `adafruit_motor.motor.DCMotor`, `SLOW_DECAY`; throttle -1..1. Two variants: **ordinary wheels**
  (differential: throttle + steer) and **mecanum** (x, y, rotation). Check the kit's wheel-to-motor assignment in its
  `Move.py` (`move(speed, direction, turn, radius)`) and mirror it; make the mapping a table in the plugin config.
- **Servos**: same PCA9685, pan = channel 0, tilt = channel 1, `adafruit_motor.servo.Servo(min_pulse=500,
  max_pulse=2400, actuation_range=180)`; limits in config.
- **Sensors**: ultrasonic `gpiozero.DistanceSensor(echo=24, trigger=23, max_distance=2)`; line tracking inputs GPIO 22
  (left), 27 (middle), 17 (right); battery through an **ADS7830 at I²C 0x48** (`read_byte_data(0x48, cmd)` per channel;
  report volts and a percentage for a 2×18650 pack).
- **Buzzer**: `gpiozero.TonalBuzzer(18)`; **WS2812 LEDs** over SPI (`spidev`, bus 0 device 0).
- **Camera**: Picamera2/libcamera. The core publishes it: prefer `rpicam-vid --codec h264 --inline -o -` piped into the
  WHIP publisher; fall back to MJPEG frames from the plugin.
- Tests: a fake I²C/GPIO layer so the plugin runs on any machine; assert the deadline stop, stdin-EOF stop, estop, and the
  exact channel numbers.

## Plugin 2: `cozmo` (a bridge plugin)

Cozmo runs closed firmware and makes its own Wi-Fi access point with no internet. The Node runs on a computer next to it
(a Pi or a laptop) joined to Cozmo's Wi-Fi on one interface and to the internet on another. Use **PyCozmo** (MIT,
`pip install pycozmo`, github.com/zayfod/pycozmo): `pycozmo.connect()` / `Client`, `drive_wheels(l, r, duration)`,
`set_head_angle`, `set_lift_height`, `play_audio` (for say: synthesize with `espeak-ng` or `pico2wave` to 22 kHz mono
PCM), `display_image` (text/faces rendered with Pillow to 128×32/64), backpack and cube lights, camera frames
(`enable_camera`, 320×240, grey or colour) handed to the core as JPEG, battery voltage, **cliff and pick-up events that
stop the wheels at once**. Check the robot's firmware at connect and report `nack` with a clear fault if PyCozmo does not
support it. Map commands so Cozmo's head and lift appear as ptz-like actuators. Tests with a fake PyCozmo client.

## Plugin 0: `dryrun`

Accepts every command, logs it, answers telemetry and a test-pattern video source — used by CI, by people trying the
Node on a laptop, and by the service's end-to-end tests.

## Video (WHIP)

`internal/video`: publish H.264 over WHIP with pion to the `whip_url` + `publish_key` the pairing returns. Sources: test
pattern (dry-run), an H.264 byte stream from a child process (`rpicam-vid`, `ffmpeg -f v4l2`), JPEG frames from a plugin
(encoded through `ffmpeg`). Reconnect on failure; video never blocks control.

## install.sh and CI

`install/install.sh` (`curl -fsSL https://openvibe.bot/install | sh -s -- --robot rob_... --code ABCD-1234 --driver adeept`):
detect OS/arch, download the release binary and the plugin bundle, create a Python virtual environment for plugins,
install the service, pair with the code.
CI: `go vet`, `go test ./...` (race detector), cross-compile every target, Python plugin tests, and a release workflow on
tags that attaches the binaries.

## Later, not in this session

Actor's computer control plugin (terminal, files, screen, input, browser, local coding agents) with control levels
(Observe, Ask, Trusted, Full control) — design the plugin and capability interfaces so it drops in without changing the
core. Say in `docs/` where it will attach.

## Done means

`go test ./...` and the Python plugin tests green in CI on the PR; `openvibe-node run` with `dryrun` pairs against the fake
server, publishes a test pattern, obeys commands and stops on every safety path; the Adeept and Cozmo plugins pass their
tests with fake hardware; `docs/protocol.md`, `docs/plugins.md` and `docs/install.md` written; README says what the Node
is and how to install it.
