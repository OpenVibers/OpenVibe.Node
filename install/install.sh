#!/bin/sh
# OpenVibe Node installer.
#
#   curl -fsSL https://openvibe.bot/install | sh
#   curl -fsSL https://openvibe.bot/install | sh -s -- --robot <rob_…> --code <CODE> [--driver adeept|adeept-mecanum|cozmo|looi|ros2|ros2-holonomic|none]
#   curl -fsSL https://openvibe.bot/install | sh -s -- --network <URL> --pairing <pair_…> --code <CODE> [--driver …]
#   curl -fsSL https://openvibe.bot/install | sh -s -- --network <URL> --pairing <pair_…> --code <CODE> --worker
#   curl -fsSL https://openvibe.bot/install | sh -s -- --setup <osk_…>
#
# Detects the OS and CPU, downloads the openvibe-node binary and the plugin bundle (checking their SHA-256), creates
# a Python virtual environment for the plugins, disables the Adeept kit's stock control server if it finds it,
# installs the system service and pairs the device with <CODE>. Run it again to upgrade; the credential is kept.
# openvibe.bot shows one of these lines with the robot id (or the Network pairing) and a fresh code filled in.
#
# With no robot and no code (the first line) it is the one command on the robot's computer: on a Raspberry Pi it turns
# on I2C and SPI, installs the camera tools and what the drivers need, looks at the I2C bus for a kit it knows (an
# Adeept ADR036 4WD at 0x5f) and at the camera, installs, then shows a code and a QR code. Its owner enters the code at
# openvibe.bot/link (signed in), sees what was found and approves it; the robot is made, this machine pairs with it,
# and the installer checks the driver and the camera are running and prints the robot's panel.
#
# With --setup (the last line) it is that same command with the setup key of a card made on openvibe.bot/setup/sd-card:
# the card's first boot runs it with nobody at a screen, so openvibe.bot approves the claim at once for the key's
# owner, with the robot's name, model and the rest chosen when the card files were made. No code is shown or needed.
#
# Options:
#   --robot ROBOT    the rob_… id of the robot to pair with (from openvibe.bot). A driver kind here (adeept, …) is the
#                    old meaning of --robot: still accepted, but deprecated; use --driver.
#   --code CODE      the one-time pairing code (or give it as the first argument).
#   --network URL    pair through OpenVibe.Network at URL (the machine gets a node credential, then Bot binds it).
#   --pairing ID     the pair_… id of that Network pairing; a code given here instead (with no --code) is the code.
#   --name NAME      the device's name on openvibe.bot (default: the hostname).
#   --driver KIND    adeept (ordinary wheels), adeept-mecanum, cozmo (bridge), looi (bridge, Bluetooth), ros2 (a ROS 2
#                    robot through rosbridge), ros2-holonomic (mecanum or omni wheels), none (dry run).
#                    Default: none.
#   --worker         this machine runs its owner's jobs (openvibe.bot/machines): the job worker is turned on, jobs run
#                    as the unprivileged user openvibe-job in the worker's sandbox with public egress (the service
#                    runs as root, which run_as and the egress need), and the OpenVibe.Codes job runner is added when
#                    Node.js 20+ is installed system-wide. Linux only. docs/worker.md has the details.
#   --desk-host      this machine hosts desks for its owner's agents (docs/computer.md "Desk hosts"; the Desks plan's
#                    step D5): an old laptop, a mini PC or a spare workstation, no screen or login needed. The
#                    service runs as its own account (openvibe-desk), the installer adds what desks need (Xvfb, a
#                    window manager, D-Bus, xdotool, ImageMagick, Chromium) and sizes them to the machine's memory
#                    (max_desks: one per 2 GiB); each agent task gets a desk of its own. Implies --computer trusted
#                    (for that account's own files only); give another level with --computer.
#   --shared         with --desk-host: the desks serve people other than the machine's owner (a desk host OpenVibe or
#                    a community member runs for others; Desks plan D5). Browser desks only, each one's browser in the
#                    worker's sandbox (its own namespaces and cgroup, as the unprivileged user openvibe-job, 1 GiB and
#                    one core at most, the public internet only), and nothing of the machine itself offered. The
#                    service runs as root, which the sandbox needs; the installer adds Google Chrome's .deb (amd64) or
#                    Debian's Chromium, nftables and iproute2. Linux only.
#   --computer LEVEL this machine's terminal, files and screen for its owner's agents (the computer plugin,
#                    docs/computer.md), at LEVEL observe, ask, trusted or full. The service then runs as YOUR account
#                    (the one that ran sudo), never root, and its folders are your home. Linux only; not with --worker.
#   --ai             this machine's own models for its owner's OpenVibe work (docs/ai.md): adds the ai plugin, which
#                    finds Ollama, LM Studio or llama.cpp's server on this machine and relays its owner's questions to
#                    it, with no port opened. The installer says which one answers. Works with any of the above; on
#                    openvibe.bot/machines turn on "Let my AI use this computer's models" for it.
#   --ai-endpoint URL  the model server's http(s) base when it is not on one of the usual ports (implies --ai).
#   --no-service     install and pair, but do not install the service.
#   --no-claim       with no robot and no code: install without pairing (no code is asked for on openvibe.bot).
#   --setup KEY      the osk_… setup key from card files made on openvibe.bot/setup/sd-card (or OPENVIBE_SETUP_KEY):
#                    the claim is approved by the key, with no code to enter. Used on its own.
#   --local DIR      install from DIR (openvibe-node-<os>-<arch> and openvibe-node-plugins.tar.gz) instead of
#                    downloading.
#   --version TAG    release to download (default: latest).
# Environment:
#   OPENVIBE_NODE_BASE   download base URL (default: GitHub releases of OpenVibers/OpenVibe.Node).
#   OPENVIBE_SERVER      OpenVibe.Bot origin (default https://openvibe.bot).
#
# The Adeept stock-server check can be pointed at plain files for testing (no root, no crontab command):
#   OPENVIBE_INSTALL_STOCK_ONLY=1   run only that check and exit.
#   OPENVIBE_CRONTAB_ROOT=FILE      edit FILE as root's crontab instead of the real one.
#   OPENVIBE_CRONTAB_USER=FILE      edit FILE as the invoking user's crontab instead of the real one.
#   OPENVIBE_RC_LOCAL=FILE          edit FILE as rc.local instead of /etc/rc.local.
# OPENVIBE_INSTALL_PAIR_ARGS_ONLY=1 prints the `openvibe-node pair` arguments the flags make, one per line, and exits.
# OPENVIBE_INSTALL_AI_CONFIG_ONLY=<config.json> adds the --ai plugin entry to that file and exits.
# OPENVIBE_INSTALL_WORKER_CONFIG_ONLY=<config.json> writes the --worker section into that file (OPENVIBE_JOB_UID,
#   OPENVIBE_JOB_GID, OPENVIBE_NODEJS and OPENVIBE_CODES_DIR stand in for the job user and the runner) and exits.
set -eu

REPO_URL="https://github.com/OpenVibers/OpenVibe.Node"
CODE=""
DRIVER="none"
ROBOT_ID=""
NAME=""
NETWORK=""
PAIRING=""
SERVICE=1
LOCAL=""
TAG="latest"
WORKER=0
COMPUTER=""
DESK_HOST=0
SHARED=0
AI=0
AI_ENDPOINT=""
CLAIM_OK=1
DRIVER_GIVEN=0
SETUP="${OPENVIBE_SETUP_KEY:-}"
# The OpenVibe.Codes release whose `openvibe-codes job` the worker runs (protocol openvibe-codes@1.0.0).
CODES_VERSION="${OPENVIBE_CODES_VERSION:-v0.3.1}"
CODES_DIR=/opt/openvibe-codes

usage() {
	echo "usage: install.sh [--robot rob_… | --network URL --pairing pair_…] [--code CODE | CODE] [--name NAME]"
	echo "                  [--driver adeept|adeept-mecanum|cozmo|looi|ros2|ros2-holonomic|none] [--worker]"
	echo "                  [--computer observe|ask|trusted|full] [--desk-host [--shared]] [--ai] [--ai-endpoint URL]"
	echo "                  [--no-service] [--no-claim] [--setup osk_…]"
	echo "                  [--local DIR] [--version TAG]"
	echo "  curl -fsSL https://openvibe.bot/install | sh      (finds the kit, then shows a code to approve on openvibe.bot/link)"
	echo "  curl -fsSL https://openvibe.bot/install | sh -s -- --robot rob_… --code ABCD-1234 --driver adeept"
	echo "  curl -fsSL https://openvibe.bot/install | sh -s -- --network https://openvibe.network --pairing pair_… --code ABCD-1234"
}
say() { printf '%s\n' "openvibe-node: $*"; }
die() { printf '%s\n' "openvibe-node: error: $*" >&2; exit 1; }
# --robot is the rob_… pairing target; it used to name the driver kind, which still works but is deprecated.
robot_arg() {
	case "$1" in
	rob_*) ROBOT_ID="$1" ;;
	*) say "warning: --robot $1 is deprecated; use --driver $1 (--robot now takes the rob_… id from openvibe.bot)"; DRIVER="$1" ;;
	esac
}

