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
# another file, a FIFO or a device), with no group or other permission bits,
# as the gateway itself requires.
[ -n "$token_file" ] && [ -f "$token_file" ] && [ ! -L "$token_file" ] || unreachable "token file unusable"
# Mode and owner from one stat, so both describe the same file.
mode_owner="$(stat -c '%a %u' "$token_file" 2>/dev/null)" || unreachable "token file unusable"
mode="${mode_owner% *}"
owner="${mode_owner#* }"
case "$owner" in "" | *[!0-9]*) unreachable "token file unusable" ;; esac
case "$mode" in
  *[!0-7]* | "") unreachable "token file unusable" ;;
  *00) ;;
  *) unreachable "token file unusable (open to group or others)" ;;
esac
# As the gateway reads it: one whole CRLF or LF line end is dropped, and what
# is left must be printable ASCII without space (0x21 to 0x7e). A quote or a
# backslash is refused as well, though the gateway takes them: either would
# change the curl config.
# The x keeps a trailing newline through the command substitution.
#
# Run as root, the file is read with its owner's rights only: the checks above
# are by path, and the owner could swap in a link to a file only root may
# read between them and the read. The owner's rights are the owner's account
# (uid and its primary gid), never the file's group, which need not be one
# the owner is in. The reader gets no environment, no standard input and no
# standard error, only the pipe back. The time limit, for a FIFO swapped in,
# is root's and outside the drop, so the owner cannot stop it.
if [ "$(id -u)" = 0 ]; then
  owner_gid="$(id -g -- "$owner" 2>/dev/null)" || unreachable "token file unusable (owner has no account)"
  case "$owner_gid" in "" | *[!0-9]*) unreachable "token file unusable (owner has no account)" ;; esac
  read_token() {
    timeout -k 1 3 setpriv --reuid="$owner" --regid="$owner_gid" --clear-groups \
      env -i head -c 4096 -- "$token_file" </dev/null 2>/dev/null
  }
else
  read_token() { timeout -k 1 3 head -c 4096 -- "$token_file" </dev/null 2>/dev/null; }
fi
raw="$(read_token && printf x)" || unreachable "token file unusable"
raw="${raw%x}"
cr="$(printf '\r')"
nl="
"
case "$raw" in
  *"$cr$nl") token="${raw%"$cr$nl"}" ;;
  *"$nl") token="${raw%"$nl"}" ;;
  *) token="$raw" ;;
esac
raw=""
case "$token" in
  "" | *[!!-~]* | *\"* | *\\*) unreachable "token file unusable" ;;
esac

# Send the token only to the gateway: the process listening on the port must
# belong to the token file's owner. While the gateway is down, another local
# user could otherwise bind the port and collect the token.
#
# Read by column, never by matching text anywhere on the line: the cgroup path
# at the end is chosen by the socket's owner and may hold spaces. Every
# listener on the port must be on 127.0.0.1 and show its owner in column 6:
# uid:N, or ino: straight away, which is how ss shows uid 0. Anything else is
# "none", which matches no owner. (A word, not "?": the loop below splits the
# list, and file name patterns are off for it.)
listeners="$(ss -Htlne "( sport = :$port )" 2>/dev/null |
  while read -r _state _recv _send laddr _peer sixth _rest; do
    if [ "$laddr" != "127.0.0.1:$port" ]; then
      echo none
      continue
    fi
    case "$sixth" in
      uid:*[!0-9]* | uid:) echo none ;;
      uid:*) echo "${sixth#uid:}" ;;
      ino:*) echo 0 ;;
      *) echo none ;;
    esac
  done)"
[ -n "$listeners" ] || unreachable "nothing listening on 127.0.0.1:$port"
set -f
for uid in $listeners; do
  [ "$uid" = "$owner" ] || unreachable "the listener on port $port is not the gateway's user"
done
set +f
# Known limit: the gateway could stop, and another user bind the port, in the
# moment between this check and curl's connect. Closing that needs a peer
# credential check, which TCP on loopback does not give.

body="$(mktemp)" || unreachable "no temporary file"
trap 'rm -f "$body"' EXIT

code="$(printf 'header = "Authorization: Bearer %s"\n' "$token" |
  curl -q -s --noproxy '*' --max-time 3 -o "$body" -w '%{http_code}' -K - "http://127.0.0.1:$port/status")"
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
