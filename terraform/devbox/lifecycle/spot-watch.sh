#!/bin/sh
# Spot interruption watcher (design note 031, section 2.4). Polls the
# instance metadata service with IMDSv2 for an interruption notice and a
# rebalance recommendation. On an interruption notice it stops the gateway
# through systemd, so it can checkpoint inside the two-minute warning.
#
#   spot-watch.sh          poll forever
#   spot-watch.sh --once   one poll (tests)
#
#   DEVBOX_LIFECYCLE_LIB         directory with gateway-status.sh and notify.sh
#   DEVBOX_LOG                   decision log (default /var/log/boot-history)
#   DEVBOX_SPOT_STATE            state directory (default /run/devbox-spot-watch)
#   DEVBOX_SPOT_POLL_SECONDS     (default 5, as EC2 recommends)
#   DEVBOX_GATEWAY_UNIT          (default ahp-gateway.service)
#   DEVBOX_GATEWAY_STOP_TIMEOUT  seconds to wait for the gateway to stop
#   DEVBOX_IMDS                  metadata base address
#   DEVBOX_PROC_STAT             where boot time is read (default /proc/stat)
set -u
export LC_ALL=C

lib="${DEVBOX_LIFECYCLE_LIB:?}"
log_file="${DEVBOX_LOG:-/var/log/boot-history}"
state="${DEVBOX_SPOT_STATE:-/run/devbox-spot-watch}"
interval="${DEVBOX_SPOT_POLL_SECONDS:-5}"
unit="${DEVBOX_GATEWAY_UNIT:-ahp-gateway.service}"
stop_timeout="${DEVBOX_GATEWAY_STOP_TIMEOUT:-60}"
imds="${DEVBOX_IMDS:-http://169.254.169.254}"
proc_stat="${DEVBOX_PROC_STAT:-/proc/stat}"

token_ttl=21600
# A notice more than this far in the past when first seen is stale.
stale_after=120
# Still running this long after the action time: the interruption did not
# happen.
withdrawn_after=300

stamp() { date '+%Y-%m-%d %H:%M:%S'; }
log() { printf '%s | spot | %s\n' "$(stamp)" "$1" >>"$log_file" || echo "spot-watch: could not write $log_file" >&2; }

mkdir -p "$state" || exit 1
chmod 700 "$state"

boot_time="$(sed -n 's/^btime //p' "$proc_stat" 2>/dev/null)"
case "$boot_time" in "" | *[!0-9]*) boot_time=0 ;; esac

# reachability <ok|reason>: log when it changes.
reachability() {
  previous=""
  [ -f "$state/imds" ] && previous="$(cat "$state/imds")"
  if [ "$1" != "$previous" ]; then
    if [ "$1" = ok ]; then
      [ -n "$previous" ] && log "metadata reachable again"
    else
      log "metadata unreachable: $1"
    fi
  fi
  printf '%s\n' "$1" >"$state/imds"
}

# session_token: a valid IMDSv2 session token, renewed 5 min before expiry.
session_token() {
  now="$(date +%s)"
  if [ -f "$state/token" ] && [ -f "$state/token-expires" ] &&
    [ "$now" -lt "$(($(cat "$state/token-expires") - 300))" ]; then
    cat "$state/token"
    return 0
  fi
  new="$(curl -s -f --max-time 2 -X PUT -H "X-aws-ec2-metadata-token-ttl-seconds: $token_ttl" "$imds/latest/api/token")" || return 1
  [ -n "$new" ] || return 1
  (umask 077 && printf '%s\n' "$new" >"$state/token")
  echo "$((now + token_ttl))" >"$state/token-expires"
  printf '%s\n' "$new"
}

# read_item <path> <outfile>: prints the HTTP status; renews the session
# token once on 401.
read_item() {
  for attempt in 1 2; do
    token="$(session_token)" || { echo 000; return; }
    code="$(curl -s --max-time 2 -o "$2" -w '%{http_code}' -H "X-aws-ec2-metadata-token: $token" "$imds/latest/meta-data/$1")" || code=000
    if [ "$code" = 401 ] && [ "$attempt" = 1 ]; then
      rm -f "$state/token" "$state/token-expires"
      continue
    fi
    echo "$code"
    return
  done
}

# key <text>: a file name for a notice.
key() { printf '%s' "$1" | tr -c 'A-Za-z0-9' '_'; }

