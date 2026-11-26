#!/usr/bin/env bash
#
# Boot an iOS Simulator, open Safari to a test page of its own, and start
# ios_webkit_debug_proxy. Prints, on stdout, the export lines of
# AGENT_IOS_SIM_WS_URL (the WebSocket URL of that page) and AGENT_IOS_SIM_PAGE_URL
# (its URL, unique by a random query) for the simulator tests. Exits
# non-zero, after its teardown, when a step fails; check the exit status
# before you evaluate the output.
#
# Run it through 'make test-e2e' (setup, simulator tests, teardown) or
# 'make sim-setup', which check its exit status and both URLs. One
# Simulator run per user account at a time: setup refuses while another
# run holds the lease. Wait until that run has finished; only after a
# crashed run, recover with 'make sim-teardown'.
#
# Usage:
#   ./scripts/sim-setup.sh                         # setup; prints the export lines,
#                                                  # also AGENT_IOS_SIM_RUN_TOKEN
#   ./scripts/sim-setup.sh --teardown-run TOKEN    # automatic teardown of the run TOKEN
#                                                  # only; no-op for an empty TOKEN
#   ./scripts/sim-setup.sh --teardown              # manual recovery after a crashed run
#   ./scripts/sim-setup.sh --boot-simulator        # steps 1-2 only (tests)
#   ./scripts/sim-setup.sh --select-page LISTING_URL PAGE_URL
#       # prints the WebSocket URL of the SIMULATOR's page PAGE_URL in the
#       # iwdp listing LISTING_URL (one attempt; tests)
#
set -euo pipefail

# The state lives in this user's private directory, never under a shared
# name that another user could plant: XDG_RUNTIME_DIR, else TMPDIR.
STATE_BASE="${XDG_RUNTIME_DIR:-${TMPDIR:-/tmp}}"
STATE_DIR="${STATE_BASE%/}/iwdp-sim-$(id -u)"
PIDFILE="$STATE_DIR/iwdp.pid"
# The state is a lease of one setup run: LEASEFILE holds that run's token
# and is created atomically, so while it exists every other setup
# refuses. SIMFILE holds "<run token> <udid> booted|reused": "booted" only
# when that run's own 'simctl boot' succeeded. Teardown releases the lease.
LEASEFILE="$STATE_DIR/lease"
SIMFILE="$STATE_DIR/device"
# Every ownership step (take the lease and boot; check the token, shut
# down and release the lease) runs while this script holds LOCKDIR, a
# directory that mkdir(2) creates atomically (macOS has no flock command).
LOCKDIR="$STATE_DIR/lock"
LOCK_WAIT_SECONDS="${AGENT_IOS_SIM_LOCK_WAIT_SECONDS:-30}"
HAVE_LOCK=""
PORT=9221 # iwdp's listing port
# iwdp-owner-check.sh proves that a recorded process is the iwdp that this
# script started; teardown signals nothing without its "verified".
OWNER_CHECK="$(dirname "${BASH_SOURCE[0]}")/iwdp-owner-check.sh"
# How long teardown waits for its iwdp to exit after SIGTERM.
STOP_WAIT_SECONDS="${AGENT_IOS_SIM_STOP_WAIT_SECONDS:-5}"

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

