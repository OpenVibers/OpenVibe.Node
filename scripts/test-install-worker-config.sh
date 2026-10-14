#!/bin/sh
# Tests install.sh --worker's config: OPENVIBE_INSTALL_WORKER_CONFIG_ONLY=<file> merges the job worker into that
# config.json (what was there stays) and exits before it downloads, installs or pairs anything. With a Node.js and a
# Codes install it declares the openvibe-codes@1.0.0 job runner, and a second run keeps the owner's keys.
set -eu

here="$(cd "$(dirname "$0")" && pwd)"
installer="$here/../install/install.sh"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

conf="$tmp/config.json"
printf '{"server": "https://openvibe.bot", "plugins": [{"name": "dryrun"}]}\n' > "$conf"
OPENVIBE_INSTALL_WORKER_CONFIG_ONLY="$conf" OPENVIBE_JOB_UID=2001 OPENVIBE_JOB_GID=2002 \
	OPENVIBE_NODEJS=/usr/bin/node OPENVIBE_CODES_DIR=/opt/openvibe-codes sh "$installer" --worker
python3 - "$conf" <<'PY' || fail "first run"
import json, sys
c = json.load(open(sys.argv[1]))
w = c['worker']
assert c['server'] == 'https://openvibe.bot' and c['plugins'] == [{'name': 'dryrun'}], 'the rest of the config stays'
assert w['enabled'] is True and w['run_as'] == {'uid': 2001, 'gid': 2002} and w['egress'] == 'public' and w['allow_same_user'] is False
assert w['caps']['max_wall_ms'] == 3600000 and w['caps']['max_mem_bytes'] == 4 << 30 and w['caps']['max_jobs'] == 1
[f] = w['functions']
assert (f['name'], f['version'], f['class'], f['artifact_dir']) == ('openvibe-codes', '1.0.0', 'code', '/opt/openvibe-codes')
assert f['command'] == ['/usr/bin/node', '/opt/openvibe-codes/node_modules/openvibe-codes/bin/openvibe-codes.js', 'job']
assert f['env']['DEEPSEEK_API_KEY'] == '' and f['env']['HOME'] == '/tmp'
PY
# The owner adds a key and changes a cap; running the installer again keeps both and declares the runner once.
python3 - "$conf" <<'PY'
import json, sys
c = json.load(open(sys.argv[1]))
c['worker']['functions'][0]['env']['DEEPSEEK_API_KEY'] = 'kept'
c['worker']['caps']['max_jobs'] = 2
json.dump(c, open(sys.argv[1], 'w'))
PY
OPENVIBE_INSTALL_WORKER_CONFIG_ONLY="$conf" OPENVIBE_JOB_UID=2001 OPENVIBE_JOB_GID=2002 \
	OPENVIBE_NODEJS=/usr/bin/node OPENVIBE_CODES_DIR=/opt/openvibe-codes sh "$installer" --worker
python3 - "$conf" <<'PY' || fail "second run"
import json, sys
w = json.load(open(sys.argv[1]))['worker']
assert len(w['functions']) == 1 and w['functions'][0]['env']['DEEPSEEK_API_KEY'] == 'kept' and w['caps']['max_jobs'] == 2
PY
# Without Node.js the worker is on and declares no runner.
conf2="$tmp/bare.json"
OPENVIBE_INSTALL_WORKER_CONFIG_ONLY="$conf2" OPENVIBE_JOB_UID=2001 OPENVIBE_JOB_GID=2002 sh "$installer" --worker
python3 - "$conf2" <<'PY' || fail "no Node.js"
import json, os, stat, sys
w = json.load(open(sys.argv[1]))['worker']
assert w['enabled'] is True and w['functions'] == []
assert stat.S_IMODE(os.stat(sys.argv[1]).st_mode) == 0o600, 'the config stays private'
PY
echo "install --worker config: ok"
