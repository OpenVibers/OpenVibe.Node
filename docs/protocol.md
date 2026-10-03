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
| `4002`, or HTTP `401`/`403`  | the credential is wrong, rotated or revoked          | retries no sooner than 10 s (jitter only adds), logs "pair again, or import the owner's rotation" |
| `4003`                       | the owner revoked the device (or rotated its credential) while it was connected | the same as `4002`             |

The link never gives up on its own: the owner may pair the device again. Bot's `error` frames (`bot.not_paired`,
`bot.not_ready`, `bot.forbidden`, …) are logged and never end the connection.

### Rotating a credential

The owner can rotate a device's credential in OpenVibe.Bot (`POST /api/v1/devices/:id/rotate`). Its answer is JSON
with `device.id`, `credential` and `publish_key`, and the old credential keeps working for 60 s. Pipe that response
body into the Node to take the new pair without pairing again:

```sh
curl -s -X POST -H "Authorization: Bearer $TOKEN" https://openvibe.bot/api/v1/devices/dev_…/rotate \
  | openvibe-node credential import
```

`import` keeps every other stored field (server, WHIP URL, robot ids, profile, version), rewrites `credential.json`
atomically at mode 0600 and never prints either secret. It refuses unless a credential file exists ("pair first") and
unless the response's `device.id` is the device this Node is paired as. A running Node keeps using the old credential
until it restarts; on a refused credential it logs the two remedies: pair again, or import the owner's rotation.

### Frames

Every frame is one JSON object in one text message with the envelope fields at the top level:

| field  | type   | meaning                                                                |
|--------|--------|------------------------------------------------------------------------|
| `v`    | int    | envelope version, `1`. Other versions are rejected.                    |
| `seq`  | uint   | per-direction counter, starting at 1 on each connection                |
| `ts`   | int    | sender's clock, Unix milliseconds                                      |
| `type` | string | message type                                                           |

Unknown `type`s are ignored (a newer server may send more). Frames are at most 1 MiB. A `heartbeat` carries the
device's send time in `t` (Unix ms) and the last measured `rtt_ms`; the server echoes `t` back in `heartbeat_ack` as
`echo` (and `t`), so the Node measures the round trip against its own clock. A `heartbeat_ack`'s envelope `seq` is the
server's own counter, never the heartbeat's.

### Server → device

