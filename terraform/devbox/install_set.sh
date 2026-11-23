#!/bin/sh
# Installs a NixOS config set on the box.
#
#   install_set.sh install <tarball> <config dir> <saved dir> <stamp>   (root)
#   install_set.sh save    <config dir> <saved dir>                     (the owner of <saved dir>)
#   install_set.sh restore <config dir> <saved dir>                     (root)
#
# `install` unpacks a pushed set and switches to it; the devbox CLI runs it
# before `nixos-rebuild switch`. `save` mirrors the set in use to the saved
# copy on the persistent volume. `restore` runs at boot and installs the saved
# set when it differs from the one in use. None of them rebuilds the system.
#
# A set is one directory, <dir>/sets/<stamp>/, holding configuration.nix, the
# devbox/ tree and, when the operator has one, local.nix. <dir>/current is a
# symlink to sets/<stamp> and <dir>/configuration.nix a fixed symlink to
# current/configuration.nix, so a set is switched with a single rename and the
# main file never sits beside a tree it does not belong to.
#
# The saved copy belongs to the user, and root reads it. Root therefore never
# follows a link out of it: a saved set is copied with tar, which stores links
# as links, and the copy is refused if it holds anything but plain files and
# directories. The saved copy is written by its owner, never by root. What is
# left: root changes directory into the saved copy's sets/ directory, so a
# user who swaps that directory for a link at the right moment makes root
# read a directory of their choosing. On this box the user can become root
# anyway.
#
# DEVBOX_SAVED_STATE tells `install` whether it can see the saved copy:
# `mounted` (the default), `unmounted` (the persistent volume exists but is
# not mounted, so what is under <saved dir> is not the real copy), or `none`
# (a new blank volume: there is no saved copy anywhere).
#
# Exit status: 0 done; 3 the pushed set is older than the one in use; 4 the
# operator's local.nix may be expected but cannot be found, or the saved copy
# is unusable; 1 anything else.
set -eu
export LC_ALL=C

fail() {
  echo "install_set: $1" >&2
  exit "${2:-1}"
}

# valid_stamp <name>: names this script makes. Anything else is left alone.
valid_stamp() {
  case "$1" in
    "" | .* | *[!A-Za-z0-9._-]*) return 1 ;;
  esac
  return 0
}

# now: a time stamp, plus the pid so two calls in one second differ.
now() {
  echo "$(date -u +%Y%m%dT%H%M%SZ)-$$"
}

# current_stamp <dir>: print the stamp <dir>/current names. Fails unless
# current is a link to exactly sets/<stamp>, the stamp is one this script
# makes, and sets/<stamp> is a real directory.
current_stamp() {
  [ -L "$1/current" ] || return 1
  target="$(readlink "$1/current")"
  case "$target" in sets/*) ;; *) return 1 ;; esac
  stamp="${target#sets/}"
  valid_stamp "$stamp" || return 1
  case "$stamp" in *.partial) return 1 ;; esac
  [ -d "$1/sets/$stamp" ] && [ ! -L "$1/sets/$stamp" ] || return 1
  printf '%s\n' "$stamp"
}

# check_set <dir>: a set holds only plain files and directories, and has a
# main file.
check_set() {
  [ -d "$1" ] && [ ! -L "$1" ] || return 1
  [ -z "$(find "$1" ! -type f ! -type d -print 2>/dev/null | head -n 1)" ] || return 1
  [ -f "$1/configuration.nix" ]
}

# layout_of <set dir>: the set's layout version; a set without one counts as 0.
layout_of() {
  if [ -f "$1/devbox/layout-version" ] && [ ! -L "$1/devbox/layout-version" ]; then
    version="$(tr -cd '0-9' <"$1/devbox/layout-version")"
    echo "${version:-0}"
  else
    echo 0
  fi
}

# switch_to <dir> <stamp>: make <dir>/current name sets/<stamp>, and
# <dir>/configuration.nix the fixed link, each by rename.
switch_to() {
  rm -f "$1/current.new" "$1/configuration.nix.new"
  ln -s "sets/$2" "$1/current.new"
  mv -T "$1/current.new" "$1/current"
  if [ "$(readlink "$1/configuration.nix" 2>/dev/null || true)" != current/configuration.nix ]; then
    ln -s current/configuration.nix "$1/configuration.nix.new"
    mv -T "$1/configuration.nix.new" "$1/configuration.nix"
  fi
}

# prune <dir>: keep the set in use and the newest other set; remove anything
# half-written. Names this script did not make are left alone.
prune() {
  in_use="$(current_stamp "$1")" || return 0
  newest_other=""
  for path in "$1"/sets/*; do
    [ -e "$path" ] || [ -L "$path" ] || continue
    name="${path##*/}"
    case "$name" in
      *.partial)
        rm -rf -- "$path"
        continue
        ;;
    esac
    valid_stamp "$name" || continue
    [ "$name" = "$in_use" ] && continue
    # The glob is sorted, and stamps sort by time: each one replaces the last.
    if [ -n "$newest_other" ]; then
      rm -rf -- "$1/sets/$newest_other"
    fi
    newest_other="$name"
  done
}

