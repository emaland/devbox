#!/bin/sh
# The boot checklist for the AHP gateway service, run by hand on a real box
# after a reboot: what only a real boot can show. It only reads. Each promise
# gets one PASS or FAIL line; the exit status is 1 if any line is FAIL. It
# never prints the token. Run it as the gateway's user: another user cannot
# see into its home directory.
#
#   sh /etc/nixos/current/devbox/ahp-gateway/boot-check.sh
#
#   AHP_BOOT_CHECK_CONFIG  the client config (default /etc/ahp-gateway/config.json)
set -u
export LC_ALL=C

config="${AHP_BOOT_CHECK_CONFIG:-/etc/ahp-gateway/config.json}"
failed=0

pass() { echo "PASS  $1"; }
fail() {
  echo "FAIL  $1"
  failed=1
}

# property <unit> <name>: one property of a unit.
property() {
  systemctl show -p "$2" "$1" 2>/dev/null | sed -n "s/^$2=//p"
}

if [ ! -r "$config" ]; then
  echo "FAIL  cannot read $config: is services.ahp-gateway enabled?"
  exit 1
fi
port="$(jq -r .port "$config")"
state_dir="$(jq -r .stateDir "$config")"
token_file="$(jq -r .tokenFile "$config")"
unit="$(jq -r '.service // "ahp-gateway.service"' "$config")"
base="http://127.0.0.1:$port"

# The unit.
active="$(property "$unit" ActiveState)"
if [ "$active" = active ]; then pass "unit active ($unit)"; else fail "unit active ($unit is $active; see journalctl -u ${unit%.service})"; fi

started="$(property "$unit" ActiveEnterTimestampMonotonic)"
home="$(property home.mount ActiveEnterTimestampMonotonic)"
case "$started" in "" | *[!0-9]*) started=0 ;; esac
case "$home" in "" | *[!0-9]*) home=0 ;; esac
if [ "$home" -gt 0 ] && [ "$started" -gt "$home" ]; then
  pass "started after /home was mounted"
else
  fail "started after /home was mounted (gateway at ${started}us, /home at ${home}us since boot)"
fi

restarts="$(property "$unit" NRestarts)"
# NRestarts counts the restarts since the unit was last started by hand or at
# boot.
if [ "${restarts:-0}" = 0 ]; then pass "no restarts since it was started"; else fail "no restarts since it was started ($restarts; see journalctl -u ${unit%.service})"; fi

# The files.
if [ -d "$state_dir" ] && [ "$(stat -c %a "$state_dir")" = 700 ]; then
  pass "state dir is private (0700)"
else
  fail "state dir is private (0700): $state_dir is $(stat -c %a "$state_dir" 2>/dev/null || echo missing)"
fi
user="$(property "$unit" User)"
if [ -f "$token_file" ] && [ "$(stat -c %a "$token_file")" = 600 ] &&
  [ -n "$user" ] && [ "$(stat -c %U "$token_file")" = "$user" ]; then
  pass "token is 0600 and owned by the gateway's user"
else
  fail "token is 0600 and owned by the gateway's user (${user:-unknown}): $token_file is $(stat -c '%a %U' "$token_file" 2>/dev/null || echo missing)"
fi

# The listener.
code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "$base/healthz" 2>/dev/null)" || code=000
if [ "$code" = 200 ]; then pass "gateway answers on $base"; else fail "gateway answers on $base (got $code)"; fi

listeners="$(ss -Htln "sport = :$port" 2>/dev/null)"
elsewhere="$(printf '%s\n' "$listeners" | awk -v want="127.0.0.1:$port" 'NF && $4 != want { print $4 }')"
if [ -n "$listeners" ] && [ -z "$elsewhere" ]; then
  pass "listens on loopback only"
else
  fail "listens on loopback only (also on: ${elsewhere:-nothing listens})"
fi

websocket_key="$(printf 'devbox boot chck' | base64)"
code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
  -H 'Connection: Upgrade' -H 'Upgrade: websocket' \
  -H 'Sec-WebSocket-Version: 13' -H "Sec-WebSocket-Key: $websocket_key" \
  "$base/ahp" 2>/dev/null)" || code=000
if [ "$code" = 401 ]; then pass "refuses a connection without a token (401)"; else fail "refuses a connection without a token (got $code)"; fi

# The hourly check.
timer="$(property ahp-gateway-check.timer ActiveState)"
if [ "$timer" = active ]; then pass "hourly check timer active"; else fail "hourly check timer active (it is $timer)"; fi

echo
echo "MainPID $(property "$unit" MainPID): note it before a config push and check it is the same after,"
echo "to confirm that a switch leaves a running gateway alone."
exit "$failed"
