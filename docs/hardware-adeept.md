# Adeept ADR036 4WD Smart Car — hardware bring-up (O30)

The device side is the OpenVibe Node (`openvibe-node`) with the `adeept_adr036` plugin. Bot serves the one-paste
installer; the panel is `openvibe.bot/panel/<rob_…>`. Nothing here runs the kit's stock control server — the Node
replaces it, and the installer disables the kit's own autostart (`docs/install.md`).

## 1. On the Pi (Raspberry Pi OS Bookworm, 64-bit)

- `sudo raspi-config` → Interface Options: enable **I2C** and **SPI**. Reboot. (Or set `dtparam=i2c_arm=on` and
  `dtparam=spi=on` in `/boot/firmware/config.txt`.)
- Confirm the kit's stock control server is not running: `systemctl is-enabled Adeept_Robot.service` must report
  `disabled` (or the unit absent), and ports 8888/5000 must be free. `install/install.sh` stops and disables
  `Adeept_Robot.service`, kills a running `WebServer.py`/`APPServer.py` process, and comments out the kit's autostart
  lines in the invoking user's crontab, root's crontab and `/etc/rc.local` (prefixing them with
  `#openvibe-node-disabled:`). If it is still on, stop and disable it before pairing.
- Python 3 + venv: the installer creates a plugin virtual environment in the Node's state dir
  (`/var/lib/openvibe-node/venv` on Linux, `--system-site-packages`) and `pip install`s the plugin bundle there. No
  manual `pip install` is needed.

## 2. Pair (one pasted command from openvibe.bot)

```sh
curl -fsSL https://openvibe.bot/install | sh -s -- --robot rob_… --code XXXX-XXXX --driver adeept
```

Network pairing, if openvibe.bot offers the Network form:

```sh
curl -fsSL https://openvibe.bot/install | sh -s -- --network https://openvibe.network --pairing pair_… --code XXXX-XXXX --driver adeept
```

Then check the written config (`cat /etc/openvibe-node/config.json`): `plugins` names `adeept_adr036` with backend
`real` and `wheels: "ordinary"`, and `video.whip_url` is set (the ingest URL + publish key the pairing returned; O30:
`https://ingest.openre.stream/whip/<key>`). `openvibe-node status --json` should show `link.connected: true` and
`video.state` at `connecting` or `live`.

## 3. Panel checklist

Drive from openvibe.bot; watch `openvibe-node status --json` for telemetry.

| Subsystem | Expected | Pass |
|---|---|---|
| Motors (PCA9685 `0x5f`, 50 Hz; M1–M4 = 15/14, 12/13, 11/10, 8/9) | forward/back + steer; wheels stop at the 300 ms deadline and on `openvibe-node stop` | |
| Servos pan ch0 / tilt ch1 (500–2400 µs) | reach min → centre → max | |
| Battery (ADS7830 `0x48`, 2×18650 6.0–8.4 V) | telemetry `battery.volts` ≈ pack, `percent` sane | |
| Ultrasonic (`DistanceSensor(echo=24, trigger=23)`) | `sensors.distance_cm` changes with a hand | |
| Line tracking (GPIO 22/27/17) | `sensors.line` flips over tape | |
| Buzzer (GPIO 18) / WS2812 (SPI 0.0) | horn + lights widgets work | |
| Camera (`rpicam-vid --codec h264 --inline -o -` → `internal/video/whip.go`) | panel shows live video (needs the Ops-WHIP row) | |
| Safety | kill Wi-Fi / Ctrl-C / panel e-stop → wheels stop; e-stop stays latched through a restart | |

Mecanum chassis: re-run with `--driver adeept-mecanum` (same profile `adeept.adr036`, plugin config `wheels: "mecanum"`).

OLED/display: **not supported** (deferred) — see `plugins/adeept_adr036/README.md`; the profile has no display
widget and a `display` command is nacked.

## 4. If the camera will not start

If `rpicam-vid` is missing, `video.source` `auto` has nothing to fall back to: the Adeept plugin offers only the
H.264 camera command (no JPEG frames, no test pattern), so the core publishes **no video** — `journalctl -u openvibe-node` shows
`camera program not found` and then `no camera to publish`, and `openvibe-node status --json` has no `video.source`.
Install `rpicam-apps` (noted in `docs/install.md`) and `sudo systemctl restart openvibe-node`. To check the WHIP path without the camera,
set `"video": {"source": "test"}` in `/etc/openvibe-node/config.json` and restart: the panel then shows the test
pattern. Report `video.state`, `video.last_error` and the journal lines above.