while [ $# -gt 0 ]; do
	case "$1" in
	--robot) robot_arg "${2:-}"; shift 2 ;;
	--robot=*) robot_arg "${1#*=}"; shift ;;
	--driver) DRIVER="${2:-}"; DRIVER_GIVEN=1; shift 2 ;;
	--driver=*) DRIVER="${1#*=}"; DRIVER_GIVEN=1; shift ;;
	--code) CODE="${2:-}"; shift 2 ;;
	--code=*) CODE="${1#*=}"; shift ;;
	--name) NAME="${2:-}"; shift 2 ;;
	--name=*) NAME="${1#*=}"; shift ;;
	--network) NETWORK="${2:-}"; shift 2 ;;
	--network=*) NETWORK="${1#*=}"; shift ;;
	--pairing) PAIRING="${2:-}"; shift 2 ;;
	--pairing=*) PAIRING="${1#*=}"; shift ;;
	--worker) WORKER=1; shift ;;
	--desk-host) DESK_HOST=1; shift ;;
	--shared) SHARED=1; shift ;;
	--ai) AI=1; shift ;;
	--ai-endpoint) AI=1; AI_ENDPOINT="${2:-}"; shift 2 ;;
	--ai-endpoint=*) AI=1; AI_ENDPOINT="${1#*=}"; shift ;;
	--computer) COMPUTER="${2:-}"; shift 2 ;;
	--computer=*) COMPUTER="${1#*=}"; shift ;;
	--no-service) SERVICE=0; shift ;;
	--no-claim) CLAIM_OK=0; shift ;;
	--setup) SETUP="${2:-}"; shift 2 ;;
	--setup=*) SETUP="${1#*=}"; shift ;;
	--local) LOCAL="${2:-}"; shift 2 ;;
	--version) TAG="${2:-}"; shift 2 ;;
	-h|--help) usage; exit 0 ;;
	-*) die "unknown option $1" ;;
	*) CODE="$1"; shift ;;
	esac
done

case "$DRIVER" in adeept|adeept-mecanum|cozmo|looi|ros2|ros2-holonomic|none) ;; *) die "--driver must be adeept, adeept-mecanum, cozmo, looi, ros2, ros2-holonomic or none" ;; esac
case "$ROBOT_ID" in ""|rob_*) ;; *) die "--robot must be a rob_… id" ;; esac
case "$AI_ENDPOINT" in ""|http://*|https://*) ;; *) die "--ai-endpoint is the model server's http(s) base, for example http://127.0.0.1:11434" ;; esac
if [ "$SHARED" = 1 ]; then
	[ "$DESK_HOST" = 1 ] || die "--shared goes with --desk-host (a desk host that serves other people)"
	# Nobody sits at a shared desk host to approve, and its desks act on other people's behalf only.
	case "$COMPUTER" in ""|trusted|observe) ;; *) die "a shared desk host runs at level trusted (or observe): nobody is at it to approve" ;; esac
	DESK_HOST=shared
fi
# --pairing takes the pair_… id; with no --code, anything else there is the code itself.
case "$PAIRING" in ""|pair_*) ;; *) [ -z "$CODE" ] || die "--pairing must be a pair_… id"; CODE="$PAIRING"; PAIRING="" ;; esac
if [ -n "$PAIRING" ] && [ -z "$NETWORK" ]; then die "--pairing needs --network, the OpenVibe.Network URL"; fi
if [ -n "$NETWORK" ] && [ -z "$CODE" ]; then die "--network needs the pairing code (--code)"; fi
case "$NETWORK" in ""|https://*|http://localhost*|http://127.0.0.1*) ;; *) die "--network must be an https:// URL" ;; esac
# A card's setup key: the one command below, approved by the key (openvibe.bot/setup/sd-card), so it stands alone.
if [ -n "$SETUP" ]; then
	printf '%s' "$SETUP" | grep -Eq '^osk_[A-Za-z0-9_-]{43}$' || die "--setup takes the osk_… key from your card files (openvibe.bot/setup/sd-card)"
	[ -z "$CODE$ROBOT_ID$NETWORK$PAIRING" ] || die "--setup is used on its own (no --robot, --code, --network or --pairing)"
	if [ "$CLAIM_OK" = 0 ] || [ "$WORKER" = 1 ] || [ -n "$COMPUTER" ] || [ "$DESK_HOST" != 0 ] || [ "$AI" = 1 ]; then die "--setup cannot be used with --no-claim, --worker, --computer, --desk-host or --ai"; fi
fi
# No robot, no code, no Network pairing, not a worker, a computer or a model machine: the one command (openvibe.bot/link,
# see the top).
CLAIM=0
if [ "$CLAIM_OK" = 1 ] && [ -z "$CODE$ROBOT_ID$NETWORK$PAIRING" ] && [ "$WORKER" = 0 ] && [ -z "$COMPUTER" ] && [ "$AI" = 0 ]; then CLAIM=1; fi

# Run "$@" pair … with the pairing the flags gave: through Network with --network, else Bot's legacy code.
pair_with() {
	set -- "$@" pair "$CODE"
	if [ -n "$NETWORK" ]; then
		set -- "$@" --network "$NETWORK"
		[ -z "$PAIRING" ] || set -- "$@" --pairing "$PAIRING"
	else
		[ -z "$ROBOT_ID" ] || set -- "$@" --robot "$ROBOT_ID"
	fi
	[ -z "$NAME" ] || set -- "$@" --name "$NAME"
	"$@"
}

if [ "${OPENVIBE_INSTALL_PAIR_ARGS_ONLY:-0}" = 1 ]; then
	pair_with printf '%s\n'
	exit 0
fi

# set_driver_config <config.json>: $PLUGINS, $KIND and $VIDEO (driver_setup's) in that config, the rest kept (a computer
# or ai plugin too). The one command writes the driver the bus suggested, then the one its owner chose, if that differs.
set_driver_config() {
	${SUDO:-} env OV_PLUGINS="$PLUGINS" OV_KIND="$KIND" OV_VIDEO="{$VIDEO}" python3 - "$1" <<'PY'
import json, os, sys
path = sys.argv[1]
try:
    with open(path) as f:
        conf = json.load(f)
except (OSError, ValueError):
    conf = {}
keep = [p for p in conf.get('plugins') or [] if p.get('name') in ('computer', 'ai')]
conf['plugins'] = json.loads(os.environ['OV_PLUGINS']) + keep
conf['device_kind'] = os.environ['OV_KIND']
conf['video'] = {**(conf.get('video') or {}), **json.loads(os.environ['OV_VIDEO'])}
os.umask(0o077)
with open(path + '.tmp', 'w') as f:
    json.dump(conf, f, indent=2)
    f.write('\n')
os.replace(path + '.tmp', path)
PY
}

