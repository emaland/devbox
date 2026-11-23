# 030: AHP gateway as a NixOS module and boot service

Status: **proposed, revision 4** after three design reviews. No code yet.

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
- **One switch point.** On the box the whole set, main file included, is
  unpacked to a new directory `devbox.<stamp>`, and `devbox` is a symlink
  switched to it in one rename. `/etc/nixos/configuration.nix` is a fixed
  symlink to `devbox/configuration.nix`. There is no moment at which a new
  main file sits beside an old tree. Old directories are removed afterwards.
- The saved copy under `~/.config/devbox/` is laid out and switched the same
  way and includes `local.nix`. The boot-time restore service restores the
  set when the saved `devbox` link names a complete directory that differs
  from the one in `/etc/nixos`, and restores an old single-file saved copy as
  it does today.
- **A fresh root volume** (instance replacement, resize) has no `local.nix`
  and has not run the restore service. The remote half of a push therefore
  restores the saved `local.nix` itself, after `/home` is mounted and before
  the switch, when none is being pushed and none is in `/etc/nixos`. The saved
  set also records whether a `local.nix` is expected; if one is expected and
  none can be found, the push stops before the switch and says so, instead of
  building a system without the operator's settings.
- `local.nix` is the operator's own module, never in this repository, pushed
  from the path in a new config key `nix_local_module`. With the key unset the
  CLI leaves the box's copy alone and says whether one is there. It is for
  package overrides and values that are not public. It is **not** for secrets:
  it is world-readable on the box and its values end up in the Nix store.
- **Older CLI binaries.** A binary from before this change pushes its own
  embedded single file, which has no gateway module: the switch would remove
  the unit and stop the gateway. The box keeps `layout-version` in
  `/etc/nixos/`; the remote half of a push refuses when the pushed set is older
  than the box's, and a set with no `layout-version` counts as 0. Binaries
  that predate the check cannot be stopped this way; the migration
  instructions say to replace them first.
- `--file` naming a main file with no `devbox/` tree beside it is an error.

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
| `providers` | (required, no default) | Agent providers: id, store path of the program, arguments, environment. Enabling the module without one fails evaluation |
| `settings` | `{}` | Extra keys merged into the gateway config. Keys the module owns (`listener`, `auth`, `database`, `providers`) are rejected at evaluation |
| `readyPath` | `/healthz` | HTTP path that answers 200 when the gateway is ready |
| `noRetryExitStatus` | `[]` | Exit statuses that mean "restarting will not help" for this build |
| `managementCommands` | `[]` | Subcommands of this build that are safe to expose on `PATH` (none for the public release) |
| `allowDowngrade` | `false` | Let a build older than the last one that ran open the database |
| `environment` / `extraPath` | `{}` / `[]` | Extra variables and `PATH` entries for agents |
| `readyTimeoutSec` / `stopTimeoutSec` | `60` / `45` | Start and stop bounds |

On this box `enable` and `providers` are set in `local.nix`, because the choice
of agent adapter is the operator's. That is why a missing `local.nix` must stop
a push (2.1) rather than quietly produce a system with no gateway.

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
- **Providers.** Each is a Nix package referenced by store path. The module
  reads the built-in provider ids from the package's default config at build
  time. A built-in that is not in `providers` is disabled. A provider whose id
  equals a built-in's is written as one entry that replaces the built-in's
  program, arguments and environment in full, so no `npx --yes` launcher stays
  enabled, including in a build that adds a built-in. Keeping the built-in id
  matters: a session stores the id of its provider, and the public release
  refuses a prompt for a session whose provider id it does not know. Sessions
  made by the hand-run gateway keep working only if the adapter keeps its id. Which
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
StartLimitIntervalSec=<6 x (2 x TimeoutStartSec + stopTimeoutSec + 2)>
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
  it. Serving and starting sessions need no network, but anything a gateway
  does on its own at start that reaches a model would fail, and on this box
  the network comes up several seconds after `/home`. An offline box still
  starts, after the target's own timeout.
