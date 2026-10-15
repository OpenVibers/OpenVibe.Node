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
| `allow_browser`      | `true`         | `false`: no browser (no `browser` capability) |
| `browser_path`       | found on PATH  | the browser to drive (Chromium, Chrome, Edge or Brave) |
| `browser_profile`    | `~/.local/share/openvibe-node/browser` | its profile folder: the agent's own, unless you choose another |
| `browser_headless`   | `true`         | `false`: show its window |
| `browser_idle_s`     | 600            | close it after this long without a command |

`describe` reports `terminal`, `files`, `screen`, `input` and `browser` with the level and folders, so Bot shows what the machine allows;
telemetry carries the sensors `computer_level` (observe while a stop is latched), `computer_waiting` and
`computer_running`.
Every request is logged by the Node (`journalctl -u openvibe-node`), with what it would do, approved or denied.
