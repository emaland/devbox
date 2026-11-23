#!/bin/sh
# Posts one message to the notification webhook, if one is configured.
# Callers write their own boot-history line; this only posts.
#
#   notify.sh <message>
#   DEVBOX_WEBHOOK_FILE  optional file with a SLACK_WEBHOOK_URL=... line
#
# The webhook file belongs to the user: it is read, never run. The address is
# given to curl on standard input, so it is not on a command line. The post is
# bounded to 10 s and its failure is reported on standard error only, so it
# never holds up a stop.
set -u
export LC_ALL=C

message="${1:?usage: notify.sh <message>}"
webhook_file="${DEVBOX_WEBHOOK_FILE:-}"

url=""
if [ -n "$webhook_file" ] && [ -f "$webhook_file" ] && [ -r "$webhook_file" ]; then
  url="$(sed -n -E "s/^[[:space:]]*(export[[:space:]]+)?SLACK_WEBHOOK_URL=[\"']?([^\"'[:space:]]*)[\"']?[[:space:]]*$/\\2/p" "$webhook_file" | tail -n 1)"
fi
# Plain http only to this machine (a local relay, or the tests).
case "$url" in
  https://* | http://127.0.0.1:*) ;;
  *) exit 0 ;;
esac
# A quote or backslash would change the curl config below. An @ would let
# "http://127.0.0.1:x@elsewhere/" pass the loopback rule above.
case "$url" in
  *[\"\\@]*) echo "notify: webhook address unusable" >&2; exit 0 ;;
esac

host="$(cat /proc/sys/kernel/hostname 2>/dev/null || echo unknown)"
payload="$(jq -n --arg text "*devbox* \`$host\`: $message" '{text: $text}')" || exit 0
printf 'url = "%s"\n' "$url" |
  curl -s --max-time 10 -X POST -H 'Content-type: application/json' --data "$payload" -K - >/dev/null ||
  echo "notify: the webhook post failed" >&2
exit 0
