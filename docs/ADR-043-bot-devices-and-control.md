# ADR-043: Bot — devices, pairing, control and safety

**Status:** Accepted 2026-09-29 (plan track T15, "Bot complete"; owner direction the same evening: openvibe.bot is an open
control panel for robots, streaming optional; the first robots are the owner's Adeept 4WD Smart Car for Raspberry Pi and
a Cozmo-based robot).

## Context and current evidence

- Robot control lives in OpenVibe.Live today (`server/controls/`: control-server 572 lines, routes 1522, ONVIF 444). A
  robot authenticates with its owner's **stream key**, the same string that appears in RTMP/WHIP publish URLs. One device
  per streamer (the socket map is keyed by stream key). No heartbeat, no deadman, no acknowledgement, no audit.
- Chat commands (`!forward`, `!headup`, `!say`…) reached the robot with no permission check; Live fb3957f put them behind
  the panel's rules as a stop-gap. Live also generates ~900 lines of Python per Cozmo owner (`routes.js`), because
  buttons are table rows rather than a profile.
- OpenRe's webrtc worker (75a5dd7) takes WHIP ingest and serves viewers; Media records; Events carries events; Network
  issues service tokens and delegated grants.
- The owner's first robot, the Adeept ADR036, is a Raspberry Pi with Adeept's Robot HAT: a PCA9685 at I²C 0x5f drives the
  four DC motors (channels 8–15) and the pan/tilt servos (0, 1); an ADS7830 at 0x48 reads battery voltage; ultrasonic,
  line and light sensors, WS2812 LEDs, a buzzer, a Pi camera through Picamera2. Its stock software listens on
  0.0.0.0:8888 with a hard-coded `admin:123456` login and serves MJPEG on :5000.
- The second, a Cozmo, runs closed firmware, makes its own Wi-Fi access point with no internet and speaks its own UDP
  protocol; PyCozmo (MIT) drives it directly: wheels, head, lift, camera (320×240), speaker, face display, lights, cubes,
  cliff/pick-up sensors, battery.

## Decision

1. **Robots, devices and credentials are separate things.** A **robot** (`rob_…`) is what the owner sees and shares; a
   **device** (`dev_…`) is a running agent attached to one or more robots (a Pi on a rover; a bridge computer driving a
   Cozmo; later a phone brain). A device holds one **device credential**: 32 random bytes, stored hashed, shown once, never
   in a URL, a log line or a browser, rotatable (the old one keeps working 60 s) and revocable (instant; the device must
   pair again). A device's camera publishes with its own **publish key** (per device, rotatable), never the owner's
   stream key. Stream keys stop being robot credentials.
2. **Pairing is a one-time code.** The owner adds a robot; Bot shows an 8-character code (Crockford base32, `XXXX-XXXX`,
   10-minute lifetime, single use, 5 wrong tries end it) as text, as a QR code and inside the one-line installer command.
   The agent sends `{code, agent_version, device_kind, drivers, capabilities}` and receives its device id,
   credential, publish key and the robot's profile. The owner sees the device appear and confirms it.
3. **Three kinds of device connection, one agent.** **On-board**: the agent runs on the robot's own computer (Raspberry
   Pi; ESP32 through a small library speaking the same protocol). **Bridge**: the agent runs on a computer next to the
   robot and reaches it over the robot's own link (Cozmo first through PyCozmo; later Vector, Tello, Sphero, LEGO); a
   bridge joins the robot's network on one interface and reaches OpenVibe on another. **Server-side**: Bot itself
   connects to ONVIF/RTSP cameras (credentials by secret reference, never in the profile). The agent is a Python 3 package
   (the Pi, PyCozmo and the kit libraries are Python), installed by `curl -fsSL https://openvibe.bot/install | sh` into a
   virtual environment with a systemd unit and the hardware watchdog.
4. **Control runs over one outbound WebSocket per device; video over WHIP.** The device dials Bot
   (`wss://openvibe.bot/device`), authenticates with its credential, and keeps one connection for commands, telemetry and
   heartbeats; this works behind any home router and on an ESP32. The camera publishes to OpenRe over WHIP; viewers watch
   through OpenRe. Operators reach Bot over a WebSocket from the panel. A WebRTC data channel may be added later as a
   second control transport when measured latency asks for it; the message set does not change.
5. **One message set** (JSON, fields `v`, `seq`, `ts` on every frame): server→device `hello`, `config` (limits, heartbeat,
   the operator's allowed commands), `command` (`id` as an idempotency key, `kind` drive/actuator/ptz/say/display/halt,
   `value`, `deadline_ms`, operator and role), `estop`, `heartbeat_ack`; device→server `status`, `telemetry` (≤ 2 Hz),
   `ack`/`nack` (with a fault code), `heartbeat`, `estop_state`; robot events (cliff, picked up, low battery) travel in
   `telemetry.events`, not as a separate message type. Commands are never queued for an offline device and
   never replayed after a reconnect.
