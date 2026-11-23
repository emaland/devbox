# Contributing

## This repository is public

Nothing that is not meant for the public may be added: not in a commit, a
branch name, a commit message, a pull request or issue, or a CI log.

That covers credentials and tokens of any kind, webhook URLs, private keys, ssh
public keys, AWS account ids, hosted zone ids, instance and volume ids, real IP
addresses and hostnames, tailnet names, email addresses, and anything copied
from a machine's live configuration.

How the code stays clear of them:

- `terraform/configuration.nix` is a template. A value that belongs to one
  machine or account is a `@@MARKER@@` that the CLI fills in when it pushes the
  config, or is read on the box from a file outside the repository. See
  "Machine-specific values" in the README. Add a
  marker; never a literal.
- Tests that need a key-shaped or id-shaped fixture build it at run time from
  pieces, and use `example.com` and the documentation address ranges.

A value that was committed and later removed is still in history. If that
happens before you push, rewrite your commits. Do not push and fix forward.

## Checks

No hosted CI is relied on (the workflow under `.github/` is not the check of
record). One command runs every check. It needs Linux with git, Go, Docker,
and Nix with a nixpkgs channel; it is not verified on macOS:

```
scripts/check.sh              # everything
scripts/check.sh go nix       # only the named checks
```

| Check | What it does |
|---|---|
| `secrets` | [gitleaks](https://github.com/gitleaks/gitleaks) over the full history of every branch |
| `go` | `go vet ./...` and `go test ./... -count=1`. The `cmd` tests start LocalStack in Docker; they never reach real AWS |
| `nix` | The NixOS template evaluates with its markers unrendered |
| `shell` | `shellcheck` on every script the NixOS template builds with `writeShellScript`. It fails if the template builds a script in a form it cannot extract |

Run it before asking for a merge. gitleaks, jq and shellcheck are taken from
nixpkgs when they are not on `PATH`.

What is safe to paste: the summary lines at the end (`passed` or `FAILED`), and
the secrets check's findings, which give the rule, commit, file and line and
never the value or the commit's author. The output of the other checks can
contain paths and names from your machine.

**Before every push, read your own diff for the list above.** The automated
checks are backstops.

1. **Secret scan.** It detects credentials only: it does not know about
   hostnames, addresses, account ids, email addresses or ssh public keys.

   If it reports something in commits that are already pushed, stop and tell the
   repository owner, naming the commit, file and rule. Do not paste the value
   anywhere, and do not add an ignore entry yourself: only the owner decides
   that a finding is acceptable. A change to `.gitleaksignore`, a
   `.gitleaks.toml`, an inline `gitleaks:allow` comment or the check script
   itself can silence the scan, so any such change needs the owner's review.

2. **Local `pre-push` hook.** The owner's clones carry a `pre-push` hook that
   blocks a push whose commits add content from the list above, including
   values it reads at push time from private files on that machine. It prints
   the commit, file and rule. Treat the hook as required if you have access to
   it.

   Never bypass it with `--no-verify`, and never set its override variable.
   Only the owner may. If it blocks something you believe is fine, ask the
   owner, naming the commit, file and rule.

Never paste the full output of a local run into this repository, a pull
request or an issue.