# write_state FILE TEXT replaces FILE with TEXT, mode 0600. It writes a
# new temporary file next to FILE and renames it to FILE, so FILE always
# holds either its old or its new text, never a partial one or none.
write_state() {
  local tmp
  tmp=$(umask 077 && mktemp "$1.XXXXXX") || return 1
  if ! printf '%s\n' "$2" > "$tmp" || ! mv -f "$tmp" "$1"; then
    rm -f "$tmp"
    return 1
  fi
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

# lock takes LOCKDIR, retrying every 0.1 s for LOCK_WAIT_SECONDS; it
# fails with advice when the lock stays taken. unlock releases it.
#
# The lock is taken with mkdir(2) through Python, which fails when the
# directory exists, and never with mkdir(1): uutils coreutils' mkdir
# (Ubuntu 26.04's mkdir) checks for the directory first and reports
# success when another process creates it between its check and its
# mkdir(2), so two setups that start together would both hold the lock.
lock() {
  local tries=$((LOCK_WAIT_SECONDS * 10)) i=0
  until python3 -c 'import os, sys; os.mkdir(sys.argv[1], 0o700)' "$LOCKDIR" 2> /dev/null; do
    if [ "$i" -eq 0 ]; then
      echo "Waiting for $LOCKDIR: another setup or teardown of this user runs..." >&2
    fi
    i=$((i + 1))
    if [ "$i" -ge "$tries" ]; then
      echo "ERROR: $LOCKDIR stayed taken for $LOCK_WAIT_SECONDS s: another setup or teardown of this user runs, or one crashed. When none runs, remove it (rmdir $LOCKDIR) and try again." >&2
      return 1
    fi
    sleep 0.1
  done
  HAVE_LOCK=1
}

unlock() {
  if [ -n "$HAVE_LOCK" ]; then
    rmdir "$LOCKDIR"
    HAVE_LOCK=""
  fi
}

# lease_token prints the token of the lease, empty when there is none or
# it is not a private file.
lease_token() {
  local t=""
  if [ -e "$LEASEFILE" ] || [ -L "$LEASEFILE" ]; then
    if check_private file "$LEASEFILE"; then
      read -r t < "$LEASEFILE" || true
    else
      echo "WARNING: $LEASEFILE is not trusted." >&2
    fi
  fi
  printf '%s' "$t"
}

# stop_iwdp stops the ios_webkit_debug_proxy that setup started and
# recorded in $PIDFILE ("<pid> <start token> <start time>"), and only that
# process. A record alone never authorizes a signal, since another program
# of this user can write it. 'iwdp-owner-check.sh --terminate' checks the
# process from one read of the record (the live process has the recorded
# start time, carries the start token in its environment and runs
# ios_webkit_debug_proxy) and, only on "verified", sends SIGTERM to that
# process itself; this function never reads a pid for a signal. Then it
# runs the plain check until it reports "gone", for up to
# STOP_WAIT_SECONDS. When a check reports "gone", forget_iwdp_record
# removes the record (see there).
# In every other case (no match, a check that cannot be made, a process
# that still runs after SIGTERM) the record stays, so the next teardown
# checks it again, and a warning gives the check's reason, which never
# holds a token or another process's command line.
stop_iwdp() {
  local out rc=0 end
  out=$(bash "$OWNER_CHECK" --terminate "$PIDFILE") || rc=$?
  case "$rc" in
    3) forget_iwdp_record || true; return 0 ;;
    0) ;;
    *)
      echo "WARNING: the ios_webkit_debug_proxy that setup started was left running, and no signal was sent (${out:-the identity check failed}). Its record stays in $PIDFILE; the next teardown checks it again." >&2
      return 0
      ;;
  esac
  end=$((SECONDS + STOP_WAIT_SECONDS))
  while :; do
    rc=0
    out=$(bash "$OWNER_CHECK" "$PIDFILE") || rc=$?
    if [ "$rc" -eq 3 ]; then
      if forget_iwdp_record; then
        echo "Stopped the ios_webkit_debug_proxy that setup started (SIGTERM)." >&2
      fi
      return 0
    fi
    if [ "$SECONDS" -ge "$end" ]; then break; fi
    sleep 0.1
  done
  echo "WARNING: the ios_webkit_debug_proxy that setup started got SIGTERM but did not exit within $STOP_WAIT_SECONDS s ($out). Its record stays in $PIDFILE; the next teardown checks it again. Report it; do not stop it with a command of your own." >&2
}

