#!/bin/sh
# Tests install.sh --computer's config: OPENVIBE_INSTALL_COMPUTER_CONFIG_ONLY=<file> adds the computer plugin at the
# level given, its folder the owner's home, in place of the dry-run plugin and of an earlier computer entry; the rest of
# the config stays, and a level that is not one of the four is refused.
set -eu

here="$(cd "$(dirname "$0")" && pwd)"
installer="$here/../install/install.sh"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

conf="$tmp/config.json"
printf '{"server": "https://openvibe.bot", "plugins": [{"name": "dryrun"}, {"name": "ros2"}]}\n' > "$conf"
OPENVIBE_INSTALL_COMPUTER_CONFIG_ONLY="$conf" OPENVIBE_COMPUTER_HOME=/home/someone sh "$installer" --computer ask
OPENVIBE_INSTALL_COMPUTER_CONFIG_ONLY="$conf" OPENVIBE_COMPUTER_HOME=/home/someone sh "$installer" --computer trusted
python3 - "$conf" <<'PY' || fail "config"
import json, sys
c = json.load(open(sys.argv[1]))
assert c['server'] == 'https://openvibe.bot', 'the rest of the config stays'
assert c['plugins'] == [{'name': 'ros2'}, {'name': 'computer', 'config': {'level': 'trusted', 'roots': ['/home/someone']}}], c['plugins']
PY
if OPENVIBE_INSTALL_COMPUTER_CONFIG_ONLY="$conf" OPENVIBE_COMPUTER_HOME=/home/someone sh "$installer" --computer root 2>/dev/null; then
	fail "an unknown level was accepted"
fi
# A desk host (--desk-host, the Desks plan's step D5): trusted by default, its desks under its own account's home, no
# notifications (nobody sits at it), the camera off; --computer still sets the level.
host="$tmp/host.json"
printf '{"server": "https://openvibe.bot", "plugins": [{"name": "dryrun"}], "video": {"source": "auto"}}\n' > "$host"
OPENVIBE_INSTALL_COMPUTER_CONFIG_ONLY="$host" OPENVIBE_COMPUTER_HOME=/var/lib/openvibe-desk sh "$installer" --desk-host
python3 - "$host" <<'PY' || fail "desk host config"
import json, sys
c = json.load(open(sys.argv[1]))
assert c['video'] == {'source': 'off'}, c['video']
assert c['plugins'] == [{'name': 'computer', 'config': {'level': 'trusted', 'roots': ['/var/lib/openvibe-desk'], 'notify': False,
    'allow_desk': True, 'desk_dir': '/var/lib/openvibe-desk/desks', 'browser_profile': '/var/lib/openvibe-desk/browser'}}], c['plugins']
PY
OPENVIBE_INSTALL_COMPUTER_CONFIG_ONLY="$host" OPENVIBE_COMPUTER_HOME=/var/lib/openvibe-desk sh "$installer" --desk-host --computer ask
python3 - "$host" <<'PY' || fail "desk host level"
import json, sys
c = json.load(open(sys.argv[1]))
assert [p['config']['level'] for p in c['plugins']] == ['ask'], c['plugins']
PY
echo "ok: install.sh --computer config"
