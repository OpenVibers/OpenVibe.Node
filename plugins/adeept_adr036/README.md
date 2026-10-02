# openvibe-adeept-adr036

OpenVibe Node plugin for the **Adeept 4WD Smart Car Kit for Raspberry Pi (ADR036)**, ordinary or mecanum wheels,
on Raspberry Pi OS Bookworm. Built on `openvibe_plugin` (deadlines, heartbeat loss, stdin EOF, e-stop and halt all stop
the wheels). The mapping was checked against Adeept's `Server/Server_*Wheels/Move.py` and `Voltage.py`; none of the
kit's code is imported.

## Keep the kit's stock server disabled

The kit's own server listens on `0.0.0.0:8888` with the fixed login `admin` / `123456` and streams MJPEG on `:5000`.
Anyone on the network could drive the car with it. The installer disables its autostart in root's and the invoking
user's crontabs and `/etc/rc.local`. From a root shell, it checks `pi`'s crontab when that account exists. It comments
out the kit's lines with the marker `#openvibe-node-disabled:` and keeps the original text, so running the installer
again changes nothing. Check with `systemctl status Adeept_Robot.service`, `crontab -l` (and
`sudo crontab -l -u pi` for a root-shell install) and
`grep openvibe-node-disabled /etc/rc.local`. Do not run it next to the Node. This plugin never starts it.

## Wiring (kit defaults)

| Part | Connection |
|---|---|
| Motors (PCA9685 at I2C `0x5f`, 50 Hz) | M1 front-left ch 15/14, M2 rear-left 12/13, M3 rear-right 11/10, M4 front-right 8/9 |
| Pan / tilt servos | PCA9685 ch 0 / ch 1, 500–2400 µs, 180° |
| Ultrasonic | echo GPIO 24, trigger GPIO 23 |
| Line tracking | left GPIO 22, middle 27, right 17 |
| Battery | ADS7830 at I2C `0x48`, channel 0, 3k/1k divider |
| Buzzer | GPIO 18 (gpiozero `TonalBuzzer`) |
| WS2812 LEDs | SPI 0.0 (MOSI, GPIO 10) |
| Camera | published by the core with `rpicam-vid` |

Enable I2C and SPI (`raspi-config`), then `pip install './plugins/sdk' './plugins/adeept_adr036[pi]'`.

## Config (`hello.config`, deep-merged over the defaults)

| Key | Default | Meaning |
|---|---|---|
| `backend` | `"auto"` | `auto` = real on a Raspberry Pi, a `hardware` fault elsewhere; `real`; `fake` (records writes, moves nothing) |
| `i2c_address` | `0x5f` | PCA9685 address |
| `pwm_frequency` | `50` | PCA9685 frequency (shared by motors and servos) |
| `reassert_frequency` | `true` | re-set the frequency before each drive update, as the kit does |
| `wheels` | `"ordinary"` | `ordinary` (differential) or `mecanum` |
| `max_throttle` | `1.0` | scales every wheel |
| `motors.<front_left\|rear_left\|rear_right\|front_right>` | `{"channels": [in1, in2], "direction": -1}` | see wiring; direction -1 = kit forward is negative throttle |
| `servo_pulse` | `{"min_pulse": 500, "max_pulse": 2400, "actuation_range": 180}` | |
| `servos.pan` | `{"channel": 0, "min_deg": 10, "max_deg": 170, "center_deg": 90, "invert": false}` | |
| `servos.tilt` | `{"channel": 1, "min_deg": 30, "max_deg": 150, "center_deg": 90, "invert": false}` | |
| `ultrasonic` | `{"echo": 24, "trigger": 23, "max_distance_m": 2.0}` | |
| `line` | `{"left": 22, "middle": 27, "right": 17, "invert": false}` | |
| `battery` | `{"i2c_bus": 1, "address": 0x48, "channel": 0, "adc_vref": 5.2, "r_top": 3000, "r_bottom": 1000, "full_volts": 8.4, "warning_volts": 6.0, "median_window": 5}` | `low_battery` event once below the warning |
| `buzzer` | `{"pin": 18, "octaves": 1, "max_seconds": 2.0}` | tones A4 ± octaves; auto-off after `max_seconds` |
| `lights` | `{"count": 8, "spi_bus": 0, "spi_device": 0, "order": "GRB", "speed_hz": 6400000}` | |
| `camera` | `{"width": 640, "height": 480, "framerate": 20}` | used in the `h264_command` the core runs |

## Commands

* `drive` (the only motion kind): ordinary `{"throttle", "steer"}` (steer + turns right); mecanum
  `{"x", "y", "rotation"}` (x forward, y left, rotation counter-clockwise) or throttle/steer.
* `ptz` `{"pan": -1..1, "tilt": -1..1}`: absolute position, 0 = center, ±1 = the configured limits.
* `actuator`: `pan`, `tilt` (-1..1), `buzzer` (`{"note": "A4"}`, `{"hz": 440}`, `null`/`0` = off),
  `lights` (`{"r", "g", "b"[, "index"]}`, `null` = off).
* `stop` / `estop` / `halt`: wheels to 0 and buzzer off; servos hold their position.

Telemetry (2 Hz): `battery {volts, percent, low}`, `sensors {distance_cm, line {left, middle, right}}` and `faults`
(sensors that failed to open or read; driving keeps working). A PCA9685 failure is a `hardware` fault at setup.

## Test

```
python3 -m pytest -q plugins/adeept_adr036/tests
```

The tests use the fake backend (no Pi needed). To try the plugin by hand:
`PYTHONPATH=plugins/sdk:plugins/adeept_adr036 python3 -m openvibe_adeept_adr036` and send
`{"op":"hello","config":{"backend":"fake"}}`. Set `OPENVIBE_FAKE_HW_LOG=/path/file.jsonl` to log every fake write.
