#!/bin/sh
# One auto-stop decision (design note 031, section 2.3). Run by
# devbox-autostop.service from a timer. Every program comes from PATH.
#
#   DEVBOX_LIFECYCLE_LIB          directory with gateway-status.sh and notify.sh
#   DEVBOX_LOG                    decision log (default /var/log/boot-history)
#   DEVBOX_AUTOSTOP_STATE         marker directory (default /run/devbox-autostop)
#   DEVBOX_AUTOSTOP_WINDOW_FILE   idle window (default /etc/devbox/autostop-after)
#   DEVBOX_AUTOSTOP_CAP_FILE      busy cap (default /etc/devbox/autostop-busy-cap)
#   DEVBOX_AUTOSTOP_DEFAULT_WINDOW, DEVBOX_AUTOSTOP_DEFAULT_CAP  (4h, 12h)
#   DEVBOX_GATEWAY_UNIT           (default ahp-gateway.service)
#   DEVBOX_GATEWAY_STOP_TIMEOUT   seconds to wait for the gateway to stop
#   DEVBOX_IMDS                   metadata base address
#
# Durations: a number with s, m, min, h or d (no unit means hours), or "off".
set -u
export LC_ALL=C

lib="${DEVBOX_LIFECYCLE_LIB:?}"
log_file="${DEVBOX_LOG:-/var/log/boot-history}"
state="${DEVBOX_AUTOSTOP_STATE:-/run/devbox-autostop}"
window_file="${DEVBOX_AUTOSTOP_WINDOW_FILE:-/etc/devbox/autostop-after}"
cap_file="${DEVBOX_AUTOSTOP_CAP_FILE:-/etc/devbox/autostop-busy-cap}"
unit="${DEVBOX_GATEWAY_UNIT:-ahp-gateway.service}"
stop_timeout="${DEVBOX_GATEWAY_STOP_TIMEOUT:-60}"
imds="${DEVBOX_IMDS:-http://169.254.169.254}"

stamp() { date '+%Y-%m-%d %H:%M:%S'; }
log() { printf '%s | auto-stop | %s\n' "$(stamp)" "$1" >>"$log_file" || echo "auto-stop: could not write $log_file" >&2; }

# seconds <value>: prints the seconds, "off", or nothing when unreadable.
seconds() {
  value="$(printf '%s' "$1" | tr -d '[:space:]')"
  case "$value" in
    off) echo off; return ;;
  esac
  number="$(printf '%s' "$value" | sed -n -E 's/^([0-9]+)(s|m|min|h|d)?$/\1/p')"
  [ -n "$number" ] || return 0
  case "$value" in
    *min | *m) echo $((number * 60)) ;;
    *s) echo "$number" ;;
    *d) echo $((number * 86400)) ;;
    *) echo $((number * 3600)) ;;
  esac
}

# setting <file> <default>: the file's duration, or the default's.
setting() {
  result=""
  [ -f "$1" ] && result="$(seconds "$(cat "$1" 2>/dev/null)")"
  [ -n "$result" ] || result="$(seconds "$2")"
  echo "$result"
}

window="$(setting "$window_file" "${DEVBOX_AUTOSTOP_DEFAULT_WINDOW:-4h}")"
cap="$(setting "$cap_file" "${DEVBOX_AUTOSTOP_DEFAULT_CAP:-12h}")"
# Row 1: off means nothing at all, except a stop the user asked for (row 10).
window_off=no
if [ "$window" = off ]; then
  [ -f "$state/stop-when-idle" ] || exit 0
  window_off=yes
  window=0
fi
[ -n "$window" ] || window=$((4 * 3600))
[ -n "$cap" ] || cap=$((12 * 3600))

mkdir -p "$state" || exit 1
now="$(date +%s)"
# The first poll of a boot starts the countdown.
[ -f "$state/last-active" ] || echo "$now" >"$state/last-active"
last_active="$(cat "$state/last-active")"
case "$last_active" in "" | *[!0-9]*) last_active="$now" ;; esac

# decide <decision>: log it when it differs from the previous poll's.
decide() {
  previous=""
  [ -f "$state/last" ] && previous="$(cat "$state/last")"
  [ "$1" = "$previous" ] || log "$1"
  printf '%s\n' "$1" >"$state/last"
}

# turns <status json>: "N running, M waiting" for notices.
turns() {
  printf '%s' "$1" | jq -r '"\(.turns.active - .turns.waitingForUser) running, \(.turns.waitingForUser) waiting"' 2>/dev/null
}

# imds <path>: one IMDSv2 read.
imds_token=""
imds() {
  if [ -z "$imds_token" ]; then
    imds_token="$(curl -s --max-time 2 -X PUT -H 'X-aws-ec2-metadata-token-ttl-seconds: 60' "$imds/latest/api/token")" || return 1
    case "$imds_token" in "" | *[!A-Za-z0-9_=+/-]*) imds_token=""; return 1 ;; esac
  fi
  # The session token goes on standard input, not on the command line.
  printf 'header = "X-aws-ec2-metadata-token: %s"\n' "$imds_token" |
    curl -s -f --max-time 2 -K - "$imds/latest/meta-data/$1"
}

