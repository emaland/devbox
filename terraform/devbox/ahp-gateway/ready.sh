#!/bin/sh
# Runs as ExecStartPost of ahp-gateway.service. The unit counts as started
# only when this exits 0, so units ordered after it see a gateway that is
# ready, is this unit's own, and is closed to tokenless connections.
#
#   MAINPID                    set by systemd: the gateway's pid
#   AHP_GATEWAY_PORT           the loopback port
#   AHP_GATEWAY_READY_PATH     answers 200 when the gateway is ready
#   AHP_GATEWAY_READY_TIMEOUT  seconds to wait for that, once prepare is done
#   AHP_GATEWAY_PREPARED_MARKER   file the launcher creates when prepare has
#                                 finished; unset means do not wait for it
#   AHP_GATEWAY_PREPARE_TIMEOUT   the launcher's bound on prepare, in seconds
#
# systemd starts this as soon as the launcher starts, so prepare (a large
# backup, say) runs while this waits. The ready deadline therefore starts
# only once prepare has finished.
#
# Waiting is only for the ready path. Everything else is a failure at once:
# a listener that is not ours, or that is open, does not get better by
# waiting.
set -u
export LC_ALL=C

port="$AHP_GATEWAY_PORT"
path="${AHP_GATEWAY_READY_PATH:-/healthz}"
timeout="${AHP_GATEWAY_READY_TIMEOUT:-60}"
main="${MAINPID:-}"
base="http://127.0.0.1:$port"

fail() {
  echo "ahp-gateway: not ready: $1" >&2
  exit 1
}

[ -n "$main" ] || fail "MAINPID is not set"

# In milliseconds: whole seconds would cut a wait short by up to a second.
now_ms() { echo $(($(date +%s%N) / 1000000)); }

# 0. Wait for prepare to finish, within the launcher's own bound.
marker="${AHP_GATEWAY_PREPARED_MARKER:-}"
if [ -n "$marker" ]; then
  prepare_timeout="${AHP_GATEWAY_PREPARE_TIMEOUT:-300}"
  prepare_deadline=$(($(now_ms) + prepare_timeout * 1000))
  while [ ! -e "$marker" ]; do
    kill -0 "$main" 2>/dev/null || fail "the gateway exited during the start-up checks (see journalctl -u ahp-gateway)"
    [ "$(now_ms)" -lt "$prepare_deadline" ] || fail "the start-up checks (prepare) did not finish within ${prepare_timeout}s"
    sleep 0.5
  done
fi

# 1. The ready path answers 200, before the deadline and while the gateway
#    runs. Polled, not slept: it returns as soon as either is decided.
deadline=$(($(now_ms) + timeout * 1000))
while :; do
  kill -0 "$main" 2>/dev/null || fail "the gateway exited before it was ready (see journalctl -u ahp-gateway)"
  code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 2 "$base$path" 2>/dev/null)" || code=000
  [ "$code" = 200 ] && break
  [ "$(now_ms)" -lt "$deadline" ] || fail "$path did not answer 200 within ${timeout}s (last answer: $code)"
  sleep 0.5
done

# 2. Every socket on the port is loopback only, and the gateway owns it.
listeners="$(ss -Htlnp "sport = :$port")" || fail "could not list listening sockets with ss"
elsewhere="$(printf '%s\n' "$listeners" | awk -v want="127.0.0.1:$port" 'NF && $4 != want { print $4 }')"
[ -z "$elsewhere" ] || fail "something listens on $elsewhere; the gateway must listen on 127.0.0.1:$port only"
[ -n "$listeners" ] || fail "nothing listens on port $port any more"
# Every socket on the port is the gateway's: another process holding one
# too (SO_REUSEPORT) would share its connections.
if ! printf '%s\n' "$listeners" | awk -v owner="pid=$main," 'NF && index($0, owner) == 0 { bad = 1 } END { exit bad }'; then
  fail "port $port is held by a process other than this unit's gateway (pid $main)"
fi

# 3. A WebSocket upgrade without a token is refused. Without an Origin
#    header, so the origin rule cannot be what refuses it. The handshake needs
#    a Sec-WebSocket-Key: 16 bytes, base64. It is a fixed protocol nonce, not
#    a secret; it is built here rather than written out, because a literal of
#    that shape reads to secret scanners like a credential.
websocket_key="$(printf 'devbox ready chk' | base64)"
code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
  -H 'Connection: Upgrade' -H 'Upgrade: websocket' \
  -H 'Sec-WebSocket-Version: 13' -H "Sec-WebSocket-Key: $websocket_key" \
  "$base/ahp" 2>/dev/null)" || code=000
[ "$code" = 401 ] || fail "a connection without a token was answered $code, not 401: the gateway is not enforcing its token"

exit 0
