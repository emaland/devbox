#!/bin/sh
# Installs a pushed NixOS config set on the box. Run as root by the devbox CLI
# before `nixos-rebuild switch`; also used to keep the saved copy on the
# persistent volume in step.
#
#   install_set.sh install <tarball> <config dir> <saved dir> <stamp>
#   install_set.sh save    <config dir> <saved dir> <owner>
#   install_set.sh restore <config dir> <saved dir>
#
# `install` unpacks a pushed set and switches to it. `save` mirrors the set in
# use to the saved copy. `restore` does the reverse at boot: when the saved
# copy holds a different set from the one in use, it is copied in and switched
# to. None of them rebuilds the system.
#
# A set is one directory, <dir>/sets/<stamp>/, holding configuration.nix, the
# devbox/ tree and, when the operator has one, local.nix. <dir>/current is a
# symlink to the set in use and <dir>/configuration.nix a fixed symlink to
# current/configuration.nix, so a set is switched with a single rename and the
# main file never sits beside a tree it does not belong to.
#
# Exit status: 0 done, 3 the pushed set is older than the one in use, 4 the
# operator's local.nix is expected but cannot be found, 1 anything else.
set -eu

fail() {
  echo "install_set: $1" >&2
  exit "${2:-1}"
}

# layout_of <set dir>: the set's layout version; a set without one counts as 0.
layout_of() {
  if [ -f "$1/devbox/layout-version" ]; then
    tr -cd '0-9' <"$1/devbox/layout-version"
  else
    echo 0
  fi
}

# switch_to <dir> <stamp>: make <dir>/current name sets/<stamp>, and
# <dir>/configuration.nix the fixed link, each by rename.
switch_to() {
  ln -s "sets/$2" "$1/current.new"
  mv -T "$1/current.new" "$1/current"
  if [ "$(readlink "$1/configuration.nix" 2>/dev/null || true)" != current/configuration.nix ]; then
    ln -s current/configuration.nix "$1/configuration.nix.new"
    mv -T "$1/configuration.nix.new" "$1/configuration.nix"
  fi
}

# prune <dir>: remove sets other than the one in use and the newest other one,
# and anything left half-written. Stamps sort by time.
prune() {
  in_use="$(basename "$(readlink "$1/current")")"
  find "$1/sets" -mindepth 1 -maxdepth 1 -printf '%f\n' | sort -r | {
    kept=0
    while read -r name; do
      [ "$name" = "$in_use" ] && continue
      case "$name" in
        *.partial) rm -rf "${1:?}/sets/$name" ;;
        *)
          if [ "$kept" -lt 1 ]; then
            kept=$((kept + 1))
          else
            rm -rf "${1:?}/sets/$name"
          fi
          ;;
      esac
    done
  }
}

install_set() {
  tarball="$1" config="$2" saved="$3" stamp="$4"
  case "$stamp" in *[!A-Za-z0-9._-]* | "" | .*) fail "bad stamp" ;; esac
  new="$config/sets/$stamp"
  [ ! -e "$new" ] || fail "set $stamp already exists"
  mkdir -p "$config/sets"
  rm -rf "$new.partial" "$config/current.new" "$config/configuration.nix.new"
  mkdir "$new.partial"
  tar -xf "$tarball" -C "$new.partial"
  [ -f "$new.partial/configuration.nix" ] || { rm -rf "$new.partial"; fail "the pushed set has no configuration.nix"; }

  # Never replace a newer layout with an older one: an old CLI pushes a set
  # without the modules the running system relies on.
  if [ -e "$config/current/configuration.nix" ]; then
    pushed="$(layout_of "$new.partial")"
    in_use="$(layout_of "$config/current")"
    if [ "${pushed:-0}" -lt "${in_use:-0}" ]; then
      rm -rf "$new.partial"
      fail "the pushed config set is layout $pushed but this box runs layout $in_use; update the devbox CLI" 3
    fi
  fi

  # The operator's local.nix travels with the set. When this push carries
  # none, bring the previous one forward: from the set in use, or from the
  # saved copy on a root volume that has never had one.
  if [ ! -e "$new.partial/local.nix" ]; then
    for candidate in "$config/current/local.nix" "$saved/current/local.nix"; do
      if [ -f "$candidate" ]; then
        cp "$candidate" "$new.partial/local.nix"
        echo "install_set: carried local.nix forward from $candidate"
        break
      fi
    done
  fi
  if [ ! -e "$new.partial/local.nix" ] && [ -e "$saved/local-module-expected" ]; then
    rm -rf "$new.partial"
    fail "this box expects an operator local.nix, but none was pushed and none is on the box or in $saved; set nix_local_module and push again" 4
  fi

  mv -T "$new.partial" "$new"
  switch_to "$config" "$stamp"
  prune "$config"
}

save_set() {
  config="$1" saved="$2" owner="$3"
  [ -e "$config/current/configuration.nix" ] || fail "no config set in use under $config"
  stamp="$(basename "$(readlink "$config/current")")"
  mkdir -p "$saved/sets"
  if [ ! -e "$saved/sets/$stamp" ]; then
    rm -rf "$saved/sets/$stamp.partial" "$saved/current.new" "$saved/configuration.nix.new"
    cp -R "$config/sets/$stamp" "$saved/sets/$stamp.partial"
    mv -T "$saved/sets/$stamp.partial" "$saved/sets/$stamp"
  fi
  # An older saved copy is a plain file at this path; the link replaces it.
  switch_to "$saved" "$stamp"
  if [ -e "$saved/sets/$stamp/local.nix" ]; then
    : >"$saved/local-module-expected"
  else
    rm -f "$saved/local-module-expected"
  fi
  prune "$saved"
  # Best effort: the user may not exist yet on a fresh box.
  chown -R "$owner" "$saved" 2>/dev/null || true
}

restore_set() {
  config="$1" saved="$2"
  [ -e "$saved/current/configuration.nix" ] || fail "no saved config set under $saved"
  stamp="$(basename "$(readlink "$saved/current")")"
  if [ -e "$config/current/configuration.nix" ] &&
    diff -r -q "$saved/current/" "$config/current/" >/dev/null 2>&1; then
    echo "install_set: the config set in use already matches the saved one"
    return 0
  fi
  mkdir -p "$config/sets"
  rm -rf "$config/sets/$stamp.partial" "$config/current.new" "$config/configuration.nix.new"
  # A set of the same name with different content is replaced, not merged.
  cp -R "$saved/sets/$stamp" "$config/sets/$stamp.partial"
  rm -rf "${config:?}/sets/$stamp"
  mv -T "$config/sets/$stamp.partial" "$config/sets/$stamp"
  switch_to "$config" "$stamp"
  prune "$config"
  echo "install_set: restored config set $stamp from $saved"
}

case "${1:-}" in
  restore)
    [ "$#" -eq 3 ] || fail "usage: install_set.sh restore <config dir> <saved dir>"
    restore_set "$2" "$3"
    ;;
  install)
    [ "$#" -eq 5 ] || fail "usage: install_set.sh install <tarball> <config dir> <saved dir> <stamp>"
    install_set "$2" "$3" "$4" "$5"
    ;;
  save)
    [ "$#" -eq 4 ] || fail "usage: install_set.sh save <config dir> <saved dir> <owner>"
    save_set "$2" "$3" "$4"
    ;;
  *) fail "usage: install_set.sh install|save|restore ..." ;;
esac