# claim_robot: ask openvibe.bot for a code, show it (and a QR), and wait up to 16 minutes for its owner to approve it
# on openvibe.bot/link. Approved, it sets ROBOT_ID, CODE (or NETWORK and PAIRING), CLAIM_DRIVER, ROBOT_NAME, PANEL_URL
# and LIVE from the answer, each checked before the shell reads it. Denied or late, it fails.
claim_robot() {
	_answer="$TMP/claim.env"
	# shellcheck disable=SC1091 # the OS's own file, read for its name only
	OV_HW_MODEL="$({ tr -d '\0' < /proc/device-tree/model; } 2>/dev/null || true)" \
	OV_HW_OS="$( (. /etc/os-release 2>/dev/null && printf '%s' "${PRETTY_NAME:-}") || true)" \
	OV_HW_ARCH="$(uname -m)" OV_HW_KIT="${KIT:-}" OV_HW_KIT_NAME="${KIT_NAME:-}" OV_HW_FOUND="${FOUND:-}" OV_HW_CAMERA="${CAMERA:-}" \
	OV_SETUP_KEY="${SETUP:-}" \
	python3 - "${OPENVIBE_SERVER:-https://openvibe.bot}" "$_answer" "${NAME:-$(hostname)}" <<'PY' || return 1
import json, os, re, subprocess, sys, time, urllib.error, urllib.request
server, out, host = sys.argv[1].rstrip('/'), sys.argv[2], sys.argv[3]
hw = {k: os.environ.get('OV_HW_' + k.upper(), '').strip() for k in ('model', 'os', 'arch', 'kit', 'kit_name', 'camera')}
hw = {k: v for k, v in hw.items() if v}
found = [x for x in os.environ.get('OV_HW_FOUND', '').split('|') if x]
if found:
    hw['found'] = found
def call(method, path, body=None, headers=None):
    req = urllib.request.Request(server + path, method=method, data=None if body is None else json.dumps(body).encode(),
                                 headers={'Content-Type': 'application/json', 'User-Agent': 'openvibe-node-installer', **(headers or {})})
    with urllib.request.urlopen(req, timeout=20) as r:
        return json.load(r)
def die(msg):
    print(f'\nopenvibe-node: error: {msg}', file=sys.stderr)
    sys.exit(1)
setup_key = os.environ.get('OV_SETUP_KEY', '')
body = {'hostname': host, 'hardware': hw}
if setup_key:
    body['setup_key'] = setup_key
try:
    c = call('POST', '/api/v1/claims', body)
except urllib.error.HTTPError as e:
    # openvibe.bot says why (a used or old setup key, too many tries): show that, not just the status.
    detail = ''
    try:
        j = json.load(e)
        detail = j.get('detail') or j.get('error') or ''
    except Exception:  # noqa: BLE001 - no JSON body: the status is all there is
        pass
    die(f'{server} refused: {detail}' if detail else f'could not ask {server} for a code: {e}')
except Exception as e:  # noqa: BLE001 - say what happened and stop
    die(f'could not ask {server} for a code: {e}')
if c.get('approved'):
    print("\n  Adding this robot to openvibe.bot with your card's setup key (no code needed).\n")
else:
    print(f"\n  Approve this robot on openvibe.bot (signed in as you):\n    open  {c['verification_url']}\n    code  {c['user_code']}\n")
    try:
        subprocess.run(['qrencode', '-t', 'ANSIUTF8', '-m', '2', c['verification_url_complete']], check=True, timeout=10)
        print('  (or scan this with your phone)\n')
    except Exception:  # noqa: BLE001 - no QR, the link is enough
        print(f"  or open {c['verification_url_complete']}\n")
interval, deadline = max(2, int(c.get('interval') or 3)), time.time() + 16 * 60
sys.stdout.write('  waiting for you to approve it')
sys.stdout.flush()
while time.time() < deadline:
    time.sleep(interval)
    try:
        s = call('GET', '/api/v1/claims/' + c['id'], headers={'X-OV-Claim-Secret': c['secret']})
    except urllib.error.HTTPError as e:
        if e.code == 404:
            die('openvibe.bot no longer knows this code: run the installer again for a new one')
        interval = min(30, interval * 2) if e.code == 429 else interval
        continue
    except Exception:  # noqa: BLE001 - the network blinked: keep waiting
        continue
    state = s.get('state')
    if state == 'pending':
        sys.stdout.write('.')
        sys.stdout.flush()
        continue
    if state == 'denied':
        die('the code was denied on openvibe.bot, so nothing was added')
    if state != 'approved':
        die(f'the code is {state}: run the installer again for a new one')
    p = s.get('pairing') or {}
    network = bool(p.get('pairing_id'))
    vals = {'ROBOT_ID': '' if network else p.get('robot') or '', 'CODE': p.get('code') or '',
            'NETWORK': p.get('network') or '' if network else '', 'PAIRING': p.get('pairing_id') or '',
            'CLAIM_DRIVER': s.get('driver') or 'none', 'ROBOT_NAME': s.get('name') or '', 'PANEL_URL': s.get('panel_url') or '',
            'LIVE': '1' if s.get('live') else '0'}
    shape = {'ROBOT_ID': r'(rob_[0-9A-Za-z]+)?', 'CODE': r'[0-9A-Za-z-]{4,20}', 'NETWORK': r'(https://[A-Za-z0-9.:/-]+)?',
             'PAIRING': r'(pair_[0-9A-Za-z]+)?', 'CLAIM_DRIVER': r'adeept|adeept-mecanum|cozmo|looi|ros2|ros2-holonomic|none',
             'ROBOT_NAME': r"[^'\n\\]{0,80}", 'PANEL_URL': r'((https://|http://(localhost|127\.0\.0\.1)[:/])[A-Za-z0-9.:/_-]*)?', 'LIVE': r'[01]'}
    for k, v in vals.items():
        if not re.fullmatch(shape[k], v):
            die(f'openvibe.bot answered an unexpected {k}')
    with open(out, 'w') as f:
        f.write(''.join(f"{k}='{v}'\n" for k, v in vals.items()))
    print(' approved')
    sys.exit(0)
die('nobody approved the code in 16 minutes: run the installer again for a new one')
PY
	# shellcheck source=/dev/null # written just above, every value checked against its shape
	. "$_answer"
}

# Test hook: ask OPENVIBE_SERVER for a code, wait for the answer, print what pairing would use, and stop.
if [ "${OPENVIBE_INSTALL_CLAIM_ONLY:-0}" = 1 ]; then
	TMP="$(mktemp -d)"
	trap 'rm -rf "$TMP"' EXIT
	claim_robot || exit 1
	printf 'ROBOT_ID=%s\nCODE=%s\nNETWORK=%s\nPAIRING=%s\nCLAIM_DRIVER=%s\nROBOT_NAME=%s\nPANEL_URL=%s\nLIVE=%s\n' \
		"$ROBOT_ID" "$CODE" "$NETWORK" "$PAIRING" "$CLAIM_DRIVER" "$ROBOT_NAME" "$PANEL_URL" "$LIVE"
	exit 0
fi

# write_worker_config <config.json> <job uid> <job gid> [<node.js> <codes dir>]: turn the job worker on in that config
# (kept as it is otherwise, created when missing): jobs as the job user, public egress, caps sized for a coding task,
# and, with a Node.js and a Codes install, the `openvibe-codes@1.0.0` job runner. Keys stay empty for the owner to add.
# write_computer_config <config.json> <level> <home> [desk-host]: add the computer plugin at that level (replacing an
# earlier entry), its folder the owner's home; the rest of the config is kept. A desk host (4th argument 1) keeps its
# desks under <home>/desks, shows no notifications (nobody sits at it) and turns the camera off; a shared one (4th
# argument shared) sandboxes every desk (desk_isolation sandbox) and offers nothing else.
write_computer_config() {
	${SUDO:-} python3 - "$@" <<'PY'
import json, os, sys
path, level, home = sys.argv[1], sys.argv[2], sys.argv[3]
desk_host = len(sys.argv) > 4 and sys.argv[4] in ('1', 'shared')
shared = len(sys.argv) > 4 and sys.argv[4] == 'shared'
with open(path) as f:
    conf = json.load(f)
plugins = [p for p in conf.get('plugins') or [] if p.get('name') not in ('computer', 'dryrun')]
computer = {'level': level, 'roots': [home]}
if desk_host:
    computer.update({'notify': False, 'allow_desk': True, 'desk_dir': os.path.join(home, 'desks'),
                     'browser_profile': os.path.join(home, 'browser')})
    conf['video'] = {'source': 'off'}
if shared:
    previous = next((p.get('config') or {} for p in conf.get('plugins') or [] if p.get('name') == 'computer'), {})
    computer.update({'desk_isolation': 'sandbox', 'desk_egress': 'public', 'allow_exec': False, 'probe': False,
                     'allow_screen_stream': False})
    for k, v in {'desk_memory_mb': 1024, 'desk_cpu_millis': 1000, 'desk_disk_mb': 512, 'desk_pids': 512}.items():
        computer[k] = previous.get(k, v)   # an operator's own sizes are kept on a second run
    if previous.get('max_desks'):
        computer['max_desks'] = previous['max_desks']
plugins.append({'name': 'computer', 'config': computer})
conf['plugins'] = plugins
old = os.umask(0o077)
with open(path + '.tmp', 'w') as f:
    json.dump(conf, f, indent=2)
    f.write('\n')
os.umask(old)
os.replace(path + '.tmp', path)
PY
}

# write_ai_config <config.json> [endpoint]: add the ai plugin (replacing an earlier entry; the dry-run plugin goes), with
# the model server's base when one was given, else the plugin looks on the usual ports; the rest of the config is kept.
write_ai_config() {
	${SUDO:-} python3 - "$@" <<'PY'
import json, os, sys
path = sys.argv[1]
endpoint = sys.argv[2] if len(sys.argv) > 2 else ''
try:
    with open(path) as f:
        conf = json.load(f)
except FileNotFoundError:
    conf = {}
plugins = [p for p in conf.get('plugins') or [] if p.get('name') not in ('ai', 'dryrun')]
plugins.append({'name': 'ai', 'config': {'endpoint': endpoint}} if endpoint else {'name': 'ai'})
conf['plugins'] = plugins
old = os.umask(0o077)
with open(path + '.tmp', 'w') as f:
    json.dump(conf, f, indent=2)
    f.write('\n')
os.umask(old)
os.replace(path + '.tmp', path)
PY
}

# ai_found [endpoint]: the first model server that answers (the one given, else Ollama, LM Studio, llama.cpp's server) and
# how many models it offers, as one line; nothing when none answers.
ai_found() {
	python3 - "$@" <<'PY' 2>/dev/null || true
import json, sys, urllib.request
given = sys.argv[1] if len(sys.argv) > 1 else ''
servers = [(given, 'the model server')] if given else [('http://127.0.0.1:11434', 'Ollama'), ('http://127.0.0.1:1234', 'LM Studio'), ('http://127.0.0.1:8080', "llama.cpp's server")]
for base, name in servers:
    try:
        with urllib.request.urlopen(base.rstrip('/') + '/v1/models', timeout=3) as r:
            data = json.load(r).get('data') or []
    except Exception:
        continue
    models = [str(m.get('id')) for m in data if isinstance(m, dict) and m.get('id')]
    print('%s at %s with %d model%s%s' % (name, base, len(models), '' if len(models) == 1 else 's', (': ' + ', '.join(models[:5]) + (', …' if len(models) > 5 else '')) if models else ''))
    break
PY
}