# import_saved <saved dir> <work dir>: copy the saved set in use into
# <work dir>/<stamp> without following links, check the copy, and set
# IMPORTED to the stamp.
import_saved() {
  [ -d "$1" ] && [ ! -L "$1" ] && [ -d "$1/sets" ] && [ ! -L "$1/sets" ] || return 1
  IMPORTED="$(current_stamp "$1")" || return 1
  mkdir "$2"
  (cd "$1/sets" && tar -cf - -- "$IMPORTED") |
    tar -xf - -C "$2" --no-same-owner --no-same-permissions || return 1
  check_set "$2/$IMPORTED"
}

install_set() {
  tarball="$1" config="$2" saved="$3" stamp="$4"
  saved_state="${DEVBOX_SAVED_STATE:-mounted}"
  case "$saved_state" in
    mounted | unmounted | none) ;;
    *) fail "DEVBOX_SAVED_STATE must be mounted, unmounted or none, not $saved_state" ;;
  esac
  valid_stamp "$stamp" || fail "bad stamp"
  case "$stamp" in *.partial) fail "bad stamp" ;; esac
  new="$config/sets/$stamp"
  [ ! -e "$new" ] || fail "set $stamp already exists"
  mkdir -p "$config/sets"
  rm -rf "$new.partial" "$new.saved.partial"
  mkdir "$new.partial"
  tar -xf "$tarball" -C "$new.partial" --no-same-owner --no-same-permissions ||
    { rm -rf "$new.partial"; fail "could not unpack the pushed set"; }
  check_set "$new.partial" ||
    { rm -rf "$new.partial"; fail "the pushed set has no configuration.nix, or holds something other than files and directories"; }

  # The set this one replaces: the one in use, or on a root volume that has
  # never had one, the saved copy.
  previous=""
  if in_use="$(current_stamp "$config")"; then
    previous="$config/sets/$in_use"
  elif [ "$saved_state" = mounted ] && { [ -e "$saved/current" ] || [ -L "$saved/current" ]; }; then
    if ! import_saved "$saved" "$new.saved.partial"; then
      rm -rf "$new.partial" "$new.saved.partial"
      fail "the saved config set under $saved is not usable (it must be a set this tool wrote, holding only files and directories); nothing was installed" 4
    fi
    previous="$new.saved.partial/$IMPORTED"
  fi

  # Never replace a newer layout with an older one: an old CLI pushes a set
  # without the modules the running system relies on.
  if [ -n "$previous" ]; then
    pushed="$(layout_of "$new.partial")"
    before="$(layout_of "$previous")"
    if [ "$pushed" -lt "$before" ]; then
      rm -rf "$new.partial" "$new.saved.partial"
      fail "the pushed config set is layout $pushed but this box runs layout $before; update the devbox CLI" 3
    fi
  fi

  # The operator's local.nix travels with the set. When this push carries
  # none, bring the previous one forward.
  if [ ! -e "$new.partial/local.nix" ] && [ -n "$previous" ] &&
    [ -f "$previous/local.nix" ] && [ ! -L "$previous/local.nix" ]; then
    cp "$previous/local.nix" "$new.partial/local.nix"
    echo "install_set: kept the operator's local.nix from the previous config set"
  fi
  if [ ! -e "$new.partial/local.nix" ]; then
    if [ -z "$previous" ] && [ "$saved_state" = unmounted ]; then
      rm -rf "$new.partial" "$new.saved.partial"
      fail "the persistent volume is not mounted, so it cannot be told whether this box needs an operator local.nix; mount /home, or push one with nix_local_module" 4
    fi
    if [ "$saved_state" = mounted ] && [ -e "$saved/local-module-expected" ]; then
      rm -rf "$new.partial" "$new.saved.partial"
      fail "this box expects an operator local.nix, but none was pushed and none is on the box or in $saved; set nix_local_module and push again" 4
    fi
  fi

  rm -rf "$new.saved.partial"
  mv -T "$new.partial" "$new"
  switch_to "$config" "$stamp"
  prune "$config" || true
}

