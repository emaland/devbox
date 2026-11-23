#!/usr/bin/env bash
# The full local check for this repository. Nothing hosted is relied on: this
# is the one command to run before asking for a merge.
#
#   scripts/check.sh            run every check
#   scripts/check.sh go nix     run only the named checks
#
# Checks:
#   secrets  gitleaks over the full history of every branch
#   go       go vet and go test (needs Docker for the LocalStack tests)
#   nix      the NixOS template evaluates (markers left unrendered)
#   shell    shellcheck on every script the NixOS template builds with
#            writeShellScript
#
# Needs: git, go, Docker, and Nix with a nixpkgs channel on NIX_PATH. gitleaks,
# jq and shellcheck are taken from nixpkgs when they are not on PATH.
# Linux only; not verified on macOS.
#
# What is safe to paste: the secrets check prints the rule, commit, file and
# line of a finding, never the value or the commit's author, and the summary
# lines at the end say only passed or FAILED. The output of the other checks
# can contain paths and names from this machine: do not paste it into the
# repository, a pull request or an issue.
set -uo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.." || exit 1
template=terraform/configuration.nix
scratch="$(mktemp -d)" || exit 1
trap 'rm -rf "$scratch"' EXIT

all=(secrets go nix shell)
if [ "$#" -gt 0 ]; then selected=("$@"); else selected=("${all[@]}"); fi
for check in "${selected[@]}"; do
  case "$check" in
    secrets | go | nix | shell) ;;
    *) echo "unknown check: $check (known: ${all[*]})" >&2; exit 2 ;;
  esac
done

# need <program>: fail with a clear message when a required program is missing.
need() {
  command -v "$1" >/dev/null 2>&1 && return 0
  echo "$1 is not installed" >&2
  return 1
}

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
  need git || return 1
  local report="$scratch/gitleaks.json" log="$scratch/gitleaks.log" status=0 expected scanned
  # gitleaks' own output goes to a file that is never printed: besides the
  # redacted match it shows the surrounding line and the commit author's name
  # and email.
  with_tool gitleaks gitleaks \
    gitleaks git --redact=100 --no-banner --no-color --log-opts=--all \
    --report-format json --report-path "$report" . >"$log" 2>&1 || status=$?

  if [ "$status" -ne 0 ]; then
    if [ -s "$report" ] && [ "$(with_tool jq jq jq length "$report")" != 0 ]; then
      echo "findings (rule, commit, file, line):"
      with_tool jq jq jq -r '.[] | "  \(.RuleID)  \(.Commit[0:7])  \(.File)  \(.StartLine)"' "$report"
      echo "Do not fix a finding forward: the value stays in history. See CONTRIBUTING.md."
    else
      echo "gitleaks exited with status $status and reported no finding"
    fi
    return 1
  fi

  # gitleaks exits 0 with an empty report when the history could not be read
  # at all (git missing, a checkout git refuses to open, a broken ref). A pass
  # only counts when it scanned every commit that carries a change.
  expected="$(git rev-list --all --no-merges --count 2>/dev/null)" || expected=""
  scanned="$(sed -nE 's/.*[^0-9]([0-9]+) commits scanned.*/\1/p' "$log" | tail -n 1)"
  if [ -z "$expected" ] || [ "$expected" -eq 0 ] || [ "$scanned" != "$expected" ]; then
    echo "gitleaks scanned ${scanned:-no} commits; the history has ${expected:-an unknown number}. The scan did not cover the history."
    return 1
  fi
}

check_go() {
  need go || return 1
  # Without Docker the cmd tests skip everything that talks to LocalStack and
  # still report ok, so a pass would mean very little.
  if ! docker info >/dev/null 2>&1; then
    echo "docker is not reachable; the LocalStack tests would be skipped"
    return 1
  fi
  go vet ./... && go test ./... -count=1
}

check_nix() {
  need nix-instantiate || return 1
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
  # Limits: a Nix variable written as ${UPPERCASE} looks like a shell variable
  # and is not noticed. The profile.d snippets set with `.text` are not
  # covered; they are sourced by login shells, not run as scripts.
  local dir="$scratch/scripts" file written builders
  mkdir -p "$dir"
  awk -v dir="$dir" -v q="'" '
    BEGIN { open_re = "writeShellScript \"[^\"]+\" " q q "$"; close_re = "^[[:space:]]*" q q "\\);[[:space:]]*$" }
    $0 ~ open_re {
      name = $0; sub(/.*writeShellScript "/, "", name); sub(/".*/, "", name)
      # The line number keeps two scripts with one name apart.
      out = dir "/" name "." NR ".sh"; print "#!/usr/bin/env bash" > out; next
    }
    out != "" && $0 ~ close_re { close(out); out = ""; next }
    out != "" { print >> out }
  ' "$template"

  # Every script builder in the template must have been extracted. A builder
  # this check does not understand (writeShellScriptBin, writeScript, a
  # trailing comment after the opening quotes) is a failure, not a skip.
  builders="$(awk '/write(ShellScript|ShellScriptBin|ShellApplication|Script|ScriptBin)[[:space:]("]/ { n++ } END { print n + 0 }' "$template")"
  written="$(find "$dir" -name '*.sh' | wc -l)"
  if [ "$written" -eq 0 ] || [ "$written" -ne "$builders" ]; then
    echo "$template builds $builders scripts but $written could be extracted; teach scripts/check.sh the new form"
    return 1
  fi

  for file in "$dir"/*.sh; do
    sed -i -E \
      -e "s/''\\\$\{/\${/g" \
      -e 's#\$\{(pkgs|config|lib)\.[A-Za-z0-9_.-]+\}#/nix/store/placeholder#g' \
      -e 's#\$\{lib\.makeBinPath \[[^]]*\]\}#/nix/store/placeholder/bin#g' \
      "$file"
    if awk '/\$\{[a-z]+\.[A-Za-z]/ { found = 1 } END { exit !found }' "$file"; then
      echo "$(basename "$file"): a Nix interpolation this check does not understand is left in the script"
      return 1
    fi
  done

  # SC1090: the scripts source a file that only exists on the box.
  with_tool shellcheck shellcheck shellcheck --shell=bash --severity=warning \
    --exclude=SC1090 "$dir"/*.sh | sed "s#$dir/##"
}

declare -A result
overall=0
for check in "${selected[@]}"; do
  echo "== $check"
  if "check_$check"; then result[$check]=passed; else result[$check]=FAILED; overall=1; fi
done

echo
for check in "${selected[@]}"; do printf '%-8s %s\n' "$check" "${result[$check]}"; done
exit "$overall"
