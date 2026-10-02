#!/bin/sh
# Tests install.sh's handling of the Adeept kit's stock-server autostart: crontab (root's and the invoking
# user's, including @reboot lines) and /etc/rc.local. The OPENVIBE_* test overrides point the checks at files in
# a temp dir, so this needs no root and touches no real crontab, rc.local, service or process.
set -eu

here="$(cd "$(dirname "$0")" && pwd)"
installer="$here/../install/install.sh"

fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
root_cron="$tmp/root-crontab"
user_cron="$tmp/user-crontab"
rc_local="$tmp/rc.local"

cat > "$root_cron" <<'EOF'
# root's crontab
PATH=/usr/local/sbin:/usr/local/bin:/sbin:/bin:/usr/sbin:/usr/bin
@reboot python3 /home/pi/adeept_awr/Server/Server_OrdinaryWheels/WebServer.py
0 4 * * * /usr/bin/backup.sh
EOF

cat > "$user_cron" <<'EOF'
@reboot python3 /home/pi/adeept_awr/Server/Server_OrdinaryWheels/app.py
*/5 * * * * /home/pi/adeept_awr/Server/Server_OrdinaryWheels/helper.py
EOF

cat > "$rc_local" <<'EOF'
#!/bin/sh -e
python3 /home/pi/adeept_awr/Server/Server_MecanumWheels/WebServer.py &
/usr/local/bin/other-thing &
exit 0
EOF

run() {
	OPENVIBE_INSTALL_STOCK_ONLY=1 \
	OPENVIBE_CRONTAB_ROOT="$root_cron" \
	OPENVIBE_CRONTAB_USER="$user_cron" \
	OPENVIBE_RC_LOCAL="$rc_local" \
		sh "$installer"
}

out="$(run)"
printf '%s\n' "$out"

# One line per disabled thing.
disabled="$(printf '%s\n' "$out" | grep -c "stock server autostart" || true)"
[ "$disabled" = 3 ] || fail "expected 3 disabled lines, got $disabled"

# The stock lines are commented out with the marker, not deleted.
grep -q '^#openvibe-node-disabled: @reboot python3 /home/pi/adeept_awr/Server/Server_OrdinaryWheels/WebServer\.py$' "$root_cron" ||
	fail "root crontab stock line not commented out"
grep -q '^#openvibe-node-disabled: @reboot python3 /home/pi/adeept_awr/Server/Server_OrdinaryWheels/app\.py$' "$user_cron" ||
	fail "user crontab @reboot stock line not commented out"
grep -q '^#openvibe-node-disabled: python3 /home/pi/adeept_awr/Server/Server_MecanumWheels/WebServer\.py &$' "$rc_local" ||
	fail "rc.local stock line not commented out"

# Unrelated lines are untouched.
grep -qx '0 4 \* \* \* /usr/bin/backup.sh' "$root_cron" || fail "unrelated root crontab line changed"
grep -qx 'PATH=/usr/local/sbin:/usr/local/bin:/sbin:/bin:/usr/sbin:/usr/bin' "$root_cron" || fail "root crontab PATH line changed"
grep -qxF "# root's crontab" "$root_cron" || fail "root crontab comment changed"
grep -qx '\*/5 \* \* \* \* /home/pi/adeept_awr/Server/Server_OrdinaryWheels/helper.py' "$user_cron" ||
	fail "unrelated user crontab line changed"
grep -qx '/usr/local/bin/other-thing &' "$rc_local" || fail "unrelated rc.local line changed"
grep -qx 'exit 0' "$rc_local" || fail "rc.local exit line changed"

# No stock-server line is left active.
if grep -Eq '^[[:space:]]*[^#[:space:]].*Server_(Ordinary|Mecanum)Wheels/(WebServer|APPServer|GUIServer|app)\.py' \
	"$root_cron" "$user_cron" "$rc_local"; then
	fail "a stock-server line is still active"
fi

# A second run is a no-op: nothing reported and nothing changed.
cp "$root_cron" "$tmp/root.before"
cp "$user_cron" "$tmp/user.before"
cp "$rc_local" "$tmp/rc.before"
out2="$(run)"
printf '%s\n' "$out2"
if printf '%s\n' "$out2" | grep -q "stock server autostart"; then fail "second run reported more changes"; fi
cmp -s "$root_cron" "$tmp/root.before" || fail "second run changed the root crontab"
cmp -s "$user_cron" "$tmp/user.before" || fail "second run changed the user crontab"
cmp -s "$rc_local" "$tmp/rc.before" || fail "second run changed rc.local"

# A host without the kit: unrelated crontab/rc.local content is left alone and nothing is reported.
none="$tmp/none"
mkdir -p "$none"
printf '0 5 * * * /usr/bin/echo hello\n' > "$none/cron"
printf 'PATH=/bin\n' > "$none/cron2"
printf '#!/bin/sh\nexit 0\n' > "$none/rc"
out3="$(OPENVIBE_INSTALL_STOCK_ONLY=1 OPENVIBE_CRONTAB_ROOT="$none/cron" OPENVIBE_CRONTAB_USER="$none/cron2" \
	OPENVIBE_RC_LOCAL="$none/rc" sh "$installer")"
printf '%s\n' "$out3"
if printf '%s\n' "$out3" | grep -q "stock server"; then fail "reported changes on a host without the kit"; fi
grep -qx '0 5 \* \* \* /usr/bin/echo hello' "$none/cron" || fail "unrelated cron line changed without the kit"
grep -qx '#!/bin/sh' "$none/rc" || fail "unrelated rc.local changed without the kit"

printf 'ok: install.sh stock-server crontab/rc.local handling\n'
