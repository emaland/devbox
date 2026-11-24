#!/bin/sh
# The box-side command the devbox CLI runs over ssh (design note 031,
# section 2.6). Runs as the user.
#
#   devbox-agents status [--json]
#   devbox-agents stop-when-idle [cancel]
#
#   DEVBOX_LIFECYCLE_LIB         directory with gateway-status.sh
#   DEVBOX_AUTOSTOP_STATE        auto-stop's marker directory
#
# There is no resume command: since D24 the gateway never continues a turn
# by itself, and has no switch for it.
#
# Exit status: 0 done; 2 the gateway did not answer; 3 no gateway on this
# box; 64 usage.
set -u
export LC_ALL=C

lib="${DEVBOX_LIFECYCLE_LIB:?}"
autostop_state="${DEVBOX_AUTOSTOP_STATE:-/run/devbox-autostop}"

usage() {
  echo "usage: devbox-agents status [--json] | stop-when-idle [cancel]" >&2
  exit 64
}

# ask: sets state, json and reason from one status request.
ask() {
  out="$(sh "$lib/gateway-status.sh")"
  code=$?
  state="" json="" reason=""
  case "$code" in
    0)
      state="$(printf '%s\n' "$out" | sed -n 1p)"
      json="$(printf '%s\n' "$out" | sed -n 2p)"
      ;;
    2) state=unreachable reason="$out" ;;
    *) state=none ;;
  esac
  return "$code"
}

status() {
  ask
  code=$?
  if [ "${1:-}" = --json ]; then
    case "$code" in
      0) printf '%s' "$json" | jq -c --arg state "$state" '{state: $state, sessions: (.sessions.total // null), running: (.turns.active - .turns.waitingForUser), waiting: .turns.waitingForUser, pending: (.resume.pending // 0), interrupted: ((.resume | objects | .interrupted | numbers) // null)}' ;;
      2) jq -n -c --arg reason "$reason" '{state: "unreachable", reason: $reason}' ;;
      *) jq -n -c '{state: "none"}' ;;
    esac
  else
    case "$code" in
      0) printf '%s' "$json" | jq -r --arg state "$state" '"agents: \($state); \(.sessions.total // "?") sessions, \(.turns.active - .turns.waitingForUser) turns running, \(.turns.waitingForUser) waiting for a person"
           + ((.resume | objects | .interrupted | numbers) // 0 | floor
              | if . > 0 then "; \(.) interrupted, waiting for Try Again" else "" end)' ;;
      2) echo "agents: the gateway is not answering ($reason)" ;;
      *) echo "agents: no gateway on this box" ;;
    esac
  fi
  [ "$code" -eq 0 ] || [ "$code" -eq 2 ] || [ "$code" -eq 3 ] || code=2
  exit "$code"
}

stop_when_idle() {
  case "${1:-}" in
    "")
      sudo mkdir -p "$autostop_state" && sudo touch "$autostop_state/stop-when-idle" || exit 1
      echo "the box will stop once no agent is working (checked every 10 minutes, and now)"
      ;;
    cancel)
      sudo rm -f "$autostop_state/stop-when-idle" || exit 1
      echo "stop when idle: cancelled"
      exit 0
      ;;
    *) usage ;;
  esac
  sudo systemctl start --no-block devbox-autostop.service
}

case "${1:-}" in
  status) shift; status "$@" ;;
  stop-when-idle) shift; stop_when_idle "$@" ;;
  *) usage ;;
esac
