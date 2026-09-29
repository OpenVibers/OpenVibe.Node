# openvibe-cozmo

OpenVibe Node plugin for Anki / Digital Dream Labs **Cozmo**, built on
[PyCozmo](https://github.com/zayfod/pycozmo) (MIT). It is a *bridge* plugin: Cozmo runs closed firmware and cannot run
the Node itself, so the Node runs on a computer next to the robot and drives it over Wi-Fi.

## Bridge setup

Cozmo is its own Wi-Fi access point (network `Cozmo_XXXXXX`, robot at `172.31.1.1`) and has no internet. The bridge
computer (a Raspberry Pi or a laptop) therefore needs **two network interfaces**:

| interface | joined to | used for |
|-----------|-----------|----------|
| `wlan0` (or the built-in Wi-Fi) | Cozmo's Wi-Fi | PyCozmo's UDP link to the robot |
| Ethernet, or a second Wi-Fi (USB dongle) | your home network | the Node's link to OpenVibe.Bot and video |

1. Put Cozmo on its charger and turn it on. Raise and lower its lift: its face shows the network name and password.
2. Join one interface to that network. Make sure the *default route stays on the internet interface*: Cozmo's network
   has no internet, and some systems prefer the newest Wi-Fi. With NetworkManager:
   `nmcli connection modify Cozmo_XXXXXX ipv4.never-default yes ipv6.never-default yes`.
3. Close the Cozmo app on any phone: the robot accepts one controller at a time.
4. Install the plugin with the robot extra and a speech engine:
   ```sh
   pip install 'openvibe-cozmo[robot]'   # pulls pycozmo
   sudo apt install espeak-ng            # or libttspico-utils (pico2wave)
   ```
5. Add the plugin to the Node's config (`driver: cozmo`) and start the Node. Setup waits `connect_timeout_s` for the
   robot; if it is unreachable the plugin reports fault `not_connected`.

## Firmware

PyCozmo 0.8 speaks the protocol of Cozmo firmware **2381** (the last release of the Cozmo app). At connect the plugin
reads the firmware signature. On another version, or on factory/recovery firmware (`build: FACTORY`), it stays up but
latches fault `firmware_unsupported`: it emits an `event` with the version found and the supported ones, lists the
fault in telemetry, and answers every command except `halt` with that nack. Update the robot with the Cozmo app. If you
know a version works, allow it with `allow_firmware: [2381, <version>]`.

## Commands

| kind | value | effect |
|------|-------|--------|
| `drive` | `{"throttle": -1..1, "steer": -1..1}` | left = throttle + steer, right = throttle - steer (steer > 0 turns right), normalised, times `max_wheel_mmps`; stops at the deadline |
| `ptz` | `{"tilt": -1..1}` | head angle, -1 = -25°, 1 = 44.5° (no pan: turn with `drive`) |
| `actuator` | `{"name": "head", "value": -1..1}` | same as tilt |
| `actuator` | `{"name": "lift", "value": 0..1}` | lift height, 0 = 32 mm (down), 1 = 92 mm (up) |
| `actuator` | `{"name": "backpack_lights", "value": {"r","g","b"}}` | all five backpack LEDs (0..255 each; `null` = off) |
| `actuator` | `{"name": "head_light", "value": true}` | the infrared head light |
| `actuator` | `{"name": "cube_lights", "value": {"r","g","b"}, "cube": id?}` | every connected cube, or one object id |
| `say` | `{"text": "..."}` | espeak-ng / pico2wave, played on Cozmo's speaker (ack = queued) |
| `display` | `{"text"}` / `{"face": "neutral"\|"happy"\|"sad"\|"surprised"\|"sleepy"\|"angry"}` / `{"image_png_b64"}` | 128x32 1-bit image on the face screen |

Cliff and pick-up: PyCozmo reports them on its own thread; the plugin stops the wheels in that callback, then stops
every motor through the runtime and emits `cliff` / `picked_up` (and `cliff_cleared` / `picked_up_cleared`). While a
cliff is seen, drive commands that would turn either wheel forward get nack `cliff` (backing away is allowed); while the
robot is picked up every drive gets nack `picked_up`. Below `low_battery_v` the plugin emits `low_battery` once.

Camera frames (320x240, grey unless `color: true`) are written as JPEG to `frame_dir` (a temp dir by default) through
a temp file and `os.replace`, and announced with `{"op":"video","format":"jpeg","path":...,"seq":n}`.

Telemetry: `battery` (`volts`, `percent` between 3.5 V and 4.2 V), `sensors` (`head_deg`, `lift_mm`, `cliff`,
`picked_up`, `cubes`), `faults`.

## Config

| key | default | |
|-----|---------|---|
| `connect_timeout_s` | 10 | how long setup waits for the robot |
| `robot_addr` | PyCozmo's (`["172.31.1.1", 5551]`) | |
| `allow_firmware` | `[2381]` | firmware versions accepted |
| `max_wheel_mmps` | 150 | capped at 200 (Cozmo's maximum) |
| `wheel_accel_mmps2` | 0 | 0 = firmware default |
| `camera` / `color` / `fps` / `jpeg_quality` / `frame_dir` | true / false / 10 / 70 / temp dir | |
| `max_say_chars` | 200 | longer text is cut |
| `voice`, `speech_rate`, `pico_lang` | -, 150, `en-US` | TTS options |
| `volume` | robot's | 0..1 |
| `low_battery_v` | 3.6 | |
| `connect_cubes` | true | connect light cubes PyCozmo hears |
| `procedural_face` | false | let PyCozmo animate the face (it overrides `display`) |

## Tests

The tests use a fake PyCozmo client (`tests/fake_pycozmo.py`) and need neither PyCozmo nor a robot:

```sh
python3 -m pytest -q plugins/cozmo/tests      # from the repository root; needs pytest and pillow
```

To try the plugin by hand against a robot, run `python3 -m openvibe_cozmo` and type JSON lines, starting with
`{"op":"hello","config":{}}` and sending `{"op":"heartbeat"}` at least every second.