# spot_interruption_under_way: the spot watcher has a current notice.
spot_interruption_under_way() {
  for notice in "${DEVBOX_SPOT_STATE:-/run/devbox-spot-watch}"/notice-*; do
    [ -f "$notice/seen" ] && [ ! -f "$notice/stale" ] && [ ! -f "$notice/done" ] && return 0
  done
  return 1
}

# stop_box <why> <notice>: notice, gateway, instance; in that order.
stop_box() {
  rm -f "$state/last"
  log "stopping: $1"
  sh "$lib/notify.sh" "auto-stop: $2"

  stopped_gateway=no
  if systemctl is-active --quiet "$unit"; then
    started="$(date +%s)"
    if timeout "$stop_timeout" systemctl stop "$unit"; then
      log "gateway stopped in $(($(date +%s) - started))s"
      stopped_gateway=yes
    else
      log "gateway did not stop cleanly within ${stop_timeout}s; the OS shutdown will try again"
    fi
  fi

  instance="$(imds instance-id)" && zone="$(imds placement/availability-zone)" &&
    [ -n "$instance" ] && [ -n "$zone" ] &&
    aws ec2 stop-instances --region "${zone%?}" --instance-ids "$instance" >/dev/null && exit 0

  # The next try is one full window (or cap) from now, not the next poll:
  # every try stops the gateway, and each stop interrupts whatever a person
  # has continued since. A requested stop is dropped; ask again.
  echo "$now" >"$state/last-active"
  [ -f "$state/busy-since" ] && echo "$now" >"$state/busy-since"
  rm -f "$state/stop-when-idle"
  log "stop-instances failed; the box stays up and tries again after a full window"
  sh "$lib/notify.sh" "auto-stop: stop-instances failed; the box stays up and tries again after a full idle window"
  if [ "$stopped_gateway" = yes ]; then
    if spot_interruption_under_way; then
      log "spot interruption under way; leaving the gateway stopped"
    else
      # Work interrupted for a stop that did not happen is picked up again.
      systemctl start --no-block "$unit"
    fi
  fi
  exit 1
}

gateway_out="$(sh "$lib/gateway-status.sh")"
gateway_exit=$?
gateway_state=""
gateway_json=""
case "$gateway_exit" in
  0)
    gateway_state="$(printf '%s\n' "$gateway_out" | sed -n 1p)"
    gateway_json="$(printf '%s\n' "$gateway_out" | sed -n 2p)"
    ;;
  2) gateway_state="unreachable: $gateway_out" ;;
  *) gateway_state="none" ;;
esac

idle_for=$((now - last_active))

# How long the gateway has been unreachable without a break. Right after
# boot, or while the gateway restarts, an unreachable answer says nothing
# about work it is about to continue. Measured in time, not polls, because a
# requested stop also runs a poll at once.
unreachable_for=0
if [ "$gateway_exit" -eq 2 ]; then
  [ -f "$state/unreachable-since" ] || echo "$now" >"$state/unreachable-since"
  unreachable_since="$(cat "$state/unreachable-since")"
  case "$unreachable_since" in "" | *[!0-9]*) unreachable_since="$now" ;; esac
  unreachable_for=$((now - unreachable_since))
else
  rm -f "$state/unreachable-since"
fi

# Row 10: asked to stop once nothing is working; logins do not count.
if [ -f "$state/stop-when-idle" ]; then
  case "$gateway_state" in
    idle | waiting | none) stop_box "requested, gateway ${gateway_state}" "stopping as requested, nothing is working" ;;
    unreachable:*)
      [ "$unreachable_for" -ge 1800 ] &&
        stop_box "requested, gateway ${gateway_state} for ${unreachable_for}s" "stopping as requested; the gateway has not answered for ${unreachable_for}s"
      ;;
  esac
fi
# With auto-stop off, only the request above can stop the box.
[ "$window_off" = yes ] && exit 0

# Row 2: someone is here.
if [ -n "$(who 2>/dev/null)" ] || [ -n "$(ss -tnH state established '( sport = :22 )' 2>/dev/null)" ]; then
  echo "$now" >"$state/last-active"
  rm -f "$state/busy-since"
  decide "active (login)"
  exit 0
fi

# Rows 3 and 4: an agent is working.
if [ "$gateway_state" = busy ]; then
  echo "$now" >"$state/last-active"
  [ -f "$state/busy-since" ] || echo "$now" >"$state/busy-since"
  busy_since="$(cat "$state/busy-since")"
  case "$busy_since" in "" | *[!0-9]*) busy_since="$now" ;; esac
  busy_for=$((now - busy_since))
  if [ "$cap" != off ] && [ "$busy_for" -ge "$cap" ]; then
    stop_box "busy ${busy_for}s >= cap ${cap}s" \
      "agents busy for ${busy_for}s, past the ${cap}s cap ($(turns "$gateway_json")); stopping anyway. Interrupted turns wait for Try Again at the next start."
  fi
  decide "active (agent busy)"
  exit 0
fi

# Rows 5 to 9: not activity.
rm -f "$state/busy-since"
case "$gateway_state" in
  waiting) reason="idle (agent waiting for a person)" ;;
  unreachable:*) reason="idle (gateway ${gateway_state})" ;;
  *) reason="idle" ;;
esac
if [ "$idle_for" -ge "$window" ]; then
  stop_box "idle ${idle_for}s >= ${window}s, ${reason}" "idle for ${idle_for}s (${reason}); stopping"
fi
decide "$reason"
exit 0