write_worker_config() {
	${SUDO:-} python3 - "$@" <<'PY'
import json, os, sys
path, uid, gid = sys.argv[1], int(sys.argv[2]), int(sys.argv[3])
node = sys.argv[4] if len(sys.argv) > 4 else ''
codes = sys.argv[5] if len(sys.argv) > 5 else ''
try:
    with open(path) as f:
        conf = json.load(f)
except FileNotFoundError:
    conf = {}
worker = conf.get('worker') or {}
worker.update({
    'enabled': True,
    'run_as': {'uid': uid, 'gid': gid},
    'allow_same_user': False,
    'egress': 'public',
})
caps = worker.get('caps') or {}
for k, v in {
    'max_ttl_ms': 3600000, 'max_wall_ms': 3600000, 'max_cpu_ms': 3600000, 'max_mem_bytes': 4 << 30,
    'max_output_bytes': 1 << 20, 'max_jobs': 1, 'max_cpu_millis': 2000, 'max_pids': 512,
    'max_disk_bytes': 3 << 30, 'max_vm_bytes': 64 << 40,
}.items():
    caps.setdefault(k, v)
worker['caps'] = caps
# A job resolves names only through resolvers it can reach by address (docs/worker.md, "DNS"): two public ones, kept
# when the owner already chose others.
worker.setdefault('dns', ['1.1.1.1', '9.9.9.9'])
functions = [f for f in worker.get('functions') or [] if f.get('name') != 'openvibe-codes']
if node and codes:
    previous = next((f for f in worker.get('functions') or [] if f.get('name') == 'openvibe-codes'), {})
    env = {'HOME': '/tmp', 'OPENVIBE_CODES_HOME': '/tmp/.openvibe-codes', 'PATH': '/usr/local/bin:/usr/bin:/bin'}
    for k, v in (previous.get('env') or {}).items():
        env.setdefault(k, v)
    for k in ('DEEPSEEK_API_KEY', 'ANTHROPIC_API_KEY', 'OPENAI_API_KEY', 'GITHUB_TOKEN'):
        env.setdefault(k, '')
    functions.append({
        'name': 'openvibe-codes', 'version': '1.0.0', 'class': 'code',
        'command': [node, os.path.join(codes, 'node_modules', 'openvibe-codes', 'bin', 'openvibe-codes.js'), 'job'],
        'artifact_dir': codes, 'env': env,
    })
worker['functions'] = functions
conf['worker'] = worker
old = os.umask(0o077)
with open(path + '.tmp', 'w') as f:
    json.dump(conf, f, indent=2)
    f.write('\n')
os.umask(old)
os.replace(path + '.tmp', path)
PY
}

if [ -n "${OPENVIBE_INSTALL_WORKER_CONFIG_ONLY:-}" ]; then
	write_worker_config "$OPENVIBE_INSTALL_WORKER_CONFIG_ONLY" "${OPENVIBE_JOB_UID:?}" "${OPENVIBE_JOB_GID:?}" \
		${OPENVIBE_NODEJS:+"$OPENVIBE_NODEJS"} ${OPENVIBE_CODES_DIR:+"$OPENVIBE_CODES_DIR"}
	exit 0
fi
if [ -n "${OPENVIBE_INSTALL_AI_CONFIG_ONLY:-}" ]; then
	[ "$AI" = 1 ] || die "OPENVIBE_INSTALL_AI_CONFIG_ONLY needs --ai or --ai-endpoint"
	write_ai_config "$OPENVIBE_INSTALL_AI_CONFIG_ONLY" ${AI_ENDPOINT:+"$AI_ENDPOINT"}
	exit 0
fi
# OPENVIBE_INSTALL_COMPUTER_CONFIG_ONLY=<config.json> with --computer LEVEL and OPENVIBE_COMPUTER_HOME adds the computer
# plugin to that config and exits (scripts/test-install-computer-config.sh).
if [ -n "${OPENVIBE_INSTALL_COMPUTER_CONFIG_ONLY:-}" ]; then
	[ "$DESK_HOST" = 0 ] || [ -n "$COMPUTER" ] || COMPUTER=trusted
	case "$COMPUTER" in observe|ask|trusted|full) ;; *) die "--computer is observe, ask, trusted or full" ;; esac
	write_computer_config "$OPENVIBE_INSTALL_COMPUTER_CONFIG_ONLY" "$COMPUTER" "${OPENVIBE_COMPUTER_HOME:?}" "$DESK_HOST"
	[ "$DESK_HOST" != shared ] || write_worker_config "$OPENVIBE_INSTALL_COMPUTER_CONFIG_ONLY" "${OPENVIBE_JOB_UID:-999}" "${OPENVIBE_JOB_GID:-999}"
	exit 0
fi

# Root for the service, /usr/local/bin and /etc.
SUDO=""

# ---- the Adeept kit's stock server: never run it (fixed admin:123456 login on 0.0.0.0:8888, MJPEG on :5000) ----
# Its installer autostarts it from the user's crontab (or root's, with an @reboot line) or from /etc/rc.local,
# depending on kit version. Besides the systemd unit and any running process, comment out those autostart lines
# (marked, never deleted) so they cannot start it again.
STOCK_SERVER_RE='Server_(Ordinary|Mecanum)Wheels/(WebServer|APPServer|GUIServer|app)\.py'
STOCK_SERVER_MARKER='#openvibe-node-disabled:'
# Root's crontab is always scanned. Scan the invoking user's crontab on the normal install path or through sudo.
# From a plain root shell (sudo -i, root login), use the kit's usual owner pi when that account exists.
CRON_USER="${SUDO_USER:-$(id -un 2>/dev/null || printf '%s' root)}"
if [ "$CRON_USER" = root ] && id pi >/dev/null 2>&1; then CRON_USER=pi; fi

# Comment out the stock-server lines in one crontab-like file: read $1, write $2, print one line per disabled
# entry. A line whose first non-blank character is "#" is left alone, so our marker makes a second run a no-op.
disable_stock_lines() {
	_src="$1" _dst="$2" _where="$3"
	: > "$_dst"
	while IFS= read -r _line || [ -n "$_line" ]; do
		_trim="${_line#"${_line%%[![:space:]]*}"}"
		if [ "${_trim#\#}" = "$_trim" ] && printf '%s\n' "$_line" | grep -Eq "$STOCK_SERVER_RE"; then
			printf '%s %s\n' "$STOCK_SERVER_MARKER" "$_line" >> "$_dst"
			say "disabled the Adeept kit's stock server autostart in $_where"
		else
			printf '%s\n' "$_line" >> "$_dst"
		fi
	done < "$_src"
}

# Edit a plain crontab-like file in place (the test-override paths). No crontab command and no sudo.
disable_stock_file() {
	_file="$1" _where="$2"
	[ -f "$_file" ] || return 0
	_before="$(mktemp)" _after="$(mktemp)"
	cp "$_file" "$_before"
	disable_stock_lines "$_before" "$_after" "$_where"
	if ! cmp -s "$_before" "$_after"; then cp "$_after" "$_file"; fi
	rm -f "$_before" "$_after"
}

# Comment out stock-server lines in one user's crontab and reinstall it only if it changed.
disable_stock_crontab() {
	_user="$1"
	command -v crontab >/dev/null 2>&1 || return 0
	_before="$(mktemp)" _after="$(mktemp)"
	$SUDO crontab -u "$_user" -l > "$_before" 2>/dev/null || true
	if [ ! -s "$_before" ]; then rm -f "$_before" "$_after"; return 0; fi
	disable_stock_lines "$_before" "$_after" "the crontab of $_user"
	if ! cmp -s "$_before" "$_after"; then $SUDO crontab -u "$_user" "$_after" || true; fi
	rm -f "$_before" "$_after"
}

# Comment out stock-server lines in rc.local (the real path; needs root to write it back).
disable_stock_rc_local() {
	_rc="$1"
	[ -f "$_rc" ] || return 0
	_before="$(mktemp)" _after="$(mktemp)"
	$SUDO cp "$_rc" "$_before" 2>/dev/null || { rm -f "$_before" "$_after"; return 0; }
	disable_stock_lines "$_before" "$_after" "$_rc"
	if ! cmp -s "$_before" "$_after"; then $SUDO cp "$_after" "$_rc" || true; fi
	rm -f "$_before" "$_after"
}

disable_stock_server() {
	# The OPENVIBE_* file overrides are test-only: when any is set they point the crontab/rc.local checks at temp
	# files, so skip the real systemd and running-process branches too (a test must touch no service or process).
	if [ -z "${OPENVIBE_CRONTAB_ROOT:-}${OPENVIBE_CRONTAB_USER:-}${OPENVIBE_RC_LOCAL:-}" ]; then
		if command -v systemctl >/dev/null 2>&1; then
			unit=Adeept_Robot.service
			if systemctl is-enabled "$unit" >/dev/null 2>&1 || systemctl is-active "$unit" >/dev/null 2>&1; then
				say "disabling the Adeept kit's stock server ($unit): it listens on 0.0.0.0:8888 with a fixed password"
				$SUDO systemctl disable --now "$unit" || true
			fi
		fi
		if command -v pgrep >/dev/null 2>&1 && pgrep -f "$STOCK_SERVER_RE" >/dev/null 2>&1; then
			say "stopping a running Adeept stock server process"
			$SUDO pkill -f "$STOCK_SERVER_RE" || true
		fi
	fi
	# Autostart: root's crontab and the invoking user's, then rc.local. OPENVIBE_CRONTAB_* / OPENVIBE_RC_LOCAL
	# are test-only file overrides so the checks can run without root and without touching the real system.
	if [ -n "${OPENVIBE_CRONTAB_ROOT:-}" ]; then
		disable_stock_file "$OPENVIBE_CRONTAB_ROOT" "the root crontab"
	else
		disable_stock_crontab root
	fi
	if [ -n "${OPENVIBE_CRONTAB_USER:-}" ]; then
		disable_stock_file "$OPENVIBE_CRONTAB_USER" "the crontab of ${CRON_USER:-$(id -un 2>/dev/null || printf '%s' root)}"
	elif [ -n "$CRON_USER" ] && [ "$CRON_USER" != root ]; then
		disable_stock_crontab "$CRON_USER"
	fi
	if [ -n "${OPENVIBE_RC_LOCAL:-}" ]; then
		disable_stock_file "$OPENVIBE_RC_LOCAL" "rc.local ($OPENVIBE_RC_LOCAL)"
	else
		disable_stock_rc_local /etc/rc.local
	fi
}