# forget_iwdp_record removes $PIDFILE after a check said "gone", with
# 'iwdp-owner-check.sh --forget': that reads the record again and removes
# it only when the process that this read names is gone and the path
# still names the same file with the same content. A record that names a
# live process stays, and so does one that changed after that read; one
# that replaced the checked record before that read is removed when its
# process is gone, too. It fails, with a warning, when nothing was
# removed. When another file took the path during the comparison,
# --forget keeps the changed record under a private name, which its
# reason gives ("it was kept as <path>"); no teardown checks that file,
# so the warning says to check it by hand.
forget_iwdp_record() {
  local out rc=0
  out=$(bash "$OWNER_CHECK" --forget "$PIDFILE") || rc=$?
  if [ "$rc" -eq 3 ]; then return 0; fi
  case "$out" in
    *"; it was kept as "*)
      echo "WARNING: the ios_webkit_debug_proxy that setup started is gone, but its record was not removed ($out). The next teardown does not check that file, and the next setup does not refuse because of it: check it by hand as docs/device-runbook.md (Stopping an iwdp by hand) describes. The file that now has the record's name stays in $PIDFILE; the next teardown checks it again." >&2
      ;;
    *)
      echo "WARNING: the ios_webkit_debug_proxy that setup started is gone, but $PIDFILE was not removed (${out:-the identity check failed}): the record that the removal read names a live process, changed after that read, or cannot be checked. The record stays in $PIDFILE; the next teardown checks it again." >&2
      ;;
  esac
  return 1
}

# teardown [RUN_TOKEN] runs under the lock (it takes it unless this
# process holds it). With RUN_TOKEN, the automatic teardown of one run, it
# acts only while the lease is that run's: it checks the token under the
# lock first, again right before the shutdown and right before it removes
# the lease, and does nothing for another run's lease or no lease. Without
# RUN_TOKEN it is the manual recovery of a crashed run.
#
# It stops only the ios_webkit_debug_proxy that this script started
# (stop_iwdp). Another iwdp, such as the user's or another agent's, is
# never stopped.
teardown() {
  local want="${1:-}" took=""
  if [ -z "$HAVE_LOCK" ]; then
    lock || return 1
    took=1
  fi
  lease=$(lease_token)
  if [ -n "$want" ] && [ "$lease" != "$want" ]; then
    echo "The lease in $STATE_DIR is not this run's (or there is none); nothing was torn down." >&2
    if [ -n "$took" ]; then unlock; fi
    return 0
  fi
  if [ -e "$PIDFILE" ] || [ -L "$PIDFILE" ]; then
    stop_iwdp
  fi
  # Shut down only a Simulator that the lease holder's own 'simctl boot'
  # booted ("booted"); one that someone else booted ("reused") may serve
  # another task, and a record of another run is not this lease's.
  if [ -e "$SIMFILE" ] || [ -L "$SIMFILE" ]; then
    if check_private file "$SIMFILE"; then
      run="" udid="" owner=""
      read -r run udid owner < "$SIMFILE" || true
      case "$udid" in '' | *[!0-9A-Fa-f-]*) udid="" ;; esac
      if [ -z "$lease" ] || [ "$run" != "$lease" ]; then
        echo "WARNING: $SIMFILE was not written by the setup that holds the lease $LEASEFILE; no simulator was shut down." >&2
      elif [ -n "$udid" ] && [ "$owner" = booted ] && [ "$(lease_token)" = "$lease" ]; then
        xcrun simctl shutdown "$udid" || echo "WARNING: could not shut down the Simulator $udid that setup booted; shut it down by hand: xcrun simctl shutdown $udid" >&2
      elif [ -n "$udid" ] && [ "$owner" = reused ]; then
        echo "The Simulator $udid was not booted by setup; it stays booted." >&2
      else
        echo "WARNING: $SIMFILE does not name a Simulator that setup booted; no simulator was shut down." >&2
      fi
    else
      echo "WARNING: $SIMFILE is not trusted; no simulator was shut down." >&2
    fi
    rm -f "$SIMFILE"
  fi
  if [ -z "$want" ] || [ "$(lease_token)" = "$want" ]; then
    rm -f "$LEASEFILE"
  fi
  if [ -n "$took" ]; then unlock; fi
}

