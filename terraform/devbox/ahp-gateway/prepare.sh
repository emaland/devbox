#!/bin/sh
# Runs as the gateway's user before every start of ahp-gateway.service
# (ExecStartPre). It never opens the database. Everything it needs comes from
# the unit's environment:
#
#   AHP_GATEWAY_STATE_DIR       state dir: token, build record, backups
#   AHP_GATEWAY_PORT            loopback port the gateway will bind
#   AHP_GATEWAY_TOKEN_FILE      bearer token, created here if missing
#   AHP_GATEWAY_BASE_CONFIG     the static config from the Nix store (no token)
#   AHP_GATEWAY_RUNTIME_CONFIG  where to write base config plus token (0600)
#   AHP_GATEWAY_BUILD_NAME, _VERSION, _PATH   the build about to start
#   AHP_GATEWAY_ALLOW_DOWNGRADE 1 to let an older or different build open the
#                               database
#
# Exit status 78 means "restarting will not help"; 1 is anything else.
set -eu
export LC_ALL=C

state="$AHP_GATEWAY_STATE_DIR"
port="$AHP_GATEWAY_PORT"
token_file="$AHP_GATEWAY_TOKEN_FILE"
base="$AHP_GATEWAY_BASE_CONFIG"
runtime="$AHP_GATEWAY_RUNTIME_CONFIG"
build_name="$AHP_GATEWAY_BUILD_NAME"
build_version="$AHP_GATEWAY_BUILD_VERSION"
build_path="$AHP_GATEWAY_BUILD_PATH"
allow_downgrade="${AHP_GATEWAY_ALLOW_DOWNGRADE:-0}"

refuse() {
  echo "ahp-gateway: $1" >&2
  exit 78
}

database="$(jq -er '.database.path' "$base")" || refuse "the base config names no database.path"

# 1. Nothing else may be serving or holding the database. The public release
#    repairs "interrupted" turns before it binds its port, so a second gateway
#    on the same database does damage before it fails.
if [ -n "$(ss -Htln "sport = :$port" 2>/dev/null)" ]; then
  refuse "port $port is already in use on this machine; is another gateway running? (ss -ltnp 'sport = :$port')"
fi
# One find over every process's open files (only this user's are readable),
# not a readlink per file: a busy machine has thousands.
holder="$(find /proc/[0-9]*/fd -maxdepth 1 \( -lname "$database" -o -lname "$database-wal" -o -lname "$database-shm" \) -print -quit 2>/dev/null || true)"
if [ -n "$holder" ]; then
  pid="${holder#/proc/}"
  pid="${pid%%/*}"
  refuse "process $pid has the gateway database open ($database); stop it first"
fi

# 2. State dir: private to the user.
mkdir -p "$state"
chmod 0700 "$state"

# 3. Token: created once, atomically, never replaced.
if [ ! -e "$token_file" ]; then
  temporary="$token_file.new.$$"
  (
    umask 077
    head -c 32 /dev/urandom | base64 | tr '+/' '-_' | tr -d '=\n' >"$temporary"
    echo >>"$temporary"
  )
  # A hard link fails if a token appeared meanwhile; the existing one wins.
  ln "$temporary" "$token_file" 2>/dev/null || true
  rm -f "$temporary"
fi
chmod 0600 "$token_file"
token_length="$(tr -d '\n' <"$token_file" | wc -c)"
if [ "$token_length" -lt 32 ]; then
  refuse "the token file $token_file is empty or too short; remove it and a new one is made at the next start"
fi

# 4. Every enabled provider is a program in the Nix store: nothing is
#    downloaded or looked up on PATH when a session starts.
jq -r '.providers[] | select(.enabled != false) | .command' "$base" | while read -r program; do
  case "$program" in
    /nix/store/*) ;;
    *) refuse "provider program $program is not a Nix store path" ;;
  esac
  [ -x "$program" ] || refuse "provider program $program does not exist"
done

# 5. Which build may open the database. The record names the last build that
#    started. A change of build is backed up first; going back is refused.
record="$state/devbox-build"
backup_dir="$state/backups"

newer_version() { # newer_version A B: A sorts after B (sort -V order)
  [ "$1" != "$2" ] && [ "$(printf '%s\n%s\n' "$1" "$2" | sort -V | tail -n 1)" = "$1" ]
}

backup() { # backup <label>
  [ -e "$database" ] || return 0
  name="devbox-$(date -u +%Y%m%dT%H%M%S.%NZ)-$1"
  mkdir -p "$backup_dir"
  chmod 0700 "$backup_dir"
  rm -rf "$backup_dir/$name.partial"
  mkdir "$backup_dir/$name.partial"
  for suffix in "" -wal -shm; do
    if [ -e "$database$suffix" ]; then
      cp -p "$database$suffix" "$backup_dir/$name.partial/"
    fi
  done
  mv "$backup_dir/$name.partial" "$backup_dir/$name"
  echo "ahp-gateway: backed up the database to $backup_dir/$name"
  # Keep the newest five of ours. Names sort by time; others are left alone.
  for old in "$backup_dir"/devbox-*.partial; do
    [ -e "$old" ] && rm -rf "$old"
  done
  total=0
  for old in "$backup_dir"/devbox-*; do
    [ -d "$old" ] && total=$((total + 1))
  done
  excess=$((total - 5))
  # The glob is sorted, oldest first.
  for old in "$backup_dir"/devbox-*; do
    [ "$excess" -gt 0 ] || break
    [ -d "$old" ] || continue
    rm -rf "$old"
    excess=$((excess - 1))
  done
}

if [ -f "$record" ]; then
  last_name="$(sed -n 1p "$record")"
  last_version="$(sed -n 2p "$record")"
  last_path="$(sed -n 3p "$record")"
  if [ "$last_path" = "$build_path" ]; then
    change=none
  elif [ "$last_name" = "$build_name" ] && [ "$last_version" = "$build_version" ]; then
    change=rebuilt
  elif [ "$last_name" = "$build_name" ] && newer_version "$build_version" "$last_version"; then
    change=newer
  else
    change=older
  fi
else
  last_version=unknown
  change=first
fi

case "$change" in
  older)
    if [ "$allow_downgrade" != 1 ]; then
      refuse "the database was last opened by $last_name $last_version; $build_name $build_version is older or a different build and may not read it. Restore the backup made before $last_version first ran, or set services.ahp-gateway.allowDowngrade"
    fi
    backup "$last_version"
    ;;
  first | rebuilt | newer) backup "$last_version" ;;
  none) ;;
esac
if [ "$change" != none ]; then
  printf '%s\n%s\n%s\n' "$build_name" "$build_version" "$build_path" >"$record.new"
  mv "$record.new" "$record"
fi

# 6. The runtime config: the base config plus the token, never on a command
#    line, written privately and renamed into place.
(
  umask 077
  jq --rawfile token "$token_file" '.auth.token = ($token | rtrimstr("\n"))' "$base" >"$runtime.new"
)
mv "$runtime.new" "$runtime"