6. **Safety is not optional.** Every motion command carries a deadline relative to receipt (`deadline_ms`, default 300 ms, at most the robot's
   `max_command_ms`); a held control re-sends every 150 ms; the **device** stops the motors at the deadline, on a lost
   heartbeat (1 s interval, stop after 2 missed) and on every disconnect or crash path, without asking the network. The
   e-stop is latched on the device and in Bot and only the owner clears it. Speed, turn and range limits and time windows
   are set by the owner; an operator never exceeds them. Cozmo additionally stops on a cliff or pick-up event. The agent
   never runs or exposes a kit's stock control server (the Adeept `admin:123456` port stays off).
7. **Robot profiles, not rows.** `bot.robot-profile@1` (Contracts): capabilities (drive differential/mecanum, pan-tilt,
   lift, head, servos, lights, speaker/say, display, sensors, cameras, battery), panel widgets bound to capabilities,
   limits and the driver mapping. The panel is a function of the profile; a new robot needs a profile, not code. First
   profiles: `adeept.adr036` (ordinary and mecanum wheels), `cozmo`, `camera.onvif`, `sim.rover` (the browser simulator on
   the openvibe.bot home). Capability and widget names are open strings checked against a registry in code, so a new
   widget never needs a schema change.
8. **Access.** Owner, operators (invited), viewers. A robot is private by default; public control exists only as a
   timed queue (a visible turn timer, a per-turn command budget, cooldowns, per-role command allowlists) with an owner
   kill switch. Chat control (Live, openvibe.chat) is a caller of the same gate: Chat or Live sends a command on behalf of
   the viewer with a service token holding `bot.robot.control`, and Bot applies the role, the allowlist and the limits.
9. **Everything is recorded.** Each command (robot, device, operator principal, kind, result, latency) goes to an audit
   table kept 30 days; `bot.robot.online|offline`, `bot.command.refused`, `bot.estop.set|cleared` and telemetry summaries
   are Events; Actor agents drive robots only through a delegated `bot.robot.control` grant and the owner's approval.
10. **The service.** OpenVibe.Bot is its own repository and service on PostgreSQL (the platform chassis: openvibe-sdk db,
    auth, limits, outbox; openvibe-shared Frame and showcase), public origin openvibe.bot. Capabilities
    `bot.robot.read`, `bot.robot.manage`, `bot.robot.control`, `bot.device.connect`. Live's `server/controls/`, its
    control tables and the stream-key device path are deleted once the panel embed works on Live channel pages and the
    existing robot owners have paired again (converting presets to profiles and whitelists to operators).

## Alternatives considered

- **WebRTC data channel as the only control transport:** rejected for now. It ties control to video negotiation, needs
  a TURN path for control alone, and does not run on an ESP32; one WebSocket carries control everywhere, and the message
  set leaves room for a data channel later.
- **Keep the stream key as the robot credential:** rejected. One string with three jobs (publish, control, URL) is the
  largest standing risk in today's design.
- **Run a kit's own server behind a proxy:** rejected. The Adeept server's fixed password and open MJPEG cannot be made
  safe by wrapping; the agent drives the hardware itself.
- **Generate per-robot scripts (Live's Cozmo approach):** rejected in favour of drivers plus profiles.

## Migration consequences

- Contracts: `bot.robot-profile@1`, `bot.device-message@1` (the envelope and each message type), the four capabilities,
  the `bot` service manifest and its events.
- Network: a `bot` service principal; the `bot.robot.control` grant for Live and Chat (on behalf of viewers) and for
  Actor agents (delegated).
- OpenRe: per-device publish keys for WHIP sessions created by Bot.
- Live: the panel embed on channel pages, then the deletion in decision 10; Extensions `hardware/` becomes a pointer to
  the Bot agent.

## Rollback

- The service is additive until Live's controls are deleted; before that, a robot owner can keep using Live's panel.
- After the deletion, the rollback is the previous Live release plus the database backup.

## Acceptance tests

- Pairing: a code works once, expires after 10 minutes, dies after 5 wrong tries; the credential is never logged or
  returned twice; rotation keeps the old credential for 60 s; revocation disconnects the device at once.
- Safety: a drive command without a newer one stops the motors at its deadline; killing the network, the WebSocket or
  the agent process stops the motors; the e-stop latches on the device and only the owner clears it; limits cap every
  operator; Cozmo stops on a cliff event.
- Access: a viewer cannot drive a private robot; a queued turn ends on its timer; chat commands pass the same gate.
- Profiles: the Adeept and Cozmo profiles validate and render a panel with no robot-specific frontend code.
- End to end: the agent in dry-run mode pairs, publishes a test pattern over WHIP, receives commands, answers
  telemetry, and the panel shows video, latency and battery.