# choose_simulator prints the UDID of an available iPhone
# Simulator, read from 'xcrun simctl list devices available --json'.
# Prefer iOS 18 or lower — iOS 19+ has breaking WebKit Inspector Protocol changes
# where many domains (Runtime, DOM, etc.) return "domain was not found" through iwdp.
choose_simulator() {
  xcrun simctl list devices available --json | python3 -c "
import json, sys, re
data = json.load(sys.stdin)
udid_re = re.compile(r'[0-9A-Fa-f]{8}-(?:[0-9A-Fa-f]{4}-){3}[0-9A-Fa-f]{12}')
candidates = []
for runtime, devs in data['devices'].items():
    if 'iOS' not in runtime and 'SimRuntime.iOS' not in runtime:
        continue
    m = re.search(r'iOS[- ](\d+)', runtime)
    ver = int(m.group(1)) if m else 0
    for d in devs:
        if 'iPhone' in d['name'] and d['isAvailable'] and udid_re.fullmatch(d['udid']):
            candidates.append((ver, d['udid']))
if not candidates:
    sys.exit('no available iPhone simulator')
# Sort: prefer iOS 18 and below (ver <= 18 first, descending), then 19+ as fallback
candidates.sort(key=lambda x: (0 if x[0] <= 18 else 1, -x[0]))
print(candidates[0][1])
"
}

# sim_state UDID prints the state of the Simulator UDID ("Booted",
# "Shutdown", ...), read from 'xcrun simctl list devices --json'; empty
# when it is not listed.
sim_state() {
  xcrun simctl list devices --json | python3 -c "
import json, sys
for devs in json.load(sys.stdin)['devices'].values():
    for d in devs:
        if d.get('udid') == sys.argv[1]:
            print(d.get('state', ''))
            sys.exit(0)
" "$1"
}

# boot_simulator chooses a Simulator, boots it, records it in $SIMFILE
# with its owner, and waits until it has booted. SIM_UDID names it
# afterwards. Ownership comes only from this run's own boot: "booted" when
# 'xcrun simctl boot' succeeds, "reused" when the Simulator is not
# Shutdown right before the boot, or the boot fails because it is already
# booted or booting (someone else started it, also between our check and
# our boot); teardown never shuts down a "reused" one. Any other boot
# failure fails the setup with nothing recorded.
boot_simulator() {
  local boot_err state
  if ! SIM_UDID=$(choose_simulator); then
    echo "ERROR: No available iPhone simulator found." >&2
    echo "Install one with: xcodebuild -downloadPlatform iOS" >&2
    return 1
  fi
  echo "Booting simulator: $SIM_UDID" >&2
  state=$(sim_state "$SIM_UDID")
  if [ "$state" != Shutdown ]; then
    write_state "$SIMFILE" "$RUN_TOKEN $SIM_UDID reused"
    echo "The Simulator $SIM_UDID is already ${state:-in an unknown state}, not booted by this setup; teardown leaves it running." >&2
  elif boot_err=$(xcrun simctl boot "$SIM_UDID" 2>&1); then
    write_state "$SIMFILE" "$RUN_TOKEN $SIM_UDID booted"
  else
    state=$(sim_state "$SIM_UDID")
    case "$state" in
      Booted | Booting)
        write_state "$SIMFILE" "$RUN_TOKEN $SIM_UDID reused"
        echo "The Simulator $SIM_UDID is already $state, not booted by this setup; teardown leaves it running." >&2
        ;;
      *)
        echo "ERROR: xcrun simctl boot $SIM_UDID failed (state now: ${state:-unknown}): $boot_err" >&2
        return 1
        ;;
    esac
  fi
}

# wait_boot waits until the Simulator SIM_UDID has booted.
wait_boot() {
  xcrun simctl bootstatus "$SIM_UDID" 2>&1 | while read -r line; do
    echo "  [sim] $line" >&2
  done
}

