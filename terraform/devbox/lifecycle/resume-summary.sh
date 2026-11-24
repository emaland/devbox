#!/bin/sh
# One line about auto-resume for the boot notification (design note 031,
# section 2.7). Waits, bounded, for the gateway to have started every
# continuation it is going to start.
#
#   DEVBOX_LIFECYCLE_LIB   directory with gateway-status.sh
#   DEVBOX_RESUME_WAIT     seconds to wait for nothing to be pending (180)
#   DEVBOX_RESUME_POLL     seconds between tries (5)
#
# Prints nothing when no gateway is installed.
set -u
export LC_ALL=C

lib="${DEVBOX_LIFECYCLE_LIB:?}"
wait_for="${DEVBOX_RESUME_WAIT:-180}"
every="${DEVBOX_RESUME_POLL:-5}"
tries=$((wait_for / every + 1))

while :; do
  out="$(sh "$lib/gateway-status.sh")"
  status=$?
  tries=$((tries - 1))
  json=""
  [ "$status" -eq 0 ] && json="$(printf '%s\n' "$out" | sed -n 2p)"
  pending="$(printf '%s' "$json" | jq -r 'if (.resume | type) == "object" and (.resume.pending | type) == "number" then .resume.pending else 0 end' 2>/dev/null)"
  case "$status" in
    3) exit 0 ;;
    0) [ "${pending:-0}" = 0 ] && break ;;
  esac
  [ "$tries" -gt 0 ] || break
  sleep "$every"
done

case "$status" in
  2) printf 'gateway: not answering (%s)\n' "$out" ;;
  0)
    # Words reduced to letters and numbers to numbers: this line goes to the
    # notification as it is.
    printf '%s' "$json" | jq -r '
      def word: tostring | gsub("[^a-z_-]"; "") | if . == "" then "?" else . end;
      def count: if type == "number" then . else 0 end;
      if (.resume | type) != "object" then "auto-resume: not reported by this gateway build"
      else .resume
        | "auto-resume: \(.policy | word) (\(.source | word)): continued \(.resumedSinceStart | count), failed \(.failed | count), skipped \(.skipped | count)"
          + (if (.pending | count) > 0 then ", still pending \(.pending)" else "" end)
      end'
    ;;
esac
exit 0
