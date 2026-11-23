#!/bin/sh
# Runs from a timer five minutes after boot and hourly after that. It catches
# what systemd's OnFailure= does not:
#   - the unit never started, for example because /home never mounted (a
#     dependency failure is not a failure of the unit itself);
#   - a slow crash loop, a gateway that becomes ready and dies every few
#     minutes and so never reaches the start limit.
#
#   AHP_CHECK_UNIT      the unit (default ahp-gateway.service)
#   AHP_CHECK_STATE     file holding the restart count at the last look
#   AHP_CHECK_RESTARTS  restarts since the last look that warrant a notice
#   AHP_NOTICE_SCRIPT   the notice program, run directly (no sh on this PATH)
#
# A gateway stopped by hand is reported too, every hour: stop this check's
# timer as well (systemctl stop ahp-gateway-check.timer) for a deliberate
# pause.
set -u
export LC_ALL=C

unit="${AHP_CHECK_UNIT:-ahp-gateway.service}"
state="$AHP_CHECK_STATE"
threshold="${AHP_CHECK_RESTARTS:-3}"
notice="$AHP_NOTICE_SCRIPT"

properties="$(systemctl show -p ActiveState,NRestarts "$unit")" || {
  "$notice" "could not read the state of $unit"
  exit 0
}
active="$(printf '%s\n' "$properties" | sed -n 's/^ActiveState=//p')"
restarts="$(printf '%s\n' "$properties" | sed -n 's/^NRestarts=//p')"
case "$restarts" in "" | *[!0-9]*) restarts=0 ;; esac

last=0
[ -f "$state" ] && last="$(cat "$state")"
case "$last" in "" | *[!0-9]*) last=0 ;; esac
# The counter starts again when the unit is restarted by hand or at boot.
[ "$restarts" -lt "$last" ] && last=0
printf '%s\n' "$restarts" >"$state"

case "$active" in
  active) ;;
  # Still starting (a slow first backup, say): the start timeout decides.
  activating | reloading) exit 0 ;;
  *)
    "$notice" "$unit is not running ($active); see journalctl -u $unit"
    exit 0
    ;;
esac
grown=$((restarts - last))
if [ "$grown" -gt "$threshold" ]; then
  "$notice" "$unit restarted $grown times since the last check; see journalctl -u $unit"
fi
exit 0
