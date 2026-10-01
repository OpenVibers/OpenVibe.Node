# Node ↔ OpenVibe.Bot protocol (bot.device-message@1, device side)

This is the wire format the Node speaks, matching OpenVibe.Bot's `docs/protocol.md`, `server/realtime.js` and
`server/api/v1.js` (Bot `d398445`), which are the source of truth. Every field name below lives in one Go package,
`internal/protocol/`; `internal/fakebot` (the test server) builds Bot's frames field by field from those files, so a
decoder that drifts from Bot fails the integration tests. Points still open on the Bot side are marked **(Bot)**.

## 1. Pairing

The owner adds a robot on openvibe.bot and gets an 8-character code (shown `XXXX-XXXX`, 10 minutes, single use, 5 wrong
tries end it). The Node normalises it (upper case; dash and spaces removed; `I`/`L` → `1`, `O` → `0`) and redeems it:

```http
POST https://openvibe.bot/api/v1/pair
Content-Type: application/json

{"robot": "rob_…", "code": "ABCD1234", "agent_version": "v0.1.0", "device_kind": "onboard",
 "drivers": ["adeept_adr036"],
 "capabilities": {"adeept_adr036": {"drive": {"type": "differential"}, "ptz": {...}, ...}},
 "name": "rover", "os": "linux", "arch": "arm64"}
```

