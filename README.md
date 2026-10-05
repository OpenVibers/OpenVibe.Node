# OpenVibe Node

One service for your computers, servers, Raspberry Pis, phones and robots: pair it once with OpenVibe and products
like **OpenVibe.Bot** (robots: drive, cameras, sensors, with safety) and, later, **OpenVibe.Actor** (computer control
for AI agents) can use the device. It connects out, needs no open ports, and keeps a local kill switch.

```sh
curl -fsSL https://openvibe.bot/install | sh -s -- --robot rob_… --code ABCD-1234 --driver adeept
```

openvibe.bot shows this command with the robot's id and a fresh code. `--driver` is `adeept`, `adeept-mecanum`, `cozmo`
or `none` (dry run; the old `--robot adeept` form still works but is deprecated). A robot paired through
OpenVibe.Network gets `--network <URL> --pairing pair_… --code ABCD-1234` instead of `--robot`. Details, manual
install, Windows and configuration: [docs/install.md](docs/install.md).

```sh
openvibe-node status     # link, plugins, video, battery, stop latch
openvibe-node stop       # kill switch: stop every motor now and keep it stopped
openvibe-node resume
```

## What is in here

- **Core** (Go, one static binary per platform): pairing, one outbound WebSocket to `wss://openvibe.bot/device`
  (credential in the `Authorization` header only), heartbeats and a deadman, the safety rules (deadlines, owner limits
  clamped before any driver sees a value, a persisted e-stop, the local kill switch), the plugin supervisor, WHIP video
  publishing with pion, and the system service (systemd, launchd, Windows).
- **Plugins** (separate processes, JSON lines over stdin/stdout): [`dryrun`](plugins/dryrun), the Adeept 4WD Smart Car
  Kit [`adeept_adr036`](plugins/adeept_adr036/README.md), and [`cozmo`](plugins/cozmo/README.md) through PyCozmo. Each
  plugin stops its own actuators when stdin closes, at a command's deadline and after 1 s without a heartbeat, so the
  core dying never leaves motors running.
- **ESP32 library** ([`esp32/`](esp32/README.md), `OpenVibeNode`): the device side of the protocol for ESP32 boards —
  pairing, the control link, drive/actuator commands, telemetry, heartbeat, deadman and e-stop — but no jobs or video.

Docs: [protocol](docs/protocol.md) (the wire format with OpenVibe.Bot), [plugins](docs/plugins.md) (the driver
protocol, the bundled drivers, and where Actor's computer-control plugin attaches), [install](docs/install.md).
Decisions: [ADR-043](docs/ADR-043-bot-devices-and-control.md).

## Development

```sh
go vet ./... && go test -race ./...                    # the end-to-end tests run the Python dry-run plugin
pip install pytest pillow numpy
for p in sdk dryrun adeept_adr036 cozmo; do python3 -m pytest -q plugins/$p/tests; done
scripts/dist.sh v0.0.0-dev                             # every binary, the plugin bundle and SHA256SUMS in dist/
```

`internal/fakebot` is a fake OpenVibe.Bot (pairing, device WebSocket, WHIP) used by the tests. Licence: AGPL-3.0
([LICENSE](LICENSE)). Security reports: [SECURITY.md](SECURITY.md).