# Test hook: run only the stock-server check against the OPENVIBE_* file overrides, then stop.
if [ "${OPENVIBE_INSTALL_STOCK_ONLY:-0}" = 1 ]; then
	disable_stock_server
	exit 0
fi

if [ "$(id -u)" -ne 0 ]; then
	command -v sudo >/dev/null 2>&1 || die "run as root (sudo is not installed)"
	SUDO="sudo"
fi

# ---- platform ----
OS="$(uname -s)"
case "$OS" in
Linux) OS=linux; BIN_DIR=/usr/local/bin; STATE=/var/lib/openvibe-node; CONF=/etc/openvibe-node ;;
Darwin) OS=darwin; BIN_DIR=/usr/local/bin; STATE="/Library/Application Support/OpenVibe Node"; CONF="$STATE" ;;
*) die "unsupported OS $OS (on Windows, download openvibe-node-windows-amd64.exe from $REPO_URL/releases)" ;;
esac
ARCH="$(uname -m)"
case "$ARCH" in
x86_64|amd64) ARCH=amd64 ;;
aarch64|arm64) ARCH=arm64 ;;
armv7l|armv7*) ARCH=armv7 ;;
armv6l) die "ARMv6 (Raspberry Pi Zero / 1) is not supported; use a Pi 3, 4, 5 or Zero 2 with a 64-bit OS" ;;
*) die "unsupported CPU $ARCH" ;;
esac
if [ "$OS" = darwin ] && [ "$ARCH" = armv7 ]; then die "unsupported platform"; fi
IS_PI=0
if [ -r /proc/device-tree/model ] && grep -q "Raspberry Pi" /proc/device-tree/model 2>/dev/null; then IS_PI=1; fi
say "platform $OS/$ARCH$( [ $IS_PI = 1 ] && printf ' (Raspberry Pi)')"

# ---- the robot's computer: its buses, its camera tools, and what is on them (the one command, or an Adeept driver) ----
# I2C carries the Adeept kit's motor and servo board and its battery ADC, SPI its lights: both on now and after a reboot
# (raspi-config applies them at once). Only missing packages are installed: i2c-tools to look at the bus, the camera
# tools the video uses, venv and lgpio for the plugins, qrencode for the code's QR.
pi_prepare() {
	[ "$OS" = linux ] && [ "$IS_PI" = 1 ] || return 0
	if command -v raspi-config >/dev/null 2>&1; then
		[ "$($SUDO raspi-config nonint get_i2c 2>/dev/null)" = 0 ] ||
			{ say "turning on I2C (the kit's motor board is on it)"; $SUDO raspi-config nonint do_i2c 0 || say "warning: raspi-config could not turn on I2C"; }
		[ "$($SUDO raspi-config nonint get_spi 2>/dev/null)" = 0 ] ||
			{ say "turning on SPI (the kit's lights)"; $SUDO raspi-config nonint do_spi 0 || say "warning: raspi-config could not turn on SPI"; }
	else
		boot_config=/boot/firmware/config.txt
		[ -f "$boot_config" ] || boot_config=/boot/config.txt
		for p in i2c_arm spi; do
			if [ -f "$boot_config" ] && ! grep -Eq "^dtparam=$p=on" "$boot_config"; then
				say "turning on ${p%_arm} in $boot_config"
				printf 'dtparam=%s=on\n' "$p" | $SUDO tee -a "$boot_config" >/dev/null
			fi
			if command -v dtparam >/dev/null 2>&1; then $SUDO dtparam "$p=on" 2>/dev/null || true; fi
		done
	fi
	$SUDO modprobe i2c-dev 2>/dev/null || true
	command -v apt-get >/dev/null 2>&1 || return 0
	need=""
	command -v i2cdetect >/dev/null 2>&1 || need="$need i2c-tools"
	command -v rpicam-vid >/dev/null 2>&1 || command -v libcamera-vid >/dev/null 2>&1 || need="$need rpicam-apps"
	python3 -c 'import ensurepip' >/dev/null 2>&1 || need="$need python3-venv"
	python3 -c 'import lgpio' >/dev/null 2>&1 || need="$need python3-lgpio"
	[ "$CLAIM" = 0 ] || command -v qrencode >/dev/null 2>&1 || need="$need qrencode"
	[ -n "$need" ] || return 0
	say "installing:$need"
	$SUDO apt-get update -qq >/dev/null 2>&1 || true
	# shellcheck disable=SC2086 # one word per package
	if ! $SUDO apt-get install -y -qq $need >/dev/null 2>&1; then
		need=$(printf '%s' "$need" | sed 's/rpicam-apps/libcamera-apps/')   # Bullseye's name for the camera tools
		$SUDO apt-get install -y -qq $need >/dev/null 2>&1 || say "warning: apt-get could not install$need"
	fi
}

# What is on the I2C bus and which camera there is, and the kit that makes it: KIT (a profile id on openvibe.bot),
# KIT_NAME and FOUND (the evidence, |-separated) for the approval page; DRIVER unless --driver chose one.
KIT="" KIT_NAME="" FOUND="" CAMERA=""
detect_kit() {
	[ "$OS" = linux ] || return 0
	addrs=""
	if [ -n "${OPENVIBE_I2C_ADDRS+x}" ]; then
		addrs=" $OPENVIBE_I2C_ADDRS "   # test-only: the bus's addresses, as i2cdetect would print them
	elif [ -e /dev/i2c-1 ] && command -v i2cdetect >/dev/null 2>&1; then
		addrs=" $($SUDO i2cdetect -y 1 2>/dev/null | awk 'NR > 1 { for (i = 2; i <= NF; i++) if ($i ~ /^[0-9a-f][0-9a-f]$/) printf "%s ", $i }')"
	fi
	case "$addrs" in *" 5f "*) FOUND="$FOUND|PCA9685 at 0x5f" ;; esac
	case "$addrs" in *" 40 "*) FOUND="$FOUND|PCA9685 at 0x40" ;; esac
	case "$addrs" in *" 48 "*) FOUND="$FOUND|ADS7830 at 0x48" ;; esac
	case "$addrs" in *" 14 "*) FOUND="$FOUND|Robot HAT at 0x14" ;; esac
	case "$addrs" in *" 3c "*) FOUND="$FOUND|OLED at 0x3c" ;; esac
	case "$addrs" in
	*" 5f "*) KIT=adeept.adr036; KIT_NAME="Adeept ADR036 4WD"; [ "$DRIVER_GIVEN" = 1 ] || DRIVER=adeept ;;
	*)
		if { ss -ltn 2>/dev/null || netstat -ltn 2>/dev/null; } | grep -q '[:.]9090 '; then
			KIT=ros2.diffdrive; KIT_NAME="ROS 2 robot (rosbridge)"; FOUND="$FOUND|rosbridge on port 9090"
			[ "$DRIVER_GIVEN" = 1 ] || DRIVER=ros2
		fi ;;
	esac
	for lister in ${OPENVIBE_CAMERA+none} rpicam-hello libcamera-hello; do
		[ "$lister" != none ] || { CAMERA="$OPENVIBE_CAMERA"; break; }   # test-only: the camera's name
		command -v "$lister" >/dev/null 2>&1 || continue
		CAMERA=$(timeout 15 "$lister" --list-cameras 2>/dev/null | sed -n 's/^ *0 : \([A-Za-z0-9_]*\).*/\1/p' | head -n 1)
		break
	done
	if [ -z "$CAMERA" ] && [ -d /dev/v4l/by-id ]; then CAMERA=$(find /dev/v4l/by-id -mindepth 1 -maxdepth 1 -printf '%f\n' 2>/dev/null | head -n 1 | cut -c1-60); fi
	FOUND="${FOUND#|}"
	say "found: ${KIT_NAME:-no kit it knows}$( [ -n "$FOUND" ] && printf ' (%s)' "$(printf '%s' "$FOUND" | sed 's/|/, /g')"); camera: ${CAMERA:-none}"
}
# Test hook: only the detection (OPENVIBE_I2C_ADDRS and OPENVIBE_CAMERA stand in for the bus and the camera).
if [ "${OPENVIBE_INSTALL_DETECT_ONLY:-0}" = 1 ]; then
	OS=linux
	detect_kit >/dev/null
	printf 'KIT=%s\nDRIVER=%s\nFOUND=%s\nCAMERA=%s\n' "$KIT" "$DRIVER" "$FOUND" "$CAMERA"
	exit 0
