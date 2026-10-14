#!/bin/sh
# OpenVibe Node installer.
#
#   curl -fsSL https://openvibe.bot/install | sh -s -- --robot <rob_…> --code <CODE> [--driver adeept|adeept-mecanum|cozmo|looi|ros2|ros2-holonomic|none]
#   curl -fsSL https://openvibe.bot/install | sh -s -- --network <URL> --pairing <pair_…> --code <CODE> [--driver …]
#   curl -fsSL https://openvibe.bot/install | sh -s -- --network <URL> --pairing <pair_…> --code <CODE> --worker
#
# Detects the OS and CPU, downloads the openvibe-node binary and the plugin bundle (checking their SHA-256), creates
# a Python virtual environment for the plugins, disables the Adeept kit's stock control server if it finds it,
# installs the system service and pairs the device with <CODE>. Run it again to upgrade; the credential is kept.
# openvibe.bot shows one of these lines with the robot id (or the Network pairing) and a fresh code filled in.
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
#   --computer LEVEL this machine's terminal, files and screen for its owner's agents (the computer plugin,
#                    docs/computer.md), at LEVEL observe, ask, trusted or full. The service then runs as YOUR account
#                    (the one that ran sudo), never root, and its folders are your home. Linux only; not with --worker.
#   --no-service     install and pair, but do not install the service.
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
# The OpenVibe.Codes release whose `openvibe-codes job` the worker runs (protocol openvibe-codes@1.0.0).
CODES_VERSION="${OPENVIBE_CODES_VERSION:-v0.3.0}"
CODES_DIR=/opt/openvibe-codes

usage() {
	echo "usage: install.sh [--robot rob_… | --network URL --pairing pair_…] [--code CODE | CODE] [--name NAME]"
	echo "                  [--driver adeept|adeept-mecanum|cozmo|looi|ros2|ros2-holonomic|none] [--worker]"
	echo "                  [--computer observe|ask|trusted|full] [--no-service]"
	echo "                  [--local DIR] [--version TAG]"
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
	--driver) DRIVER="${2:-}"; shift 2 ;;
	--driver=*) DRIVER="${1#*=}"; shift ;;
	--code) CODE="${2:-}"; shift 2 ;;
	--code=*) CODE="${1#*=}"; shift ;;
	--name) NAME="${2:-}"; shift 2 ;;
	--name=*) NAME="${1#*=}"; shift ;;
	--network) NETWORK="${2:-}"; shift 2 ;;
	--network=*) NETWORK="${1#*=}"; shift ;;
	--pairing) PAIRING="${2:-}"; shift 2 ;;
	--pairing=*) PAIRING="${1#*=}"; shift ;;
	--worker) WORKER=1; shift ;;
	--computer) COMPUTER="${2:-}"; shift 2 ;;
	--computer=*) COMPUTER="${1#*=}"; shift ;;
	--no-service) SERVICE=0; shift ;;
	--local) LOCAL="${2:-}"; shift 2 ;;
	--version) TAG="${2:-}"; shift 2 ;;
	-h|--help) usage; exit 0 ;;
	-*) die "unknown option $1" ;;
	*) CODE="$1"; shift ;;
	esac
done

case "$DRIVER" in adeept|adeept-mecanum|cozmo|looi|ros2|ros2-holonomic|none) ;; *) die "--driver must be adeept, adeept-mecanum, cozmo, looi, ros2, ros2-holonomic or none" ;; esac
case "$ROBOT_ID" in ""|rob_*) ;; *) die "--robot must be a rob_… id" ;; esac
# --pairing takes the pair_… id; with no --code, anything else there is the code itself.
case "$PAIRING" in ""|pair_*) ;; *) [ -z "$CODE" ] || die "--pairing must be a pair_… id"; CODE="$PAIRING"; PAIRING="" ;; esac
if [ -n "$PAIRING" ] && [ -z "$NETWORK" ]; then die "--pairing needs --network, the OpenVibe.Network URL"; fi
if [ -n "$NETWORK" ] && [ -z "$CODE" ]; then die "--network needs the pairing code (--code)"; fi
case "$NETWORK" in ""|https://*|http://localhost*|http://127.0.0.1*) ;; *) die "--network must be an https:// URL" ;; esac

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

