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
  older layout only once it runs sets. Build or install the CLI from this
  repository's `main`:

  ```
  go install github.com/instantcocoa/devbox@latest
  ```

- **Your extra ssh keys** in `~/.config/devbox/authorized_keys` (see "Machine-specific
  values" in the README). A push installs exactly those.
- **The agent adapter** you want, as a program in the Nix store. The module has
  no default: there is no npx launcher.

## 1. Write your `local.nix`

Where you run devbox, create `~/.config/devbox/local.nix` and point the CLI
at it:

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

## 2. Stop the hand-run gateway

On the box, stop the gateway you started by hand, and stop whatever restarts
it (a tmux session, a login script). Its database stays where it is: the
service uses the same state dir, `~/.ahp-acp-gateway/`.

Check that nothing listens on the port and nothing holds the database:

```
ss -ltnp 'sport = :8765'
find /proc/[0-9]*/fd -maxdepth 1 -lname '*/.ahp-acp-gateway/ahp-acp-gateway.db*' 2>/dev/null
```

Both should print nothing. The service makes the same checks at every start
and refuses to start otherwise.

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

On the box:

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
the token. Also note the `MainPID` the check prints, run `devbox nix-update`,
and check the pid is unchanged: a config push never restarts a running
gateway.

## Undo

1. Where you run devbox, set `services.ahp-gateway.enable = false;` in your
   `local.nix` (or remove the block) and run `devbox nix-update`. The switch
   stops and removes the service.
2. On the box, if the service ran a newer gateway build than your hand-run
   one, put the backup back first: remove `ahp-acp-gateway.db-wal` and
   `ahp-acp-gateway.db-shm`, copy the files from the backup directory in
   `~/.ahp-acp-gateway/backups/devbox/` over the database, and delete
   `~/.ahp-acp-gateway/devbox-build`. With the same build this is not needed.
3. Start your hand-run gateway again, and remove the token from the editors if
   it does not use one.
