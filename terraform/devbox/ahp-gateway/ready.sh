#!/bin/sh
# Runs as ExecStartPost of ahp-gateway.service. The unit counts as started
# only when this exits 0, so units ordered after it see a gateway that is
# ready, is this unit's own, and is closed to tokenless connections.
#
#   MAINPID                    set by systemd: the gateway's pid
#   AHP_GATEWAY_PORT           the loopback port
#   AHP_GATEWAY_READY_PATH     answers 200 when the gateway is ready
#   AHP_GATEWAY_READY_TIMEOUT  seconds to wait for that
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

# 1. The ready path answers 200, before the deadline and while the gateway
#    runs. Polled, not slept: it returns as soon as either is decided.
# In milliseconds: whole seconds would cut the wait short by up to a second.
now_ms() { echo $(($(date +%s%N) / 1000000)); }
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
case "$listeners" in
  *"pid=$main,"*) ;;
  *) fail "port $port belongs to another process, not to this unit's gateway (pid $main)" ;;
esac

# 3. A WebSocket upgrade without a token is refused. Without an Origin
#    header, so the origin rule cannot be what refuses it.
code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
  -H 'Connection: Upgrade' -H 'Upgrade: websocket' \
  -H 'Sec-WebSocket-Version: 13' -H 'Sec-WebSocket-Key: ZGV2Ym94LXJlYWR5LWNoZWNr' \
  "$base/ahp" 2>/dev/null)" || code=000
[ "$code" = 401 ] || fail "a connection without a token was answered $code, not 401: the gateway is not enforcing its token"

exit 0