fi
if [ "$CLAIM" = 1 ] || [ "$DRIVER" = adeept ] || [ "$DRIVER" = adeept-mecanum ]; then pi_prepare; fi
[ "$CLAIM" = 0 ] || detect_kit

# ---- download ----
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
BIN_NAME="openvibe-node-$OS-$ARCH"
if [ -n "$LOCAL" ]; then
	cp "$LOCAL/$BIN_NAME" "$TMP/$BIN_NAME" || die "$LOCAL/$BIN_NAME not found"
	cp "$LOCAL/openvibe-node-plugins.tar.gz" "$TMP/" || die "$LOCAL/openvibe-node-plugins.tar.gz not found"
else
	if [ "$TAG" = latest ]; then
		BASE="${OPENVIBE_NODE_BASE:-$REPO_URL/releases/latest/download}"
	else
		BASE="${OPENVIBE_NODE_BASE:-$REPO_URL/releases/download/$TAG}"
	fi
	fetch() {
		if command -v curl >/dev/null 2>&1; then curl -fsSL --retry 3 -o "$2" "$1"
		elif command -v wget >/dev/null 2>&1; then wget -q -O "$2" "$1"
		else die "curl or wget is needed"; fi
	}
	say "downloading $BIN_NAME"
	fetch "$BASE/$BIN_NAME" "$TMP/$BIN_NAME" || die "download failed: $BASE/$BIN_NAME"
	fetch "$BASE/openvibe-node-plugins.tar.gz" "$TMP/openvibe-node-plugins.tar.gz" || die "download failed: plugin bundle"
	fetch "$BASE/SHA256SUMS" "$TMP/SHA256SUMS" || die "download failed: SHA256SUMS"
	( cd "$TMP"
	  grep -E " ($BIN_NAME|openvibe-node-plugins\.tar\.gz)\$" SHA256SUMS > sums || die "no checksums for $BIN_NAME"
	  if command -v sha256sum >/dev/null 2>&1; then sha256sum -c sums >/dev/null
	  else shasum -a 256 -c sums >/dev/null; fi ) || die "checksum mismatch: refusing to install"
fi

# ---- binary ----
$SUDO install -m 0755 "$TMP/$BIN_NAME" "$BIN_DIR/openvibe-node"
say "installed $BIN_DIR/openvibe-node ($("$BIN_DIR/openvibe-node" version))"

# ---- service account (Linux): hardware groups, no login ----
# A worker (--worker) runs the service as root: the job sandbox's run_as and its public egress (a veth, NAT and the
# Node's nftables table) need it, and each job still runs as the unprivileged openvibe-job user.
SVC_USER=""
if [ "$WORKER" = 1 ] && [ "$OS" != linux ]; then die "--worker is for Linux"; fi
COMPUTER_HOME=""
if [ "$DESK_HOST" = shared ]; then
	# A desk host for other people: the service runs as root, which the desk sandbox needs (namespaces, a cgroup per
	# desk, its network), and every desk's browser runs in it as the unprivileged openvibe-job, as a worker's jobs do.
	[ "$OS" = linux ] || die "--desk-host --shared is for Linux (namespaces and cgroup v2)"
	[ "$WORKER" = 0 ] || die "--desk-host --shared already runs the worker's sandbox for its desks; leave out --worker"
	[ -n "$COMPUTER" ] || COMPUTER=trusted
	COMPUTER_HOME=/var/lib/openvibe-desk
	$SUDO mkdir -p "$COMPUTER_HOME/desks"
	$SUDO chown -R root:root "$COMPUTER_HOME"
	$SUDO chmod 700 "$COMPUTER_HOME"
elif [ "$DESK_HOST" = 1 ]; then
	# A desk host: the desks run as an account of their own, never the person's, so an agent there never reaches the
	# owner's files, browser or sessions. --computer still sets the level (trusted by default).
	[ "$OS" = linux ] || die "--desk-host is for Linux (desks are Xvfb displays)"
	[ "$WORKER" = 0 ] || die "--desk-host and --worker cannot share a machine yet (the worker's service runs as root)"
	[ -n "$COMPUTER" ] || COMPUTER=trusted
	SVC_USER=openvibe-desk
	COMPUTER_HOME=/var/lib/openvibe-desk
	if ! id "$SVC_USER" >/dev/null 2>&1; then
		$SUDO useradd --system --create-home --home-dir "$COMPUTER_HOME" --shell /usr/sbin/nologin --user-group "$SVC_USER" 2>/dev/null ||
			$SUDO adduser --system --home "$COMPUTER_HOME" --shell /usr/sbin/nologin --group "$SVC_USER"
	fi
	$SUDO mkdir -p "$COMPUTER_HOME/desks"
	$SUDO chown -R "$SVC_USER": "$COMPUTER_HOME"
	$SUDO chmod 700 "$COMPUTER_HOME"
elif [ -n "$COMPUTER" ]; then
	case "$COMPUTER" in observe|ask|trusted|full) ;; *) die "--computer is observe, ask, trusted or full" ;; esac
	[ "$OS" = linux ] || die "--computer is for Linux in this installer (docs/computer.md: run the Node in your own session elsewhere)"
	[ "$WORKER" = 0 ] || die "--computer and --worker cannot share a machine yet (the worker's service runs as root)"
	SVC_USER="${SUDO_USER:-$(id -un)}"
	[ "$SVC_USER" != root ] || die "--computer runs the Node as your own account: run the installer from it (it uses sudo itself), not as root"
	COMPUTER_HOME=$(getent passwd "$SVC_USER" | cut -d: -f6)
	if [ -z "$COMPUTER_HOME" ] || [ ! -d "$COMPUTER_HOME" ]; then die "no home folder for $SVC_USER"; fi
fi
case "$COMPUTER" in ""|observe|ask|trusted|full) ;; *) die "--computer is observe, ask, trusted or full" ;; esac
if [ "$OS" = linux ] && [ "$SERVICE" = 1 ] && [ "$WORKER" = 0 ] && [ -z "$COMPUTER" ] && [ "$DESK_HOST" != shared ]; then
	SVC_USER=openvibe-node
	if ! id "$SVC_USER" >/dev/null 2>&1; then
		$SUDO useradd --system --home-dir "$STATE" --no-create-home --shell /usr/sbin/nologin "$SVC_USER" 2>/dev/null ||
			$SUDO adduser --system --home "$STATE" --no-create-home --shell /usr/sbin/nologin "$SVC_USER"
	fi
	for g in gpio i2c spi video audio dialout plugdev render; do
		if getent group "$g" >/dev/null 2>&1; then $SUDO usermod -a -G "$g" "$SVC_USER"; fi
	done
fi
$SUDO mkdir -p "$CONF" "$STATE"
$SUDO chmod 700 "$CONF" "$STATE"

# ---- plugins and their Python environment ----
command -v python3 >/dev/null 2>&1 || die "python3 is needed for the plugins (sudo apt install python3 python3-venv)"
$SUDO rm -rf "$STATE/plugins"
$SUDO mkdir -p "$STATE/plugins"
$SUDO tar -xzf "$TMP/openvibe-node-plugins.tar.gz" -C "$STATE/plugins"
if [ ! -x "$STATE/venv/bin/python" ]; then
	say "creating the plugin environment in $STATE/venv"
	# --system-site-packages lets the Pi use the OS's picamera2/lgpio builds.
	$SUDO python3 -m venv --system-site-packages "$STATE/venv" || die "python3 -m venv failed (sudo apt install python3-venv)"
fi
PIP="$STATE/venv/bin/pip"
$SUDO "$PIP" install --quiet --upgrade pip >/dev/null 2>&1 || true
$SUDO "$PIP" install --quiet "$STATE/plugins/sdk" "$STATE/plugins/dryrun" "$STATE/plugins/relay"
# driver_setup: the plugin of $DRIVER into the venv, and its PLUGINS, KIND and VIDEO for the config. Run again when the
# owner chose another model on openvibe.bot/link than the one the bus suggested.
driver_setup() {
PLUGINS='[{"name": "dryrun"}]'
KIND=onboard
VIDEO='"source": "auto"'
case "$DRIVER" in
adeept|adeept-mecanum)
	[ $IS_PI = 1 ] || say "warning: this is not a Raspberry Pi; the Adeept plugin will report a hardware fault"
	$SUDO "$PIP" install --quiet "$STATE/plugins/adeept_adr036[pi]"
	WHEELS=ordinary; [ "$DRIVER" = adeept-mecanum ] && WHEELS=mecanum
	PLUGINS="[{\"name\": \"adeept_adr036\", \"config\": {\"backend\": \"real\", \"wheels\": \"$WHEELS\"}}]"
	;;
cozmo)
	$SUDO "$PIP" install --quiet "$STATE/plugins/cozmo[robot]"
	PLUGINS='[{"name": "cozmo"}]'
	KIND=bridge
	command -v espeak-ng >/dev/null 2>&1 || say "tip: install espeak-ng so Cozmo can speak (sudo apt install espeak-ng)"
	;;