# summary: the gateway's sessions and turns, for a notice.
summary() {
  out="$(sh "$lib/gateway-status.sh")"
  case $? in
    0) printf '%s\n' "$out" | sed -n 2p | jq -r '"\(.sessions.total // "?") sessions, \(.turns.active - .turns.waitingForUser) turns running, \(.turns.waitingForUser) waiting"' 2>/dev/null ;;
    2) echo "gateway not answering ($out)" ;;
    *) echo "no gateway" ;;
  esac
}

on_interruption() {
  body="$1"
  action="$(jq -r '.action // empty' "$body" 2>/dev/null)"
  at="$(jq -r '.time // empty' "$body" 2>/dev/null)"
  [ -n "$action" ] && [ -n "$at" ] || { problem="instance-action is not the expected JSON"; return; }
  dir="$state/notice-$(key "$action-$at")"
  mkdir -p "$dir"

  if [ ! -f "$dir/seen" ]; then
    at_epoch="$(date -u -d "$at" +%s 2>/dev/null)" || at_epoch=0
    echo "$at_epoch" >"$dir/time"
    now="$(date +%s)"
    if [ "$at_epoch" -lt "$boot_time" ] || [ $((now - at_epoch)) -gt "$stale_after" ]; then
      touch "$dir/stale" "$dir/seen"
      log "stale interruption notice ignored: $action at $at"
      return
    fi
    touch "$dir/seen"
    log "interruption notice: $action at $at"
  fi
  [ -f "$dir/stale" ] && return

  if [ ! -f "$dir/stopped" ]; then
    sessions="$(summary)"
    if [ ! -f "$dir/noticed" ]; then
      (sh "$lib/notify.sh" "spot interruption: $action at $at; $sessions. Stopping the gateway so its turns can be continued." &&
        touch "$dir/noticed") &
    fi
    if systemctl is-active --quiet "$unit"; then
      started="$(date +%s)"
      if timeout "$stop_timeout" systemctl stop "$unit"; then
        log "gateway stopped in $(($(date +%s) - started))s ($sessions)"
        echo ours >"$dir/stopped"
      else
        log "gateway did not stop within ${stop_timeout}s"
      fi
    else
      echo "not running" >"$dir/stopped"
    fi
    wait
  fi
}

# check_withdrawn: a gateway this watcher stopped is started again when the
# instance is still running well after the action time.
check_withdrawn() {
  for dir in "$state"/notice-*; do
    [ -d "$dir" ] || continue
    [ -f "$dir/done" ] && continue
    [ "$(cat "$dir/stopped" 2>/dev/null)" = ours ] || continue
    at_epoch="$(cat "$dir/time" 2>/dev/null)"
    case "$at_epoch" in "" | *[!0-9]*) continue ;; esac
    if [ "$(date +%s)" -ge $((at_epoch + withdrawn_after)) ]; then
      touch "$dir/done"
      log "interruption did not happen; starting the gateway again"
      systemctl start --no-block "$unit"
      sh "$lib/notify.sh" "the spot interruption did not happen; the gateway is started again"
    fi
  done
}

on_rebalance() {
  at="$(jq -r '.noticeTime // empty' "$1" 2>/dev/null)"
  [ -n "$at" ] || { problem="rebalance recommendation is not the expected JSON"; return; }
  dir="$state/rebalance-$(key "$at")"
  [ -d "$dir" ] && return
  mkdir -p "$dir"
  log "rebalance recommendation at $at (no action)"
  sh "$lib/notify.sh" "spot rebalance recommendation at $at; an interruption may follow. $(summary)."
}

# One reachability result per poll: the first failure, or ok.
poll() {
  body="$(mktemp)" || return
  problem=""
  code="$(read_item spot/instance-action "$body")"
  case "$code" in
    200) on_interruption "$body" ;;
    404) ;;
    *) problem="instance-action answered HTTP $code" ;;
  esac
  code="$(read_item events/recommendations/rebalance "$body")"
  case "$code" in
    200) on_rebalance "$body" ;;
    404) ;;
    *) [ -n "$problem" ] || problem="metadata answered HTTP $code" ;;
  esac
  rm -f "$body"
  reachability "${problem:-ok}"
  check_withdrawn
}

if [ "${1:-}" = --once ]; then
  poll
  exit 0
fi
while :; do
  poll
  sleep "$interval"
done