save_set() {
  config="$1" saved="$2"
  stamp="$(current_stamp "$config")" || fail "no config set in use under $config"
  mkdir -p "$saved/sets"
  name="$stamp"
  # A saved set of the same name with other content is not trusted: save
  # beside it, and let prune remove it.
  if [ -e "$saved/sets/$name" ] &&
    ! diff -r -q "$config/sets/$stamp" "$saved/sets/$name" >/dev/null 2>&1; then
    name="$stamp.saved.$(now)"
  fi
  if [ ! -e "$saved/sets/$name" ]; then
    rm -rf "$saved/sets/$name.partial"
    cp -R "$config/sets/$stamp" "$saved/sets/$name.partial"
    mv -T "$saved/sets/$name.partial" "$saved/sets/$name"
  fi
  # An older saved copy is a plain file at configuration.nix; the link
  # replaces it.
  switch_to "$saved" "$name"
  # Remove first, so a link put in its place is not written through.
  rm -f "$saved/local-module-expected"
  if [ -e "$saved/sets/$name/local.nix" ]; then
    : >"$saved/local-module-expected"
  fi
  prune "$saved" || true
}

restore_set() {
  config="$1" saved="$2"
  { [ -e "$saved/current" ] || [ -L "$saved/current" ]; } || fail "no saved config set under $saved"
  mkdir -p "$config/sets"
  work="$config/sets/restore.partial"
  rm -rf "$work"
  if ! import_saved "$saved" "$work"; then
    rm -rf "$work"
    fail "the saved config set under $saved is not usable (it must be a set this tool wrote, holding only files and directories); the config in use is unchanged"
  fi
  if in_use="$(current_stamp "$config")" &&
    diff -r -q "$work/$IMPORTED" "$config/sets/$in_use" >/dev/null 2>&1; then
    rm -rf "$work"
    echo "install_set: the config set in use already matches the saved one"
    return 0
  fi
  # Never replace a set in place: a set of the same name gets a new name.
  name="$IMPORTED"
  if [ -e "$config/sets/$name" ] || [ -L "$config/sets/$name" ]; then
    name="$IMPORTED.restored.$(now)"
  fi
  mv -T "$work/$IMPORTED" "$config/sets/$name"
  rm -rf "$work"
  switch_to "$config" "$name"
  prune "$config" || true
  echo "install_set: restored config set $name from $saved"
}

case "${1:-}" in
  install)
    [ "$#" -eq 5 ] || fail "usage: install_set.sh install <tarball> <config dir> <saved dir> <stamp>"
    install_set "$2" "$3" "$4" "$5"
    ;;
  save)
    [ "$#" -eq 3 ] || fail "usage: install_set.sh save <config dir> <saved dir>"
    save_set "$2" "$3"
    ;;
  restore)
    [ "$#" -eq 3 ] || fail "usage: install_set.sh restore <config dir> <saved dir>"
    restore_set "$2" "$3"
    ;;
  *) fail "usage: install_set.sh install|save|restore ..." ;;
esac
