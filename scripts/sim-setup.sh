#!/usr/bin/env bash
#
# Boot an iOS Simulator, open Safari to a test page, and start ios_webkit_debug_proxy.
# Exports IWDP_SIM_WS_URL to stdout (last line) for tests to use.
#
# Usage:
#   eval "$(./scripts/sim-setup.sh)"           # sets IWDP_SIM_WS_URL
#   ./scripts/sim-setup.sh --teardown          # clean shutdown
#
set -euo pipefail

# The state lives in this user's private directory, never under a shared
# name that another user could plant: XDG_RUNTIME_DIR, else TMPDIR.
STATE_BASE="${XDG_RUNTIME_DIR:-${TMPDIR:-/tmp}}"
STATE_DIR="${STATE_BASE%/}/iwdp-sim-$(id -u)"
PIDFILE="$STATE_DIR/iwdp.pid"
SIMFILE="$STATE_DIR/device"
PORT=9221 # iwdp's listing port
TOKEN_ENV=IWDP_SIM_START_TOKEN # carries the start token of the iwdp this script starts

# check_private dir|file PATH fails unless PATH (not followed when it is a
# symlink) is this user's directory with mode 0700, or this user's regular
# file that group and others cannot write.
check_private() {
  python3 - "$1" "$2" <<'PY'
import os, stat, sys
kind, path = sys.argv[1], sys.argv[2]
try:
    st = os.lstat(path)
except OSError as e:
    sys.exit(f"{path}: {e.strerror}")
if kind == "dir":
    ok = stat.S_ISDIR(st.st_mode) and stat.S_IMODE(st.st_mode) == 0o700
else:
    ok = stat.S_ISREG(st.st_mode) and not st.st_mode & 0o022
if not ok or st.st_uid != os.getuid():
    sys.exit(f"{path} is not a private {kind} of this user")
PY
}

# write_state FILE TEXT writes TEXT to a new file of mode 0600 in STATE_DIR.
write_state() {
  rm -f "$1"
  (umask 077 && set -C && printf '%s\n' "$2" > "$1")
}

# port_free succeeds only when 127.0.0.1:$PORT and 0.0.0.0:$PORT can both be
# bound (with SO_REUSEADDR, as proxy.portFree): a listener, or a socket that
# holds the port without listening, makes it fail. A refused connection or
# a missing LISTEN row does not prove that the port is free.
port_free() {
  python3 - "$PORT" <<'PY'
import socket, sys
port = int(sys.argv[1])
for host in ("127.0.0.1", "0.0.0.0"):
    s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    try:
        s.bind((host, port))
    except OSError as e:
        sys.exit(f"{host}:{port}: {e.strerror}")
    finally:
        s.close()
PY
}

# listing_owners prints the pids of the processes that listen on $PORT.
listing_owners() {
  lsof -nP -t -iTCP:"$PORT" -sTCP:LISTEN 2>/dev/null || true
}

# proc_start prints the start time of process $1 (empty when it is gone),
# with single spaces, in the C locale.
proc_start() {
  LC_ALL=C ps -p "$1" -o lstart= 2>/dev/null | awk '{$1=$1; print}' || true
}

# new_token prints a random start token (128 bits, hex).
new_token() {
  python3 -c 'import secrets; print(secrets.token_hex(16))'
}

# proc_has_token succeeds only when one entry of the environment of
# process $1 is exactly $TOKEN_ENV=$2. Only Linux gives the environment
# alone with exact entry boundaries: /proc/<pid>/environ, NUL-separated,
# readable only for this user's processes. A token text in an argument, or
# inside another variable's value, is not that entry. On macOS, 'ps -E'
# prints the arguments and the environment in one space-separated column,
# so an argument cannot be told from an environment entry: the check
# fails there, and so does any read error.
proc_has_token() {
  [ "$(uname -s)" = Linux ] || return 1
  python3 - "$1" "$TOKEN_ENV=$2" <<'PY'
import sys
try:
    with open("/proc/%s/environ" % sys.argv[1], "rb") as f:
        entries = f.read().split(b"\0")
except OSError:
    sys.exit(1)
sys.exit(0 if sys.argv[2].encode() in entries else 1)
PY
}

