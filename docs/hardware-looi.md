# LOOI on the OpenVibe Node

LOOI is the tracked desk robot whose face is a phone. Its base talks Bluetooth Low Energy, so it is a **bridge**
robot: the Node runs on a computer next to it (Windows, macOS or Linux), and a driver plugin drives the base over
Bluetooth. The phone streams its own video, for example from its browser to the owner's OpenVibe.Live channel, so the
Node publishes none.

The Generic LOOI driver is written by the community (LOOI owners), not bundled with the Node. This page is the
contract it implements and how to run it.

## On openvibe.bot

Add a robot with the **LOOI desktop robot** profile (`looi`). Its panel has a drive pad and a head-tilt control, and
it shows battery and link latency. The profile's limits apply before any value reaches the driver: speed at most 0.5,
turn at most 0.6, every motion command lasts at most 300 ms unless it is re-sent, heartbeat 1 s. The owner can lower
them per robot.

Who may drive is Bot's job, not the driver's: the owner, invited operators, an optional public queue with timed
turns, cooldowns and per-role allowed commands, all enforced by Bot's command gate before a command reaches the Node.
The e-stop is latched in Bot and only the owner clears it. Each command tells the driver who sent it (`cmd.operator`,
`{"subject", "role"}`), and when a queue turn ends or its holder leaves Bot halts the robot before the next person
drives (the driver gets `stop`), so commands from the last driver never come after it
([plugins.md](plugins.md#core--plugin)). The owner or an operator driving during someone's turn pauses that turn and
halts the robot first, the same way. Each command also carries `cmd.turn_epoch`: the core already refuses one older
than the newest halt's, and a driver may check it again against its own record.

To show the panel under a Live stream, bind the robot to the channel on the Live dashboard (the robot card). The
channel page then shows the robot's panel beside the phone's stream.

## What the driver receives

The driver is a Node plugin ([plugins.md](plugins.md)): JSON lines over stdin and stdout. For `looi` it receives:

| command | value | meaning |
|---|---|---|
| `drive` | `{"throttle": -1..1, "steer": -1..1}` | tracks: left = throttle + steer, right = throttle − steer (steer > 0 turns right), normalised so neither exceeds 1 |
| `actuator` | `{"name": "head", "value": -1..1}` | head tilt, -1 fully down, 1 fully up |
| `halt` | — | stop everything now; it always passes, even while stopped |

Every `drive` has a `deadline_ms`. The tracks must stop when it passes without a newer command (the panel re-sends a
held control every 150 ms). With the Python runtime (`openvibe_plugin`, [plugins/sdk](../plugins/sdk)) set
`motion_kinds = {"drive"}`, and it enforces the deadline, the 1 s heartbeat, stdin closing, `stop`, `estop` and
`halt` for you.

The driver should send telemetry with `battery` (`{"percent": 0..100}` or `{"volts": …}`) about twice a second.

## Stopping

- **The robot must stop on its own when commands stop.** The Node and the runtime stop the driver's motors on a
  passed deadline, a missed heartbeat or a closed stdin, but if the bridge computer sleeps, crashes or loses
  Bluetooth, only the robot can stop itself. Before any motor test, check that the base stops when the Bluetooth link
  drops or commands stop arriving. If it does not, keep every command short-lived so it cannot run on.
- **A stop on the robot itself.** If the driver has its own stop (a button, a hotkey, a sensor it trusts), call
  `self.local_stop("reason")` (or send `{"op": "local_stop", "reason": "…"}`). Every actuator stops at once, the
  Node latches its **local** stop (persisted across restarts), and the panel shows the robot as stopped on the robot.
  The owner's remote clear does not lift it: only `openvibe-node resume` on the bridge computer does, and then the
  owner clears the panel's e-stop. Running `openvibe-node stop` does the same from a terminal.

## Installing the driver

The Node runs a plugin named `looi` as `python -m openvibe_looi`, so the driver is a Python package with that module,
installed into the Python the Node uses.

**Linux or macOS bridge.** `curl -fsSL https://openvibe.bot/install | sh -s -- --robot rob_… --code XXXX-XXXX
--driver looi` installs and pairs the Node as a bridge with video off. Then install the driver into the Node's
environment (`sudo /var/lib/openvibe-node/venv/bin/pip install <the driver>`) and restart the Node's service. Until
the driver is installed the panel shows the plugin's fault and the robot never moves.

**Windows bridge.** Bluetooth through the Windows runtime may not work from a service running as LocalSystem, so
run the Node in your own session:

```bat
set OVHOME=%LOCALAPPDATA%\OpenVibe Node
openvibe-node pair XXXX-XXXX --robot rob_… --kind bridge --home "%OVHOME%"
openvibe-node install --user-session --home "%OVHOME%"
```

`config.json` in that directory:

```json
{
  "server": "https://openvibe.bot",
  "device_kind": "bridge",
  "python": "C:\\Users\\you\\AppData\\Local\\Programs\\Python\\Python312\\python.exe",
  "plugins": [{"name": "looi"}],
  "video": {"source": "off"}
}
```

`install --user-session` starts the Node when you sign in, in a console window that shows its log (Windows keeps no
service log yet). Closing the window stops the Node and every actuator; nothing restarts it until the next sign-in.
`openvibe-node uninstall --user-session` removes the sign-in start.

Try the driver without the robot first: `openvibe-node plugins` asks each plugin to describe itself without touching
hardware, and `openvibe-node run --dry-run` drives only the simulated plugin.
