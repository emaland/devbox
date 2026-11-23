# Rules for agents working in this repo

**This repo is public.** Nothing non-public may be added to it: not in commits,
branch names, commit messages, pull request or issue text, or CI logs.

Non-public means: credentials and tokens of any kind, webhook URLs, the contents
of `~/.secrets`, private keys, ssh public keys, AWS account ids, hosted zone
ids, instance and volume ids, real IP addresses and hostnames, tailnet names,
email addresses, and anything copied from a private repo or from a machine's
live configuration.

- Never copy the live NixOS config (`/etc/nixos`, `~/.config/devbox`) into the
  repo. Turn each such value into a module option, or read it at run time from
  a file outside the repo.
- A value that was committed and then removed is still in history. Rewrite the
  commits before pushing. Do not push and fix forward.
- Read your own diff for this before every push.
- Never bypass the `pre-push` hook with `--no-verify`, and never set its
  override variable. Only the repo owner may. If the hook blocks something you
  believe is fine, stop and ask the owner, naming the commit, file and rule.
- If something non-public does reach the remote, stop and tell the owner at
  once.
