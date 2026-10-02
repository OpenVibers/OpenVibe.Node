# Node ↔ OpenVibe.Bot protocol (bot.device-message@1, device side)

OpenVibe.Bot's own `docs/protocol.md` (section 1, the `/device` socket) and its `POST /api/v1/pair` handler are the
source of truth. Every field name below lives in one Go package, `internal/protocol/`, and the exact frames from Bot's
doc are pinned verbatim in [`internal/protocol/testdata/bot`](../internal/protocol/testdata/bot) (its README names the
Bot commit). `go test ./internal/protocol ./internal/link` decodes every one of them and drives the fake server
(`internal/fakebot`) through pair → connect → command → ack → reconnect with those exact bytes.

## 1. Pairing

The owner adds a robot on openvibe.bot and gets an 8-character code (shown `XXXX-XXXX`, 10 minutes, single use, 5 wrong
tries end it) and an installer command with the robot's id: `… | sh -s -- --robot rob_… --code XXXX-XXXX`. The Node
normalises the code (upper case; dash and spaces removed; `I`/`L` → `1`, `O` → `0`, as Bot does) and redeems it over
HTTPS, which Bot offers for agents that pair without a socket:

```http
POST https://openvibe.bot/api/v1/pair
Content-Type: application/json

{"robot": "rob_01J8Z4M2Q0R7T9YV3K6N8P1W2X", "code": "7Q2M4XZP", "agent_version": "v0.1.0", "device_kind": "onboard",
 "drivers": ["adeept_adr036"],
 "capabilities": {"adeept_adr036": {"drive": {"type": "differential"}, "ptz": {...}, ...}},
 "name": "rover"}
```

The body has exactly the fields of Bot's `pair` frame without the envelope. `robot` (from `--robot`) attributes a wrong
try to that robot's code; `name` (from `--name`, default the hostname, at most 80 characters) is what the owner sees.
`device_kind` is `onboard` (the Node runs on the robot) or `bridge` (it drives the robot over the robot's own link, as
with Cozmo). `capabilities` is keyed by plugin name and holds each plugin's `describe` capabilities
([plugins.md](plugins.md)).

Answer `201`:

```json
{"device_id": "dev_…", "credential": "…", "publish_key": "…", "robot_id": "rob_…",
 "profile": {"id": "adeept.adr036", "limits": {"max_command_ms": 300, "heartbeat_ms": 1000}, …}}
```

This is Bot's `paired` frame with `robot_ids[0]` as `robot_id` (and no `profile_id`; it is `profile.id`). Errors are
RFC 9457 problems; the Node turns their `code` into a sentence: `bot.pairing_code_invalid` (wrong code),
`bot.pairing_code_locked` (5 wrong tries), `bot.pairing_code_used`, `bot.pairing_code_expired`, `bot.no_pairing_code`,
`bot.invalid_pairing_code` (malformed); `429` → "wait a minute". Plain `http://` is refused except for `localhost`
(tests).

The credential and publish key are stored in `credential.json` (mode 0600, with the device and robot ids) and never
appear in a URL, a log line, an error, `openvibe-node status` or telemetry (the Go type `credentials.Secret` prints as
`[redacted]` in every format).

## 2. The control link

One WebSocket per device, opened by the device:

```http
GET wss://openvibe.bot/device
Authorization: Bearer <credential>
User-Agent: openvibe-node/<version>
```

Bot upgrades first and authenticates after, so the Node sends nothing and counts the link as up only once `hello`
arrives (Bot answers any frame on a not-yet-authenticated socket with `error` `bot.not_paired`). Close codes:

| code / answer                | meaning                                              | the Node                                               |
|------------------------------|------------------------------------------------------|--------------------------------------------------------|
| `4000`                       | a second connection with this credential replaced it | reconnects with the normal backoff                      |
| `4002`, or HTTP `401`/`403`  | the credential is wrong, rotated or revoked          | retries no sooner than 10 s (jitter only adds), logs "pair again, or install the current credential" |
| `4003`                       | the owner revoked the device (or rotated its credential) while it was connected | the same as `4002`             |

The link never gives up on its own: the owner may pair the device again. Bot's `error` frames (`bot.not_paired`,
`bot.not_ready`, `bot.forbidden`, …) are logged and never end the connection.

### Frames

Every frame is one JSON object in one text message with the envelope fields at the top level:

