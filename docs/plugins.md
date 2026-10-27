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
| `hello`     | `config` {…}, `probe` bool, `halt_epoch`? (the newest a halt carried, 0.17) | answer `describe`; then set up (unless probe) |
| `command`   | `id`, `kind`, `value`, `deadline_ms`, `expires_at_mono_ns`? (0.18), `operator`?, `turn_epoch`?, `provenance`? | act, answer `ack` or `nack` with the same `id` |
| `heartbeat` | `t` (core clock, ms)                                          | note the time                                          |
| `stop`      | `cause` (0.17), `turn_epoch`? (a halt's)                      | stop every actuator; latch nothing                     |
| `estop`     | `cause`, `reason`?, `replayed`?, `first_cause`? (0.17)         | stop every actuator and refuse motion until `resume`   |
| `resume`    | `cause` (`cleared`, 0.17)                                     | accept motion again; never re-arm anything of its own  |
| `approval`  | `id`, `approved` bool                                         | the owner's answer to an `approval` event it raised    |
| `tray`      | `attached` bool, `ttl_s`                                      | (computer plugin only) the tray shows its approvals for `ttl_s` s unless told again: its own desktop prompts may stay quiet |
| `voice`     | `on` bool, `call_word`                                        | (plugins that describe a `voice` capability) the owner's Vibe by voice setting from Bot's config: listen for `call_word` only while `on` ([voice.md](voice.md)) |
| `voice_answer` | `id`, `state` (working, answered, refused), `text`, `url`  | (voice plugins) Vibe's answer to a `voice_ask` event the plugin raised: speak and show it |

`value` has already been validated and clamped to the owner's limits by the core ([protocol.md](protocol.md#commands)).
`deadline_ms` is relative to receipt and already capped. Since Node 0.18 the core counts it down to the moment it writes
the line, and adds `expires_at_mono_ns`: the instant the command expires on `CLOCK_MONOTONIC`, the clock every process on
the machine shares (absent on Windows). A plugin that reads that clock when the line arrives takes off the time the line
sat in the pipe too (answer 8); the Python runtime does.

`operator` is who sent the command, as Bot's gate saw them: `{"subject": "usr_…", "role": "owner" | "operator" |
"queue"}` (`queue` is the holder of a public robot's turn); it is absent when the core got none. Bot has already
decided the command may run, so a driver uses it to log or to apply a rule of its own, never to grant more. Commands
reach a plugin in the order Bot sent them, and when a public robot's turn ends or its holder leaves Bot halts the robot
before the next person's first command, which a plugin gets as `stop`: nothing from the last driver comes after it.
Python: `cmd.operator` (a dict, or `None`).

`turn_epoch` is the robot's control epoch Bot decided the command under (milliseconds on Bot's clock; Bot#155,
contracts 0.213.0). It rises when a queue turn ends, its holder leaves, or the owner takes the turn over, and Bot's halt
for that change carries the new value. The core keeps the newest epoch a halt carried per robot and nacks (`expired`) a
command older than it, so it never reaches a plugin; a driver may keep the newest it saw and refuse older ones too, as
defence in depth (a desk robot's own controller, for one). Absent from older Bots and on robots without a queue.
A halt's epoch also comes with the `stop` it causes (`{"op": "stop", "turn_epoch": …}`), so a driver knows the newest
epoch even before the next command. Python: `cmd.turn_epoch` (an int, or `None`); the runtime keeps the newest halt's
and refuses an older command itself (`expired`).

`provenance` is where the command came from, passed unchanged from Bot (Node 0.16):

| field | meaning |
|---|---|
| `session` | the link session it arrived on (Bot's `hello`; a new id on every connection, and ids sort by time) |
| `seq` | its frame's number on that session: strictly increasing, with gaps (other frames share the count) |
| `issued_at_ms` | Bot's clock when it sent the command, which is when it minted the id (`cmd_<ULID>`, whose time part is the same) |
| `expires_at_ms` | the absolute deadline on Bot's clock (motion kinds); `deadline_ms` is what is left of it when the core writes the line |
| `clock_uncertainty_ms` | (0.17) how far the core's estimate of Bot's clock may trail it: the connection's fastest round trip, plus 1 ms for Bot's whole-millisecond stamps since 0.18, rounded up from nanoseconds. `deadline_ms` already counts from estimate + this |

Python: `cmd.provenance` (a dict, or `None` from an older Node). The runtime refuses (`expired`) a command whose `seq`
is not above the last one it handled on the same `session`.

#### What a driver can rely on: commands are never redelivered

Asked by the Generic LOOI driver's authors (2026-10-26). From the code of Bot and this Node:

- **Bot sends a command once.** Each send gets a new id (`cmd_<ULID>`, minted at that moment). A command is sent only
  while the device is online and is never queued: offline, it is refused, not held. One that is not acknowledged by
  its deadline plus 1.5 s is recorded as `expired` and never sent again. An operator who repeats their own id gets the
  first result back and causes no second device command. Nothing is replayed after a reconnect, a Bot restart or a turn
  change. Jobs (`job_…`) do resend after a reconnect, but they are a different frame and never reach a driver as a
  `command`.
- **The Node delivers a command at most once.** It remembers command ids for 10 minutes (up to 4096) across
  reconnects and answers a repeated id with the first result, without passing it on. It refuses a command whose absolute
  deadline has passed on Bot's clock (`expired`), so a frame delayed or replayed with its original deadline never
  reaches a driver. Within a connection, commands reach a plugin in the order Bot sent them.
- **The boundaries.**
  - A turn change is a halt carrying a new `turn_epoch`. Epochs are milliseconds on Bot's clock, so they rise across
    Bot restarts. The core and the Python runtime both refuse older commands after it.
  - A connection is a `session`. The link is authenticated with the device's credential, and a plugin's stdin comes
    only from the Node that started it, so whatever arrives there is from that Node.
- **What is not kept.** The Node's id memory and newest-halt epochs start empty when the Node restarts. So does a
  plugin's state when the plugin restarts. Commands sent before a restart can no longer arrive afterwards: their
  connection is gone and their deadlines (at most `max_command_ms`, 300 ms on LOOI) have long passed.

So a driver needs no growing replay cache. Refuse a command if any of these is true:
- `deadline_ms` has run out;
- `turn_epoch` is older than the newest halt's;
- `seq` is not above the last on the same `session`.

The runtime does all three. A driver that wants more can also refuse an `issued_at_ms` older than the newest it has
handled by more than `max_command_ms`.

#### Deadlines, turns and stops: the platform's answers

Asked by the Generic LOOI driver's authors (2026-10-27). Each answer is what Bot (`server/realtime.js`,
`server/domain`), this core (`internal/node`, `internal/plugins`) and the Python runtime do:

1. **Where a deadline comes from.** Bot's control gate sets it when it decides a command:
   - The deadline is Bot's clock now plus the robot's `max_command_ms`: 300 ms unless the owner set less.
   - An operator may ask for less with `ms`, never more.
   - Only `drive`, `actuator`, `ptz` and a held button's `down` get one.
   - The command is sent in the same step. Its id (`cmd_<ULID>`) and `issued_at_ms` are minted then, after the gate's
     last await.
   - The core turns the absolute instant into what is left of it. It compares the instant with the latest Bot's clock
     can be: its estimate (the local monotonic clock plus the largest `ts − arrival` seen on the connection) plus that
     estimate's bound (answer 6). So time a frame spent queued on the way counts as lateness, and a delayed run of
     frames can never make a deadline look open again.
   - It caps what is left at the robot's `max_command_ms`, and fixes the instant it runs out on the machine's own clock:
     receipt plus that remainder.
   - The plugin gets what is left of it when the core writes the line, as `deadline_ms`, with that instant as
     `expires_at_mono_ns` (0.18, answer 8), and `expires_at_ms` in `provenance` unchanged.
   - Neither a viewer nor Live can choose a later deadline.
2. **Nothing re-stamps a command.**
   - Bot sends each device command once and never again, under any id. One with no answer by its deadline plus 1.5 s
     is recorded `expired` and dropped.
   - The core passes `provenance` through unchanged.
   - An operator who repeats their own id gets the first result, and no second command reaches the robot.
   - A retry under a new id, or a held control re-sent every 150 ms, is a **new** command. It passes the gate again at
     that moment: whose turn it is, the epoch, the e-stop and the limits. Its id, `seq`, `issued_at_ms`, deadline and
     `turn_epoch` are all new.
   - So a stamp is always a fresh decision, never an old command made current.
3. **Order across a change of driver.** A queue turn can end, its holder can leave, or the owner can take it over. Then:
   - Bot ends the turn, moves the robot's epoch and sends its `halt` in one synchronous step.
   - A command decided under the old turn but not yet sent is refused (`bot.not_your_turn`). There is no await between
     that check and the send.
   - On the one device connection, everything from turn N therefore precedes N's halt. The halt carries the new epoch,
     and every later command carries it or a newer one.
   - The core and the runtime refuse an older epoch after a halt (`expired`).
   - On a takeover, the halt goes first and the owner's command follows with the new epoch.
   - A waiting viewer who leaves causes no halt.
   - Owners and operators of a robot without a queue have no turns. Their commands reach the driver in the order Bot
     sent them, and the newest wins.
   - Acks may come back in any order. Commands never reach a plugin out of order.
4. **`stop` versus `estop`.**
   - `stop` stops every actuator **now and latches nothing**. The next valid command moves the robot again. It comes
     from:
     - a `halt` (a change of driver, with its `turn_epoch`, or an operator's halt);
     - the link to Bot dropping;
     - the core shutting down (then stdin closes);
     - `openvibe-node quit`;
     - a plugin missing its reply window.
   - `estop` stops every actuator **and latches**. The core refuses `drive`, `ptz` and `actuator` itself (`estopped`,
     `local_stop`), and so does the runtime, until `resume`. The latch:
     - is set by Bot's e-stop (an owner or operator presses it), by `openvibe-node stop`, or by a driver's `local_stop`;
     - is written to disk, survives a Node restart, and is sent again at once to a plugin that restarts;
     - is reapplied from Bot's config on every reconnect.
   - Only the owner clears Bot's e-stop. A local stop is cleared only by `openvibe-node resume` on the machine.
   - A `halt` still passes while latched.
   - The driver's own stops (a motion deadline passing, a second without a heartbeat, stdin closing) are `stop`s.
     Heartbeat loss also refuses motion until heartbeats return.
5. **No command is held past its validity.**
   - Bot never queues: an offline device is refused (`bot.device_offline`), and nothing is replayed after a reconnect.
   - The core decides at arrival:
     - it refuses a command whose deadline has passed;
     - it refuses a plugin that is down or not ready at once (`plugin_down`, `not_ready`) rather than holding the command;
     - it queues the rest for the plugin's stdin. A plugin that stops reading until 256 lines are waiting is killed.
       Until then lines can wait in that queue behind a full pipe; since 0.18 that time counts against a command, and one
       whose time runs out there is answered `expired` and never written (answer 8).
   - A plugin that does not answer within max(deadline, 1 s) is told to `stop`, and the command fails with
     `plugin_timeout`.
   - The Python runtime counts each deadline from the moment its line arrived. It refuses (`expired`) a motion command
     whose deadline passed while it waited behind a slow handler, so the driver never sees it. Before Node 0.16.1 such a
     command still reached `handle` and was stopped on the next tick. Since 0.18 it also takes off the time the line sat
     in the pipe before it was read (`expires_at_mono_ns`).
   - A driver without the runtime should do the same: note the time each line is read (on a reader thread) and refuse
     motion that is already past its `deadline_ms` when its turn comes.

Asked next by the same authors (2026-10-27, P7 LOOI shadow integration), and changed in Node 0.17.0 to answer them:

6. **The clock's error, its bound, and when motion waits for it.**
   - **The error.** Each frame from Bot gives one sample, `ts − arrival` = the clock offset less that frame's one-way
     transit. The estimate keeps the largest sample on the connection, so it trails Bot's real clock by the fastest
     one-way transit among them. That is a late reading: on its own it would grant a command up to that transit more
     than its deadline allows.
   - **The bound.** Every `heartbeat_ack` is one of those samples, and its one-way transit is at most its own round
     trip. So the connection's fastest heartbeat round trip bounds the error, whatever the asymmetry of the path.
     The core checks every deadline against estimate + bound (the latest Bot's clock can be). This is fail-closed: a
     command can only get less time than its deadline allows, never more. (0.18 made the arithmetic strict: answer 8.)
   - **What else holds.** Clock drift over a connection is tracked by the newest samples or reads early (fail-closed).
     The local clock is monotonic, so a wall-clock step on the device changes nothing. Bot's clock stepping back reads
     as early until the next connection; stepping forward moves the estimate with the next frame. Latency and jitter
     only make commands arrive later, so they read as expired, never as open. The window a plugin gets is also capped
     at the robot's `max_command_ms`, and two missed heartbeats end the link (`stop`, `link_lost`).
   - **Before a bound exists.** The first heartbeat goes out as soon as Bot's hello arrives, and its ack sets the bound
     (one round trip, usually tens of milliseconds). Until then the core refuses `drive`, `actuator`, `ptz`, `button` and
     `point` that carry a deadline (`not_ready`, "the link's clock is not synchronized yet"). Every new connection starts
     unsynchronized. A link whose fastest round trip is over 1 s (`MaxClockUncertaintyMS`) times no motion either
     (`not_ready`). Speech and other non-motion kinds are not held.
   - **A stricter cap of your own.** The bound reaches the plugin as `provenance.clock_uncertainty_ms`. A driver may refuse
     motion above a cap it chooses (say 150 ms for P7) without inventing another clock or replay mechanism. The deadline,
     epoch and seq contract stays as it is.
7. **Why a plugin is stopped (`cause`), and how a driver tells an ordinary stop from an emergency.**
   - Every `stop` and `estop` names its cause, and `hello` carries the newest halt epoch, so the meaning holds across a
     plugin restart and a Node reconnect.

     | op | `cause` | when | latches |
     |---|---|---|---|
     | `stop` | `halt` | Bot's halt: a turn ended, a viewer handoff, a release, a timeout, an unbind (with `turn_epoch`) | no |
     | `stop` | `link_lost` | the link to Bot dropped (two missed heartbeats, a close) | no |
     | `stop` | `shutdown` | the Node is stopping (service stop, `openvibe-node quit`), or the core is stopping this process | no |
     | `stop` | `timeout` | a command got no answer from the driver within its window | no |
     | `estop` | `remote` | the platform's e-stop (a robot owner or staff), with its `reason` | yes, until the owner clears it |
     | `estop` | `local` | `openvibe-node stop` on the device | yes, until `openvibe-node resume` |
     | `estop` | `driver` | a driver's own `local_stop` (this plugin's or another's) | yes, until `openvibe-node resume` |
     | `estop` | `latched` | the latch was already set when the Node or this process started (`replayed: true`, `first_cause` when known) | yes |
     | `resume` | `cleared` | the owner cleared the platform's latch | clears the core's and the runtime's refusal only |

   - **Python (`openvibe_plugin` 0.17).** Every stop the runtime makes calls `on_stop(info)`, and `info` is a `StopInfo`:
     `cause`, `emergency` (true for `estop`), `latched`, `reason`, `turn_epoch`, `halt_epoch`, `replayed`, `first_cause`,
     `motion_only`. The runtime's own causes are `setup` (the stop every setup ends with), `deadline` (motion only: stop
     what moves on its own), `heartbeat` (the core went quiet), `handler_error` and `eof`. An older core sends no cause:
     `stop` or `estop`. The default `on_stop` calls `stop_motion()` for a deadline and `stop()` otherwise, so existing
     drivers behave as before. `on_resume(info)` does nothing by default.
   - **For P7.** Override `on_stop`: stop every motor in every case. Latch P7's own STOP only when `info.emergency` is
     true; an ordinary halt, a disconnect, a timeout or a handoff only revokes remote motion. Treat `on_resume` as
     information. Never ARM P7, clear its physical latch or restore a remote grant there; the next command still passes
     every check. Across a plugin restart the latch arrives again as `estop` `latched` with `replayed: true`, and the halt
     epoch comes in `hello`. Across a Node reconnect the link's loss is a `stop` (`link_lost`); a platform e-stop set
     while offline is applied from Bot's config before any command.

Reviewed again by the same authors against v0.17.0 (2026-10-27, `632207f`), and changed in Node 0.18.0:

8. **Node-to-plugin transit, the bound's rounding, and what a driver may assume.**
   - **Transit extended a command (confirmed, fixed).** In 0.17 the core turned the deadline into `deadline_ms` when the
     frame arrived, then queued the line for the plugin's stdin. Lines wait in that queue while the pipe is full (a
     plugin slow to read), and the Python runtime counted `deadline_ms` from when its reader got the line. So queue and
     pipe time were not counted: a command with 100 ms left that waited 200 ms could still move for 100 ms. Nothing in
     0.17 prevented it. In 0.18:
     - The core fixes the instant a command expires when it arrives: receipt plus what was left (capped at
       `max_command_ms`).
     - The writer stamps the line when it writes it: `deadline_ms` is the whole milliseconds left then, and
       `expires_at_mono_ns` the instant on `CLOCK_MONOTONIC`. A command with less than 1 ms left is never written. The
       core answers it `expired`, saying how long it waited.
     - The Python runtime reads both clocks when a line arrives (its own clock first, then the shared one, so the time
       left can only come out short). It refuses (`expired`) a motion command whose `expires_at_mono_ns` has passed, so
       `handle()` never runs. Otherwise the command gets the smaller of `deadline_ms` and what that instant leaves.
     - Time the line then waits behind a slow handler counts as before (answer 5).
     - The tests:
       - `TestWriterQueueDelayCountsAgainstTheDeadline` (Go) fills the pipe while the plugin sleeps 400 ms. A command
         with 100 ms is answered `expired` and never reaches the plugin. One with 1.5 s reaches it with at most what is
         left: its `deadline_ms`, and on the plugin's own clock.
       - `test_a_real_pipe_delay_never_extends_a_deadline` (Python) leaves lines in an OS pipe for 200 ms. The 100 ms
         command never reaches `handle()`, and the 1 s one gets at most 800 ms.
       - `test_pipe_delay_counts_against_the_deadline` covers the edges with fake clocks: 0.999999 ms left is expired,
         and pipe plus inbox delay add up.
       - Each test fails with the correction removed.
   - **Clock domains.** There are three:
     - Bot's clock: the absolute deadline, read through the link's estimate and its bound (answer 6).
     - The core's monotonic clock: Go's, so a wall-clock step changes nothing.
     - `CLOCK_MONOTONIC`, between the core and the plugin. The core reads it with `clock_gettime`; Python reads it with
       `time.clock_gettime_ns(time.CLOCK_MONOTONIC)`. It is one clock for every process on the machine, and the core's
       monotonic clock runs at the same rate.

     The correction assumes the plugin runs on the same machine as the core (the core starts it, over a pipe). It also
     assumes both read the same clock id. On Windows there is no shared clock: `expires_at_mono_ns` is absent, and only
     the core's queue time is counted, up to the write.
   - **The bound's rounding (confirmed, fixed).** 0.17 cut a round trip to whole microseconds before rounding up, so
     1,000,001 ns came out as 1 ms. Two more roundings were not counted at all:
     - Bot stamps `ts` in whole milliseconds, rounded down. A frame may have left up to 1 ms after its stamp, so the
       estimate could trail by that much more than the round trip.
     - The core kept receipt times in whole milliseconds.

     In 0.18:
     - The estimate is kept in nanoseconds.
     - The bound is the fastest round trip plus 1 ms for the stamps.
     - `timeLeft` works in nanoseconds and rounds the time left down.
     - `clock_uncertainty_ms` and the `MaxClockUncertaintyMS` check round the bound up from nanoseconds. A bound of
       exactly 1 s is still admitted, and 1 s + 1 ns is refused.
     - So `clock_uncertainty_ms` reads about 1 ms higher than in 0.17.
     - Admission is otherwise unchanged: no motion before a connection's first round trip, none over 1 s.
     - The tests: `TestCeilMSRoundsUpFromNanoseconds` (nanosecond and microsecond boundaries),
       `TestServerNowKeepsNanoseconds`, and the bound in `TestClockUncertaintyIsTheFastestRoundTrip`.
   - **Stops and local authority (confirmed, no change).**
     - Every stop the runtime makes calls `on_stop(StopInfo)`:
       - `info.emergency` is true only for `estop`.
       - `info.cause` says which (answer 7).
       - `info.halt_epoch` is the newest halt epoch the runtime knows, and comes from `hello` after a restart.
       - `info.turn_epoch` is the epoch of the halt that caused this stop.
     - An ordinary stop (`halt`, `link_lost`, `shutdown`, `timeout`, `deadline`, `heartbeat`) only says "stop now". It
       latches nothing in the core or the runtime, so an adapter may revoke remote motion without touching a newer local
       owner's command.
     - `on_resume` is information. Its default does nothing, and the runtime re-arms nothing, so P7 is not re-armed and
       no local grant comes back.
   - **A replayed e-stop comes before any motion.** The core keeps the latch on disk and sets it before it starts any
     plugin.
     - **Plugin restart.** The process gets `hello` with the latest halt epoch, then the e-stop again (`latched`,
       `replayed: true`, `first_cause`). No command is routed to it before its `ready`, and lines reach it in the order
       they were queued. So the replay arrives before any command, and the runtime refuses motion (`estopped`) from
       then on.
     - **Node reconnect.** No command is accepted until Bot's config has been applied on the new connection, which sets
       the latch first.
     - **Defence in depth.** The core itself refuses `drive`, `actuator`, `ptz`, `button` and `point` while latched,
       whatever the plugin's state. `test_a_replayed_estop_is_applied_before_any_motion`, `TestCrashRestartsWithEstop` and
       `TestLatchSetBeforeStartIsReplayedFirst` hold the order. (Before 0.18 a Node booting latched told its plugins just
       after starting them. That was also safe, by the core's own refusal, but is now in the order written here.)
   - **What a strict guarantee still assumes.** Each of these makes a command read as expired, never as open:
     - Bot's clock does not step back during a connection (that reads as expired until the next one).
     - The host is not suspended mid-command: `CLOCK_MONOTONIC` stops while suspended, and the link is lost on resume
       anyway.
     - The plugin is on the same machine.

     Bounded but not removed:
     - The gap between the runtime's check and the driver's own write to the hardware (the driver's code).
     - Time inside `handle()`.
   - **Versions.** The Python SDK package now carries the Node release (`openvibe_plugin.__version__`, `pyproject.toml`:
     0.18.0; it said 0.1.0 before). For an exact source, pin the commit, not the version.

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

Two `event` names are the core's own, for voice plugins: `voice_ready` asks for the owner's setting (the core answers
with `voice`), and `voice_ask` (`id`, `text`, optional `lang`) is what the person said after the call word. The core sends
it to Bot as a `voice_ask` frame and never as a telemetry event, and routes the `voice_answer` frames back to the plugin
that asked.

For `video`, the plugin writes each frame to a file (write to a temporary name, then rename, and rotate a few names so
a slow reader never sees a partial file) and announces it with `{"op": "video", "format": "jpeg", "path": …}`
(`emit_video` in [`plugins/sdk`](../plugins/sdk)). The core reads the file at once and encodes it: with `ffmpeg` if
installed, otherwise with its built-in encoder at up to 5 frames per second. The core side of this contract is
`JPEGSource` in [`internal/video/sources.go`](../internal/video/sources.go#L298), selected when the plugin's
`describe` advertises `camera.jpeg: true` (`internal/node/node.go`).

## Safety rules (every plugin)

A plugin must stop every actuator itself — the core dying must never leave motors running:

- when its **stdin closes** (EOF), then exit;
- when a **motion command's deadline passes** without a newer command (`deadline_ms` after receipt), and refuse one
  whose deadline passed before its turn to run (`expired`);
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

A driver with a safety latch of its own tells an ordinary stop from an emergency with `on_stop` (Node 0.17; "Why a
plugin is stopped" above):

```python
    def on_stop(self, info):              # every stop: a halt, a lost link, a deadline, an e-stop…
        self.stop()                       # always stop every actuator first
        if info.emergency:                # the platform's e-stop (remote, local, driver, or replayed as latched)
            self.hardware_latch.set()     # only then latch the hardware's own STOP

    def on_resume(self, info):            # the owner cleared the platform's latch: information, never a re-arm
        self.log("platform latch cleared; the hardware latch stays until the person on site clears it")
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
