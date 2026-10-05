#!/bin/sh
# run.sh — build and run the desktop conformance harness for ov_core.
#
# It fetches the pinned ArduinoJson v7 single header into esp32/test/.deps (gitignored), checks its SHA256, builds the
# harness with the same flags CI uses (g++ -std=c++17 -Wall -Wextra -Werror against src/ov_core.cpp) and runs it over
# internal/protocol/testdata/bot. Exit status is the harness's.
set -eu

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
root=$(CDPATH= cd -- "$here/../.." && pwd)
deps="$here/.deps"
header="$deps/ArduinoJson.h"

# Pinned ArduinoJson v7 release and the SHA256 of its single header.
json_version="7.4.2"
json_url="https://github.com/bblanchon/ArduinoJson/releases/download/v${json_version}/ArduinoJson-v${json_version}.h"
json_sha="a05ac98f4481d2398c103ca5ffcce8e2fcd7fa2fa1d8d9f38d5757454f448d97"

mkdir -p "$deps"

if ! (printf '%s  %s\n' "$json_sha" "$header" | sha256sum -c - >/dev/null 2>&1); then
  printf 'downloading ArduinoJson v%s ...\n' "$json_version"
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL --retry 2 -o "$header" "$json_url"
  elif command -v wget >/dev/null 2>&1; then
    wget -qO "$header" "$json_url"
  elif command -v python3 >/dev/null 2>&1; then
    python3 - "$header" "$json_url" <<'PY'
import sys, urllib.request
urllib.request.urlretrieve(sys.argv[2], sys.argv[1])
PY
  else
    printf 'error: need a downloader (see README) for ArduinoJson\n' >&2
    exit 1
  fi
fi

printf '%s  %s\n' "$json_sha" "$header" | sha256sum -c - >/dev/null

build="${TMPDIR:-/tmp}/openvibe-esp32-conformance.$$"
mkdir -p "$build"
trap 'rm -rf "$build"' EXIT

g++ -std=c++17 -Wall -Wextra -Werror \
  -I "$here/../src" -I "$deps" \
  "$here/../src/ov_core.cpp" "$here/conformance.cpp" \
  -o "$build/conformance"

"$build/conformance" "$root/internal/protocol/testdata/bot"
