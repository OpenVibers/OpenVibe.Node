#!/bin/sh
# Tests install.sh's flag parsing into the `openvibe-node pair` command: the legacy Bot form (--robot rob_… --code)
# unchanged, and the Network form (--network URL --pairing pair_… --code). OPENVIBE_INSTALL_PAIR_ARGS_ONLY=1 makes
# the installer print the arguments and exit before it downloads, installs or pairs anything.
set -eu

here="$(cd "$(dirname "$0")" && pwd)"
installer="$here/../install/install.sh"

fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

# args <installer flags…>: the pair arguments, space-joined.
args() { OPENVIBE_INSTALL_PAIR_ARGS_ONLY=1 sh "$installer" "$@" | tr '\n' ' ' | sed 's/ $//'; }

expect() {
	want="$1"
	shift
	got="$(args "$@")" || fail "install.sh $* exited non-zero"
	[ "$got" = "$want" ] || fail "install.sh $*: got '$got', want '$want'"
}

refuses() {
	if OPENVIBE_INSTALL_PAIR_ARGS_ONLY=1 sh "$installer" "$@" >/dev/null 2>&1; then fail "install.sh $* was accepted"; fi
}

robot=rob_01J8Z4M2Q0R7T9YV3K6N8P1W2X
pairing=pair_01J8Z4M2Q0R7T9YV3K6N8P1W2Y

expect "pair ABCD-1234 --robot $robot" --robot "$robot" --code ABCD-1234 --driver adeept
expect "pair ABCD-1234 --robot $robot --name Rover" --robot="$robot" --code=ABCD-1234 --name Rover
expect "pair ABCD-1234" ABCD-1234
expect "pair ABCD-1234 --network https://openvibe.network --pairing $pairing" \
	--network https://openvibe.network --pairing "$pairing" --code ABCD-1234 --driver adeept
expect "pair ABCD-1234 --network https://openvibe.network --pairing $pairing --name Rover" \
	--network=https://openvibe.network --pairing="$pairing" --code=ABCD-1234 --name=Rover
expect "pair ABCD-1234 --network https://openvibe.network" --network https://openvibe.network --pairing ABCD-1234

refuses --pairing "$pairing" --code ABCD-1234
refuses --network https://openvibe.network --pairing "$pairing"
refuses --network http://openvibe.network --pairing "$pairing" --code ABCD-1234
refuses --network https://openvibe.network --pairing nope --code ABCD-1234

echo "install.sh pair arguments: ok"
