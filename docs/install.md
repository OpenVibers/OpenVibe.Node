# Installing the OpenVibe Node

The Node is one program, `openvibe-node`, plus Python plugins for robots. It connects **out** to OpenVibe (no open
ports, works behind any home router), and a local kill switch stops everything it drives.

## Quick start (Linux, Raspberry Pi, macOS)

1. On openvibe.bot, add a robot. You get a code like `ABCD-1234` (valid 10 minutes).
2. On the device:

   ```sh
   curl -fsSL https://openvibe.bot/install | sh -s -- ABCD-1234 --robot adeept
   ```

   `--robot` is `adeept` (Adeept 4WD Smart Car, ordinary wheels), `adeept-mecanum`, `cozmo`, or `none` (the dry-run
   plugin with a test-pattern camera, for trying the Node on a laptop).
3. Confirm the new device on openvibe.bot.

The installer detects the OS and CPU (linux amd64/arm64/armv7, macOS amd64/arm64), downloads the binary and the plugin
bundle and checks them against `SHA256SUMS`, creates a Python virtual environment for the plugins, writes
`config.json`, disables the Adeept kit's stock server if it finds it, creates the `openvibe-node` service account
(Linux; with the `gpio`, `i2c`, `spi`, `video`, `audio` groups), installs and starts the service, and pairs. Running it
again upgrades and keeps the credential. Options: `--no-service`, `--version vX.Y.Z`, `--local DIR` (install from a
directory holding the release files). `OPENVIBE_NODE_BASE` overrides the download location, `OPENVIBE_SERVER` the
server.

Requirements: `python3` with `venv` (`sudo apt install python3 python3-venv`), `curl` or `wget`. For the Adeept camera,
`rpicam-apps` (preinstalled on Raspberry Pi OS). For Cozmo's voice, `espeak-ng`. `ffmpeg` is optional (better video
from plugins that send JPEG frames).

## By hand

```sh
sudo install -m 755 openvibe-node-linux-arm64 /usr/local/bin/openvibe-node
sudo openvibe-node pair ABCD-1234        # stores /etc/openvibe-node/credential.json (mode 600)
sudo openvibe-node install               # systemd / launchd / Windows service, starts it
openvibe-node status
```

Without root, everything goes to your user config directory (`~/.config/openvibe-node`), or to any directory with
`--home DIR` / `OPENVIBE_NODE_HOME`:

```sh
openvibe-node --home ~/node pair ABCD-1234
openvibe-node --home ~/node run --dry-run      # foreground; Ctrl-C stops it
```

**Windows**: download `openvibe-node-windows-amd64.exe`, then in an Administrator prompt `openvibe-node pair <CODE>`
and `openvibe-node install` (the service runs as LocalSystem; files go to `%ProgramData%\OpenVibe Node`). Robot plugins
need Python; set `"python"` in `config.json`.

## Commands

| command                                   | what it does                                                               |
|-------------------------------------------|----------------------------------------------------------------------------|
| `openvibe-node pair <CODE> [--force]`     | redeem a pairing code; `--server`, `--kind onboard\|bridge` override config |
| `openvibe-node run [--dry-run]`           | run in the foreground (what the service runs)                              |
| `openvibe-node install [--user NAME]`     | install and start the service                                              |
| `openvibe-node uninstall`                 | stop and remove the service (config and credential are kept)               |
| `openvibe-node status [--json]`           | link, stop latch, limits, plugins, video, battery                          |
| `openvibe-node stop`                      | **kill switch**: stop every actuator now and hold it stopped              |
| `openvibe-node resume`                    | release the local stop (and a server e-stop)                               |
| `openvibe-node plugins [--json]`          | plugins and their capabilities                                             |

`stop` works even when the Node does not answer: it latches `latch.json` in the state directory, which a running Node
obeys within a quarter second and a starting Node obeys before anything moves. The latch survives restarts.

## Files