# write_worker_config <config.json> <job uid> <job gid> [<node.js> <codes dir>]: turn the job worker on in that config
# (kept as it is otherwise, created when missing): jobs as the job user, public egress, caps sized for a coding task,
# and, with a Node.js and a Codes install, the `openvibe-codes@1.0.0` job runner. Keys stay empty for the owner to add.
# write_computer_config <config.json> <level> <home>: add the computer plugin at that level (replacing an earlier entry),
# its folder the owner's home; the rest of the config is kept.
write_computer_config() {
	${SUDO:-} python3 - "$@" <<'PY'
import json, os, sys
path, level, home = sys.argv[1], sys.argv[2], sys.argv[3]
with open(path) as f:
    conf = json.load(f)
plugins = [p for p in conf.get('plugins') or [] if p.get('name') not in ('computer', 'dryrun')]
plugins.append({'name': 'computer', 'config': {'level': level, 'roots': [home]}})
conf['plugins'] = plugins
old = os.umask(0o077)
with open(path + '.tmp', 'w') as f:
    json.dump(conf, f, indent=2)
    f.write('\n')
os.umask(old)
os.replace(path + '.tmp', path)
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
# OPENVIBE_INSTALL_COMPUTER_CONFIG_ONLY=<config.json> with --computer LEVEL and OPENVIBE_COMPUTER_HOME adds the computer
# plugin to that config and exits (scripts/test-install-computer-config.sh).
if [ -n "${OPENVIBE_INSTALL_COMPUTER_CONFIG_ONLY:-}" ]; then
	case "$COMPUTER" in observe|ask|trusted|full) ;; *) die "--computer is observe, ask, trusted or full" ;; esac
	write_computer_config "$OPENVIBE_INSTALL_COMPUTER_CONFIG_ONLY" "$COMPUTER" "${OPENVIBE_COMPUTER_HOME:?}"
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
if [ -n "$COMPUTER" ]; then
	case "$COMPUTER" in observe|ask|trusted|full) ;; *) die "--computer is observe, ask, trusted or full" ;; esac
	[ "$OS" = linux ] || die "--computer is for Linux in this installer (docs/computer.md: run the Node in your own session elsewhere)"
	[ "$WORKER" = 0 ] || die "--computer and --worker cannot share a machine yet (the worker's service runs as root)"
	SVC_USER="${SUDO_USER:-$(id -un)}"
	[ "$SVC_USER" != root ] || die "--computer runs the Node as your own account: run the installer from it (it uses sudo itself), not as root"
	COMPUTER_HOME=$(getent passwd "$SVC_USER" | cut -d: -f6)
	if [ -z "$COMPUTER_HOME" ] || [ ! -d "$COMPUTER_HOME" ]; then die "no home folder for $SVC_USER"; fi
fi
if [ "$OS" = linux ] && [ "$SERVICE" = 1 ] && [ "$WORKER" = 0 ] && [ -z "$COMPUTER" ]; then
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
if [ -n "$COMPUTER" ]; then
	# The computer plugin, and Pillow for compact screen captures when pip can get it.
	$SUDO "$PIP" install --quiet "$STATE/plugins/computer[screen]" 2>/dev/null || $SUDO "$PIP" install --quiet "$STATE/plugins/computer"
	[ "$DRIVER" = none ] && VIDEO='"source": "off"'
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
fi
if [ -n "$COMPUTER" ]; then
	write_computer_config "$CONF/config.json" "$COMPUTER" "$COMPUTER_HOME"
	say "the computer plugin is on at level $COMPUTER for $SVC_USER (folder $COMPUTER_HOME); change it in $CONF/config.json"
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

say "done. Check it with: sudo openvibe-node status   Stop everything with: sudo openvibe-node stop"