`robot` is optional (`openvibe-node pair <CODE> --robot rob_…`): with it, a wrong code is charged to that robot's live
code, so five wrong tries end it. `name` is what the owner sees (`--name`, default: the hostname). `device_kind` is
`onboard` (the Node runs on the robot) or `bridge` (it drives the robot over the robot's own link, as with Cozmo).
`capabilities` is keyed by plugin name and holds each plugin's `describe` capabilities ([plugins.md](plugins.md)).

Answer `201`:

```json
{"device_id": "dev_…", "credential": "…", "publish_key": "…", "robot_id": "rob_…", "profile": {…bot.robot-profile@1…}}
```

The Node also reads optional `whip_url`, `device_url` and `ice_servers`; without `device_url` it dials
`wss://<origin>/device`. **(Bot)** Bot does not send `whip_url` yet, and video starts only when it is present.

Refusals are RFC 9457 `application/problem+json` with `code` and `detail`; the Node turns Bot's codes into what to do
next:

| status | `code`                     | the Node says                                                    |
|--------|----------------------------|------------------------------------------------------------------|
| 422    | `bot.invalid_pairing_code` | a pairing code is 8 letters and digits                           |
| 404    | `bot.no_pairing_code`      | this robot has no pairing code; ask the owner for a new one      |
| 403    | `bot.pairing_code_invalid` | that is not the pairing code (5 wrong tries end a code)          |
| 403    | `bot.pairing_code_locked`  | too many wrong tries: the code is dead                           |
| 403    | `bot.pairing_code_used`    | the code has already been used                                   |
| 403    | `bot.pairing_code_expired` | the code has expired (codes last 10 minutes)                     |
| 429    | `rate_limited`             | too many attempts from this address; wait a minute               |

Anything else is reported as `pairing refused: HTTP <status> <code>: <detail>`. Plain `http://` is refused except for
`localhost` (tests).

The credential and publish key are stored in `credential.json` (mode 0600) and never appear in a URL, a log line, an
error, `openvibe-node status` or telemetry (the Go type `credentials.Secret` prints as `[redacted]` in every format).

When the owner rotates the credential (`POST /api/v1/devices/<id>/rotate`, answered once with `credential` and
`publish_key`; the old credential works for 60 s more), install it with
`openvibe-node credential set`, which reads either the credential alone or the whole JSON answer from stdin, checks the
device id, writes `credential.json` (mode 0600), never prints a secret and restarts the service if it runs:

```sh
pbpaste | sudo openvibe-node credential set
```

## 2. The control link

One WebSocket per device, opened by the device:

```http
GET wss://openvibe.bot/device
Authorization: Bearer <credential>
User-Agent: openvibe-node/<version>
```

Bot upgrades first and checks the credential afterwards, then sends `hello` and `config`. The Node sends nothing until
the first server frame (a frame before authentication is answered with `error bot.not_paired`), and its first `status`
and `estop_state` only once both `hello` and `config` have arrived.

Close codes from the server:

| code | meaning                                                         | the Node                                                                 |
|------|-----------------------------------------------------------------|--------------------------------------------------------------------------|
| 4000 | a newer connection of this device replaced this one             | reconnects with the ordinary backoff                                     |
| 4002 | unknown credential (or a rotated one past its 60 s grace)       | logs "re-pair or update the credential"; waits at least 10 s + jitter    |
| 4003 | the owner revoked the device                                    | same as 4002                                                             |

HTTP `401`/`403` on the upgrade are treated like 4002.

### Frames

Every frame is one JSON object in one text message with the envelope fields at the top level:

| field  | type   | meaning                                                                |
|--------|--------|------------------------------------------------------------------------|
| `v`    | int    | envelope version, `1`. Other versions are rejected.                    |
| `seq`  | uint   | per-direction counter, starting at 1 on each connection                |
| `ts`   | int    | sender's clock, Unix milliseconds                                      |
| `type` | string | message type                                                           |

Unknown `type`s are ignored (a newer server may send more). Frames are at most 1 MiB.

### Server → device

| type            | fields                                                                                           |
|-----------------|--------------------------------------------------------------------------------------------------|
| `hello`         | `session_id`, `device_id`, `robot_ids` [rob_…], `server_time` (ISO-8601, e.g. `2026-09-29T19:20:00.000Z`) |
| `config`        | `heartbeat_ms`, `limits` {`max_speed` 0..1, `max_turn` 0..1, `max_command_ms`}, `allowed_commands` [kinds], `estop_latched` |
| `command`       | `id` (server-minted `cmd_…`, the idempotency key), `ref`, `kind`, `value`, `deadline_ms` (absolute, epoch ms; `null` for non-motion kinds), `operator` {`subject`, `role`}, `robot_id`, `target` (optional plugin name, Node extension) |
| `estop`         | `latched`, `by`, `at` — `latched:true` latches, `latched:false` is the owner's clear              |
| `heartbeat_ack` | `seq` (the heartbeat's seq, in the envelope field), `server_time`; `echo` is read instead of `seq` when present **(Bot)** |
| `error`         | `code`, `detail` — a frame the server refused; the Node logs it and carries on                   |

`config.estop_latched` is the robot's e-stop on the server, applied on every connection before any command: `true`
latches (persisted), `false` clears an e-stop the owner lifted while the device was away. A command that arrives
before `config` is refused with `nack not_ready`. `allowed_commands` absent means no restriction; present, only those
kinds (and `halt`) run.

### Device → server

| type          | fields                                                                                              |
|---------------|-----------------------------------------------------------------------------------------------------|
| `status`      | `firmware`, `capabilities` {…}, `faults` [{`code`, `driver`, `message`}], `estop_latched` (anything latched on the device); extra: `local_stop`, `agent_version`, `device_kind`, `os`, `arch`, `drivers` [{`name`, `driver`, `version`, `state`, `capabilities`}], `video` |
| `telemetry`   | `battery` (charge 0..1, from the plugin's percent), `voltage` (V), `rssi`, `sensors` {…}, `events` [{`name`, `driver`, `ts`, `fields`}] |
| `ack`         | `id`, `latency_ms` (receipt → plugin ack)                                                           |
| `nack`        | `id`, `fault_code`, `message`                                                                       |
| `heartbeat`   | `rtt_ms` (round trip of the previous heartbeat, once measured)                                      |
| `estop_state` | `latched`, `by` (`device`), `at`                                                                    |

Bot reads the fields listed first and stores the frames as they are; the extra fields are for people reading them.
`firmware` is `openvibe-node-<version>`; `capabilities` merges the plugins' `describe` capabilities (the first plugin
with a capability serves it).

Bot sets the robot's e-stop to whatever `estop_state.latched` says, so the Node never echoes the server's e-stop back:
`estop_state` reports the local kill switch only. `latched:true` goes out when `openvibe-node stop` latches;
`latched:false` only when nothing is latched on the device (after `openvibe-node resume`, or as the first report on a
clear device, which Bot ignores). **(Bot)** Bot should not let a device's `latched:false` lift an owner's e-stop; the
Node never sends it while it holds the server's latch.

`status` is sent once `hello` and `config` have arrived and whenever a plugin changes state, faults or the latch
changes. Plugin events (`cliff`, `picked_up`, `low_battery`, `heartbeat_lost`, …) travel in `telemetry.events` and are
sent within 100 ms; sensor telemetry is sent at most every 500 ms (≤ 2 Hz, Bot drops faster frames). **(Bot)** An event
inside Bot's 500 ms window is dropped by Bot today.

### Commands

| kind       | value                                                                                                   | motion |
|------------|---------------------------------------------------------------------------------------------------------|--------|
| `drive`    | differential `{"throttle": -1..1, "steer": -1..1}`; mecanum `{"x": -1..1, "y": -1..1, "rotation": -1..1}` | yes    |
| `ptz`      | `{"pan": -1..1, "tilt": -1..1, "zoom": -1..1}` absolute positions (0 = centre)                         | guarded |
| `actuator` | `{"name": "lift", "value": …}` — number, object or bool per actuator ([plugins.md](plugins.md))        | guarded |
| `say`      | `{"text": "…"}` (1–1000 bytes; plugins may cap lower)                                                    | no     |
| `display`  | `{"text": "…"}`, `{"face": "happy"}` or `{"image_png_b64": "…"}`                                        | no     |
| `halt`     | `{}` — stop every actuator now (not latched); always accepted                                           | —      |

Sign conventions: `throttle`/`x` positive = forward; `steer` positive = turn right; `y` positive = strafe left;
`rotation` positive = counter-clockwise (left); `pan` positive = right, `tilt` positive = up (plugins can invert a
servo in their config to make the hardware match).

`deadline_ms` is an **absolute instant** on the server's clock (Unix ms). The Node estimates the server's clock from
`hello.server_time` (so a device whose clock is off, such as a Raspberry Pi without a real-time clock before NTP, still
judges deadlines right). A command that arrives at or after its deadline is refused with `nack expired` and never
reaches a plugin; otherwise the plugin gets the time left, capped at `max_command_ms` (default 1000), and stops on its
own timer when it runs out. Without a deadline the plugin gets 300 ms. A held control is re-sent every 150 ms, each
with a new `id`.

What the Node does with a command, in order:

1. A repeated `id` is answered with the first result and never executed again (the cache lives for 10 minutes and
   across reconnects).
2. Unknown kind → `nack unsupported`.
3. `halt` → every plugin stops, `ack`.
4. No `config` yet on this connection → `nack not_ready`. Kind not in `config.allowed_commands` → `nack not_allowed`.
5. The deadline has passed → `nack expired`.
6. `drive`, `ptz`, `actuator` while the local kill switch is latched → `nack local_stop`; while the remote e-stop is
   latched → `nack estopped`.
7. The value is clamped in the core, before any plugin sees it: `throttle`, `x`, `y` to ±`max_speed`; `steer`,
   `rotation` to ±`max_turn`; `pan`, `tilt`, `zoom` and a numeric actuator `value` to ±1. The limits are the stricter of
   the server's `config.limits` and the local config's `limits`. Non-numbers → `nack bad_value`.
8. It goes to the plugin named by `target`, or the first plugin whose `describe` declares the kind (for actuators, the
   first listing the name). None → `nack unsupported`.
9. The plugin's `ack`/`nack` is relayed. No answer within max(time left, 1 s) → the plugin is told to stop and the
   server gets `nack plugin_timeout`.

Commands are never queued while offline and never replayed after a reconnect.

### Fault codes

`bad_frame`, `bad_value`, `unsupported`, `not_allowed`, `expired`, `estopped`, `local_stop`, `no_heartbeat`,
`not_ready`, `plugin_down`, `plugin_timeout`, `hardware`, `firmware_unsupported`, `not_connected`, `cliff`,
`picked_up`, `shutting_down`. Plugins may add their own; they are passed through.

### Heartbeat, deadman, reconnect

- Once the server has spoken, the device sends `heartbeat` every `heartbeat_ms` (default 1000). The server answers
  `heartbeat_ack` with the heartbeat's `seq`; the Node measures the round trip and sends it as `rtt_ms` on the next
  heartbeat (Bot shows it as the robot's latency).
- If nothing at all arrives from the server for two heartbeat intervals (2 s by default, before Bot's 5 s offline
  mark), the Node closes the link. Until `hello` arrives (the server is still checking the credential) it waits up to
  10 s instead. Every close stops every actuator at once.
- Reconnect: exponential backoff from 0.5 s to 30 s with full jitter; after 4002/4003 (or HTTP 401/403) at least 10 s,
  the jitter on top.

## 3. Safety paths (ADR-043 decision 6)

| event                                        | what stops it                                      | latched?                                |
|----------------------------------------------|----------------------------------------------------|-----------------------------------------|
| motion command arrives after its deadline    | the core: `nack expired`, never sent to a plugin   | no                                      |
| motion command's deadline passes             | the plugin (its own timer)                         | no                                      |
| 2 missed heartbeats / link closed            | the core → `stop` to every plugin                  | no                                      |
| server `estop {latched:true}` or `config.estop_latched:true` | the core → `estop` to every plugin | yes, persisted; `estop {latched:false}` / `config.estop_latched:false` from the server, or `openvibe-node resume` |
| `openvibe-node stop` (local kill switch)     | the core → `estop` to every plugin                 | yes, persisted; only `openvibe-node resume` |
| core shutdown                                | `stop`, then stdin closed                          | no                                      |
| core killed / crashed                        | the plugin: stdin EOF, or 1 s without a heartbeat  | no                                      |
| plugin crash                                 | restarted with backoff; starts stopped; `estop` re-sent if latched | —                        |
| Cozmo cliff or pick-up                       | the Cozmo plugin, in the event callback            | until the condition clears              |

The latch file is `latch.json` in the state directory. A corrupt latch file is read as "local stop latched".

## 4. Video (WHIP)

The Node publishes one H.264 track (Constrained Baseline, `profile-level-id=42e01f`, packetization-mode 1) to
`whip_url` with `Authorization: Bearer <publish_key>`: `POST` with `Content-Type: application/sdp` → `201` with the
answer and a `Location`; `DELETE <Location>` on shutdown. PLI/FIR from the far end request a keyframe. The session is
re-established with backoff; video runs on its own goroutines and never waits on control or vice versa.
