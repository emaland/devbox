#!/bin/sh
# The box-side command the devbox CLI runs over ssh (design note 031,
# section 2.6). Runs as the user.
#
#   devbox-agents status [--json]
#   devbox-agents resume status|on|off
#   devbox-agents stop-when-idle [cancel]
#
#   DEVBOX_LIFECYCLE_LIB         directory with gateway-status.sh
#   DEVBOX_GATEWAY_COMMAND       absolute path of the gateway module's wrapper
#   DEVBOX_GATEWAY_HAS_RESUME    1 when the wrapper offers `resume`
#   DEVBOX_AUTOSTOP_STATE        auto-stop's marker directory
#
# Exit status: 0 done; 2 the gateway did not answer; 3 no gateway on this
# box; 4 not offered by this gateway build; 64 usage.
set -u
export LC_ALL=C

lib="${DEVBOX_LIFECYCLE_LIB:?}"
gateway_command="${DEVBOX_GATEWAY_COMMAND:-}"
has_resume="${DEVBOX_GATEWAY_HAS_RESUME:-0}"
autostop_state="${DEVBOX_AUTOSTOP_STATE:-/run/devbox-autostop}"

usage() {
  echo "usage: devbox-agents status [--json] | resume status|on|off | stop-when-idle [cancel]" >&2
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
      0) printf '%s' "$json" | jq -c --arg state "$state" '{state: $state, sessions: (.sessions.total // null), running: (.turns.active - .turns.waitingForUser), waiting: .turns.waitingForUser, pending: (.resume.pending // 0), resume: (.resume // null)}' ;;
      2) jq -n -c --arg reason "$reason" '{state: "unreachable", reason: $reason}' ;;
      *) jq -n -c '{state: "none"}' ;;
    esac
  else
    case "$code" in
      0) printf '%s' "$json" | jq -r --arg state "$state" '"agents: \($state); \(.sessions.total // "?") sessions, \(.turns.active - .turns.waitingForUser) turns running, \(.turns.waitingForUser) waiting for a person"' ;;
      2) echo "agents: the gateway is not answering ($reason)" ;;
      *) echo "agents: no gateway on this box" ;;
    esac
  fi
  [ "$code" -eq 0 ] || [ "$code" -eq 2 ] || [ "$code" -eq 3 ] || code=2
  exit "$code"
}

resume() {
  case "${1:-}" in
    status)
      ask
      code=$?
      [ "$code" -eq 0 ] || {
        [ "$code" -eq 2 ] && echo "auto-resume: unknown, the gateway is not answering ($reason)" || echo "auto-resume: no gateway on this box"
        exit "$code"
      }
      printf '%s' "$json" | jq -e '.resume | type == "object"' >/dev/null 2>&1 || {
        echo "auto-resume: not reported by this gateway build"
        exit 4
      }
      printf '%s' "$json" | jq -r '.resume | "auto-resume: \(.policy) (\(.source)); pending \(.pending // 0), continued \(.resumedSinceStart // 0), failed \(.failed // 0), skipped \(.skipped // 0)"'
      ;;
    on | off)
      # Only the gateway's own command changes the switch. Never run the
      # wrapper for a subcommand it does not offer: for a build without
      # subcommands that would start a second server.
      # Only the module's own wrapper, by absolute path: a copy of the
      # gateway earlier on the user's PATH could be a build without
      # subcommands.
      case "$gateway_command" in
        /*) ;;
        *) gateway_command="" ;;
      esac
      if [ "$has_resume" != 1 ] || [ -z "$gateway_command" ] || [ ! -x "$gateway_command" ]; then
        echo "auto-resume: this gateway build does not offer the resume command" >&2
        exit 4
      fi
      exec "$gateway_command" resume "$1"
      ;;
    *) usage ;;
  esac
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
  resume) shift; resume "$@" ;;
  stop-when-idle) shift; stop_when_idle "$@" ;;
  *) usage ;;
esac
