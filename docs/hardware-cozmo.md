# Anki / Digital Dream Labs Cozmo — bridge bring-up (O31)

Cozmo runs closed firmware, makes its own Wi-Fi access point with no internet, and cannot run the Node itself. The
device side is therefore a **bridge**: `openvibe-node` with the `cozmo` plugin runs on a computer next to the robot —
a Raspberry Pi or a laptop — and drives it over Wi-Fi through PyCozmo. The bridge joins Cozmo's network on one
interface and reaches OpenVibe on another. Bot serves the one-paste installer; the panel is
`openvibe.bot/panel/<rob_…>`.

## 1. Two interfaces (the bridge computer)

Cozmo is an access point (`Cozmo_XXXXXX`, robot at `172.31.1.1`) and has no internet, so the bridge computer needs
**two network interfaces**:

| interface | joined to | used for |
|-----------|-----------|----------|
| `wlan0` (or the built-in Wi-Fi) | Cozmo's Wi-Fi | PyCozmo's UDP link to the robot |
| Ethernet, or a second Wi-Fi (USB dongle) | your home network | the Node's link to OpenVibe.Bot and video |

1. Put Cozmo on its charger and turn it on. Raise and lower its lift: its face shows the network name and password.
2. Join one interface to that network. Make sure the **default route stays on the internet interface**: Cozmo's network
   has no internet, and some systems prefer the newest Wi-Fi. With NetworkManager:

   ```sh
   nmcli connection modify Cozmo_XXXXXX ipv4.never-default yes ipv6.never-default yes
   ```

3. Close the Cozmo app on any phone: the robot accepts one controller at a time.
4. The installer `pip install`s the plugin with the robot extra (`openvibe-cozmo[robot]`, which pulls PyCozmo) into the
   Node's plugin virtual environment. It only *tips* about speech — install a TTS engine yourself:

   ```sh
   sudo apt install espeak-ng            # or libttspico-utils (pico2wave)
   ```

## 2. Pair (one pasted command from openvibe.bot)

```sh
curl -fsSL https://openvibe.bot/install | sh -s -- --robot rob_… --code XXXX-XXXX --driver cozmo
```

Network pairing, if openvibe.bot offers the Network form:

```sh
curl -fsSL https://openvibe.bot/install | sh -s -- --network https://openvibe.network --pairing pair_… --code XXXX-XXXX --driver cozmo
```

Then check the written config (`cat /etc/openvibe-node/config.json`): `device_kind` is `bridge`, `plugins` names
`cozmo`, and `video.source` is `auto` (the plugin supplies JPEG camera frames). The config has no `video.whip_url`: the
ingest URL + publish key the pairing returned is stored in `/etc/openvibe-node/credential.json`, and video publishes
there unless a `video.whip_url` in the config overrides it. Setup waits `connect_timeout_s` (default 10 s) for the
robot; if it is unreachable the plugin reports fault `not_connected`. `openvibe-node status --json` should show
`link.connected: true`, `sensors` (`head_deg`, `lift_mm`, `cliff`, `picked_up`, `cubes`) and `video.state` at
`connecting` or `live`.

## 3. Firmware

PyCozmo 0.8 speaks the protocol of Cozmo firmware **2381** (the last release of the Cozmo app). At connect the plugin
reads the firmware signature. On another version, or on factory/recovery firmware (`build: FACTORY`), it stays up but
**latches fault `firmware_unsupported`**: it emits an `event` with the version found and the supported ones, lists the
fault in telemetry, and answers every command except `halt` with that nack. Update the robot with the Cozmo app. If you
know a version works, allow it with `allow_firmware: [2381, <version>]` in the plugin config.

## 4. Panel checklist

Drive from openvibe.bot; watch `openvibe-node status --json` for telemetry.

| Subsystem | Expected | Pass |
|---|---|---|
| Drive (`drive` `{"throttle": -1..1, "steer": -1..1}`) | left = throttle + steer, right = throttle − steer (steer > 0 turns right), × `max_wheel_mmps`; wheels stop at the deadline and on `openvibe-node stop` | |
| Head (`ptz` `{"tilt": -1..1}` / actuator `head`) | −1 = 25° down → +1 = 44.5° up (no pan: turn with `drive`) | |
| Lift (actuator `lift` 0..1) | 0 = 32 mm down → 1 = 92 mm up | |
| Say (`say` `{"text"}`) | espeak-ng / pico2wave played on Cozmo's speaker (ack = queued) | |
| Display (`display` text / `{"face": …}` / `{"image_png_b64"}`) | text, any of the six faces, and a PNG on the 128 × 32 1-bit screen | |
| Backpack lights (actuator `backpack_lights` `{"r","g","b"}`, `null` = off) | all five LEDs change | |
| Cube lights (actuator `cube_lights` `{"r","g","b"}, "cube": id?`) | every connected cube, or one by object id | |
| Cliff (`sensors.cliff`, event `cliff` / `cliff_cleared`) | wheels stop in the callback; forward drives nack `cliff`, backing away allowed | |
| Pick-up (`sensors.picked_up`, event `picked_up` / `picked_up_cleared`) | every drive nack `picked_up`; all motors stopped | |
| Battery (`battery.volts`, `percent` 3.5–4.2 V) | sane pack voltage; `low_battery` event once below `low_battery_v` | |
| Camera (320 × 240 JPEG files) | panel shows video; `color: true` for colour (grey by default) | |

## 5. If it will not connect or shows no video

- **No telemetry / `not_connected`**: confirm the second interface is joined to `Cozmo_XXXXXX`, the default route is
  still on the internet NIC (`ip route`), the Cozmo app is closed everywhere, and the robot is on its charger.
- **No video**: the plugin writes JPEG frames to `frame_dir` (a temp dir by default) and announces each as
  `{"op":"video","format":"jpeg","path":…,"seq":n}`; the core publishes them. Check `openvibe-node status --json` for
  `video.state` / `video.last_error` and journalctl for `openvibe-node`. To check the WHIP path without the camera, set
  `"video": {"source": "test"}` in `/etc/openvibe-node/config.json` and restart: the panel then shows the test pattern.
- **A command answers `nack`**: read the fault — `firmware_unsupported` is the firmware latch above; `cliff` and
  `picked_up` are the safety stops in the checklist.
