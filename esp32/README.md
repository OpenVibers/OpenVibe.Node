# OpenVibeNode — the ESP32 client

`OpenVibeNode` is an Arduino/ESP32 library that makes a board a **device** on OpenVibe.Bot's device protocol
(`bot.device-message@1`, [docs/protocol.md](../docs/protocol.md) sections 1–3): pair once, keep an outbound control
link, drive the motors, report telemetry, and hold the safety rules locally.

It is **compile-checked in CI and conformance-tested on desktop, not yet run on a board.** The protocol core is
exercised against every fixture in `internal/protocol/testdata/bot` and the scenarios below by `esp32/test/run.sh`;
the `esp32-compile` CI job only compiles the example for `esp32:esp32:esp32`. Nothing here has been on real hardware.

## What it supports

- **Pairing** — `POST /api/v1/pair` over HTTPS with the code normalised as Bot does (upper case, dash/space removed,
  `I`/`L` → `1`, `O` → `0`); the device id and credential are kept in NVS. A refusal is turned into the same sentence
  the Node prints. Plain `http://` is used only for `localhost` test servers.
- **The control link** — one outbound WebSocket to the control URL, the credential in the upgrade's `Authorization`
  header only. The device sends nothing until the server's `hello`, and the first `status`/`estop_state` wait for the
  connection's `config`.
- **Drive and actuator commands** — `drive` (differential and mecanum fields), `actuator` and `halt`. Values are
  clamped in the core, before any driver sees them, to the stricter of the server's `config.limits` and the local caps.
  `ptz`, `say` and `display` are valid protocol kinds this device has no driver for, so they are `nack unsupported`.
- **Telemetry** — `battery`, `voltage`, `rssi` and `sensors`, at most every 500 ms (Bot's cap).
- **Heartbeat and deadman** — a `heartbeat` every `config.heartbeat_ms` once `hello` has arrived, both `heartbeat_ack`
  shapes (the newer `echo`/`t` and the older `seq`) measured for RTT; two missed heartbeats stop the motors and drop
  the link; a motion command stops when its deadline lapses.
- **E-stop** — the server's `estop` latches or clears the remote stop (`config.estop_latched` re-applies a latch set
  while offline); the local kill switch can also be set through `set_local_stop()`. Every stop path calls `on_stop`.
- **Credential rotation** — `importCredential()` takes the owner's `POST /api/v1/devices/:id/rotate` response, refuses
  it when it is for another device, and rewrites the stored credential without pairing again.
- **Close codes** — 4002 and 4003 (and an upgrade refused with 401/403) stop the motors and wait at least 10 s,
  logging "pair again, or import the owner's rotation"; other drops use the documented exponential backoff with full
  jitter. (arduinoWebSockets 2.x does not surface the close code or the upgrade's status, so the transport treats a
  socket that ends before `hello` as the refused case; see *Limitations* below.)

## What it does not

- **Jobs / worker** — nothing runs jobs. A job frame is refused exactly as the protocol says for a node without the
  worker: `nack unsupported` with `class not available` (or `invalid inputs` / `missing or invalid args, ttl_ms or
  limits` when those are the first problem).
- **Video / WHIP** — no camera publishing.
- **OpenVibe.Network pairing and node tokens** — only Bot credential pairing.
- **The local CLI** — there is no `openvibe-node` binary on a board; the local kill switch is the API, not a command.

## Layout

- `src/ov_core.{h,cpp}` — the protocol core in portable C++17, no Arduino headers, only ArduinoJson v7 (header-only).
  Everything stateful lives here; time is injected and every side effect is a callback, so it runs on desktop.
- `src/OpenVibeNode.{h,cpp}` — the Arduino transport: WiFi, TLS with the ESP32 CA bundle (never `setInsecure()`), the
  arduinoWebSockets client, `HTTPClient` for pairing, and `Preferences` (NVS) for the device id and credential.
- `examples/Basic/Basic.ino` — a differential-drive example.
- `test/` — the desktop conformance harness (`run.sh` fetches the pinned ArduinoJson single header into `.deps`,
  checks its SHA256, builds with `g++ -std=c++17 -Wall -Wextra -Werror`, and runs it).

## Pairing and running the example

1. Add the robot on openvibe.bot and copy the 8-character code it shows (`XXXX-XXXX`, 10 minutes, single use).
2. In `examples/Basic/Basic.ino`, fill in `OV_WIFI_SSID`, `OV_WIFI_PASSWORD`, `OV_ROBOT` (the `rob_…` id) and
   `OV_CODE`. `OV_NAME` is what the owner sees. On later boots the stored credential is reused, so `OV_CODE` is only
   needed for the first pairing.
3. Install the ESP32 core (3.x) and the `ArduinoJson` (7.x) and `WebSockets` (2.x) libraries, then compile and upload
   `examples/Basic` for your board with the `OpenVibeNode` library from this tree.

## Wiring the example

An L298N-class dual H-bridge; change the pins in `Basic.ino` if they clash with your board.

| ESP32 | driver |
|-------|--------|
| GPIO25 | ENA (left motor PWM) |
| GPIO26 | IN1 |
| GPIO27 | IN2 |
| GPIO14 | ENB (right motor PWM) |
| GPIO32 | IN3 |
| GPIO33 | IN4 |
| GND | GND |
| battery + / − | the driver's motor supply |

The two enable pins are the `ledc` PWM channels (20 kHz, 8-bit) and the four IN pins the direction. `onDrive` mixes
`throttle ± steer` for the two wheels; the core has already clamped them. `onStop` is the only place that zeroes the
motors, and it runs on `halt`, on a motion deadline, on two missed heartbeats, on an e-stop and whenever the link
closes.

## Building the conformance harness

```sh
sh esp32/test/run.sh
```

It needs `g++` with C++17 and a way to fetch one file (curl, wget or python3). The harness prints one line per
scenario and exits non-zero on a failure.

## Limitations

- Not run on hardware yet (above).
- arduinoWebSockets 2.x delivers `WStype_DISCONNECTED` without the WebSocket close code or the upgrade's HTTP status.
  The transport therefore treats "disconnected before `hello`" as the 4002/401/403 case (10 s floor) and "disconnected
  after `hello`" as a normal drop; a 4003 sent while connected cannot be told apart and uses the normal backoff. The
  core itself implements 4002 and 4003 and the desktop harness tests them.
- Bot's `ts`/`deadline_ms` are Unix milliseconds. Before NTP the board's clock is not Unix, so envelope `ts` is only
  monotonic until the clock syncs; the core estimates the server's clock from every frame's `ts`, so deadlines still
  work, but a paired board should run NTP (`configTime`) for correct timestamps.