- **Session environment.** A system unit has no login session, so the unit
  sets `HOME`, `SHELL` (the user's configured shell), `XDG_RUNTIME_DIR` and
  `NIX_PATH`, wants and follows `user@<uid>.service` so the runtime directory
  exists, and takes anything else from `environment`. There is no unit-wide
  umask: agents create the user's own files. The token and the runtime config
  are created under a tight umask in prepare, and the database is protected by
  the 0700 state dir.
- **Prepare** runs as the user, never root, and does not open the database.
  1. Refuse if anything is listening on the port, or if any process of this
     user has the database file open (a scan of `/proc/*/fd`). The second
     check catches a gateway on another port, and one still in its start-up
     repair before it has bound.
  2. State dir: create with mode 0700, or tighten an existing one.
  3. Token: if missing, write 32 random bytes (base64url) to a temporary file
     under `umask 077` and hard-link it into place, which is atomic and fails
     if a token exists. An existing token is never replaced; its mode is
     tightened to 0600. An empty or short token stops the start.
  4. Refuse if an enabled provider's program is not an absolute store path.
  5. **Which build may open the database.** The state dir holds a record of
     the last build that started: package name, version and store path.
     - Same store path: go on.
     - No record but a database exists (the first start after the hand-run
       gateway): back up, then record.
     - Same name and version, different store path: back up, then record.
     - Same name, newer version: back up, then record.
     - Same name, older version, or a different name: refuse, unless
       `allowDowngrade` is set.

     Versions are compared the way `sort -V` and Nix compare them, in which a
     suffixed version such as `1.2.3-x.1` is newer than `1.2.3`, and `x.10` is
     newer than `x.2`. The rule is fixed here because other schemes order a
     suffix the other way.

     A backup is a copy of the database file and its `-wal` and `-shm`
     companions to a temporary directory, renamed to
     `<stateDir>/backups/devbox-<time>-<old version>/` when complete. Only
     complete directories with that prefix are pruned, to the newest five, and
     never the one just made. The record is written after the backup and
     before the build starts, so a build that fails its first start is still
     "the last build": going back to the previous generation then needs
     `allowDowngrade` and, if the database was changed, the backup.

     This is what stops the public release from opening a database another
     build has migrated when the operator's override is lost, and what gives a
     generation rollback something to restore. It cannot tell two builds apart
     that share a name, a version and a schema change; a build that changes
     the database must change its version.
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
  WebSocket upgrade to the listener path, sent with no `Origin` header and no
  token, is answered 401 (a plain GET would be 404, and an unlisted origin
  403, so neither proves anything). The main process must be among the
  socket's owners and the socket must be bound to `127.0.0.1`. Any other
  result fails the start, and the check stops at once if the main process has
  exited. This holds whatever the build does with config keys it does not
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
  fit inside it, allowing a full start timeout for prepare and another for the
  ready wait; with a fixed 300 s window and a 60 s ready timeout the limit
  would never be reached.
- **Telling someone.** `ahp-gateway-failed.service` is a one-shot defined by
  this module, started by `OnFailure=`. It appends a line to the box's boot
  history, which is on the root volume, and posts to the notification webhook
  the box already uses, if one is configured. A second one-shot on a timer,
  five minutes after boot and hourly after that, does the same when the unit
  is not active or has restarted more than a few times since the last look,
  which covers a slow crash loop and the case systemd does not count as a
  failure: `/home` never mounted, so the unit never started. Neither one-shot
  depends on `/home`. The webhook address, however, is read from a file on
  `/home`, so when `/home` is missing the only trace is the boot-history line
  shown at the next login. That gap is accepted and stated.
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
| Network | No ordering after the network | Anything the gateway starts on its own at boot would run before the network is up |

## 4. Migration and rollback

`enable` stays `false` in the template until the boot checklist in section 5
has been run once.

Migration: replace older CLI binaries; stop the hand-run gateway and stop using
its launcher; enable and switch; start the unit (prepare finds a database and
no record, so it backs up first, then records the build); point the editors at
the token
file, because the hand-run gateway had no token; check that an existing session
is listed **and answers a prompt**.

Undo: stop and disable the unit, switch, start the hand-run gateway, remove the
token from the editors' settings. If the build the unit ran has changed the
database, first restore the backup prepare made before that build's first
start: remove the live database file and its `-wal` and `-shm` companions, copy
the three backed-up files in, and delete the build record.

## 5. Test plan

Everything in the first table runs locally through `scripts/check.sh`, with no
VM. The merge gate has to run that script for these tests to count; today it
runs only the Go checks. The suite needs a user systemd manager, user
namespaces, network access the first time the gateway's dependencies are
built, and it runs one process at a time. Unit behaviour is tested with transient units (`systemd-run --user`) built
from the same generated settings and a small stub gateway that can be told to
misbehave.

| Promise | Test | Kind |
|---|---|---|
| Template and module evaluate: disabled, enabled, enabled without a provider (must fail) | `nix-instantiate` | evaluation |
| Generated unit has the intended ordering, limits, environment and no secret; the limit window covers five never-ready starts | Assertions on the evaluated unit attributes | evaluation |
| Built-in providers are all disabled in the resolved config | The package's own `--print-config` on the generated config | build-time |
| Prepare: port in use; state dir and token modes; token never replaced; empty token; provider path check | Shell tests of the prepare script in a temporary directory | shell |
| Prepare: every row of the build-record rule, including a database with no record, `1.2.3` to `1.2.3-x.1`, `x.2` against `x.10`, and the reverse refused; a killed backup leaves no partial directory and evicts nothing | Shell tests | shell |
| Prepare: another process has the database open | Shell test with a process holding the file | shell |
| `settings` cannot set a module-owned key | `nix-instantiate`, must fail | evaluation |
| A session made by the public release under its default provider id still answers a prompt after the switch to the generated config, and after a restart | Local run: real release and fake agent on a temporary state dir, stop, prepare plus generated config, prompt the old session | integration |
| A push to a box with no `local.nix` restores the saved one, and stops when one is expected and missing | Go tests on the remote script | Go |
| Ready check: not ours, no 401, never ready | Transient unit with the stub | unit |
| Never ready: retried, limit reached, failure unit runs | Transient unit with scaled-down timeouts | unit |
| A crash is restarted; a listed exit status is not | Transient unit with the stub | unit |
| One child killed for memory: main process survives | Transient unit with the child in its own memory-limited scope | unit |
| Stop mid-turn ends within the timeout and leaves no process, with a lingering grandchild in the group | Transient unit with the stub | unit |
| A listed exit status during the ready wait is not retried and does not wait for the deadline | Transient unit with the stub | unit |
| The closure variable keeps provider paths alive | `nix-store --gc --print-roots` against a running transient unit | unit |
| CLI pushes and restores the set; switch is atomic; stale files removed; older set refused; old single-file saved copy still restores | Go tests on the file set and the remote script | Go |
| The real release starts from the generated config, serves a session through a scripted fake agent with gateway, agent and client together in a network namespace that has only loopback, and refuses a wrong token and an unlisted origin | Local run on a temporary state dir and a free port | integration |
| The system builds on the box | `nixos-rebuild build`, no switch | by hand |

**Unverified until someone runs it on a machine with KVM or on a spare box**
(one script, `scripts/boot-check.sh`, on the owner's checklist): the unit
starts only after a slow `/home` mount and stops before unmount; a reboot
brings the gateway back with its sessions; a switch that changes the unit
leaves a running gateway alone; the failure notice arrives after an unattended
boot; a push to a fresh root volume; the restore of the set at boot; the
layout refusal against a real box; the environment an agent sees under `User=`.
Transient user units are not system units: ordering against mounts, `User=`,
the failure units running as root and the system runtime directory (the local
tests use the user manager's runtime directory instead) cannot be shown
locally.

## 6. Open points

1. Nothing in the public release reads `/etc/ahp-gateway/config.json`. A
   build whose own tooling can install a user-level unit of the same name
   should refuse when that file says the service is system-managed. Until one
   does, prepare's checks limit the damage.
2. A lock in the gateway on its state dir is the only complete answer to a
   second gateway on the same database.
3. The stop and ready timeouts should follow the bounds of the build in use.
4. Backups are pruned only by this module, and only its own directories. If a
   build's tooling keeps backups in the same directory they must not share the
   `devbox-` prefix.
5. Which agent adapter build the box runs is set outside this repository.
