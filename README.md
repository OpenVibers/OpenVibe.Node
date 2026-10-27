# OpenVibe Node

One service for your computers, servers, Raspberry Pis, phones and robots: pair it once with OpenVibe and products
like **OpenVibe.Bot** (robots: drive, cameras, sensors, with safety) and **OpenVibe.Actor** (computer control for AI
agents, at the level you set on the machine) can use the device. It connects out, needs no open ports, and keeps a local kill switch.

On a robot's Raspberry Pi (or other Linux computer), the one command:

```sh
curl -fsSL https://openvibe.bot/install | sh
```

It turns on I2C and SPI, installs the camera tools, finds the kit (an Adeept ADR036 4WD, a ROS 2 robot), shows a code
to approve at openvibe.bot/link, and pairs: the robot is made, drivable on its panel and, if you chose it, live on your
OpenVibe.Live channel ([docs/install.md](docs/install.md#the-one-command-raspberry-pi-and-other-linux-robots)).

With a robot already added on openvibe.bot:

```sh
curl -fsSL https://openvibe.bot/install | sh -s -- --robot rob_… --code ABCD-1234 --driver adeept
```

openvibe.bot shows this command with the robot's id and a fresh code. `--driver` is `adeept`, `adeept-mecanum`, `cozmo`, `ros2`,
`looi` (the community's Generic LOOI driver, [docs/hardware-looi.md](docs/hardware-looi.md)) or `none` (dry run; the old `--robot adeept` form still works but is deprecated). A robot paired through
OpenVibe.Network gets `--network <URL> --pairing pair_… --code ABCD-1234` instead of `--robot`. Details, manual
install, Windows and configuration: [docs/install.md](docs/install.md).

```sh
openvibe-node status     # link, plugins, video, battery, stop latch
openvibe-node stop       # kill switch: stop every motor now and keep it stopped
openvibe-node resume
openvibe-node quit       # end the node cleanly (nothing latched), e.g. from a stream launcher
openvibe-node approvals  # the computer plugin at level ask: what waits; approve <id> / deny <id>
openvibe-node tray       # the same in your notification area, with Approve/Deny notifications (Linux, Windows)
```

## What is in here

- **Core** (Go, one static binary per platform): pairing, one outbound WebSocket to `wss://openvibe.bot/device`
  (credential in the `Authorization` header only), heartbeats and a deadman, the safety rules (deadlines, owner limits
  clamped before any driver sees a value, a persisted e-stop, the local kill switch), the plugin supervisor, WHIP video
  publishing with pion, and the system service (systemd, launchd, Windows).
- **Plugins** (separate processes, JSON lines over stdin/stdout): [`dryrun`](plugins/dryrun), the Adeept 4WD Smart Car
  Kit [`adeept_adr036`](plugins/adeept_adr036/README.md), [`cozmo`](plugins/cozmo/README.md) through PyCozmo, and
  [`relay`](plugins/relay) for existing Live hardware clients, and [`ros2`](plugins/ros2/README.md) for ROS 2 robots
  (`cmd_vel`, battery, scan/range, odometry and camera, as a node with rclpy or through rosbridge). LOOI's driver is the community's
  ([docs/hardware-looi.md](docs/hardware-looi.md) is its contract). Each plugin stops its own actuators when stdin closes,
  at a command's deadline and after 1 s without a heartbeat, so the core dying never leaves motors running.
- **ESP32 library** ([`esp32/`](esp32/README.md), `OpenVibeNode`): the device side of the protocol for ESP32 boards —
  pairing, the control link, drive/actuator commands, telemetry, heartbeat, deadman and e-stop — but no jobs or video.

**Community robots:** a profile people submit on openvibe.bot (`community.<name>`) is reviewed by an OpenVibe admin
and signed by Bot (Ed25519 over its canonical JSON). `openvibe-node profile verify community.<name>` checks that
signature against the key Bot publishes (`--key-id` pins one) and names the plugin the robots run; pairing with a robot
on a community profile runs the same check and warns when it fails. Install a community plugin only when it passes
([docs/plugins.md](docs/plugins.md#community-plugins)).

**Your computer for your agents:** `--computer observe|ask|trusted|full` adds the `computer` plugin (terminal, files,
screen, mouse and keyboard, and a browser of its own for OpenVibe.Actor), running as your own account, at the level you set on the machine; `ask` waits for
your answer in the tray, a notification's Approve button or `openvibe-node approve <id>` ([docs/computer.md](docs/computer.md)).
With it on, you can also watch the machine's screen live on openvibe.bot. Only you see it, it runs for up to 30
minutes, and `openvibe-node stop` ends it at once ([the live screen](docs/computer.md#the-live-screen-screen_stream-node-v0120)).

**Your own models for your OpenVibe work:** the `ai` plugin lets OpenVibe use a model you already run at home (Ollama,
LM Studio or llama.cpp's server) for your own work, at no cost and with no open port, once you turn on "Let my AI use
this computer's models" on openvibe.bot/machines ([docs/ai.md](docs/ai.md)). The installer sets it up with `--ai`.

**A machine that runs your jobs:** add it on openvibe.bot/machines, which prints the same installer with `--worker`: the
job worker runs each job as an unprivileged user in a sandbox, and the OpenVibe.Codes job runner lets you start a coding
task on openvibe.codes/run from any browser and have this machine do it ([docs/worker.md](docs/worker.md)).

Docs: [protocol](docs/protocol.md) (the wire format with OpenVibe.Bot), [plugins](docs/plugins.md) (the driver
protocol, the bundled drivers, and where Actor's computer-control plugin attaches), [install](docs/install.md).
Decisions: [ADR-043](docs/ADR-043-bot-devices-and-control.md).

## Development

```sh
go vet ./... && go test -race ./...                    # the end-to-end tests run the Python dry-run plugin
pip install pytest pillow numpy
for p in sdk dryrun adeept_adr036 cozmo relay ros2 computer; do python3 -m pytest -q plugins/$p/tests; done
scripts/dist.sh v0.0.0-dev                             # every binary, the plugin bundle and SHA256SUMS in dist/
```

`internal/fakebot` is a fake OpenVibe.Bot (pairing, device WebSocket, WHIP) used by the tests. Licence: AGPL-3.0
([LICENSE](LICENSE)). Security reports: [SECURITY.md](SECURITY.md).
