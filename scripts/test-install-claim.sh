#!/bin/sh
# Tests the one command (install.sh with no robot and no code): the kit it reads off the I2C bus, and the claim it makes
# on openvibe.bot (POST /api/v1/claims, then GET /api/v1/claims/:id with the secret until the owner approves it on
# openvibe.bot/link). OPENVIBE_INSTALL_DETECT_ONLY=1 and OPENVIBE_INSTALL_CLAIM_ONLY=1 make the installer do only that and
# exit before it downloads, installs or pairs anything; a small fake openvibe.bot answers the claim.
set -eu

here="$(cd "$(dirname "$0")" && pwd)"
installer="$here/../install/install.sh"
work="$(mktemp -d)"
server_pid=""
trap 'if [ -n "$server_pid" ]; then kill "$server_pid" 2>/dev/null || true; fi; rm -rf "$work"' EXIT

fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

# detect <i2c addresses> <camera>: the detection's KIT, DRIVER, FOUND and CAMERA lines.
detect() { OPENVIBE_INSTALL_DETECT_ONLY=1 OPENVIBE_I2C_ADDRS="$1" OPENVIBE_CAMERA="$2" sh "$installer" 2>/dev/null; }
has() { printf '%s\n' "$1" | grep -qxF "$2" || fail "detect: want '$2' in: $(printf '%s' "$1" | tr '\n' ';')"; }

out="$(detect "48 5f" imx219)"
has "$out" "KIT=adeept.adr036"
has "$out" "DRIVER=adeept"
has "$out" "FOUND=PCA9685 at 0x5f|ADS7830 at 0x48"
has "$out" "CAMERA=imx219"
out="$(detect "40 48" "")"
has "$out" "KIT="
has "$out" "DRIVER=none"
has "$out" "FOUND=PCA9685 at 0x40|ADS7830 at 0x48"
out="$(OPENVIBE_INSTALL_DETECT_ONLY=1 OPENVIBE_I2C_ADDRS="5f" OPENVIBE_CAMERA="" sh "$installer" --driver cozmo 2>/dev/null)"
has "$out" "DRIVER=cozmo"

