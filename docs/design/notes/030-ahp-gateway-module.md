# 030: AHP gateway as a NixOS module and boot service

Status: **proposed, revision 2** after a first design review. No code yet.

## 1. Problem

The box runs an [AHP gateway](https://github.com/formulahendry/ahp-acp-gateway)
so editors can attach to long-running coding-agent sessions. Today it is an npm
install in the home directory, started by hand in tmux. It is gone after every
reboot, nothing restarts it, and nothing stops it cleanly before `/home` is
unmounted.

Goal: the gateway is built by Nix, starts at boot after `/home` is mounted, is
restarted on failure, stops cleanly at shutdown, and needs no network to start a
session.

Two facts shape the design:

- **This repository is public.** It can hold a module and a build recipe, but
  not the source or the pin of a build that is not public, and public CI cannot
  fetch one.
- **The config is delivered as one file.** The CLI embeds
  `terraform/configuration.nix`, renders its markers, and pushes that single
  file to `/etc/nixos/configuration.nix`; a boot service restores that single
  file from `/home`. A module in its own file does not reach the box today.

## 2. Decisions

### 2.1 Layout and delivery

```
terraform/
  configuration.nix            imports ./devbox/modules/ahp-gateway.nix, and ./local.nix if present
  devbox/modules/ahp-gateway.nix   options and the unit
  devbox/pkgs/ahp-acp-gateway.nix  build recipe, parameterised by source
```

- The CLI pushes `configuration.nix` plus the `devbox/` tree. Both come from the
  same place, either the checkout on disk or the copy embedded in the binary,
  never one from each.
- On the box the tree is staged beside its destination and renamed into place,
  so an interrupted push never leaves a main file importing a module that is
  not there. `/etc/nixos/devbox/` is owned by the CLI: a file that is no longer
  in the tree is removed.
- The saved copy under `~/.config/devbox/` has the same shape, and the
  boot-time restore service syncs the same set. The main file keeps its current
  saved path, so an older saved copy still restores.
- `local.nix` is the operator's own module. It is never in this repository. The
  CLI pushes it from the path in a new config key `nix_local_module` (absent
  means: leave whatever is on the box alone, and say whether one is there). It
  is the place for a package override and for values that are not public. It is
  **not** a place for secrets: it is world-readable on the box and what it sets
  ends up in the Nix store. Secrets stay in files read at run time.
- An older CLI binary pushes only the main file, which then fails to evaluate
  because the import is missing. That is a failed switch, not a changed system.

### 2.2 Options (`services.ahp-gateway`)

| Option | Default | Meaning |
|---|---|---|
| `enable` | `false` | Install the unit |
| `package` | public upstream release, built by `devbox/pkgs/ahp-acp-gateway.nix` | The gateway build |
| `user` | (required) | Account the gateway and its agents run as |
| `port` | `8765` | Loopback port. There is no host option: the module binds `127.0.0.1` only |
| `stateDir` | `/home/<user>/.ahp-acp-gateway` | Database and token; must be on persistent storage |
| `tokenFile` | `<stateDir>/token` | Bearer token, created on first start if missing |
| `allowedOrigins` | `[]` | Browser origins allowed to connect |
| `resumeOnStart` | `"off"` | `"off"` or `"continue"`: whether interrupted turns are continued after a restart |
| `providers` | Claude adapter from nixpkgs | Attribute set of agent providers, each a store path and arguments. At least one |
| `extraPath` | `[]` | Packages added to the agents' `PATH` |
| `environmentFile` | `null` | Optional file of extra environment for one start (for example a per-start resume override) |
| `readyTimeoutSec` / `stopTimeoutSec` | `60` / `45` | Start and stop bounds |

**Features.** A package declares what it supports in `passthru.ahpFeatures`:
`tokenFile`, `readyz`, `resume`, `stateLock`, `cli` (has `status` and `doctor`).
The recipe sets them from the version it builds; an override sets them with the
source. The module uses only what is declared. A declaration is a claim, so the
unit also checks at every start that the listener really refuses a request
without a token (2.4); a wrong declaration fails the start instead of leaving
the listener open.

### 2.3 Packages

- **Gateway.** `buildNpmPackage` with Node 24, from `source`, `version` and
  `npmDepsHash` arguments. The default source is the public upstream tag
  (verified: v0.2.2 builds this way on nixpkgs 26.05 and runs). The argument is
  not called `src` because `callPackage` would fill that name from an unrelated
  nixpkgs attribute. A build from a source that is not public is an override in
  `local.nix`, with the source imported by path and hash at evaluation time.
- **Providers.** Each provider is a Nix package referenced by store path.
  Upstream's built-in providers launch through `npx --yes`, which downloads and
  runs code when a session starts. The module reads the built-in provider ids
  from the package's own default config at build time and disables each one, so
  a build that adds a built-in cannot bring a downloading launcher with it. The
  prepare step refuses to start if any enabled provider command is not an
  absolute store path.
- **Other client tools** are installed through `local.nix` with
  `environment.systemPackages`, which gives them the stable path
  `/run/current-system/sw/bin/<name>`.

Nothing is fetched at service start or session start.

### 2.4 Service: a system unit with `User=`

`ahp-gateway.service` is a system unit that runs as the configured user, not a
`systemd --user` unit.

| Need | System unit with `User=` | User unit with linger |
|---|---|---|
| Start only after `/home` is mounted, stop before it is unmounted | `RequiresMountsFor=/home` gives both orderings | A user unit cannot be ordered against a system mount; the user manager may start before a slow `/home` |
| Other boot services wait for or stop the gateway (auto-stop, interruption handling) | Ordinary `After=`, `systemctl stop` | System units cannot depend on user units |
| Bounded, ordered stop at shutdown | Its own `TimeoutStopSec` | Shares the user manager's stop timeout with every other user unit |
| Managing it needs root | Yes (`sudo systemctl`); the box has passwordless sudo | No |

The first two rows decide it. Costs, and what the design does about each:

- **No login session.** The unit sets what a user unit would inherit: `HOME`,
  `SHELL` (the user's configured shell), `XDG_RUNTIME_DIR=/run/user/<uid>` and
  the matching `DBUS_SESSION_BUS_ADDRESS` (linger keeps that directory alive;
  the unit is ordered after `user@<uid>.service`), and `NIX_PATH`.
- **A second, user-level unit of the same name** could be installed by the
  gateway's own `setup` or by an editor extension, and would race this one for
  the port and the database. The module writes `/etc/ahp-gateway/config.json`
  (static, no secrets, names the port, state dir and token file, and says the
  service is managed by the system). That file exists whether or not the
  service is up. The gateway's installer commands are asked to refuse when it
  is present (section 6).
- **Everything the gateway starts is in the unit's control group.** A tmux
  server or dev server that an agent leaves running is stopped when the gateway
  stops. This is the price of "no agent outlives the service".

```ini
[Unit]
After=home.mount user@<uid>.service
RequiresMountsFor=/home
StartLimitIntervalSec=300
StartLimitBurst=5
OnFailure=<the box's existing notification unit>

[Service]
User=<user>
Group=users
WorkingDirectory=/home/<user>
UMask=0077
Environment=HOME=… SHELL=… PATH=… XDG_RUNTIME_DIR=… NIX_PATH=…
UnsetEnvironment=CLAUDECODE
EnvironmentFile=-<environmentFile>
ExecStartPre=<prepare>
ExecStart=<node> <package>/…/cli.js --config <config>
ExecStartPost=<wait until ready, and check it is ours and closed>
Restart=on-failure
RestartSec=2
RestartPreventExitStatus=78
KillMode=mixed
OOMPolicy=continue
TimeoutStartSec=<readyTimeoutSec + 15>
TimeoutStopSec=<stopTimeoutSec>
```

NixOS options on the unit: `restartIfChanged = false`.

- **A config push never restarts a running gateway.** By default NixOS
  restarts a service whose unit changed, and this unit changes with every
  nixpkgs bump. That would interrupt every active turn on an unrelated
  `devbox nix-update`. With `restartIfChanged = false` the new build takes
  effect at the next deliberate restart or boot. `nix-update` says when the
  running gateway is older than the installed one. Restarting it, with a check
  that no agent is working, is a separate explicit command.
- **No network dependency.** The listener is loopback and sessions start
  offline, so the unit does not wait for `network-online.target`.
- **Prepare** runs as the user, never root, and opens nothing in the database.
  1. Refuse if anything is already listening on the port. A second gateway on
     the same database repairs "interrupted" turns that the first one is still
     running, before it ever fails to bind; a build without `stateLock` has no
     other protection.
  2. State dir: create with mode 0700, or tighten an existing one to 0700.
  3. Token: if missing, write 32 random bytes (base64url) to a temporary file
     created under `umask 077`, then hard-link it into place, which is atomic
     and fails if a token already exists. An existing token is never replaced,
     and its mode is tightened to 0600. A token that is empty or shorter than
     expected stops the start.
  4. Refuse if any enabled provider command is not an absolute store path.
  5. Config. For a package with `tokenFile`, the config is the static file in
     `/etc/ahp-gateway/` and prepare writes nothing more. For a package
     without it, prepare writes a copy with the token added to a private
     runtime directory (0700, file 0600) and the gateway is started with that.
     The token is never in the Nix store, the unit file, a command line or the
     journal.
- **Config written by the module:** `listener.host = 127.0.0.1`, the port,
  `listener.allowAnyOrigin = false`, `allowedOrigins`, the token (by file or
  inline as above), the database path under `stateDir`, the resume policy, and
  the providers.
- **Ready means three things**, checked by `ExecStartPost` with a deadline:
  the readiness endpoint answers (`/readyz`, or `/healthz` without the
  `readyz` feature); the process listening on the port is this unit's main
  process, not some other gateway; and a connection attempt without a token is
  refused. If any fails the start fails. Units ordered after this one see a
  ready, closed gateway.
- **Restart.** `on-failure` with a start limit, as the gateway's own unit
  definition does: a crash is restarted, a deliberate stop is not. Status 78
  from the gateway ("restarting will not help") is not retried. Failures found
  by prepare, and every failure of a build that does not use 78, are retried
  up to the start limit and then left failed; `OnFailure=` reports that, since
  nobody watches an unattended boot.
- **Stop.** `KillMode=mixed` sends SIGTERM to the gateway only, so it can stop
  its agents in order; whatever is left is killed at the timeout.
- **Out of memory.** `OOMPolicy=continue`: when the kernel kills one agent's
  runaway build, that turn fails and the gateway and the other sessions keep
  running. The system default would stop the whole unit.
- **`PATH` for agents.** `~/.local/bin`, the user's profile directories,
  `/run/wrappers/bin`, `/run/current-system/sw/bin`, then a fixed base (git,
  bash, coreutils, findutils, grep, sed, awk, make, gcc, curl, openssh, Node,
  nix) and `extraPath`.
- **Not sandboxed, by design.** Agents edit the user's files and run the user's
  tools, including `sudo` on this box, so `ProtectHome`, `NoNewPrivileges` and
  similar settings would break the product. The boundary is the listener:
  loopback only, origin allow-list, token.

### 2.5 CLI on `PATH`

Only for a package with the `cli` feature: `environment.systemPackages` gets a
wrapper that offers `status` and `doctor` and points them at
`/etc/ahp-gateway/config.json`. A package without `cli` (the public release) is
not put on `PATH`, because its only command starts a second server on the same
database.

## 3. Options considered

| Topic | Rejected | Why |
|---|---|---|
| Delivery | Inline the module into the single file at render time | Hides the module boundary; the pushed file no longer matches the repository |
| Pin | A fetch of a non-public repository in this repository | Unbuildable for anyone else and in public CI |
| Unit type | `systemd --user` with linger | Ordering against `/home` and against system services (table above) |
| Config push | Restart the gateway when its unit changes | Interrupts every active turn on unrelated changes |
| Token | Token in the Nix-generated config | The store is world-readable |
| Token | `LoadCredential=` | Readable only by the service; the user's editors need to read the same token file |
| Token check | Trust the declared features | A stale override would leave the listener open with a "ready" unit |
| Providers | Keep upstream's `npx --yes` launchers | Network and unpinned code at session start |
| Readiness | `Type=notify` | The gateway does not speak the notify protocol |
| Restart | `Restart=always` | Same policy as the gateway's own unit definition; a clean exit is treated as intended |

## 4. Migration and rollback

`enable` stays `false` in the template until the migration from the hand-run
gateway has been rehearsed in a VM, starting from a copy of a database written
by the current release.

Migration: stop the hand-run gateway; copy the database files to a dated
backup; enable and switch; start the unit; check that sessions are listed.

Undo: stop and disable the unit, switch, start the hand-run gateway. If the unit
ran a newer build that changed the database, restore the backup first; the note
for that build says whether it does. The module itself changes nothing in the
state dir beyond permissions and a missing token.

## 5. Test plan

Public CI can build only the public release, so the branches for newer features
would otherwise run nowhere public. A small stub gateway package that declares
every feature (and can be told to misbehave) covers them.

| Promise | Test | Where |
|---|---|---|
| Template and module evaluate, enabled and disabled | `nix-instantiate` with stub hardware | CI, every push |
| CLI pushes and restores the tree; removes stale files; old single-file layout still restores; interrupted push leaves a consistent tree | Go tests on the file set and the remote script | CI, every push |
| Boot, ready, session, turn, reboot, session still listed and usable | VM test, public release, scripted fake agent | CI |
| Starts only after `/home` is mounted, stops before it is unmounted | VM test with a delayed `/home` mount | CI |
| Token is 0600 from creation, never replaced; absent from store, unit, process list and journal; empty token stops the start | VM test with a canary token | CI |
| A package that declares `tokenFile` but ignores it does not become ready | VM test with the stub | CI |
| Listener policy is enforced: wrong token and unlisted origin are refused | VM test | CI |
| A switch that changes the unit does not restart a gateway with an active turn | VM test | CI |
| A gateway already on the port: unit does not start, database unchanged | VM test with a second instance and an active turn | CI |
| One agent child killed for memory: gateway and other sessions survive | VM test with a memory-limited child | CI |
| Stop mid-turn finishes within the timeout and leaves no process | VM test | CI |
| Never ready: start fails, is retried to the limit, failure is reported | VM test with the stub | CI |
| Status 78 is not retried; a crash is | VM test with the stub | CI |
| An agent can run git, a shell and nix, and sees the session variables | VM test through the fake agent | CI |
| No network at session start, including the real adapter's start-up handshake | VM test with networking off after boot | CI |
| Migration from a database of the current release, and undo | VM test | CI |
| Builds on the real machine | `nixos-rebuild build`, no switch | By hand, output in the pull request |

## 6. Open points

1. Requests to the gateway's CLI authors, not yet agreed: read
   `/etc/ahp-gateway/config.json` when present; installer commands refuse when
   it says the service is system-managed.
2. `stopTimeoutSec` and `readyTimeoutSec` should follow the gateway's own
   shutdown and start-up bounds once those are fixed.
3. The nixpkgs Claude adapter is behind the upstream release; whether it works
   with this gateway and starts offline is unverified. The provider package is
   overridable for that reason.
4. Whether hosted CI runners run this many VM tests at a useful speed is
   unverified.
5. What a package should do when both a token and a token file are configured.