| Linux                                 | macOS / Windows                                       | contents                          |
|---------------------------------------|-------------------------------------------------------|-----------------------------------|
| `/etc/openvibe-node/config.json`      | `…/OpenVibe Node/config.json`                         | configuration                     |
| `/etc/openvibe-node/credential.json`  | `…/OpenVibe Node/credential.json`                     | device credential (mode 600)      |
| `/var/lib/openvibe-node/latch.json`   | `…/OpenVibe Node/latch.json`                          | e-stop / kill switch latch        |
| `/var/lib/openvibe-node/node.sock`    | `…/OpenVibe Node/node.sock`                           | local control socket (mode 600)   |
| `/var/lib/openvibe-node/venv`         | `…/OpenVibe Node/venv`                                | plugin Python environment         |

macOS: `/Library/Application Support/OpenVibe Node`. Windows: `%ProgramData%\OpenVibe Node`.

## Configuration

```json
{
  "server": "https://openvibe.bot",
  "device_kind": "onboard",
  "plugins": [{"name": "adeept_adr036", "config": {"backend": "real", "wheels": "ordinary"}}],
  "video": {"source": "auto"},
  "limits": {"max_speed": 0.6, "max_turn": 0.5, "max_command_ms": 500},
  "log_level": "info"
}
```

- `device_kind`: `onboard` or `bridge`.
- `plugins`: see [plugins.md](plugins.md#configuration).
- `video.source`: `auto` (the plugin's camera: its `h264_command` if the program exists, else its JPEG frames, else the
  test pattern if it asks for one), `test`, `command` (with `video.command`: a program writing H.264 Annex B to stdout,
  e.g. `["ffmpeg", "-f", "v4l2", "-i", "/dev/video0", "-c:v", "libx264", "-preset", "ultrafast", "-tune",
  "zerolatency", "-profile:v", "baseline", "-g", "30", "-f", "h264", "-"]`), `plugin` (with `video.plugin`), `off`.
  `fps`, `width`, `height`, `ffmpeg` tune the rest.
- `limits`: local caps on top of the owner's limits from openvibe.bot; the stricter value wins.
- `python`: interpreter for bundled plugins (default `<state>/venv/bin/python`, then `python3`).

Unknown keys are rejected, so a typo does not silently change behaviour.

## Robots

### Adeept 4WD Smart Car Kit for Raspberry Pi (ADR036)

Raspberry Pi OS Bookworm (64-bit recommended). Enable I²C and SPI: `sudo raspi-config` → Interface Options, or
`dtparam=i2c_arm=on` and `dtparam=spi=on` in `/boot/firmware/config.txt`, then reboot. Install with
`--robot adeept` (or `adeept-mecanum`).

The kit's own software (`Adeept_Robot.service`, `WebServer.py` on `0.0.0.0:8888` with the fixed login
`admin:123456`, MJPEG on `:5000`) must not run: anyone on the network could drive the car. The installer disables it;
check with `systemctl status Adeept_Robot.service`. The Node replaces it.

If a direction is wrong on your car, fix it in `config.json` → `plugins[0].config`: `motors.<wheel>.direction`,
`pan.invert`, `tilt.invert`, `line.invert`. See [the plugin README](../plugins/adeept_adr036/README.md).

### Cozmo (bridge)

Cozmo makes its own Wi-Fi network without internet. Use a Raspberry Pi or laptop with two network interfaces: one
joined to Cozmo's network (`Cozmo_…`; raise and lower the lift to see the password on its face), the other to the
internet (Ethernet, or a second Wi-Fi adapter). Install with `--robot cozmo`; the device kind is `bridge`. See
[the plugin README](../plugins/cozmo/README.md).

## Troubleshooting

- `openvibe-node status` shows the last link error. "the server refused the device credential" means it was revoked:
  `sudo openvibe-node pair --force <NEW-CODE>`.
- Logs: `journalctl -u openvibe-node -f` (Linux), `/var/log/openvibe-node.*.log` (macOS), Event Viewer (Windows).
- A plugin in `faulted` state says why in `status` (missing library, not a Raspberry Pi, robot not reachable, firmware).
- `openvibe-node plugins` works without the service running: it asks each plugin to describe itself without touching
  hardware.

## Uninstall

```sh
sudo openvibe-node uninstall
sudo rm -rf /usr/local/bin/openvibe-node /etc/openvibe-node /var/lib/openvibe-node
sudo userdel openvibe-node
```

Then remove the device on openvibe.bot, which revokes its credential.
