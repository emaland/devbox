#!/bin/sh
# Tells someone the gateway is in trouble: a line in the box's log, and a
# post to a webhook if one is configured. Runs as root from
# ahp-gateway-failed.service and ahp-gateway-check.service. It must work when
# /home is not mounted, which is one of the reasons it runs, so it needs
# nothing from /home; the webhook address happens to live there, and without
# it only the log line is written.
#
#   notice.sh <reason>
#   AHP_NOTICE_LOG           file to append to
#   AHP_NOTICE_WEBHOOK_FILE  optional file with a SLACK_WEBHOOK_URL=... line
#
# The webhook file is read, never run: it belongs to the user. The address is
# handed to curl on standard input, so it is not on a command line.
set -u

reason="${1:-unknown}"
log="${AHP_NOTICE_LOG:-/var/log/boot-history}"
webhook_file="${AHP_NOTICE_WEBHOOK_FILE:-}"
stamp="$(date '+%Y-%m-%d %H:%M:%S')"
host="$(cat /proc/sys/kernel/hostname 2>/dev/null || echo unknown)"

printf '%s | ahp-gateway | %s\n' "$stamp" "$reason" >>"$log" ||
  echo "ahp-gateway notice: could not write $log" >&2

url=""
if [ -n "$webhook_file" ] && [ -f "$webhook_file" ] && [ -r "$webhook_file" ]; then
  # Only the first 64 KiB: the file is the user's and could be anything, an
  # endless one included.
  url="$(head -c 65536 "$webhook_file" | sed -n -E "s/^[[:space:]]*(export[[:space:]]+)?SLACK_WEBHOOK_URL=[\"']?([^\"'[:space:]]*)[\"']?[[:space:]]*$/\\2/p" | tail -n 1)"
fi
case "$url" in
  https://* | http://*) ;;
  *) url="" ;;
esac

if [ -n "$url" ]; then
  payload="$(jq -n --arg text ":warning: *ahp-gateway* on \`$host\`: $reason" '{text: $text}')"
  printf 'url = "%s"\n' "$url" |
    curl -q -s --fail --max-time 10 -X POST -H 'Content-type: application/json' --data "$payload" -K - >/dev/null ||
    echo "ahp-gateway notice: the webhook post failed" >&2
fi
exit 0
