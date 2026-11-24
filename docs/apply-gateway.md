# Turning on the AHP gateway service

How to move a box from a gateway started by hand to the `ahp-gateway.service`
that `terraform/devbox/modules/ahp-gateway.nix` defines, keep its sessions,
check it, and go back. The design is in
`docs/design/notes/030-ahp-gateway-module.md`.

Run the commands under "On the box" on the box, and the ones under "Where you
run devbox" on the machine you run the `devbox` CLI from.

## Before you start

- **A current `devbox` CLI.** The config is pushed as a set (section 2.1 of the
  note). A CLI from before that pushes a single file, and the box refuses an
  older layout only once it runs sets. Install the CLI from this repository's
  `main` (not `@latest`: that is the newest tag, which is older than sets):

  ```
  go install github.com/instantcocoa/devbox@main
  ```

- **Your extra ssh keys** in `~/.config/devbox/authorized_keys` (see "Machine-specific
  values" in the README). A push installs exactly those.
- **The agent adapter** you want, as a program in the Nix store. The module has
  no default: there is no npx launcher.

## 1. Write your `local.nix`

Where you run devbox, add the block below to your local module. If
`devbox config` already shows a `nix_local_module`, add it to that file
and keep what is there. If not, create `~/.config/devbox/local.nix` and point
the CLI at it:

```
devbox config set nix_local_module ~/.config/devbox/local.nix
```

```nix
{ pkgs, lib, ... }: {
  # nixpkgs' Claude adapter depends on claude-code, which nixpkgs marks as
  # unfree; NixOS refuses to build it unless this allows it.
  nixpkgs.config.allowUnfreePredicate = pkg: builtins.elem (lib.getName pkg) [ "claude-code" ];
  services.ahp-gateway = {
    enable = true;
    user = "<your user on the box>";
    # Keep the built-in id `claude`, so sessions made before keep working.
    providers.claude = {
      command = "${pkgs.claude-agent-acp}/bin/claude-agent-acp";
    };
    # Optional: a file with a SLACK_WEBHOOK_URL=... line for failure notices.
    # failureNotice.webhookFile = "/home/<you>/.secrets";
  };
}
```

The user needs a fixed `uid` and `linger = true`; the template's user has both.
To use another gateway build or adapter version, override `package` or the
provider's `command` here.

**Use a gateway build no older than your hand-run one.** The service packages
release 0.2.2 (`terraform/devbox/pkgs/ahp-acp-gateway.nix`). On its first
start it finds no record of which build last opened the database, so it backs
the database up but cannot tell whether your hand-run gateway was newer. An
older gateway opening a database a newer one wrote may fail or damage it.
Check the version of the gateway you run by hand, for example in the
`package.json` of the copy it runs. If it is newer than 0.2.2, set `package`
to that version, as the comment at the top of the package file shows.

## 2. Stop the hand-run gateway

On the box, stop the gateway you started by hand, and stop whatever restarts
it (a tmux session, a login script). Its database stays where it is: the
service uses the same state dir, `~/.ahp-acp-gateway/`.

Check that nothing listens on the port and nothing holds the database:

```
ss -Hltnp 'sport = :8765'
find /proc/[0-9]*/fd -maxdepth 1 -lname '*/.ahp-acp-gateway/ahp-acp-gateway.db*' 2>/dev/null
```

Both should print nothing. The service makes the same checks at every start
and refuses to start otherwise. (8765 is the service's port. If your hand-run
gateway used another one, check that one too, and update the editors to 8765
in step 4.)

## 3. Push and switch

Where you run devbox:

```
devbox nix-update
```

It prints the operator module it installs and the number of extra ssh keys.
On the first start the service finds a database and no build record, so it
backs the database up to `~/.ahp-acp-gateway/backups/devbox/` before the
gateway opens it.

## 4. Check it

On the box, as the gateway's user (another user cannot see into its home
directory, and the check would report the files as missing):

```
sh /etc/nixos/current/devbox/ahp-gateway/boot-check.sh
journalctl -u ahp-gateway -b
```

Every line of the check should say PASS. Then open your editor and confirm
that a session made before the move is listed **and answers a prompt**.

**The editors now need the token.** The service always requires one; the
hand-run gateway may not have. It is in `~/.ahp-acp-gateway/token` (mode
0600). Give it to each editor as "VS Code and Zed against the devbox" in the
README describes, and never paste it anywhere else.

## 5. The boot checklist

Once, before relying on the service, reboot the box and run the check again:

```
sudo reboot
# after the box is back:
sh /etc/nixos/current/devbox/ahp-gateway/boot-check.sh
```

This is what only a real boot shows: the service starts after `/home` is
mounted, comes back with its sessions, and is closed to connections without
the token.

Last, check that a config push leaves a running gateway alone. Note the
`MainPID` the check prints. Change something in the unit, for example add
`environment.DEVBOX_SWITCH_TEST = "1";` to the `services.ahp-gateway` block,
and run `devbox nix-update`. Run the check again: the pid must be the same.
Then remove the line and push again. (A push that leaves the unit unchanged
would not show this.)

## Undo

1. Where you run devbox, set `services.ahp-gateway.enable = false;` in your
   `local.nix` (or remove the block) and run `devbox nix-update`. The switch
   stops and removes the service.
2. On the box, only if the service ran a newer gateway build than your
   hand-run one: put back the database from before that build first opened
   it. With the same build, skip this: every session made since is kept.
   - `~/.ahp-acp-gateway/backups/devbox/` holds up to five backups, each
     named `<UTC time>-<label>`, where the label is the version of the build
     that last wrote the database (`unknown` for the one taken at the move).
     Pick the newest one taken just before the newer build's first start.
     Sessions made after it are lost.
   - With the service stopped (step 1), as the gateway's user, in
     `~/.ahp-acp-gateway/`: remove `ahp-acp-gateway.db-wal` and
     `ahp-acp-gateway.db-shm`, copy the backup's files over the database with
     `cp -p`, and delete `devbox-build`.
3. Start your hand-run gateway again, and remove the token from the editors if
   it does not use one.
