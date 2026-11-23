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

# A regular file, not a link (root runs this and must not be pointed at
# another file, a FIFO or a device), private to its owner, as the gateway
# itself requires.
[ -n "$token_file" ] && [ -f "$token_file" ] && [ ! -L "$token_file" ] || unreachable "token file unusable"
[ "$(stat -c %a "$token_file" 2>/dev/null)" = 600 ] || unreachable "token file unusable (not mode 0600)"
owner="$(stat -c %u "$token_file" 2>/dev/null)" || unreachable "token file unusable"
# One line, then only the characters a generated token uses. Anything else
# (a quote or a backslash would change the curl config) is refused.
[ "$(head -c 4096 "$token_file" | wc -l)" -le 1 ] || unreachable "token file unusable"
token="$(head -c 4096 "$token_file")" || unreachable "token file unusable"
case "$token" in
  "" | *[!A-Za-z0-9._~+/=-]*) unreachable "token file unusable" ;;
esac

# Send the token only to the gateway: the process listening on the port must
# belong to the token file's owner. While the gateway is down, another local
# user could otherwise bind the port and collect the token.
listeners="$(ss -Htlne "( sport = :$port )" 2>/dev/null | sed -n "s/.*[[:space:]]127\.0\.0\.1:${port}[[:space:]].*uid:\([0-9][0-9]*\).*/\1/p")"
[ -n "$listeners" ] || unreachable "nothing listening on 127.0.0.1:$port"
for uid in $listeners; do
  [ "$uid" = "$owner" ] || unreachable "the listener on port $port is not the gateway's user"
done

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
    | (if (.resume | type) == "object" and (.resume.pending | type) == "number"
       then .resume.pending else 0 end) as $pending
    | if $working > 0 or $pending > 0 then "busy"
      elif .turns.active > 0 then "waiting"
      else "idle" end
  end' "$body" 2>/dev/null)" || unreachable "answer is not JSON"
[ "$state" != malformed ] || unreachable "answer has no turn counts"

printf '%s\n' "$state"
jq -c . "$body"