# teardown stops only the ios_webkit_debug_proxy that this script started:
# $PIDFILE, a private file in the private STATE_DIR, holds its pid, its
# start token and its start time. A record alone never authorizes a
# signal, since another program of this user can write it: the live
# process must have the recorded start time and carry the token in its
# environment (proc_has_token). Otherwise nothing is stopped and the
# manual command is printed; on macOS, where the environment cannot be
# read exactly, that is always the case. A state file that is not private is not
# trusted. Another iwdp, such as the user's or another agent's, is never
# stopped.
teardown() {
  if [ -e "$PIDFILE" ] || [ -L "$PIDFILE" ]; then
    if check_private file "$PIDFILE"; then
      pid="" token="" started=""
      read -r pid token started < "$PIDFILE" || true
      case "$pid" in '' | *[!0-9]*) pid="" ;; esac
      if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
        if [ -n "$token" ] && [ -n "$started" ] && [ "$(proc_start "$pid")" = "$started" ] && proc_has_token "$pid" "$token"; then
          kill "$pid" 2>/dev/null || true
        elif [ "$(uname -s)" = Darwin ]; then
          echo "WARNING: on macOS this script cannot read the environment of process $pid exactly, so it cannot verify that process as the ios_webkit_debug_proxy it started; nothing was stopped." >&2
          echo "If it is yours, stop it by hand: kill $pid" >&2
        else
          echo "WARNING: process $pid is not verified as the ios_webkit_debug_proxy this script started (its start time or start token in $PIDFILE does not match, or its environment cannot be read); nothing was stopped." >&2
          echo "If it is yours, stop it by hand: kill $pid" >&2
        fi
      fi
    else
      echo "WARNING: $PIDFILE is not trusted; no process was stopped." >&2
    fi
    rm -f "$PIDFILE"
  fi
  if [ -e "$SIMFILE" ] || [ -L "$SIMFILE" ]; then
    if check_private file "$SIMFILE"; then
      xcrun simctl shutdown "$(cat "$SIMFILE")" 2>/dev/null || true
    else
      echo "WARNING: $SIMFILE is not trusted; no simulator was shut down." >&2
    fi
    rm -f "$SIMFILE"
  fi
}

mkdir -p "$STATE_BASE"
mkdir -m 700 "$STATE_DIR" 2>/dev/null || true
if ! check_private dir "$STATE_DIR"; then
  echo "ERROR: the state directory $STATE_DIR is not private (this user's, mode 0700, not a symlink); remove it and run this script again." >&2
  exit 1
fi

if [ "${1:-}" = "--teardown" ]; then
  teardown
  exit 0
fi

# Teardown any previous run.
teardown

# Never stop an iwdp that this script did not start: the user or another
# agent may use it. Refuse while any process holds the listing port.
if ! why=$(port_free 2>&1); then
  owners=$(listing_owners | xargs)
  echo "ERROR: port $PORT is in use ($why; owner pid: ${owners:-unknown})." >&2
  echo "It may be the iwdp of the user or another agent, so this script does not stop it." >&2
  echo "If it is yours, stop it (kill <pid>) and run this script again." >&2
  exit 1
fi

