# The computer plugin: your machine for your agents

The `computer` plugin gives the machine's terminal, files and screen to its owner's agents (OpenVibe.Actor, plan
T14/T17), through OpenVibe.Bot and the Node's one outbound link. It is **off** unless you turn it on, it never runs
as root, and **you set what it may do on the machine itself**: nothing in the cloud can raise that level.

## Turn it on

On Linux, from your own account (the installer uses sudo itself):

```sh
curl -fsSL https://openvibe.bot/install | sh -s -- --network <URL> --pairing pair_… --code ABCD-1234 --computer ask
```

`--computer LEVEL` runs the Node's service as **your** account (never root, never the `openvibe-node` system account)
and adds the plugin with your home folder as its only folder. It cannot share a machine with `--worker` yet (the job
worker's service runs as root).

By hand, or on another system, add it to `plugins` in the Node's `config.json` and run the Node as your own account
(`openvibe-node run` in your session, or `openvibe-node install --user <you>`):

```json
{"name": "computer", "config": {"level": "ask", "roots": ["/home/you/projects"]}}
```

## Control levels

| level     | screen, list, stat, read (inside the folders) | run programs, write files (inside the folders), mouse and keyboard | outside the folders |
|-----------|----------------------------------------------|--------------------------------------------------------------------|---------------------|
| `observe` | yes                                          | no (`nack not_allowed`)                                            | no                  |
| `ask`     | yes                                          | after you approve each one on the machine                          | no                  |
| `trusted` | yes                                          | yes                                                                | no                  |
| `full`    | yes                                          | yes                                                                | yes                 |

`observe` is the default. Folders are `roots` (default: the home folder of the account running the Node); a path is
checked after symlinks are resolved, so a link out of a folder is outside it.

**Approvals** (level `ask`): a request waits on the machine until you answer it, for at most `approval_timeout_s`
(default 120 s) and never longer than the request's own deadline:

```sh
openvibe-node approvals          # what waits: id, kind, what it would do
openvibe-node approve <ID>       # let it run
openvibe-node deny <ID>          # refuse it
```

**The tray** (Linux and Windows): `openvibe-node tray` puts the Node in your notification area. The installer's
`--computer` starts it when you sign in (`openvibe-node tray --autostart on|off` does it by hand). It runs in your
desktop session, not in the service, and talks to the Node over the same local socket as the commands above:

- the icon is blue when all is well, amber while a request waits, red while a stop is latched, grey when the Node does
  not answer;
- the menu lists what waits (kind and what it would do), each with **Approve** and **Deny**, then **Stop everything**
  (the kill switch), **Resume** and your machines page on openvibe.bot;
- a new request raises a notification with **Approve** and **Deny** buttons (Linux, any desktop with
  org.freedesktop.Notifications; without buttons it names the command to answer with).

KDE, XFCE, Cinnamon and MATE show tray icons; GNOME needs the AppIndicator extension, and its notifications still ask.
While the tray runs, the plugin's own prompts below stay quiet.

**Without the tray**, when the Node runs in your graphical session, the plugin asks itself (config `notify: false` turns
it off): on Linux a notification with Approve and Deny buttons (`notify-send` from libnotify 0.7.10 on; older ones show
the command to answer with); on macOS a dialog with **Deny** as its default button, which gives up when the request
expires. A prompt goes away once the request is answered anywhere else, expires or is stopped.

**An approval is not taken while the agent drives the mouse or keyboard**, nor for 2 seconds after: an approved click
must not be able to press another request's Approve button. `openvibe-node approve` then answers "answer again in a
moment", and the tray and the plugin ask again. Deny always goes through.

At most 8 requests wait at once. The panel on openvibe.bot sees `approval` and `approval_done` events, so it can say
that a request is waiting on the machine.

**Kill switch:** `openvibe-node stop` (and an owner's e-stop) drops the plugin to `observe`: what runs is killed, what
waits is denied, the agent's browser is closed, and the core also refuses `exec`, file writes, `input` and the browser's
acting actions until `openvibe-node resume`. `halt` kills what runs.

## What it does

| kind     | value                                                                                         | answer (`ack.data`) |
|----------|-----------------------------------------------------------------------------------------------|---------------------|
| `exec`   | `argv` (a program and its arguments: never a shell line), `cwd`, `timeout_s`, `stdin` (≤ 64 KB) | `exit_code`, `stdout`, `stderr` (each cut at `max_output_bytes`), `truncated`, `timed_out`, `duration_ms` |
| `fs`     | `op` `list` / `stat` / `read` (`offset`, `length`) / `write` (`content_b64`, `mode` `create`, `overwrite` or `append`) / `mkdir`, and an absolute `path` | `entries` (≤ 1000), the stat, `content_b64` (≤ `max_read_bytes`), `written`, `created` |
| `probe`  | `target`: `http(s)://…` (one GET, no redirect followed) or `tcp://host:port`; `timeout_s` | `status`, `content_type`, `etag`, `last_modified`, `body` (≤ 64 KB), `ms`; or `open`, `ms` |
| `screen` | `max_width` (default 1280)                                                                    | `format` (`jpeg` with Pillow, else `png` ≤ 300 KB), `width`, `height`, `content_b64` |
| `input`  | `action`: `move`, `click`, `double_click`, `right_click` (`x`, `y`), `scroll` (`dy` clicks, ±20), `type` (`text`, ≤ 2000), `key` (`keys`: one key or chord, `ctrl+s`, `Return`); `width`, `height`: the picture `x`, `y` are measured in | `done`, `action`, and the real `x`, `y` it used |
| `browser` | `action`: `open` (`url`, http or https), `click` (`selector`, or the `text` a button or link shows), `fill` (`selector`, `text` ≤ 2000), `press` (`key`: Enter, Tab, Escape, Backspace, Delete, arrows, PageUp, PageDown, Home, End, Space), `back`, `read`, `screenshot` (`max_width`), `close` | a page: `url`, `title`, `text` (its visible text, ≤ 20,000 characters), `truncated`, `links` (the first 60); `screenshot`: `format`, `width`, `height`, `content_b64`; `fill`: `done`, `chars` |

A program runs in its own process group with the Node account's rights, at most 2 at a time, and is killed at
`timeout_s` (the owner's `timeout_s` is the cap). A write with `overwrite` replaces the file atomically. Screens come
from `screencapture` (macOS), `grim` (Wayland) or `import`, `scrot` or `gnome-screenshot` (X11), so the Node must run in
your graphical session for `screen`; without one the plugin simply does not offer it.

### The live screen (`screen_stream`, Node v0.12.0)

You can watch your machine's screen live on openvibe.bot (**Your machines** → **Show screen**), for 30 seconds to
30 minutes at a time:

- **Only you see it.** Bot gives your machine's start command the WHIP address and key of its own private
  OpenRestream stream (no recording, never on Live). No operator or agent supplies them, and agents cannot start it.
- **How it works.** The plugin captures a JPEG frame with the same tools as `screen`, 1 to 5 times a second (2 by
  default), at most 1600 px wide when Pillow is installed. The core encodes the frames (ffmpeg, else its built-in
  encoder) and publishes them on a WebRTC session of their own, separate from a robot's camera.
- **You are told.** The desktop says when it starts ("Your screen is live on OpenVibe"), and `openvibe-node status`
  shows `Screen: live … until …` while it runs.
- **It ends** when its time is up, when you press Stop, when capturing fails five times in a row, and at once on
  `openvibe-node stop` or an e-stop. While a stop is latched, a new start is refused.
- **Levels.** It runs at every level, like a screenshot. To never offer it, set `allow_screen_stream: false`.
- **Without Pillow** the capture tool must write JPEG itself: `screencapture`, `grim`, `import` and `scrot` do;
  `gnome-screenshot` does not.

**Mouse and keyboard** (`input`) is an acting command like running a program: refused at `observe`, approved one by one
at `ask` (the approval says exactly what it would do: "click at 640, 360 of a 1280×720 picture of the screen",
"type 12 characters: 'hello there'", "press ctrl+s"). Coordinates are in the space of the screen picture the agent last
saw (`width` × `height`), scaled to your real screen. It uses `xdotool` on an X11 session and `cliclick` on macOS
(`brew install cliclick`; no scrolling there); Wayland has no general input tool yet, so it is not offered there. Typed
text and keys are passed to the tool as arguments, never through a shell, and `keys` takes only letters, digits, `+`,
`-` and `_`.

**The browser** (`browser`) is a Chromium-family browser (Chromium, Chrome, Edge or Brave) the agent drives:
- **Its own profile.** It runs in a folder of its own (`browser_profile`, by default `~/.local/share/openvibe-node/browser`), so the agent never sees the sessions of your own browser unless you point it at one. Sign it in to a site yourself (`browser_headless: false` shows its window) if you want the agent to work there as you.
- **No port.** The Node starts it on the first command with `--remote-debugging-pipe`: it speaks the DevTools protocol over two file descriptors, so nothing else on the machine can drive it.
- **Levels.** `read`, `screenshot` and `close` run at every level. `open`, `click`, `fill`, `press` and `back` are acting commands: refused at `observe`, approved one by one at `ask`. The approval says exactly what would happen: "open https://… in the browser", "type 5 characters into #q in the browser: 'hello'".
- **Your own network.** Below level `full`, `open` refuses an address on the machine's own network (private, loopback or link-local, checked for every address the name resolves to).
- **Closing.** It closes after `browser_idle_s` without a command and at the kill switch.
- **Platforms.** Linux and macOS; not offered on Windows yet.

### The private desk (`desk`, Node v0.15.0)

An agent can work on a desk of its own instead of your screen (plan track K: Vibe's agent mode):
- **What it is.** An Xvfb display on your machine with a window manager (metacity, openbox, xfwm4 or fluxbox, whichever
  is installed) and a D-Bus session of its own, so apps opened there open there, not on your screen.
- **Using it.** `desk` `{"action": "start", "width"?, "height"?}` (1280×800 by default), `stop` or `status`. Then
  `screen`, `screen_stream`, `input` and `browser` take `"desk": "private"`: screenshots and the live screen of the desk,
  the mouse and keyboard on the desk (points scale to the desk's own size), and the agent's browser shown on the desk
  in a profile of its own (`browser_profile` + `-desk`). Without `desk` (or with `main`) they act on your own screen,
  as before.
- **Levels.** Starting and stopping a desk are acting commands (refused at `observe`, approved one by one at `ask`);
  `status` runs at every level. What runs on the desk keeps its own rules: input and the browser still act.
- **Watching and taking over.** Start the live screen with `"desk": "private"` (your desktop is not told, since nothing of
  yours is shown) and drive it the same way.
- **It ends** on `stop`, and at once on `openvibe-node stop` or an e-stop: every process the desk started is ended by
  handle, never by name.
- **Needs** Linux with `Xvfb` (Debian/Ubuntu: `apt install xvfb`), `xdotool` for input and ImageMagick `import` or `scrot`
  for screenshots. Not offered elsewhere; `allow_desk: false` turns it off.

## Config

| key                  | default        | meaning |
|----------------------|----------------|---------|
| `level`              | `observe`      | `observe`, `ask`, `trusted` or `full` |
| `roots`              | your home      | the folders it may use (absolute) |
| `allow_exec`         | `true`         | `false`: never run programs (no `terminal` capability) |
| `timeout_s`          | 30 (≤ 600)     | the longest a program runs |
| `max_output_bytes`   | 65536 (≤ 192 KB) | stdout and stderr are each cut there |
| `max_read_bytes`     | 262144 (≤ 256 KB) | the most one read or write moves |
| `approval_timeout_s` | 120 (≤ 600)    | how long a request waits for you |
| `notify`             | `true`         | the plugin's own prompt when a request waits and no tray shows it (graphical session only) |
| `probe`              | `true`         | `false`: no probes (OpenVibe.Watch's `node` source) |
| `allow_input`        | `true`         | `false`: never use the mouse or keyboard (no `input` capability) |
| `allow_desk`         | `true`         | `false`: never start a private desk (no `desk` capability) |
| `allow_browser`      | `true`         | `false`: no browser (no `browser` capability) |
| `allow_screen_stream` | `true`        | `false`: never show the screen live (`screen.stream` is false) |
| `browser_path`       | found on PATH  | the browser to drive (Chromium, Chrome, Edge or Brave) |
| `browser_profile`    | `~/.local/share/openvibe-node/browser` | its profile folder: the agent's own, unless you choose another |
| `browser_headless`   | `true`         | `false`: show its window |
| `browser_idle_s`     | 600            | close it after this long without a command |

`describe` reports `terminal`, `files`, `screen`, `input` and `browser` with the level and folders, so Bot shows what the machine allows;
telemetry carries the sensors `computer_level` (observe while a stop is latched), `computer_waiting`,
`computer_running` and `computer_screen_stream` (`live` or `off`). `screen.stream` says whether the live screen is
offered.
Every request is logged by the Node (`journalctl -u openvibe-node`), with what it would do, approved or denied.