ros2|ros2-holonomic)
	# A ROS 2 robot through rosbridge (plugins/ros2/README.md): no ROS 2 libraries are needed here, and the service
	# needs no sourced ROS environment. Topics, speeds and the rosbridge address are plugin config in config.json.
	$SUDO "$PIP" install --quiet "$STATE/plugins/ros2"
	HOLO=false; [ "$DRIVER" = ros2-holonomic ] && HOLO=true
	PLUGINS="[{\"name\": \"ros2\", \"config\": {\"transport\": \"rosbridge\", \"rosbridge_url\": \"ws://127.0.0.1:9090\", \"holonomic\": $HOLO}}]"
	say "tip: on the robot, run a rosbridge server (ros2 launch rosbridge_server rosbridge_websocket_launch.xml) and set"
	say "     rosbridge_url, cmd_vel, max_linear and image_topic in $CONF/config.json if yours differ"
	;;
looi)
	# LOOI's camera is its phone, which streams on its own (docs/hardware-looi.md): the Node publishes no video.
	PLUGINS='[{"name": "looi"}]'
	KIND=bridge
	VIDEO='"source": "off"'
	if ! "$STATE/venv/bin/python" -c 'import openvibe_looi' >/dev/null 2>&1; then
		say "the Generic LOOI driver is not in $STATE/venv yet: install it with \"$SUDO $PIP install <the driver>\", then"
		say "restart the Node's service. Until then the panel shows the plugin's fault and LOOI never moves."
	fi
	;;
esac
}
driver_setup
if [ -n "$COMPUTER" ]; then
	# The computer plugin, and Pillow for compact screen captures when pip can get it.
	$SUDO "$PIP" install --quiet "$STATE/plugins/computer[screen]" 2>/dev/null || $SUDO "$PIP" install --quiet "$STATE/plugins/computer"
	[ "$DRIVER" = none ] && VIDEO='"source": "off"'
fi
if [ "$DESK_HOST" = shared ]; then
	# What a sandboxed browser desk needs: a browser installed under /usr or /opt (never a snap, which cannot run in
	# the sandbox), fonts, and the sandbox's network tools (its egress is a veth the Node NATs).
	if command -v apt-get >/dev/null 2>&1; then
		say "installing what shared desks need (a browser, fonts, nftables, iproute2)"
		$SUDO apt-get install -y nftables iproute2 util-linux fonts-dejavu-core fonts-noto-color-emoji fonts-liberation >/dev/null 2>&1 ||
			say "warning: apt-get could not install nftables iproute2 util-linux and fonts; install them yourself"
		if [ ! -x /opt/google/chrome/chrome ] && [ "$(dpkg --print-architecture 2>/dev/null)" = amd64 ]; then
			say "installing Google Chrome (its .deb from dl.google.com)"
			if ! curl -fsSL -o "$TMP/chrome.deb" https://dl.google.com/linux/direct/google-chrome-stable_current_amd64.deb ||
				! $SUDO apt-get install -y "$TMP/chrome.deb" >/dev/null 2>&1; then
				say "warning: could not install Google Chrome"
			fi
		elif [ ! -x /opt/google/chrome/chrome ]; then
			# No Chrome for Linux on ARM: Debian's Chromium is a .deb (Ubuntu's is a snap, which cannot run sandboxed).
			$SUDO apt-get install -y chromium >/dev/null 2>&1 ||
				say "warning: no Chromium .deb here (Ubuntu's is a snap): use Debian on an ARM desk host, or a Chromium build under /opt"
		fi
	else
		say "install Google Chrome (or a Chromium that is not a snap), fonts, nftables and iproute2: shared desks need them"
	fi
	if [ "$(cat /proc/sys/net/ipv4/ip_forward 2>/dev/null || echo 0)" != 1 ]; then
		say "turning on IPv4 forwarding (the sandbox NATs each desk's own network; /etc/sysctl.d/90-openvibe-node-worker.conf)"
		$SUDO sh -c "printf 'net.ipv4.ip_forward = 1\n' > /etc/sysctl.d/90-openvibe-node-worker.conf"
		$SUDO sysctl -q -p /etc/sysctl.d/90-openvibe-node-worker.conf || say "warning: could not turn IPv4 forwarding on"
	fi
elif [ "$DESK_HOST" = 1 ]; then
	# What a desk needs: a display (Xvfb), a window manager, its own D-Bus, input (xdotool), screenshots (ImageMagick),
	# fonts, and a Chromium-family browser.
	if command -v apt-get >/dev/null 2>&1; then
		say "installing what desks need (Xvfb, openbox, D-Bus, xdotool, ImageMagick, fonts, Chromium)"
		$SUDO apt-get install -y xvfb openbox dbus-x11 xdotool imagemagick fonts-dejavu-core fonts-noto-color-emoji >/dev/null 2>&1 ||
			say "warning: apt-get could not install every desk tool; install xvfb openbox dbus-x11 xdotool imagemagick yourself"
		if ! command -v chromium >/dev/null 2>&1 && ! command -v chromium-browser >/dev/null 2>&1 && ! command -v google-chrome >/dev/null 2>&1; then
			$SUDO apt-get install -y chromium >/dev/null 2>&1 || say "tip: no Chromium package here: install Google Chrome (a .deb from google.com/chrome) for the desks' browser"
		fi
	else
		say "install Xvfb, openbox, D-Bus, xdotool, ImageMagick and Chromium with your package manager: desks need them"
	fi
fi
if [ ! -f "$CONF/config.json" ]; then
	$SUDO sh -c "umask 077; cat > '$CONF/config.json'" <<EOF
{
  "server": "${OPENVIBE_SERVER:-https://openvibe.bot}",
  "device_kind": "$KIND",
  "plugins": $PLUGINS,
  "video": {$VIDEO}
}
EOF
	say "wrote $CONF/config.json"
else
	say "keeping the existing $CONF/config.json"
	# The one command sets up the robot it found: the driver's plugins, kind and video replace the old ones.
	[ "$CLAIM" = 0 ] || set_driver_config "$CONF/config.json"
fi
if [ "$DESK_HOST" = shared ]; then
	if ! id openvibe-job >/dev/null 2>&1; then
		$SUDO useradd --system --no-create-home --shell /usr/sbin/nologin --user-group openvibe-job 2>/dev/null ||
			$SUDO adduser --system --no-create-home --shell /usr/sbin/nologin --group openvibe-job
	fi
	# The worker is on (no functions: no jobs are offered) so the Node sets up its cgroup before any desk starts, and
	# its sandbox, run_as and egress are the desks'.
	write_worker_config "$CONF/config.json" "$(id -u openvibe-job)" "$(id -g openvibe-job)"
	write_computer_config "$CONF/config.json" "$COMPUTER" "$COMPUTER_HOME" shared
	mem_kb=$(awk '/^MemTotal:/ {print $2}' /proc/meminfo 2>/dev/null || echo 0)
	# A browser desk counts as half a desk against max_desks and may use 1 GiB: keep 1 GiB for the system.
	desks=$(( (mem_kb / 1048576 - 1) / 2 )); [ "$desks" -ge 1 ] || desks=1; [ "$desks" -le 10 ] || desks=10
	$SUDO python3 - "$CONF/config.json" "$desks" <<'PY'
import json, os, sys
path, desks = sys.argv[1], int(sys.argv[2])
conf = json.load(open(path))
for p in conf.get('plugins') or []:
    if p.get('name') == 'computer':
        p.setdefault('config', {}).setdefault('max_desks', desks)
old = os.umask(0o077)
with open(path + '.tmp', 'w') as f:
    json.dump(conf, f, indent=2)
    f.write('\n')
os.umask(old)
os.replace(path + '.tmp', path)
PY
	say "this machine is a shared desk host: up to $((desks * 2)) browser desks at once for other people's agents, each"
	say "in its own sandbox (1 GiB, one core, the public internet only); nothing of this machine is offered to them"
	say "(desk_memory_mb, desk_cpu_millis and max_desks in $CONF/config.json change it)."
elif [ "$DESK_HOST" = 1 ]; then
	write_computer_config "$CONF/config.json" "$COMPUTER" "$COMPUTER_HOME" 1
	mem_kb=$(awk '/^MemTotal:/ {print $2}' /proc/meminfo 2>/dev/null || echo 0)
	desks=$(( mem_kb / 2097152 )); [ "$desks" -ge 1 ] || desks=1; [ "$desks" -le 8 ] || desks=8
	say "this machine is a desk host: up to $desks desks at once for your agents, each its own screen, browser and files"
	say "(max_desks in $CONF/config.json changes it). On https://openvibe.bot/machines turn on \"Let my agent use this"
	say "computer\" for it: your agents' tasks then each get a desk of their own here."
elif [ -n "$COMPUTER" ]; then
	write_computer_config "$CONF/config.json" "$COMPUTER" "$COMPUTER_HOME"
	say "the computer plugin is on at level $COMPUTER for $SVC_USER (folder $COMPUTER_HOME); change it in $CONF/config.json"
	# The tray starts when they sign in: what waits for approval with Approve and Deny, the stop and resume.
	had_config=0
	[ -d "$COMPUTER_HOME/.config" ] && had_config=1
	if XDG_CONFIG_HOME="$COMPUTER_HOME/.config" "$BIN_DIR/openvibe-node" tray --autostart on >/dev/null 2>&1; then
		if [ "$(id -u)" = 0 ]; then
			[ "$had_config" = 1 ] || chown "$SVC_USER": "$COMPUTER_HOME/.config" 2>/dev/null || true
			chown "$SVC_USER": "$COMPUTER_HOME/.config/autostart" "$COMPUTER_HOME/.config/autostart/openvibe-node-tray.desktop" 2>/dev/null || true
		fi
		say "the tray starts when $SVC_USER signs in (openvibe-node tray; openvibe-node tray --autostart off removes it)"
	fi