# select_page LISTING_URL PAGE_URL prints the webSocketDebuggerUrl of the
# one page with the URL PAGE_URL on the one SIMULATOR entry of the iwdp
# listing LISTING_URL. It fails, with the reason on stderr, when the
# listing has no or several SIMULATOR entries, or the Simulator lists no
# or several such pages: a page of another device or another tab is never
# selected. iwdp 1.9.2 (ios_webkit_debug_proxy.c) lists the Simulator with
# deviceId "SIMULATOR" and url "host:port", and each page with its url and
# webSocketDebuggerUrl "ws://host:port/devtools/page/N".
select_page() {
  python3 - "$1" "$2" <<'PY'
import json, re, sys, urllib.request
listing, page_url = sys.argv[1].rstrip("/"), sys.argv[2]

def get_list(url):
    try:
        with urllib.request.urlopen(url, timeout=2) as resp:
            data = json.load(resp)
    except (OSError, ValueError) as e:
        sys.exit(f"{url}: {e}")
    if not isinstance(data, list):
        sys.exit(f"{url}: not a JSON list")
    return [e for e in data if isinstance(e, dict)]

sims = [d for d in get_list(listing + "/json") if d.get("deviceId") == "SIMULATOR"]
if len(sims) != 1:
    sys.exit(f"the iwdp listing {listing}/json has {len(sims)} SIMULATOR entries; want 1")
dev = sims[0].get("url", "")
if not dev.startswith("http"):
    dev = "http://" + dev
pages = [p for p in get_list(dev + "/json") if p.get("url") == page_url]
if len(pages) != 1:
    sys.exit(f"the SIMULATOR ({dev}/json) lists {len(pages)} pages with the URL {page_url}; want 1")
ws = pages[0].get("webSocketDebuggerUrl", "")
if not re.fullmatch(r"ws://[A-Za-z0-9.:\[\]-]+/devtools/page/[0-9]+", ws):
    sys.exit(f"the SIMULATOR's page {page_url} has no usable webSocketDebuggerUrl: {ws!r}")
print(ws)
PY
}

case "${1:-}" in
  --select-page)
    [ "$#" -eq 3 ] || { echo "usage: $0 --select-page LISTING_URL PAGE_URL" >&2; exit 2; }
    select_page "$2" "$3"
    exit
    ;;
  --teardown-run)
    [ "$#" -eq 2 ] || { echo "usage: $0 --teardown-run RUN_TOKEN (empty when the run took no lease)" >&2; exit 2; }
    ;;
  '' | --teardown | --boot-simulator) ;;
  *)
    echo "ERROR: unknown argument $1 (want --teardown-run, --teardown, --boot-simulator or --select-page)" >&2
    exit 2
    ;;
esac

mkdir -p "$STATE_BASE"
mkdir -m 700 "$STATE_DIR" 2>/dev/null || true
if ! check_private dir "$STATE_DIR"; then
  echo "ERROR: the state directory $STATE_DIR is not private (this user's, mode 0700, not a symlink); remove it and run this script again." >&2
  exit 1
fi

if [ "${1:-}" = "--teardown-run" ]; then
  if [ -z "$2" ]; then
    echo "No run token: this run took no lease, so nothing is torn down." >&2
    exit 0
  fi
  teardown "$2"
  exit
fi
if [ "${1:-}" = "--teardown" ]; then
  teardown
  exit
fi

# Take the lease. An earlier setup that is still recorded may still be in
# use: never tear it down here; its owner runs the teardown. The lease
# file is created with noclobber, so of two setups that start together
# only one gets it.
RUN_TOKEN=$(new_token)
lock
if [ -e "$PIDFILE" ] || [ -L "$PIDFILE" ] || [ -e "$SIMFILE" ] || [ -L "$SIMFILE" ] ||
  ! (umask 077 && set -C && printf '%s\n' "$RUN_TOKEN" > "$LEASEFILE") 2> /dev/null; then
  unlock
  echo "ERROR: another Simulator run of this user holds the lease in $STATE_DIR (one run per user account at a time)." >&2
  echo "Wait until that run has finished. Only after a crashed run, recover with 'make sim-teardown' (or $0 --teardown), then run the setup again." >&2
  exit 1
fi

# From here on, every failure exit tears down what this run started, and
# only that: the teardown checks this run's token.
trap 'rc=$?; if [ "$rc" -ne 0 ]; then echo "ERROR: setup failed (exit $rc); tearing down what it started." >&2; teardown "$RUN_TOKEN"; fi; unlock' EXIT

if [ "${1:-}" = "--boot-simulator" ]; then
  boot_simulator
  unlock
  wait_boot
  echo "export AGENT_IOS_SIM_RUN_TOKEN='$RUN_TOKEN'"
  exit 0
fi

