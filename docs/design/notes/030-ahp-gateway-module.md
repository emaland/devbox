# 030: AHP gateway as a NixOS module and boot service

Status: **proposed, revision 3** after two design reviews. No code yet.

## 1. Problem

The box runs an [AHP gateway](https://github.com/formulahendry/ahp-acp-gateway)
so editors can attach to long-running coding-agent sessions. Today it is an npm
install in the home directory, started by hand in tmux. It is gone after every
reboot, nothing restarts it, and nothing stops it cleanly before `/home` is
unmounted.

Goal: the gateway is built by Nix, starts at boot after `/home` is mounted, is
restarted on failure, stops cleanly at shutdown, and needs no network to start a
session.

Facts that shape the design:

- **This repository is public.** It holds a module and a build recipe for the
  public gateway release. A build from any other source is supplied by the
  operator, outside this repository.
- **The config is delivered as one file.** The CLI embeds
  `terraform/configuration.nix`, renders its markers, and pushes that single
  file; a boot service restores that single file from `/home`. A module in its
  own file does not reach the box today.
- **There is no hosted CI and this machine has no KVM.** Tests must run as
  ordinary local commands. What only a booted VM can show is listed separately
  as unverified (section 5).

Facts about the public release (0.2.2) that the unit must live with, each read
in its source: it repairs "interrupted" turns in the database when it starts,
before it tries to bind its port; it ignores config keys it does not know; it
accepts connections without a token unless one is configured; its built-in
agent providers are launched through `npx --yes`; its only command starts a
server.

## 2. Decisions

### 2.1 Layout and delivery

```
terraform/
  configuration.nix                 imports ./devbox/modules/ahp-gateway.nix, and ./local.nix if present
  devbox/modules/ahp-gateway.nix    options and the unit
  devbox/pkgs/ahp-acp-gateway.nix   build recipe, parameterised by source
  devbox/layout-version             a number, raised when the layout changes
```

- The CLI pushes `configuration.nix`, the `devbox/` tree and, when configured,
  `local.nix`. All come from one place: the directory of the main file when it
  is read from disk (also with `--file`), or the copy embedded in the binary.
  Never a mix.
- On the box the tree is unpacked to a new directory `devbox.<stamp>` and
  `devbox` is a symlink switched to it in one rename, after the main file has
  been written beside it under a temporary name and renamed. Old tree
  directories are removed afterwards. An interrupted push leaves the previous
  consistent set.
- The saved copy under `~/.config/devbox/` holds the same set, including
  `local.nix`, and the boot-time restore service restores the set. The main
  file keeps its current saved path, so an older saved copy still restores.
- `local.nix` is the operator's own module, never in this repository, pushed
  from the path in a new config key `nix_local_module`. With the key unset the
  CLI leaves the box's copy alone and says whether one is there. It is for
  package overrides and values that are not public. It is **not** for secrets:
  it is world-readable on the box and its values end up in the Nix store.
- **Older CLI binaries.** A binary from before this change pushes its own
  embedded single file, which has no gateway module: the switch would remove
  the unit and stop the gateway. The box keeps `layout-version` in
  `/etc/nixos/`; the remote half of a push refuses when the pushed set is older
  than the box's. Binaries that predate the check cannot be stopped this way;
  the migration instructions say to replace them first.

### 2.2 Options (`services.ahp-gateway`)

| Option | Default | Meaning |
|---|---|---|
| `enable` | `false` | Install the unit |
| `package` | public release, built by `devbox/pkgs/ahp-acp-gateway.nix` | The gateway build |
| `user` | (required) | Account the gateway and its agents run as |
| `port` | `8765` | Loopback port. There is no host option: the module binds `127.0.0.1` only |
| `stateDir` | `/home/<user>/.ahp-acp-gateway` | Database and token; must be on persistent storage |
| `tokenFile` | `<stateDir>/token` | Bearer token, created on first start if missing |
| `allowedOrigins` | `[]` | Browser origins allowed to connect |
| `providers` | (required, no default) | Agent providers: id, store path of the program, arguments. Enabling the module without one fails evaluation |
| `settings` | `{}` | Extra keys merged into the gateway config (for example a resume policy, for a build that has one) |
| `readyPath` | `/healthz` | HTTP path that answers 200 when the gateway is ready |
| `noRetryExitStatus` | `[]` | Exit statuses that mean "restarting will not help" for this build |
| `managementCommands` | `[]` | Subcommands of this build that are safe to expose on `PATH` (none for the public release) |
| `allowDowngrade` | `false` | Let a build older than the last one that ran open the database |
| `environment` / `extraPath` | `{}` / `[]` | Extra variables and `PATH` entries for agents |
| `readyTimeoutSec` / `stopTimeoutSec` | `60` / `45` | Start and stop bounds |

There is no table of "features" for the module to trust. Everything that
matters for safety is either the same for every build (the token, below) or
checked against the running process at start.

### 2.3 Packages

- **Gateway.** `buildNpmPackage` with Node 24, from `source`, `version` and
  `npmDepsHash` arguments, defaulting to the public release tag (verified:
  0.2.2 builds this way on nixpkgs 26.05 and runs). The argument is not called
  `src` because `callPackage` would fill that name from an unrelated nixpkgs
  attribute. Another build is an override in `local.nix`, with its source
  imported by path and hash at evaluation time.
- **Providers.** Each is a Nix package referenced by store path, under an id
  of its own. The module reads the built-in provider ids from the package's
  default config at build time and disables each one, so no `npx --yes`
  launcher stays enabled, including in a build that adds a built-in. Which
  agent adapter to run, and at which version, is the operator's choice and is
  set in `local.nix`; the nixpkgs adapter is far behind the adapter's own
  releases, so this note does not make it a default.
- **Other client tools** are installed through `local.nix` with
  `environment.systemPackages`, which gives them the stable path
  `/run/current-system/sw/bin/<name>`.

Nothing is fetched at service start or session start.

### 2.4 Service: a system unit with `User=`

`ahp-gateway.service` is a system unit that runs as the configured user.

| Need | System unit with `User=` | User unit with linger |
|---|---|---|
| Start only after `/home` is mounted, stop before it is unmounted | `RequiresMountsFor=` gives both orderings | Cannot be ordered against a system mount |
| Other boot services wait for or stop the gateway | Ordinary `After=`, `systemctl stop` | System units cannot depend on user units |
| Bounded, ordered stop at shutdown | Its own `TimeoutStopSec` | Shares the user manager's stop timeout |
| Managing it needs root | Yes; the box has passwordless sudo | No |

```ini
[Unit]
Wants=network-online.target user@<uid>.service
After=network-online.target user@<uid>.service
RequiresMountsFor=<stateDir> /home/<user>
StartLimitBurst=5
StartLimitIntervalSec=<6 x (readyTimeoutSec + stopTimeoutSec + 2)>
OnFailure=ahp-gateway-failed.service

[Service]
User=<user>
Group=users
WorkingDirectory=/home/<user>
RuntimeDirectory=ahp-gateway
RuntimeDirectoryMode=0700
Environment=HOME=… SHELL=… PATH=… XDG_RUNTIME_DIR=… NIX_PATH=…
Environment=AHP_GATEWAY_STATE_DIR=<stateDir>
Environment=AHP_GATEWAY_CLOSURE=<store path that references package, providers, config>
UnsetEnvironment=CLAUDECODE
ExecStartPre=<prepare>
ExecStart=<node> <package>/…/cli.js --config /run/ahp-gateway/config.json
ExecStartPost=<wait until ready, and check it is ours and closed>
Restart=on-failure
RestartSec=2
RestartPreventExitStatus=<noRetryExitStatus>
KillMode=mixed
OOMPolicy=continue
TimeoutStartSec=<readyTimeoutSec + 15>
TimeoutStopSec=<stopTimeoutSec>
```

NixOS options on the unit: `restartIfChanged = false`.

**Starting**

- **Network.** The unit waits for `network-online.target` but does not require
  it. Serving and starting sessions need no network, but a build that
  continues interrupted turns at start calls its model at once, and on this
  box the network comes up several seconds after `/home`. Without the wait the
  one automatic continuation would be spent on a network error. An offline box
  still starts, after the target's own timeout.
- **Session environment.** A system unit has no login session, so the unit
  sets `HOME`, `SHELL` (the user's configured shell), `XDG_RUNTIME_DIR` and
  `NIX_PATH`, wants and follows `user@<uid>.service` so the runtime directory
  exists, and takes anything else from `environment`. There is no unit-wide
  umask: agents create the user's own files. The token and the runtime config
  are created under a tight umask in prepare, and the database is protected by
  the 0700 state dir.
- **Prepare** runs as the user, never root, and does not open the database.
  1. Refuse if anything is listening on the port.
  2. State dir: create with mode 0700, or tighten an existing one.
  3. Token: if missing, write 32 random bytes (base64url) to a temporary file
     under `umask 077` and hard-link it into place, which is atomic and fails
     if a token exists. An existing token is never replaced; its mode is
     tightened to 0600. An empty or short token stops the start.
  4. Refuse if an enabled provider's program is not an absolute store path.
  5. **Which build may open the database.** The state dir holds a record of
     the last build that started (name and version). Same build: go on. A
     newer version of the same name: copy the database files to
     `<stateDir>/backups/<time>-<old version>/` first (nothing has the
     database open at this point), keep the newest five, then update the
     record. An older version, or a different package name: refuse, unless
     `allowDowngrade` is set. This is what stops the public release from
     opening a database another build has migrated when `local.nix` is lost,
     and what gives a generation rollback something to restore.
  6. Write `/run/ahp-gateway/config.json` (0600): the static settings plus the
     token. Every build accepts a token in its config, so there is one code
     path. The token is never in the Nix store, the unit file, a command line
     or the journal.
- **Config written by the module:** `listener.host = 127.0.0.1`, the port,
  `listener.allowAnyOrigin = false`, `allowedOrigins`, the token, the database
  path under `stateDir`, the providers, and `settings`.
- **For clients, `/etc/ahp-gateway/config.json`:** static, no secrets, always
  present. It names the port, the state dir and the token file, and says the
  service is managed by the system. It is not what the gateway reads. After a
  switch that changes the port it describes the next start, not the running
  process; `nix-update` says so when it reports that the running gateway is
  older than the installed one.
- **Ready means three things**, checked by `ExecStartPost` with a deadline:
  `readyPath` answers 200; the process listening on the port is this unit's
  main process (the service user can see the owner of its own socket); and a
  WebSocket upgrade without a token is answered 401. Any other answer fails
  the start. This holds whatever the build does with config keys it does not
  know.
- **A second gateway.** Step 1 and the "is it ours" check cover a gateway that
  is already running. They cannot stop someone starting the old hand-run
  launcher afterwards: that process would run its repair on the live database
  and then fail to bind. Only a lock in the gateway itself closes this. The
  migration removes the launcher from use, and the residual risk is stated in
  the migration instructions. For the same reason the public release is not
  put on `PATH`.

**Running**

- **A config push never restarts a running gateway**
  (`restartIfChanged = false`); it would interrupt every active turn for an
  unrelated change. The new build takes effect at the next restart or boot,
  including a restart after a crash; step 5 of prepare makes that safe.
  `nix-update` reports when the running gateway is older than the installed
  one. Note that a switch does start a gateway that was stopped by hand.
- **Garbage collection.** The old process keeps running from store paths that
  only an older generation references, and this box collects garbage daily.
  Nix treats the paths in a live process's environment as roots, but not the
  paths in its arguments or in a config file it has read. The unit therefore
  carries one variable naming a store path whose closure is the package, the
  providers and the static config, so the running gateway's files outlive
  their generation.
- **Out of memory.** `OOMPolicy=continue`: when the kernel kills one agent's
  runaway build, that turn fails and the rest keep running.
- **Everything the gateway starts is in the unit's control group**, including
  a tmux or dev server an agent leaves behind. It is stopped with the gateway.

**Failing and stopping**

- **Restart.** `on-failure`: a crash is restarted, a deliberate stop is not.
  Statuses in `noRetryExitStatus` are not retried. Everything else is retried
  until five starts fail inside the limit window, then the unit stays failed.
  The window is computed from the timeouts so that five never-ready starts
  fit inside it; with a fixed 300 s window and a 60 s ready timeout the limit
  would never be reached.
- **Telling someone.** `ahp-gateway-failed.service` is a one-shot defined by
  this module, started by `OnFailure=`. It appends a line to the box's boot
  history and posts to the notification webhook the box already uses, if one
  is configured. A second one-shot on a timer five minutes after boot does
  the same when the unit is not active, which also covers the case systemd
  does not count as a failure: `/home` never mounted, so the unit never
  started.
- **Stop.** `KillMode=mixed` sends SIGTERM to the gateway only, so it can stop
  its agents in order; whatever is left is killed at the timeout. A build that
  does not exit by itself makes every stop last the full timeout.
- **`PATH` for agents.** `~/.local/bin`, the user's profile directories,
  `/run/wrappers/bin`, `/run/current-system/sw/bin`, then a fixed base (git,
  bash, coreutils, findutils, grep, sed, awk, make, gcc, curl, openssh, Node,
  nix) and `extraPath`.
- **Not sandboxed, by design.** Agents edit the user's files and run the user's
  tools, including `sudo` on this box. The boundary is the listener: loopback
  only, origin allow-list, token.

### 2.5 Commands on `PATH`

For each name in `managementCommands` the module installs a wrapper that runs
that subcommand with `AHP_GATEWAY_STATE_DIR` and the `/etc` config path set.
The bare server command is never installed.

## 3. Options considered

| Topic | Rejected | Why |
|---|---|---|
| Delivery | Inline the module into the single file at render time | Hides the module boundary; the pushed file no longer matches the repository |
| Unit type | `systemd --user` with linger | Ordering against `/home` and against system services |
| Config push | Restart the gateway when its unit changes | Interrupts every active turn on unrelated changes |
| Token | Token in the Nix-generated config | The store is world-readable |
| Token | A token-file config key where the build has one | Two code paths, and a build that ignores the key would start open; the inline token works everywhere |
| Token | `LoadCredential=` | The user's editors need to read the same token file |
| Safety | A per-package feature table | A stale table fails open; checks against the running process do not |
| Providers | Keep the `npx --yes` launchers | Network and unpinned code at session start |
| Readiness | `Type=notify` | The gateway does not speak the notify protocol |
| Start limit | A fixed 300 s window | Never reached when each failed start takes a minute |
| Network | No ordering after the network | Spends the one automatic continuation on a network error at boot |

## 4. Migration and rollback

`enable` stays `false` in the template until the boot checklist in section 5
has been run once.

Migration: replace older CLI binaries; stop the hand-run gateway and stop using
its launcher; enable and switch; start the unit (prepare records the build and,
from then on, backs up before any newer build); point the editors at the token
file, because the hand-run gateway had no token; check that an existing session
is listed **and answers a prompt**.

Undo: stop and disable the unit, switch, start the hand-run gateway, remove the
token from the editors' settings. If a newer build has changed the database,
restore the backup that prepare made before that build first ran.

## 5. Test plan

Everything in the first table runs locally through `scripts/check.sh`, with no
VM. Unit behaviour is tested with transient units (`systemd-run --user`) built
from the same generated settings and a small stub gateway that can be told to
misbehave.

| Promise | Test | Kind |
|---|---|---|
| Template and module evaluate: disabled, enabled, enabled without a provider (must fail) | `nix-instantiate` | evaluation |
| Generated unit has the intended ordering, limits, environment and no secret; the limit window covers five never-ready starts | Assertions on the evaluated unit attributes | evaluation |
| Built-in providers are all disabled in the resolved config | The package's own `--print-config` on the generated config | build-time |
| Prepare: port in use; state dir and token modes; token never replaced; empty token; provider path check | Shell tests of the prepare script in a temporary directory | shell |
| Prepare: same build, newer build (backup made, five kept), older build and different name (refused; allowed with the option) | Shell tests | shell |
| Ready check: not ours, no 401, never ready | Transient unit with the stub | unit |
| Never ready: retried, limit reached, failure unit runs | Transient unit with scaled-down timeouts | unit |
| A crash is restarted; a listed exit status is not | Transient unit with the stub | unit |
| One child killed for memory: main process survives | Transient unit with a memory-limited child | unit |
| Stop mid-turn ends within the timeout and leaves no process | Transient unit with the stub | unit |
| The closure variable keeps provider paths alive | `nix-store --gc --print-roots` against a running transient unit | unit |
| CLI pushes and restores the set; switch is atomic; stale files removed; older set refused; old single-file saved copy still restores | Go tests on the file set and the remote script | Go |
| The real release starts from the generated config, serves a session through a scripted fake agent with networking unavailable to it, and refuses a wrong token and an unlisted origin | Local run on a temporary state dir and a free port | integration |
| The system builds on the box | `nixos-rebuild build`, no switch | by hand |

**Unverified until someone runs it on a machine with KVM or on a spare box**
(one script, `scripts/boot-check.sh`, on the owner's checklist): the unit
starts only after a slow `/home` mount and stops before unmount; a reboot
brings the gateway back with its sessions; a switch that changes the unit
leaves a running gateway alone; the failure notice arrives after an unattended
boot; migration from a database of the current release, and undo. Transient
user units are not system units: ordering against mounts and `User=` cannot be
shown locally.

## 6. Open points

1. Asked of the gateway's authors, not yet agreed: management commands that
   read `/etc/ahp-gateway/config.json`, and installers that refuse when it says
   the service is system-managed. Until then nothing stops a user-level unit of
   the same name being installed by hand; prepare's port check limits the
   damage.
2. A lock in the gateway on its state dir is the only complete answer to a
   second gateway on the same database.
3. The stop and ready timeouts should follow the gateway's own bounds once a
   build has them.
4. Which agent adapter build the box runs is set outside this repository.