| field  | type   | meaning                                                                |
|--------|--------|------------------------------------------------------------------------|
| `v`    | int    | envelope version, `1`. Other versions are rejected.                    |
| `seq`  | uint   | per-direction counter, starting at 1 on each connection                |
| `ts`   | int    | sender's clock, Unix milliseconds                                      |
| `type` | string | message type                                                           |

Unknown `type`s are ignored (a newer server may send more). Frames are at most 1 MiB. `heartbeat` and `heartbeat_ack`
carry their own `seq` (the echoed heartbeat number) in the same object as the envelope's; the Node sets a heartbeat's
`seq` equal to its envelope `seq`, so either reading gives the same number.

### Server → device

| type            | fields                                                                                           |
|-----------------|--------------------------------------------------------------------------------------------------|
| `hello`         | `session_id`, `device_id`, `robot_ids` [], `server_time` (RFC 3339) — on every authenticated connection |
| `config`        | `heartbeat_ms`, `limits` {`max_speed` 0..1, `max_turn` 0..1, `max_command_ms`, `heartbeat_ms`}, `allowed_commands` [kinds], `estop_latched` — right after `hello` |
| `command`       | `id` (server-minted: the idempotency and ack key), `ref` (the operator's own id, logs only), `kind`, `value`, `deadline_ms` (absolute, motion kinds only), `operator` {`subject`, `role`}, `robot_id`; Node only: `target` (plugin name) |
| `estop`         | `latched` (true: the e-stop latched; false: the owner cleared it), `by`, `at`                    |
| `heartbeat_ack` | `seq` (the heartbeat's), `server_time`; the Node measures RTT from its own send time             |
| `error`         | `code`, `detail` — a frame the server refused; logged, never fatal                               |

`config.estop_latched` is the robot's e-stop as the server holds it: an owner e-stop set while the device was offline
latches the Node on reconnect, and a clear made while offline releases it. Until a connection's `config` has been
applied the Node runs nothing but `halt` (`nack not_ready`), so that latch is in force before any motion.
`config.allowed_commands` absent allows every kind; present (Bot always sends it, `halt` included) it is the list.
`estop` `latched:false` is the owner's clear: it clears the remote latch only, never the local kill switch.

### Device → server

| type          | fields                                                                                              |
|---------------|-----------------------------------------------------------------------------------------------------|
| `status`      | `firmware` (`openvibe-node-<version>`), `capabilities` {per plugin}, `faults` [{`code`, `driver`, `message`}], `estop_latched`; Node extras: `agent_version`, `device_kind`, `os`, `arch`, `drivers` [{`name`, `driver`, `version`, `state`, `capabilities`}], `local_stop`, `video` |
| `telemetry`   | `battery` (0..1), `voltage` (V), `rssi`, `sensors` {…}; Node extra: `events` [{`name`, `driver`, `ts`, `fields`}] |
| `ack`         | `id`; Node extra: `latency_ms` (receipt → plugin ack)                                               |
| `nack`        | `id`, `fault_code`; Node extra: `message`                                                           |
| `heartbeat`   | `seq`, `rtt_ms` (the last measured round trip, once there is one)                                   |
| `estop_state` | `latched` (the device is stopped: remote e-stop or local kill switch), `by` (`device`), `at`; Node extras: `local_stop` (local kill switch), `reason` |

The first `status` and `estop_state` go out once the connection's `config` has been applied, never right after the
upgrade or `hello`; then `status` whenever a plugin changes state or faults and `estop_state` after every `config`,
`estop` and latch change. Bot reads `latched:true` as the device stopping the robot and latches it (only the owner
clears that); `latched:false` is a report only. Plugin events (`cliff`, `picked_up`,
`low_battery`, `heartbeat_lost`, …) travel in `telemetry.events` and are sent within 100 ms; sensor telemetry is sent
at most every 500 ms (≤ 2 Hz, Bot's cap). There is no separate `event` message type.

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

`deadline_ms` is an **absolute** instant in Unix milliseconds on the server's clock, present on `drive`, `actuator` and
`ptz`. A device's clock may be minutes off, so the Node never compares it with its own clock: it estimates the
server's clock from the `ts` of every frame on the connection (the largest `ts` − receive time: the skew less the
fastest transit, kept for the whole connection so a run of delayed frames cannot reopen an expired deadline) and
compares `deadline_ms` with that at receipt. A command that arrives at or after its deadline →
`nack expired` and never reaches a plugin; otherwise the plugin gets only the time that is left. No `deadline_ms` →
300 ms. Always capped at `max_command_ms` (default 1000). A held control is re-sent by the panel every 150 ms, each
with a new `id`. Plugins still get a relative `deadline_ms` and keep their own stop timer ([plugins.md](plugins.md)).

What the Node does with a command, in order:

1. A repeated `id` is answered with the first result and never executed again (the cache lives for 10 minutes and
   across reconnects).
2. Unknown kind → `nack unsupported`.
3. `halt` → every plugin stops, `ack`.
4. No `config` yet on this connection → `nack not_ready`. Kind not in `config.allowed_commands` → `nack not_allowed`.
5. `drive`, `ptz`, `actuator` while the local kill switch is latched → `nack local_stop`; while the remote e-stop is
   latched → `nack estopped`; past its `deadline_ms` → `nack expired`.
6. The value is clamped in the core, before any plugin sees it: `throttle`, `x`, `y` to ±`max_speed`; `steer`,
   `rotation` to ±`max_turn`; `pan`, `tilt`, `zoom` and a numeric actuator `value` to ±1. The limits are the stricter of
   the server's `config.limits` and the local config's `limits`. Non-numbers → `nack bad_value`.
7. It goes to the plugin named by `target`, or the first plugin whose `describe` declares the kind (for actuators, the
   first listing the name). None → `nack unsupported`.
8. The plugin's `ack`/`nack` is relayed. No answer within max(deadline, 1 s) → the plugin is told to stop and the
   server gets `nack plugin_timeout`.

Commands are never queued while offline and never replayed after a reconnect.

### Fault codes

`bad_frame`, `bad_value`, `unsupported`, `not_allowed`, `estopped`, `local_stop`, `expired`, `no_heartbeat`,
`not_ready`, `plugin_down`, `plugin_timeout`, `hardware`, `firmware_unsupported`, `not_connected`, `cliff`,
`picked_up`, `shutting_down`. Plugins may add their own; they are passed through.

### Heartbeat, deadman, reconnect

- The device sends `heartbeat` every `config.heartbeat_ms` (default 1000). The server answers `heartbeat_ack`.
- If nothing at all arrives from the server for 2 × `heartbeat_ms` (two missed heartbeats), the Node closes the link.
  Every close stops every actuator at once. (Bot marks the device offline after `heartbeat_ms × 2 + 3000 ms`.)
- Reconnect: exponential backoff from 0.5 s to 30 s with full jitter; see the close-code table for `4000`, `4002` and
  `4003`.

## 3. Safety paths (ADR-043 decision 6)

| event                                        | what stops it                                      | latched?                                |
|----------------------------------------------|----------------------------------------------------|-----------------------------------------|
| motion command's deadline passes             | the plugin (its own timer)                         | no                                      |
| 2 missed heartbeats / link closed            | the core → `stop` to every plugin                  | no                                      |
| server `estop`                               | the core → `estop` to every plugin                 | yes, persisted; `estop` `latched:false` or `openvibe-node resume` |
| `openvibe-node stop` (local kill switch)     | the core → `estop` to every plugin                 | yes, persisted; only `openvibe-node resume` |
| core shutdown                                | `stop`, then stdin closed                          | no                                      |
| core killed / crashed                        | the plugin: stdin EOF, or 1 s without a heartbeat  | no                                      |
| plugin crash                                 | restarted with backoff; starts stopped; `estop` re-sent if latched | —                        |
| Cozmo cliff or pick-up                       | the Cozmo plugin, in the event callback            | until the condition clears              |

The latch file is `latch.json` in the state directory. A corrupt latch file is read as "local stop latched".

## 4. Video (WHIP)

Bot hands out only the publish key (in the pairing answer); it does not own media and names no WHIP endpoint (the
camera publishes to OpenRe). So the endpoint is local config, `video.whip_url` in `config.json`; without it video stays
off and the log says so. When it is set, the Node publishes one H.264 track (Constrained Baseline,
`profile-level-id=42e01f`, packetization-mode 1) to it with `Authorization: Bearer <publish_key>`: `POST` with `Content-Type: application/sdp` → `201` with the
answer and a `Location`; `DELETE <Location>` on shutdown. PLI/FIR from the far end request a keyframe. The session is
re-established with backoff; video runs on its own goroutines and never waits on control or vice versa.