# Never stop an iwdp that this script did not start: the user or another
# agent may use it. Refuse while any process holds the listing port.
if ! why=$(port_free 2>&1); then
  owners=$(listing_owners | xargs)
  echo "ERROR: port $PORT is in use ($why; owner pid: ${owners:-unknown})." >&2
  echo "It may be the iwdp of the user or another agent, so this script does not stop it." >&2
  echo "Wait until the port is free, or ask whoever started that process; stop it only when you started it yourself and nobody else uses it, as docs/device-runbook.md (Stopping an iwdp by hand) describes. Then run this script again." >&2
  exit 1
fi

# 1-2. Choose, boot and record under the lock; then wait for the boot.
boot_simulator
unlock
wait_boot

# 3. Open Safari to a test page of this run: its random query tells it
#    from every other tab and device.
#    Right after a fresh boot, openurl can time out (exit 60) while
#    SpringBoard is still starting; it is tried 3 times, each with a new
#    token, so a page that an earlier attempt opened late is never selected.
for attempt in 1 2 3; do
  SIM_PAGE_URL="https://example.com/?iwdp-sim=$(new_token)"
  echo "Opening Safari at $SIM_PAGE_URL (attempt $attempt)..." >&2
  rc=0
  xcrun simctl openurl "$SIM_UDID" "$SIM_PAGE_URL" || rc=$?
  [ "$rc" -eq 0 ] && break
  [ "$attempt" -eq 3 ] && exit "$rc"
  echo "  openurl failed (exit $rc); trying again in 5 s" >&2
  sleep 5
done
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
  exit 1
fi
echo "Found socket: $SOCKET_PATH" >&2

# 5. Start ios_webkit_debug_proxy (redirect stdout/stderr to stderr to avoid
#    contaminating the eval-able output on stdout).
#    The start token goes into iwdp's environment by an assignment, never
#    in an argument that ps would show. The record holds the pid, the
#    token and the start time, which exec does not change; teardown proves
#    the process with iwdp-owner-check.sh before it signals it.
TOKEN=$(new_token)
AGENT_IOS_SIM_START_TOKEN=$TOKEN ios_webkit_debug_proxy -s "unix:$SOCKET_PATH" --no-frontend >&2 2>&1 &
IWDP_PID=$!
write_state "$PIDFILE" "$IWDP_PID $TOKEN $(proc_start "$IWDP_PID")"

# Wait until the started iwdp, not another process, serves the listing.
for i in $(seq 1 15); do
  if ! kill -0 "$IWDP_PID" 2>/dev/null; then
    echo "ERROR: ios_webkit_debug_proxy exited during its start" >&2
    exit 1
  fi
  if listing_owners | grep -qx "$IWDP_PID" && curl -sf -m 5 "http://127.0.0.1:$PORT/json" > /dev/null 2>&1; then
    break
  fi
  if [ "$i" -eq 15 ]; then
    echo "ERROR: iwdp not responding on port 9221 after 15 seconds" >&2
    exit 1
  fi
  sleep 1
done

# 6. Wait until the Simulator lists the test page, and take its WebSocket
#    URL; never a page of another device or another tab.
WS_URL="" reason=""
for i in $(seq 1 15); do
  if out=$(select_page "http://127.0.0.1:$PORT" "$SIM_PAGE_URL" 2>&1); then
    WS_URL=$out
    break
  fi
  reason=$out
  echo "  Waiting for the test page (attempt $i): $reason" >&2
  sleep 2
done

if [ -z "$WS_URL" ]; then
  echo "ERROR: the Simulator did not list the test page $SIM_PAGE_URL: $reason" >&2
  echo "Devices:" >&2
  curl -sf "http://127.0.0.1:$PORT/json" 2>/dev/null | python3 -m json.tool >&2 || true
  exit 1
fi

echo "Simulator ready. WebSocket URL: $WS_URL" >&2
# select_page allows only [A-Za-z0-9.:[]-] and "/" in WS_URL, and
# SIM_PAGE_URL has a hex token: neither can hold a quote.
echo "export AGENT_IOS_SIM_WS_URL='$WS_URL'"
echo "export AGENT_IOS_SIM_PAGE_URL='$SIM_PAGE_URL'"
echo "export AGENT_IOS_SIM_RUN_TOKEN='$RUN_TOKEN'"