# A fake openvibe.bot: the claim is pending on the first poll and answered on the second (with $1's answer).
serve() {
	python3 - "$work" "$1" <<'PY' &
import json, sys
from http.server import BaseHTTPRequestHandler, HTTPServer
work, mode = sys.argv[1], sys.argv[2]
polls = []
answers = {
    'approved': {'state': 'approved', 'robot_id': 'rob_01JZTEST', 'name': 'Garage rover', 'profile_id': 'adeept.adr036.mecanum',
                 'driver': 'adeept-mecanum', 'pairing': {'robot': 'rob_01JZTEST', 'code': 'ABCD-1234'},
                 'panel_url': 'https://openvibe.bot/panel/rob_01JZTEST', 'live': True},
    'network': {'state': 'approved', 'robot_id': 'rob_01JZTEST', 'name': 'Garage rover', 'driver': 'adeept',
                'pairing': {'network': 'https://openvibe.network', 'pairing_id': 'pair_01JZTEST', 'code': 'WXYZ-5678'},
                'panel_url': 'https://openvibe.bot/panel/rob_01JZTEST', 'live': False},
    'denied': {'state': 'denied'},
    'hostile': {'state': 'approved', 'robot_id': 'rob_01JZTEST', 'name': "x'; rm -rf / #", 'driver': 'adeept',
                'pairing': {'robot': 'rob_01JZTEST', 'code': 'ABCD-1234'}, 'panel_url': 'https://openvibe.bot/panel/rob_01JZTEST'},
}
class H(BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def send(self, code, body):
        data = json.dumps(body).encode()
        self.send_response(code); self.send_header('Content-Type', 'application/json'); self.send_header('Content-Length', str(len(data))); self.end_headers(); self.wfile.write(data)
    def do_POST(self):
        body = self.rfile.read(int(self.headers.get('Content-Length') or 0))
        open(f'{work}/claim-body.json', 'wb').write(body)
        if self.path != '/api/v1/claims': return self.send(404, {})
        if mode == 'spent':
            return self.send(403, {'code': 'bot.setup_key_invalid', 'detail': "this card's setup key was used, revoked or is more than 7 days old"})
        self.send(201, {**({'approved': True} if mode == 'setup' else {}), 'id': 'clm_01JZTEST', 'user_code': 'KQ7M-2XRD', 'secret': 's3cret', 'interval': 1, 'expires_at': '2099-01-01T00:00:00Z',
                        'verification_url': 'https://openvibe.bot/link', 'verification_url_complete': 'https://openvibe.bot/link?code=KQ7M-2XRD'})
    def do_GET(self):
        if self.path != '/api/v1/claims/clm_01JZTEST' or self.headers.get('X-OV-Claim-Secret') != 's3cret': return self.send(404, {})
        polls.append(1)
        self.send(200, {'state': 'pending'} if len(polls) == 1 and mode != 'setup' else answers['approved' if mode == 'setup' else mode])
srv = HTTPServer(('127.0.0.1', 0), H)
open(f'{work}/port', 'w').write(str(srv.server_port))
srv.serve_forever()
PY
	server_pid=$!
	i=0
	while [ ! -s "$work/port" ]; do i=$((i + 1)); [ "$i" -lt 50 ] || fail "the fake openvibe.bot did not start"; sleep 0.1; done
}
stop() { kill "$server_pid" 2>/dev/null || true; wait "$server_pid" 2>/dev/null || true; server_pid=""; rm -f "$work/port"; }
claim() { OPENVIBE_INSTALL_CLAIM_ONLY=1 OPENVIBE_SERVER="http://127.0.0.1:$(cat "$work/port")" sh "$installer" --name raspi 2>"$work/err"; }

serve approved
out="$(claim)" || fail "the approved claim failed: $(cat "$work/err")"
stop
has "$out" "ROBOT_ID=rob_01JZTEST"
has "$out" "CODE=ABCD-1234"
has "$out" "NETWORK="
has "$out" "CLAIM_DRIVER=adeept-mecanum"
has "$out" "ROBOT_NAME=Garage rover"
has "$out" "PANEL_URL=https://openvibe.bot/panel/rob_01JZTEST"
has "$out" "LIVE=1"
printf '%s\n' "$out" | grep -q "code  KQ7M-2XRD" || fail "the code is shown"
python3 -c 'import json,sys; b=json.load(open(sys.argv[1])); assert b["hostname"] == "raspi" and b["hardware"]["arch"], b' "$work/claim-body.json" ||
	fail "the claim names the machine"

serve network
out="$(claim)" || fail "the Network-paired claim failed: $(cat "$work/err")"
stop
has "$out" "ROBOT_ID="
has "$out" "NETWORK=https://openvibe.network"
has "$out" "PAIRING=pair_01JZTEST"
has "$out" "CODE=WXYZ-5678"
has "$out" "LIVE=0"

serve denied
if claim >/dev/null; then fail "a denied claim was taken"; fi
grep -q "denied" "$work/err" || fail "a denial says so: $(cat "$work/err")"
stop

serve hostile
if claim >/dev/null; then fail "a name that could break out of the shell's quotes was taken"; fi
grep -q "unexpected ROBOT_NAME" "$work/err" || fail "an odd answer says which value: $(cat "$work/err")"
stop

# A card's setup key (openvibe.bot/setup/sd-card): sent with the claim, approved at once, no code shown.
key="osk_$(printf 'k%.0s' $(seq 1 43))"
serve setup
out="$(OPENVIBE_INSTALL_CLAIM_ONLY=1 OPENVIBE_SERVER="http://127.0.0.1:$(cat "$work/port")" sh "$installer" --name mecanum --setup "$key" 2>"$work/err")" ||
	fail "the setup-key claim failed: $(cat "$work/err")"
stop
has "$out" "ROBOT_ID=rob_01JZTEST"
has "$out" "CODE=ABCD-1234"
printf '%s\n' "$out" | grep -q "setup key (no code needed)" || fail "it says the key added it"
if printf '%s\n' "$out" | grep -q "code  KQ7M-2XRD"; then fail "no code is shown with a setup key"; fi
python3 -c 'import json,sys; b=json.load(open(sys.argv[1])); assert b["setup_key"] == sys.argv[2] and b["hostname"] == "mecanum", b' "$work/claim-body.json" "$key" ||
	fail "the claim carries the setup key"

serve spent
if OPENVIBE_INSTALL_CLAIM_ONLY=1 OPENVIBE_SERVER="http://127.0.0.1:$(cat "$work/port")" sh "$installer" --setup "$key" >/dev/null 2>"$work/err"; then fail "a spent key was taken"; fi
grep -q "used, revoked or is more than 7 days old" "$work/err" || fail "a refused key says why: $(cat "$work/err")"
stop

if OPENVIBE_INSTALL_CLAIM_ONLY=1 sh "$installer" --setup osk_short >/dev/null 2>"$work/err"; then fail "a malformed key was taken"; fi
grep -q "takes the osk_" "$work/err" || fail "a malformed key says so: $(cat "$work/err")"
if OPENVIBE_INSTALL_CLAIM_ONLY=1 sh "$installer" --setup "$key" --code ABCD-1234 >/dev/null 2>"$work/err"; then fail "--setup with a code was taken"; fi
grep -q "on its own" "$work/err" || fail "--setup with a code says so: $(cat "$work/err")"

echo "install.sh one command (detection, claim, setup key): ok"
