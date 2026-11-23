#!/usr/bin/env bash
# The full local check for this repository. There is no hosted CI: this is the
# one command to run before asking for a merge.
#
#   scripts/check.sh            run every check
#   scripts/check.sh go nix     run only the named checks
#
# Checks:
#   secrets  gitleaks over the full history of every branch
#   go       go vet and go test
#   nix      the NixOS template evaluates (markers left unrendered)
#   shell    shellcheck on every script embedded in the NixOS template
#
# Output is safe to paste anywhere: a secret finding prints the rule, commit,
# file and line, never the value. Tools that are not on PATH are taken from
# nixpkgs when nix-shell is available; otherwise that check fails with a
# message saying what to install. Linux only; not verified on macOS.
set -uo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."
template=terraform/configuration.nix
scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT

# with_tool <program> <nixpkgs attribute> <command...>: run the command with
# the program available, from PATH or from nixpkgs.
with_tool() {
  local program="$1" attribute="$2"
  shift 2
  if command -v "$program" >/dev/null 2>&1; then
    "$@"
  elif command -v nix-shell >/dev/null 2>&1; then
    nix-shell -p "$attribute" --run "$(printf '%q ' "$@")"
  else
    echo "$program is not installed (and nix-shell is not available to fetch it)" >&2
    return 127
  fi
}

check_secrets() {
  local report="$scratch/gitleaks.json" status=0
  # gitleaks' own output is discarded: besides the redacted match it prints
  # the surrounding line and the commit author's name and email.
  with_tool gitleaks gitleaks \
    gitleaks git --redact=100 --no-banner --no-color --log-opts=--all \
    --report-format json --report-path "$report" . >/dev/null 2>&1 || status=$?
  [ "$status" -eq 0 ] && return 0
  if [ -s "$report" ]; then
    echo "findings (rule, commit, file, line):"
    with_tool jq jq jq -r '.[] | "  \(.RuleID)  \(.Commit[0:7])  \(.File)  \(.StartLine)"' "$report"
    echo "Do not fix a finding forward: the value stays in history. See CONTRIBUTING.md."
  else
    echo "gitleaks exited with status $status and wrote no report"
  fi
  return 1
}

check_go() {
  go vet ./... && go test ./... -count=1
}

check_nix() {
  # The template imports the EC2 image module, which stands in for hardware,
  # so it evaluates on any machine with nixpkgs. Unrendered markers are a
  # supported state and must evaluate too.
  nix-instantiate '<nixpkgs/nixos>' -A system \
    -I nixos-config="$PWD/$template" --add-root "$scratch/system.drv" >/dev/null
}

check_shell() {
  # Cut each `writeShellScript "<name>" '' ... ''` body out of the template
  # and undo the Nix quoting, so shellcheck sees what bash will run:
  #   ''${x}        is a literal ${x}
  #   ${pkgs.foo}   is a store path; any fixed path will do
  local dir="$scratch/scripts" name file failed=0
  mkdir -p "$dir"
  awk -v dir="$dir" '
    match($0, /writeShellScript "[^"]+" \x27\x27$/) {
      name = $0; sub(/.*writeShellScript "/, "", name); sub(/".*/, "", name)
      out = dir "/" name ".sh"; print "#!/usr/bin/env bash" > out; next
    }
    out != "" && /^[[:space:]]*\x27\x27\);[[:space:]]*$/ { close(out); out = ""; next }
    out != "" { print >> out }
  ' "$template"
  for file in "$dir"/*.sh; do
    [ -e "$file" ] || { echo "no embedded scripts found in $template"; return 1; }
    name="$(basename "$file" .sh)"
    sed -i -E \
      -e "s/''\\\$\{/\${/g" \
      -e 's#\$\{(pkgs|config|lib)\.[A-Za-z0-9_.-]+\}#/nix/store/placeholder#g' \
      -e 's#\$\{lib\.makeBinPath \[[^]]*\]\}#/nix/store/placeholder/bin#g' \
      "$file"
    if awk '/\$\{[a-z]+\.[A-Za-z]/ { found = 1 } END { exit !found }' "$file"; then
      echo "$name: a Nix interpolation this check does not understand is left in the script"
      failed=1
      continue
    fi
    # SC1090: the scripts source a file that only exists on the box.
    with_tool shellcheck shellcheck shellcheck --shell=bash --severity=warning \
      --exclude=SC1090 "$file" \
      | sed "s#$dir/##" || failed=1
  done
  return "$failed"
}

all=(secrets go nix shell)
[ "$#" -gt 0 ] && selected=("$@") || selected=("${all[@]}")

declare -A result
overall=0
for check in "${selected[@]}"; do
  case "$check" in
    secrets | go | nix | shell) ;;
    *) echo "unknown check: $check (known: ${all[*]})" >&2; exit 2 ;;
  esac
  echo "== $check"
  if "check_$check"; then result[$check]=passed; else result[$check]=FAILED; overall=1; fi
done

echo
for check in "${selected[@]}"; do printf '%-8s %s\n' "$check" "${result[$check]}"; done
exit "$overall"