fi

# ---- your own models (--ai): the ai plugin, and which model server answers now ----
if [ "$AI" = 1 ]; then
	write_ai_config "$CONF/config.json" ${AI_ENDPOINT:+"$AI_ENDPOINT"}
	found="$(ai_found ${AI_ENDPOINT:+"$AI_ENDPOINT"})"
	if [ -n "$found" ]; then
		say "your own models: found $found"
	else
		say "your own models: no model server answers yet. Install one (Ollama: https://ollama.com, then: ollama pull llama3.1:8b),"
		say "  or start LM Studio's or llama.cpp's server; the Node finds it on its usual port (--ai-endpoint URL for another)."
	fi
	say "on https://openvibe.bot/machines turn on \"Let my AI use this computer's models\" for this machine: OpenVibe then"
	say "  asks them for your own work only (your Vibe, your AI members), and nobody else's."
fi

# ---- the job worker (--worker): the job user, the egress tools, the Codes job runner, the worker config ----
if [ "$WORKER" = 1 ]; then
	if ! id openvibe-job >/dev/null 2>&1; then
		$SUDO useradd --system --no-create-home --shell /usr/sbin/nologin --user-group openvibe-job 2>/dev/null ||
			$SUDO adduser --system --no-create-home --shell /usr/sbin/nologin --group openvibe-job
	fi
	JOB_UID="$(id -u openvibe-job)"
	JOB_GID="$(id -g openvibe-job)"
	missing=""
	for tool in nft ip nsenter; do command -v "$tool" >/dev/null 2>&1 || missing="$missing $tool"; done
	if [ -n "$missing" ] && command -v apt-get >/dev/null 2>&1; then
		say "installing what the worker's network sandbox needs:$missing"
		$SUDO apt-get install -y nftables iproute2 util-linux >/dev/null 2>&1 || say "warning: apt-get could not install nftables iproute2 util-linux"
	fi
	for tool in nft ip nsenter; do command -v "$tool" >/dev/null 2>&1 || say "warning: $tool is missing: jobs that need the internet are refused until it is installed"; done
	if [ "$(cat /proc/sys/net/ipv4/ip_forward 2>/dev/null || echo 0)" != 1 ]; then
		say "turning on IPv4 forwarding (the worker NATs each job's own network; /etc/sysctl.d/90-openvibe-node-worker.conf)"
		$SUDO sh -c "printf 'net.ipv4.ip_forward = 1\n' > /etc/sysctl.d/90-openvibe-node-worker.conf"
		$SUDO sysctl -q -p /etc/sysctl.d/90-openvibe-node-worker.conf || say "warning: could not turn IPv4 forwarding on"
	fi
	NODEJS=""
	for candidate in /usr/bin/node /usr/local/bin/node; do
		if [ -x "$candidate" ] && [ "$("$candidate" -p 'process.versions.node.split(".")[0]' 2>/dev/null || echo 0)" -ge 20 ]; then NODEJS="$candidate"; break; fi
	done
	if [ -n "$NODEJS" ]; then
		say "adding the OpenVibe.Codes job runner $CODES_VERSION in $CODES_DIR"
		$SUDO mkdir -p "$CODES_DIR"
		$SUDO "$(dirname "$NODEJS")/npm" install --prefix "$CODES_DIR" --omit=dev --no-audit --no-fund --no-save --loglevel=error \
			"https://codeload.github.com/OpenVibers/OpenVibe.Codes/tar.gz/refs/tags/$CODES_VERSION" >/dev/null ||
			die "could not install OpenVibe.Codes $CODES_VERSION into $CODES_DIR"
		$SUDO chown -R root:root "$CODES_DIR"
		$SUDO chmod -R go-w "$CODES_DIR"
		$SUDO touch "$CONF/config.json"
		write_worker_config "$CONF/config.json" "$JOB_UID" "$JOB_GID" "$NODEJS" "$CODES_DIR"
	else
		say "no Node.js 20+ installed system-wide (/usr/bin/node or /usr/local/bin/node): the worker is on, but the coding"
		say "job runner is not. Install Node.js 22 (https://nodejs.org) and run this installer again with --worker."
		$SUDO touch "$CONF/config.json"
		write_worker_config "$CONF/config.json" "$JOB_UID" "$JOB_GID"
	fi
	say "the job worker is on: jobs run as openvibe-job in the worker's sandbox, with public egress"
	say "add your agent keys (DEEPSEEK_API_KEY, ANTHROPIC_API_KEY, OPENAI_API_KEY, and GITHUB_TOKEN to push branches) to"
	say "  worker.functions[openvibe-codes].env in $CONF/config.json, and install agent CLIs system-wide"
	say "  (sudo npm install -g @anthropic-ai/claude-code @openai/codex), then: sudo systemctl restart openvibe-node"
fi

# ---- the Adeept kit's stock server: never run it (fixed admin:123456 login on 0.0.0.0:8888, MJPEG on :5000) ----
disable_stock_server

# ---- service ----
if [ "$SERVICE" = 1 ]; then
	# An upgrade rewrites the service definition (this version's adds Delegate=yes, which the worker needs): the
	# service manager refuses to install over an existing one. Config and credential are kept.
	$SUDO "$BIN_DIR/openvibe-node" uninstall >/dev/null 2>&1 || true
	if [ -n "$SVC_USER" ]; then
		$SUDO "$BIN_DIR/openvibe-node" install --user "$SVC_USER" >/dev/null 2>&1 || $SUDO "$BIN_DIR/openvibe-node" install --user "$SVC_USER"
	else
		$SUDO "$BIN_DIR/openvibe-node" install >/dev/null 2>&1 || $SUDO "$BIN_DIR/openvibe-node" install
	fi
	say "service installed and started"
fi

# ---- the one command: the owner approves the code on openvibe.bot/link, then this machine pairs at once ----
if [ "$CLAIM" = 1 ]; then
	if $SUDO "$BIN_DIR/openvibe-node" status --json 2>/dev/null | grep -q '"paired":true'; then
		say "already paired; keeping it (to make it a new robot: sudo openvibe-node pair --force <CODE> with a code from openvibe.bot)"
		CLAIM=0
	else
		claim_robot || die "not paired: nothing was added on openvibe.bot"
		if [ "$CLAIM_DRIVER" != "$DRIVER" ]; then
			say "setting up the driver for the model you chose: $CLAIM_DRIVER"
			DRIVER="$CLAIM_DRIVER"
			driver_setup
			set_driver_config "$CONF/config.json"
		fi
	fi
fi

# ---- pair ----
if [ -n "$CODE" ]; then
	if $SUDO "$BIN_DIR/openvibe-node" status --json 2>/dev/null | grep -q '"paired":true'; then
		say "already paired; keeping the credential (to pair again: sudo openvibe-node pair --force <CODE>, adding --network <URL> --pairing <pair_…> for a Network pairing)"
	else
		pair_with $SUDO "$BIN_DIR/openvibe-node"
	fi
else
	say "not paired yet: get a code on openvibe.bot and run: sudo openvibe-node pair <CODE>"
fi

# ---- the one command: is it driving and streaming? ----
if [ "$CLAIM" = 1 ] && [ "$SERVICE" = 1 ]; then
	say "checking the link, the driver and the camera (up to a minute)"
	_status="" _i=0
	while [ "$_i" -lt 30 ]; do
		_status="$($SUDO "$BIN_DIR/openvibe-node" status --json 2>/dev/null || true)"
		if printf '%s' "$_status" | grep -q '"connected":true' && printf '%s' "$_status" | grep -q '"state":"live"'; then break; fi
		_i=$((_i + 1))
		sleep 2
	done
	printf '%s' "$_status" | python3 -c '
import json, sys
try:
    s = json.load(sys.stdin)
except ValueError:
    sys.exit("openvibe-node: the service did not answer: sudo journalctl -u openvibe-node -n 50")
link = s.get("link") or {}
print("  link     " + ("connected" if link.get("connected") else "not connected" + (": " + link["last_error"] if link.get("last_error") else "")))
for p in s.get("plugins") or []:
    fault = p.get("fault") or {}
    print("  driver   %s: %s" % (p.get("name"), p.get("state") + (" (" + (fault.get("message") or fault.get("code") or "") + ")" if fault else "")))
v = s.get("video") or {}
print("  camera   %s%s" % (v.get("state") or "off", (" (" + v["last_error"] + ")") if v.get("last_error") else (" from " + v["source"] if v.get("source") else "")))
' || true
	say "drive it: ${PANEL_URL:-https://openvibe.bot/robots}"
	[ "${LIVE:-0}" = 0 ] || say "its camera is on your OpenVibe.Live channel while it streams"
fi

say "done. Check it with: sudo openvibe-node status   Stop everything with: sudo openvibe-node stop"
