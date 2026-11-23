#!/bin/sh
# Asks the local AHP gateway for its status (design note 031, section 2.2).
#
#   gateway-status.sh
#
#   DEVBOX_GATEWAY_CONFIG  the gateway module's client config
#                          (default /etc/ahp-gateway/config.json): port and
#                          token file
#
# Output and exit status:
#   0  two lines: busy, waiting or idle; then the status JSON on one line
#   2  one line: why the gateway could not be asked or did not answer
#   3  no gateway is installed on this box (no client config); no output
#
# The token goes to curl as a config file on standard input, so it is never on
# a command line, in the environment or in the output.
set -u
export LC_ALL=C

config="${DEVBOX_GATEWAY_CONFIG:-/etc/ahp-gateway/config.json}"
[ -f "$config" ] || exit 3

unreachable() {
  printf '%s\n' "$1"
  exit 2
}

port="$(jq -r '.port // empty' "$config" 2>/dev/null)" || unreachable "client config unreadable"
token_file="$(jq -r '.tokenFile // empty' "$config" 2>/dev/null)" || unreachable "client config unreadable"
case "$port" in
  "" | *[!0-9]*) unreachable "client config has no port" ;;
esac
[ "$port" -ge 1 ] && [ "$port" -le 65535 ] || unreachable "client config has no port"

[ -n "$token_file" ] && [ -r "$token_file" ] || unreachable "token file unusable"
# One line, then only the characters a generated token uses. Anything else
# (a quote or a backslash would change the curl config) is refused.
[ "$(wc -l <"$token_file")" -le 1 ] || unreachable "token file unusable"
token="$(cat "$token_file")" || unreachable "token file unusable"
case "$token" in
  "" | *[!A-Za-z0-9._~+/=-]*) unreachable "token file unusable" ;;
esac

body="$(mktemp)" || unreachable "no temporary file"
trap 'rm -f "$body"' EXIT

code="$(printf 'header = "Authorization: Bearer %s"\n' "$token" |
  curl -s --max-time 3 -o "$body" -w '%{http_code}' -K - "http://127.0.0.1:$port/status")"
status=$?
token=""
[ "$status" -eq 0 ] || unreachable "no answer (curl exit $status)"
[ "$code" = 200 ] || unreachable "HTTP $code"

# The busy rule is the gateway's own: a turn that is working and not waiting
# for a person, or a continuation about to start.
state="$(jq -r '
  if (.turns | type) != "object"
     or (.turns.active | type) != "number"
     or (.turns.waitingForUser | type) != "number"
  then "malformed"
  else
    (.turns.active - .turns.waitingForUser) as $working
    | ((.resume.pending // 0) | if type == "number" then . else 0 end) as $pending
    | if $working > 0 or $pending > 0 then "busy"
      elif .turns.active > 0 then "waiting"
      else "idle" end
  end' "$body" 2>/dev/null)" || unreachable "answer is not JSON"
[ "$state" != malformed ] || unreachable "answer has no turn counts"

printf '%s\n' "$state"
jq -c . "$body"