| type            | fields                                                                                           |
|-----------------|--------------------------------------------------------------------------------------------------|
| `hello`         | `session_id`, `device_id`, `robot_ids` [], `server_time` (RFC 3339) — on every authenticated connection |
| `config`        | `heartbeat_ms`, `limits` {`max_speed` 0..1, `max_turn` 0..1, `max_command_ms`, `heartbeat_ms`}, `allowed_commands` [kinds], `estop_latched` — right after `hello` |
| `command`       | `id` (server-minted: the idempotency and ack key), `ref` (the operator's own id, logs only), `kind`, `value`, `deadline_ms` (absolute, motion kinds only), `operator` {`subject`, `role`}, `robot_id`; Node only: `target` (plugin name) |
| `estop`         | `latched` (true: the e-stop latched; false: the owner cleared it), `by`, `at`                    |
| `heartbeat_ack` | `echo` (the heartbeat's `t`, or null), `t` (the same send time when the server has it), `server_time` (RFC 3339); the Node measures RTT from the echoed send time |
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
| `heartbeat`   | `t` (send time, Unix ms), `rtt_ms` (the last measured round trip, once there is one; 0 is a real measurement) |
| `estop_state` | `latched` (the device is stopped: remote e-stop or local kill switch), `by` (`device`), `at`; Node extras: `robot_id` (when `hello` named exactly one robot), `local_stop` (local kill switch), `reason` |

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

### Jobs

The job frames of OpenVibe.Contracts `platform.job-frame@1` (plan T14 Run) ride the same link and envelope. The Node
runs class `function` only, and only when the owner turned the worker on in the local config, the OS is Linux and a
boot-time probe could start a process in its own user, network and PID namespaces ([worker.md](worker.md)).
Otherwise it advertises no class and refuses every well-formed job with `class not available`, and nothing executes.

| direction        | type           | fields                                                                                    |
|------------------|----------------|-------------------------------------------------------------------------------------------|
| server → device  | `job`          | `job` {`id` (`job_<ULID>`: the idempotency and ack key), `class`, `artifact` {`name`, `version`} (required for `function`, one exact version), `args` {}, `ttl_ms`, `limits` {`wall_ms`, `cpu_ms`, `mem_bytes`}, `net` (`deny`, the default)} |
| server → device  | `job_cancel`   | `id`                                                                                      |
| server → device  | `job_exit_ack` | `id`                                                                                      |
| device → server  | `job_started`  | `id`, `started_ms`                                                                        |
| device → server  | `job_stdout`   | `id`, `chunk_seq` (the job's own counter from 1; the envelope owns `seq`), `chunk`        |
| device → server  | `job_usage`    | `id`, `started_ms`, `second`, `cpu_ms`                                                    |
| device → server  | `job_exit`     | `id`, `reason` (`exited`, `cancelled`, `ttl`, `limit`, `stopped`, `failed`), `code`, `result`, `usage` {`started_ms`, `wall_ms`, `cpu_ms`, `mem_peak_bytes`} |

A `job` is answered `ack` (accepted) or `nack` keyed by the job id. A nack's `message` names the first problem found,
checked in this order:

| `message`                                    | `fault_code`  | when                                                        |
|----------------------------------------------|---------------|-------------------------------------------------------------|
| `missing or malformed job id`                | `bad_value`   | `id` is not `job_` + 26 ULID characters (the nack echoes it) |
| `malformed job`                              | `bad_frame`   | the `job` object does not decode (a field of the wrong type) |
| `unknown class`                              | `unsupported` | `class` is not one of `function`, `code`, `browser`, `linux`, `desktop`, `gpu` |
| `function job needs an artifact with an exact version` | `bad_value` | a `function` job without `artifact`, or a name or version outside the contract's pattern (a range) |
| `net policy not supported`                   | `unsupported` | `net` is present and not `deny`                             |
| `missing or invalid args, ttl_ms or limits`  | `bad_value`   | `args` is not an object, or `ttl_ms` or a limit is missing or below 1 |
| `class not available`                        | `unsupported` | the class is valid but this Node does not run it (every class but `function`; every class while the worker is off) |
| `the e-stop or local stop is latched`        | `estopped` or `local_stop` | the stop latch is set: no job starts until it clears |
| `class not available`                        | `shutting_down` | the Node is stopping                                      |
| `unknown artifact`                           | `unsupported` | `artifact` is not a function the owner declared, at that exact version |
| `worker busy`                                | `not_ready`   | `worker.caps.max_jobs` jobs are already running              |

A resent `job` with an id already answered gets the same answer again, never a second acceptance and never a second
process: `ack` while it runs, its `job_exit` once it ended; the Node remembers the last 1024 well-formed ids.

An accepted job runs once the `ack` is out: `job_started` (`started_ms` anchors every usage second), its stdout as
`job_stdout` chunks (at most 16 KiB each as sent and `max_output_bytes` in all, valid UTF-8 with invalid bytes as
U+FFFD, `chunk_seq` from 1; best effort, not resent), one `job_usage` for
every wall-clock second once it has fully elapsed (`second` from 0, `cpu_ms` that second's CPU time), then
`job_exit`. Second `n` is sent once `started_ms + (n + 1) s` has passed (`second` 0 at one whole second, 1 at two,
and so on): the seconds are contiguous, with no duplicate and no gap, while the job runs.

`job_exit.usage` is **authoritative for metering**. Bot turns each `job_usage` into one `usage-sample`
(`service:"run"`, `operation:"function.invoke"`, unit `s`) whose idempotency key is `run:<job id>:<n>`, where `<n>`
is the 0-based wall second — the `second` of the `job_usage` it covers, both counted from `started_ms`. A resent
`job` or `job_cancel` repeats the same `job_exit` and so the same keys, never a second reading and never a `:<n>`
for a second already metered. `job_exit.usage.wall_ms` is at least `(n + 1) * 1000` for every second `n` already
sent, so the exit's own usage covers everything the per-second frames announced — it is never corrected downward
by the resent `job_exit`. The Node resends `job_started` for running jobs and `job_exit` for ended ones after every
reconnect, and `job_exit` again on a `job` or `job_cancel` for that id, until `job_exit_ack`. `job_started` goes out
only once the job's CPU limit is set; a job that fails before (no namespaces, spawn or CPU limit failed) ends
`failed` without it.
A Node shutting down sends `job_exit` (`stopped`) also for a job acked but not started yet. `job_exit.reason`:

| `reason`    | `code`  | when                                                                                                |
|-------------|---------|-----------------------------------------------------------------------------------------------------|
| `exited`    | status  | the process ended by itself (`code` is null if a signal it did not get from the worker ended it); `result` is what it wrote to fd 3 when it exited 0 |
| `cancelled` | null    | `job_cancel`: the process group is killed; a job cancelled before it started never starts            |
| `ttl`       | null    | `ttl_ms` (from receipt, clamped to `worker.caps.max_ttl_ms`) ran out, before or while it ran         |
| `limit`     | null    | `wall_ms`, `cpu_ms` or `mem_bytes` (each clamped to the local cap), more stdout than `max_output_bytes`, or more than 256 KiB of result |
| `stopped`   | null    | the e-stop or the local stop latched, or the Node shut down                                          |
| `failed`    | null    | the worker could not run it: the namespaces could not be created, the spawn failed, or its CPU limit could not be set |

`job_cancel` for an id that is not running (unknown, refused or ended) and `job_exit_ack` for an unknown id are
ignored, with no answer: an `ack` keyed by a job id means the job was accepted.
A Node that runs jobs lists its classes and effective local worker caps in `status.capabilities.worker`:
`{"runtime_classes":["function"],"max_jobs":1,"max_ttl_ms":600000,"max_wall_ms":300000,"max_cpu_ms":300000,"max_mem_bytes":536870912,"max_output_bytes":1048576}`.
The limits are `worker.caps` with defaults applied; the key is absent while the worker is off, and `worker` is
reserved as a plugin name.

### Fault codes

`bad_frame`, `bad_value`, `unsupported`, `not_allowed`, `estopped`, `local_stop`, `expired`, `no_heartbeat`,
`not_ready`, `plugin_down`, `plugin_timeout`, `hardware`, `firmware_unsupported`, `not_connected`, `cliff`,
`picked_up`, `shutting_down`. Plugins may add their own; they are passed through.

### Heartbeat, deadman, reconnect

- The device sends `heartbeat` every `config.heartbeat_ms` (default 1000), starting once `hello` has arrived. The
  server answers `heartbeat_ack`. Until `hello`, a new connection may stay silent for up to 10 s (Bot may still be
  checking the credential); the deadman below applies from `hello` on.
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
