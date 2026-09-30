# Node ↔ OpenVibe.Bot protocol (bot.device-message@1, device side)

This is the wire format the Node speaks today, from ADR-043 decisions 1, 2, 4, 5 and 6. OpenVibe.Bot's own
`docs/protocol.md` is the source of truth; every field name below lives in one Go package, `internal/protocol/`, so
aligning with it is a change to that package only. The ADR-043 text mirrored in this repo is updated to match (pairing
field `code`, relative `deadline_ms`, plugin events in `telemetry.events`). Points still to be confirmed with the
service are marked **(to align)**.

## 1. Pairing

The owner adds a robot on openvibe.bot and gets an 8-character code (Crockford base32, shown `XXXX-XXXX`, 10 minutes,
single use, 5 wrong tries end it). The Node normalises it (upper case; dash and spaces removed; `I`/`L` → `1`, `O` → `0`)
and redeems it:

```http
POST https://openvibe.bot/api/v1/pair
Content-Type: application/json

{"code": "ABCD1234", "agent_version": "v0.1.0", "device_kind": "onboard",
 "drivers": ["adeept_adr036"],
 "capabilities": {"adeept_adr036": {"drive": {"type": "differential"}, "ptz": {...}, ...}},
 "os": "linux", "arch": "arm64", "hostname": "rover"}
```

`device_kind` is `onboard` (the Node runs on the robot) or `bridge` (it drives the robot over the robot's own link, as
with Cozmo). `capabilities` is keyed by plugin name and holds each plugin's `describe` capabilities
([plugins.md](plugins.md)). The code field is `code`, as in Bot's `docs/protocol.md`.

Answer `2xx`:

```json
{"device_id": "dev_…", "credential": "<32 random bytes, encoded>", "publish_key": "…",
 "profile": {…bot.robot-profile@1…},
 "whip_url": "https://…/whip/…", "device_url": "wss://openvibe.bot/device",
 "ice_servers": [{"urls": ["turn:…"], "username": "…", "credential": "…"}]}
```

`whip_url`, `device_url` and `ice_servers` are optional; without `device_url` the Node dials `wss://<origin>/device`
**(to align: where the WHIP URL comes from)**. Errors: `404`/`410` or `{"error":"invalid_code"|"expired"}` → "code wrong,
used or expired"; `429` → "too many tries". Plain `http://` is refused except for `localhost` (tests).

The credential and publish key are stored in `credential.json` (mode 0600) and never appear in a URL, a log line, an
error, `openvibe-node status` or telemetry (the Go type `credentials.Secret` prints as `[redacted]` in every format).

## 2. The control link

One WebSocket per device, opened by the device:

```http
GET wss://openvibe.bot/device
Authorization: Bearer <credential>
User-Agent: openvibe-node/<version>
```

`401`/`403` means the credential was revoked or rotated away: the Node logs "pair again" and keeps retrying slowly.

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
| `hello`         | `device_id`, `robot_id`, `session`, `server_time`                                                |
| `config`        | `limits` {`max_speed` 0..1, `max_turn` 0..1, `max_command_ms`}, `heartbeat_ms`, `deadman_ms`, `telemetry_ms` (≥ 500), `allowed` [kinds] |
| `command`       | `id` (idempotency key), `kind`, `value`, `deadline_ms`, `operator`, `role`, `target` (optional plugin name) |
| `estop`         | `reason`, `by` — latched                                                                         |
| `estop_clear`   | `by` — the owner clears the remote e-stop                                                        |
| `heartbeat_ack` | `t` (echo of the heartbeat's `t`; the Node measures RTT from it)                                 |

### Device → server

| type          | fields                                                                                              |
|---------------|-----------------------------------------------------------------------------------------------------|
| `status`      | `agent_version`, `device_kind`, `os`, `arch`, `drivers` [{`name`, `driver`, `version`, `state`, `capabilities`}], `faults` [{`code`, `driver`, `message`}], `estop_latched`, `local_stop`, `video` |
| `telemetry`   | `battery` {`volts`, `percent`}, `rssi`, `sensors` {…}, `events` [{`name`, `driver`, `ts`, `fields`}] |
| `ack`         | `id`, `latency_ms` (receipt → plugin ack)                                                           |
| `nack`        | `id`, `fault_code`, `message`                                                                       |
| `heartbeat`   | `t` (device clock, ms)                                                                              |
| `estop_state` | `latched` (remote e-stop), `local_stop` (local kill switch), `reason`                              |

`status` and `estop_state` are sent on every connect and whenever a plugin changes state, faults or the latch changes.
Plugin events (`cliff`, `picked_up`, `low_battery`, `heartbeat_lost`, …) travel in `telemetry.events` and are sent
within 100 ms; sensor telemetry is sent at most every `telemetry_ms` (default 500 ms, so ≤ 2 Hz). There is no
separate `event` message type.

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

`deadline_ms` is **relative to receipt** (milliseconds the command stays valid), not an absolute timestamp: a device's
clock may be minutes off, and a relative deadline cannot be stretched by clock skew. Default 300 ms; capped at
`max_command_ms` (default 1000). A held control is re-sent by the panel every 150 ms, each with a new `id`.

What the Node does with a command, in order:

1. A repeated `id` is answered with the first result and never executed again (the cache lives for 10 minutes and
   across reconnects).
2. Unknown kind → `nack unsupported`. Kind not in `config.allowed` → `nack not_allowed` (`halt` is always allowed).
3. `halt` → every plugin stops, `ack`.
4. `drive`, `ptz`, `actuator` while the local kill switch is latched → `nack local_stop`; while the remote e-stop is
   latched → `nack estopped`.
5. The value is clamped in the core, before any plugin sees it: `throttle`, `x`, `y` to ±`max_speed`; `steer`,
   `rotation` to ±`max_turn`; `pan`, `tilt`, `zoom` and a numeric actuator `value` to ±1. The limits are the stricter of
   the server's `config.limits` and the local config's `limits`. Non-numbers → `nack bad_value`.
6. It goes to the plugin named by `target`, or the first plugin whose `describe` declares the kind (for actuators, the
   first listing the name). None → `nack unsupported`.
7. The plugin's `ack`/`nack` is relayed. No answer within max(deadline, 1 s) → the plugin is told to stop and the
   server gets `nack plugin_timeout`.

Commands are never queued while offline and never replayed after a reconnect.

### Fault codes

`bad_frame`, `bad_value`, `unsupported`, `not_allowed`, `estopped`, `local_stop`, `no_heartbeat`,
`not_ready`, `plugin_down`, `plugin_timeout`, `hardware`, `firmware_unsupported`, `not_connected`, `cliff`,
`picked_up`, `shutting_down`. Plugins may add their own; they are passed through.

### Heartbeat, deadman, reconnect

- The device sends `heartbeat` every `heartbeat_ms` (default 1000). The server answers `heartbeat_ack`.
- If nothing at all arrives from the server for `deadman_ms` (default 2 × `heartbeat_ms`: two missed heartbeats), the
  Node closes the link. Every close stops every actuator at once.
- Reconnect: exponential backoff from 0.5 s to 30 s with full jitter; at least 10 s after a `401`/`403`.

## 3. Safety paths (ADR-043 decision 6)

| event                                        | what stops it                                      | latched?                                |
|----------------------------------------------|----------------------------------------------------|-----------------------------------------|
| motion command's deadline passes             | the plugin (its own timer)                         | no                                      |
| 2 missed heartbeats / link closed            | the core → `stop` to every plugin                  | no                                      |
| server `estop`                               | the core → `estop` to every plugin                 | yes, persisted; `estop_clear` or `openvibe-node resume` |
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