# 1. Find an available iPhone simulator.
# Prefer iOS 18 or lower — iOS 19+ has breaking WebKit Inspector Protocol changes
# where many domains (Runtime, DOM, etc.) return "domain was not found" through iwdp.
DEVICE_NAME=$(xcrun simctl list devices available --json | \
  python3 -c "
import json, sys, re
data = json.load(sys.stdin)
candidates = []
for runtime, devs in data['devices'].items():
    if 'iOS' not in runtime and 'SimRuntime.iOS' not in runtime:
        continue
    m = re.search(r'iOS[- ](\d+)', runtime)
    ver = int(m.group(1)) if m else 0
    for d in devs:
        if 'iPhone' in d['name'] and d['isAvailable']:
            candidates.append((ver, runtime, d['name']))
if not candidates:
    print('', end='')
    sys.exit(0)
# Sort: prefer iOS 18 and below (ver <= 18 first, descending), then 19+ as fallback
candidates.sort(key=lambda x: (0 if x[0] <= 18 else 1, -x[0]))
print(candidates[0][2])
")

if [ -z "$DEVICE_NAME" ]; then
  echo "ERROR: No available iPhone simulator found." >&2
  echo "Install one with: xcodebuild -downloadPlatform iOS" >&2
  exit 1
fi

echo "Booting simulator: $DEVICE_NAME" >&2
write_state "$SIMFILE" "$DEVICE_NAME"

# 2. Boot and wait.
xcrun simctl bootstatus "$DEVICE_NAME" -b 2>&1 | while read -r line; do
  echo "  [sim] $line" >&2
done

# 3. Open Safari to a test page.
echo "Opening Safari..." >&2
xcrun simctl openurl booted "https://example.com"
sleep 3

# 4. Find the webinspectord socket.
SOCKET_PATH=""
for i in $(seq 1 10); do
  SOCKET_PATH=$(lsof -aUc launchd_sim 2>/dev/null \
    | grep 'com.apple.webinspectord_sim.socket' \
    | head -1 | awk '{print $NF}') || true
  if [ -n "$SOCKET_PATH" ]; then
    break
  fi
  echo "  Waiting for webinspectord socket (attempt $i)..." >&2
  sleep 1
done

if [ -z "$SOCKET_PATH" ]; then
  echo "ERROR: Could not find webinspectord_sim.socket" >&2
  echo "The simulator may not have Safari Web Inspector available." >&2
  teardown
  exit 1
fi
echo "Found socket: $SOCKET_PATH" >&2

# 5. Start ios_webkit_debug_proxy (redirect stdout/stderr to stderr to avoid
#    contaminating the eval-able output on stdout).
TOKEN=$(new_token)
env "$TOKEN_ENV=$TOKEN" ios_webkit_debug_proxy -s "unix:$SOCKET_PATH" --no-frontend >&2 2>&1 &
IWDP_PID=$!
write_state "$PIDFILE" "$IWDP_PID $TOKEN $(proc_start "$IWDP_PID")"

# Wait until the started iwdp, not another process, serves the listing.
for i in $(seq 1 15); do
  if ! kill -0 "$IWDP_PID" 2>/dev/null; then
    echo "ERROR: ios_webkit_debug_proxy exited during its start" >&2
    teardown
    exit 1
  fi
  if listing_owners | grep -qx "$IWDP_PID" && curl -sf -m 5 "http://127.0.0.1:$PORT/json" > /dev/null 2>&1; then
    break
  fi
  if [ "$i" -eq 15 ]; then
    echo "ERROR: iwdp not responding on port 9221 after 15 seconds" >&2
    teardown
    exit 1
  fi
  sleep 1
done

# 6. Find the WebSocket URL by scanning all device ports (not just 9222,
#    since a physical device may also be connected and shift port assignments).
WS_URL=""
for i in $(seq 1 15); do
  WS_URL=$(curl -sf http://localhost:9221/json 2>/dev/null | python3 -c "
import json, sys, urllib.request
try:
    devices = json.load(sys.stdin)
    for dev in devices:
        url = dev.get('url', '')
        if not url:
            continue
        if not url.startswith('http'):
            url = 'http://' + url
        try:
            resp = urllib.request.urlopen(url + '/json', timeout=2)
            pages = json.loads(resp.read())
            for p in pages:
                ws = p.get('webSocketDebuggerUrl', '')
                if ws:
                    print(ws)
                    sys.exit(0)
        except:
            continue
except:
    pass
print('', end='')
" 2>/dev/null) || true
  if [ -n "$WS_URL" ]; then
    break
  fi
  echo "  Waiting for debuggable page (attempt $i)..." >&2
  sleep 2
done

if [ -z "$WS_URL" ]; then
  echo "ERROR: No debuggable Safari page found" >&2
  echo "Devices:" >&2
  curl -sf http://localhost:9221/json 2>/dev/null | python3 -m json.tool >&2 || true
  teardown
  exit 1
fi

echo "Simulator ready. WebSocket URL: $WS_URL" >&2
echo "export IWDP_SIM_WS_URL=\"$WS_URL\""
