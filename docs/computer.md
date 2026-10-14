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

| level     | screen, list, stat, read (inside the folders) | run programs, write files (inside the folders) | outside the folders |
|-----------|----------------------------------------------|-----------------------------------------------|---------------------|
| `observe` | yes                                          | no (`nack not_allowed`)                       | no                  |
| `ask`     | yes                                          | after you approve each one on the machine     | no                  |
| `trusted` | yes                                          | yes                                           | no                  |
| `full`    | yes                                          | yes                                           | yes                 |

`observe` is the default. Folders are `roots` (default: the home folder of the account running the Node); a path is
checked after symlinks are resolved, so a link out of a folder is outside it.

**Approvals** (level `ask`): a request waits on the machine until you answer it, for at most `approval_timeout_s`
(default 120 s) and never longer than the request's own deadline:

```sh
openvibe-node approvals          # what waits: id, kind, what it would do
openvibe-node approve <ID>       # let it run
openvibe-node deny <ID>          # refuse it
```

When the Node runs in your graphical session, a waiting request also shows a desktop notification (`notify-send` on Linux, `osascript` on macOS; config `notify: false` turns it off). At most 8 requests wait at once. The panel on openvibe.bot sees `approval` and `approval_done` events, so it can say
that a request is waiting on the machine.

**Kill switch:** `openvibe-node stop` (and an owner's e-stop) drops the plugin to `observe`: what runs is killed, what
waits is denied, and the core also refuses `exec` and file writes until `openvibe-node resume`. `halt` kills what runs.

## What it does

| kind     | value                                                                                         | answer (`ack.data`) |
|----------|-----------------------------------------------------------------------------------------------|---------------------|
| `exec`   | `argv` (a program and its arguments: never a shell line), `cwd`, `timeout_s`, `stdin` (≤ 64 KB) | `exit_code`, `stdout`, `stderr` (each cut at `max_output_bytes`), `truncated`, `timed_out`, `duration_ms` |
| `fs`     | `op` `list` / `stat` / `read` (`offset`, `length`) / `write` (`content_b64`, `mode` `create`, `overwrite` or `append`) / `mkdir`, and an absolute `path` | `entries` (≤ 1000), the stat, `content_b64` (≤ `max_read_bytes`), `written`, `created` |
| `screen` | `max_width` (default 1280)                                                                    | `format` (`jpeg` with Pillow, else `png` ≤ 300 KB), `width`, `height`, `content_b64` |

A program runs in its own process group with the Node account's rights, at most 2 at a time, and is killed at
`timeout_s` (the owner's `timeout_s` is the cap). A write with `overwrite` replaces the file atomically. Screens come
from `screencapture` (macOS), `grim` (Wayland) or `import`, `scrot` or `gnome-screenshot` (X11), so the Node must run in
your graphical session for `screen`; without one the plugin simply does not offer it.

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
| `notify`             | `true`         | a desktop notification when a request waits (graphical session only) |

`describe` reports `terminal`, `files` and `screen` with the level and folders, so Bot shows what the machine allows;
telemetry carries the sensors `computer_level` (observe while a stop is latched), `computer_waiting` and
`computer_running`.
Every request is logged by the Node (`journalctl -u openvibe-node`), with what it would do, approved or denied.
