# OpenVibeNode — the ESP32 client

`OpenVibeNode` is an Arduino/ESP32 library that makes a board a **device** on OpenVibe.Bot's device protocol
(`bot.device-message@1`, [docs/protocol.md](../docs/protocol.md) sections 1–3): pair once, keep an outbound control
link, drive the motors, report telemetry, and hold the safety rules locally.

It is **conformance-tested on desktop and compile-checked in CI, not yet run on a board.** The protocol core is
exercised against every fixture in `internal/protocol/testdata/bot` and the scenarios below by `esp32/test/run.sh`;
the `esp32-compile` CI job compiles `examples/Basic` for `esp32:esp32:esp32` against the pinned core (3.1.1),
ArduinoJson (7.4.2) and WebSockets (2.6.1). Nothing here has been on real hardware.

## What it supports

- **Pairing** — `POST /api/v1/pair` over HTTPS with the code normalised as Bot does (upper case, dash/space removed,
  `I`/`L` → `1`, `O` → `0`); the device id and credential are kept in NVS. A refusal is turned into the same sentence
  the Node prints. A 4xx answer (wrong, used, locked or expired code) is definitive: the board stops trying until it
  is rebooted with a new code, so it never burns extra tries toward Bot's 5-try lock. Transport errors and 5xx answers
  retry with backoff (30 s doubling to 5 min). Plain `http://`/`ws://` is refused unless the host is `localhost` or
  `127.0.0.1` (or the library is built with `-DOPENVIBE_ALLOW_INSECURE_HTTP` for a test server).
- **The control link** — one outbound WebSocket to the control URL, the credential in the upgrade's `Authorization`
  header only. The device sends nothing until the server's `hello`, and the first `status`/`estop_state` wait for the
  connection's `config`.
- **Drive and actuator commands** — `drive` (differential and mecanum fields), `actuator` and `halt`. Values are
  clamped in the core, before any driver sees them, to the stricter of the server's `config.limits` and the local caps.
  `ptz`, `say` and `display` are valid protocol kinds this device has no driver for, so they are `nack unsupported`.
  A command id or kind over 64 bytes is rejected, and the dedup cache answers a repeated id for the spec's 10 minutes
  (across reconnects) while storing only fixed-size messages.
- **Telemetry** — `battery`, `voltage`, `rssi` and `sensors`, at most every 500 ms (Bot's cap).
- **Heartbeat and deadman** — a `heartbeat` every `config.heartbeat_ms` (capped at 5000 ms so a server cannot stretch
  the deadman for hours) once `hello` has arrived, both `heartbeat_ack` shapes (the newer `echo`/`t` and the older
  `seq`) measured for RTT; two missed heartbeats stop the motors and drop the link; a motion command stops when its
  deadline lapses. Every timer runs on `esp_timer_get_time()`, a 64-bit monotonic clock, so the `millis()` wrap at
  49.7 days and an NTP step cannot move the deadman, a deadline or the cadence; the wall clock is read only for the
  envelope `ts` and the protocol's Unix timestamps.
- **E-stop** — the server's `estop` latches or clears the remote stop (`config.estop_latched` re-applies a latch set
  while offline); a frame with a wrong-typed `latched`/`estop_latched` is ignored and keeps the latch, as the Go Node's
  decoder does. The local kill switch (`setLocalStop(true)` / `resume()`) is persisted in NVS and restored in
  `begin()` before WiFi comes up; only `resume()` clears it. Every stop path calls `on_stop`.
- **Credential rotation** — `importCredential()` takes the owner's `POST /api/v1/devices/:id/rotate` response, refuses
  it when it is for another device, rewrites the stored credential without pairing again, and the next reconnect uses
  it. A failed NVS write is logged rather than passed over silently.
- **Close codes** — 4002 and 4003 (and an upgrade refused with 401/403) stop the motors and wait at least 10 s, with
  jitter that only adds, logging "pair again, or import the owner's rotation"; other drops use the documented
  exponential backoff with full jitter, and a link that has been up for 30 s resets it (as the Go Node does).
  arduinoWebSockets 2.x does not surface the close code or the upgrade's status, so the transport treats a socket that
  ends before `hello` as the refused case and logs that the cause is uncertain; see *Limitations* below.
- **Frame and memory bounds** — the protocol allows frames up to 1 MiB; this device bounds them at 16 KiB and a JSON
  depth of 10. A frame over either bound, or one the JSON parser cannot allocate, stops the motors and drops the link
  (it could have been an e-stop; the reconnect re-reads `config.estop_latched`). A frame that loses fields to a failed
  allocation is never sent half-formed.

## What it does not

- **Jobs / worker** — nothing runs jobs. A job frame is refused exactly as the protocol says for a node without the
  worker: `nack unsupported` with `class not available` (or `invalid inputs` / `missing or invalid args, ttl_ms or
  limits` when those are the first problem).
- **Video / WHIP** — no camera publishing.
- **OpenVibe.Network pairing and node tokens** — only Bot credential pairing.
- **The local CLI** — there is no `openvibe-node` binary on a board; the local kill switch is the API, not a command.

## Layout

- `src/ov_core.{h,cpp}` — the protocol core in portable C++17, no Arduino headers, only ArduinoJson v7 (header-only).
  Everything stateful lives here; time is injected as two clocks (monotonic and Unix) and every side effect is a
  callback, so it runs on desktop.
- `src/OpenVibeNode.{h,cpp}` — the Arduino transport: WiFi, TLS with the ESP32 CA bundle (never `setInsecure()`), the
  arduinoWebSockets client, `HTTPClient` for pairing, and `Preferences` (NVS) for the device id, credential and local
  kill switch. TLS verifies against the core's embedded bundle (`_binary_x509_crt_bundle_start`), the same one the
  core's own HTTPS client uses.
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
4. The library never starts SNTP. Add `configTime(...)` in the sketch (NTP) so the protocol's Unix timestamps are
   right; the safety timers do not depend on it.

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
scenario and exits non-zero on a failure. Its scenarios cover the fixtures, the deadman, the e-stop latch (including
wrong-typed frames), clamping, nack codes, job refusals, close codes, heartbeat cadence, a wall clock that steps
backwards, 64-bit monotonic time across the 32-bit `millis()` wrap, oversize/too-deep/allocation-failing frames,
over-long ids and kinds, the dedup cache's 10-minute limit, the heartbeat cap, guarded commands while the link is
lost, the backoff reset after a stable link, and the 10 s floor after a refused reconnect.

## Limitations

- Not run on hardware yet (above).
- The library never starts SNTP (step 4). Before the sketch syncs time, `ts`, `estop_state.at` and `heartbeat.t` carry
  the monotonic clock rather than Unix milliseconds. The core's timers never read that clock, so safety is unaffected.
- NVS is not encrypted by default: the credential and the local kill switch live in flash readable to anyone with
  physical access unless the sketch enables NVS encryption. The remote e-stop is not stored at all; each connection's
  `config.estop_latched` re-applies it (including after a reboot).
- arduinoWebSockets 2.x delivers `WStype_DISCONNECTED` without the WebSocket close code or the upgrade's HTTP status.
  The transport therefore treats "disconnected before `hello`" as the 4002/401/403 case (10 s floor, logged as an
  uncertain cause) and "disconnected after `hello`" as a normal drop; a 4003 sent while connected cannot be told
  apart, so it uses the normal backoff and can retry once below the spec's 10 s floor before the pre-hello rule
  applies. The core itself implements the close-code policy and the desktop harness tests it.
