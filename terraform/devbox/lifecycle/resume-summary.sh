#!/bin/sh
# One line about interrupted turns for the boot notification (design note
# 031, section 2.7). Since D24 the gateway never continues a turn by itself:
# it restores each chat and marks each interrupted turn, which waits for a
# person's Try Again or a new message. Nothing is going to start, so this
# asks once and does not wait.
#
#   DEVBOX_LIFECYCLE_LIB   directory with gateway-status.sh
#
# Prints nothing when no gateway is installed.
set -u
export LC_ALL=C

lib="${DEVBOX_LIFECYCLE_LIB:?}"

out="$(sh "$lib/gateway-status.sh")"
case $? in
  3) ;;
  2) printf 'gateway: not answering (%s)\n' "$out" ;;
  0)
    # A whole number in range only: this line goes to the notification as
    # it is.
    printf '%s\n' "$out" | sed -n 2p | jq -r 'def count: if type == "number" and . >= 0 and . < 1e9 then floor else null end;
      ((.resume | objects | .interrupted | count) // null)
      | if . == null then "interrupted turns: not reported by this gateway build"
        elif . == 0 then "interrupted turns: none"
        else "interrupted turns: \(.), waiting for Try Again or a new message"
        end'
    ;;
esac
exit 0
