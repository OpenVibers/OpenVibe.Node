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
echo "ok: install.sh --computer config"
