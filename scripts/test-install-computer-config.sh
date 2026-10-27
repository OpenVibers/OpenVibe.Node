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
# A shared desk host (--desk-host --shared, Desks plan D5): every desk sandboxed, at trusted, nothing of the machine
# offered (no programs, probes or live view), the worker on with the job user (its sandbox is the desks'), and an
# operator's own sizes kept on a second run; it refuses a level that waits for someone at the machine.
shared="$tmp/shared.json"
printf '{"server": "https://openvibe.bot", "plugins": [{"name": "dryrun"}]}\n' > "$shared"
OPENVIBE_INSTALL_COMPUTER_CONFIG_ONLY="$shared" OPENVIBE_COMPUTER_HOME=/var/lib/openvibe-desk OPENVIBE_JOB_UID=990 OPENVIBE_JOB_GID=991 \
	sh "$installer" --desk-host --shared
python3 - "$shared" <<'PY' || fail "shared desk host config"
import json, sys
c = json.load(open(sys.argv[1]))
[p] = c['plugins']
cfg = p['config']
assert (p['name'], cfg['level'], cfg['desk_isolation'], cfg['desk_egress']) == ('computer', 'trusted', 'sandbox', 'public'), cfg
assert (cfg['allow_exec'], cfg['probe'], cfg['allow_screen_stream'], cfg['notify']) == (False, False, False, False), cfg
assert (cfg['desk_memory_mb'], cfg['desk_cpu_millis'], cfg['desk_disk_mb'], cfg['desk_pids']) == (1024, 1000, 512, 512), cfg
w = c['worker']
assert (w['enabled'], w['run_as'], w['egress'], w['functions']) == (True, {'uid': 990, 'gid': 991}, 'public', []), w
assert c['video'] == {'source': 'off'}
cfg['desk_memory_mb'] = 768
json.dump(c, open(sys.argv[1], 'w'))
PY
OPENVIBE_INSTALL_COMPUTER_CONFIG_ONLY="$shared" OPENVIBE_COMPUTER_HOME=/var/lib/openvibe-desk sh "$installer" --desk-host --shared
python3 -c 'import json, sys; c = json.load(open(sys.argv[1])); assert c["plugins"][0]["config"]["desk_memory_mb"] == 768, c' "$shared" ||
	fail "an operator's desk size was not kept"
if OPENVIBE_INSTALL_COMPUTER_CONFIG_ONLY="$shared" OPENVIBE_COMPUTER_HOME=/x sh "$installer" --desk-host --shared --computer ask 2>/dev/null; then
	fail "a shared desk host at level ask was accepted"
fi
if OPENVIBE_INSTALL_COMPUTER_CONFIG_ONLY="$shared" OPENVIBE_COMPUTER_HOME=/x sh "$installer" --shared 2>/dev/null; then
	fail "--shared without --desk-host was accepted"
fi
# --ai (AI on your own machines): the ai plugin, in place of the dry-run plugin and of an earlier ai entry, the rest kept;
# --ai-endpoint gives the server's base, and a base that is not http(s) is refused. A driver rewrite keeps it.
ai="$tmp/ai.json"
printf '{"server": "https://openvibe.bot", "plugins": [{"name": "dryrun"}, {"name": "computer", "config": {"level": "ask"}}]}\n' > "$ai"
OPENVIBE_INSTALL_AI_CONFIG_ONLY="$ai" sh "$installer" --ai
OPENVIBE_INSTALL_AI_CONFIG_ONLY="$ai" sh "$installer" --ai-endpoint http://127.0.0.1:8090
python3 - "$ai" <<'PY' || fail "ai config"
import json, sys
c = json.load(open(sys.argv[1]))
assert c['server'] == 'https://openvibe.bot'
assert c['plugins'] == [{'name': 'computer', 'config': {'level': 'ask'}}, {'name': 'ai', 'config': {'endpoint': 'http://127.0.0.1:8090'}}], c['plugins']
PY
if OPENVIBE_INSTALL_AI_CONFIG_ONLY="$ai" sh "$installer" --ai-endpoint ftp://x 2>/dev/null; then
	fail "a base that is not http(s) was accepted"
fi
grep -q "if p.get('name') in ('computer', 'ai')" "$installer" || fail "a driver rewrite must keep the ai plugin"
echo "ok: install.sh --computer config"
echo "ok: install.sh --ai config"
