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
  config, or is read on the box from a file outside the repository. Add a
  marker; never a literal.
- Tests that need a key-shaped or id-shaped fixture build it at run time from
  pieces, and use `example.com` and the documentation address ranges.

A value that was committed and later removed is still in history. If that
happens before you push, rewrite your commits. Do not push and fix forward.

## Checks

**Before every push, read your own diff for the list above.** The two automated
checks are backstops.

1. **CI secret scan.** `.github/workflows/secrets.yml` runs
   [gitleaks](https://github.com/gitleaks/gitleaks) over the full history on
   every push to any branch and on every pull request, and fails on any
   finding. It prints the rule, commit, file and line, never the value. It
   detects credentials only: it does not know about hostnames, addresses,
   account ids, email addresses or ssh public keys. To run the same scan
   locally:

   ```
   gitleaks git --redact=100 --log-opts="--all" .
   ```

   If it reports something in commits that are already pushed, stop and tell the
   repository owner, naming the commit, file and rule. Do not paste the value
   anywhere, and do not add an ignore entry yourself: only the owner decides
   that a finding is acceptable. A change to `.gitleaksignore`, a
   `.gitleaks.toml`, an inline `gitleaks:allow` comment or the scan workflow
   itself can silence the scan, so any such change needs the owner's review.

2. **Local `pre-push` hook.** The owner's clones carry a `pre-push` hook that
   blocks a push whose commits add content from the list above, including
   values it reads at push time from private files on that machine. It prints
   the commit, file and rule. A clone without the hook is protected only by the
   CI scan, which runs after the content is already public, so treat the hook
   as required if you have access to it.

   Never bypass it with `--no-verify`, and never set its override variable.
   Only the owner may. If it blocks something you believe is fine, ask the
   owner, naming the commit, file and rule.

CI logs are public. No workflow step may print the environment, a config file
from a machine, or a token.

## Local checks for code

```
go vet ./...
go test ./...
```

The `cmd` tests start LocalStack in Docker; they never reach real AWS.
