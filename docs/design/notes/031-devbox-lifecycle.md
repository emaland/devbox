# 031: Devbox lifecycle: auto-stop that sees agents, spot notices, clean stop, resume on boot

Status: **accepted at design review, revision 4.** Revision 4 records what
the code reviews of the implementation changed (sections 2.2, 2.3, 2.4, 2.6,
2.8). Revision 2 answered the
first review (every CLI stop path, stale spot notices, a requested stop while
the gateway restarts, where `resume status` reads its source); revision 3
takes the diff check's two remaining points (the unreachable rule in time,
and no cap exemption for attached clients). After revision 4, issue 14
widened the token file check to the gateway's own rule, read `ss` by column,
read the token as its owner, refused a root-owned token file and any answer
that is not exactly one JSON document of at most 64 KiB, and ended a spot
notice 5 min after its action time (sections 2.2 and 2.3). Builds on note 030 (the gateway as a
system unit). Covers auto-stop, the spot interruption watcher, the stop paths
and resume on boot. The CLI commands that sit on top (`devbox agents`,
`devbox stop` refusing while busy, `devbox tunnel`) are listed in section 2.6
only for what they need from the box.

## 1. Problem

The box is meant for "start a long agent task and walk away". Today:

- `devbox-autostop` counts the box as in use only when someone is logged in or
  has an inbound connection on port 22. An agent working with nobody connected
  is "idle", so the box stops itself in the middle of the work after the idle
  window (default 4 h).
- A spot interruption gives a two-minute warning that nothing reads.
- Nothing stops the gateway on purpose before the instance goes down. It is
  left to the OS shutdown, whose length EC2 does not state (section 1.1).
- When the box comes back, interrupted turns stay interrupted unless someone
  continues each one by hand.

### 1.1 How long does a stop give systemd? (DU1)

From the EC2 documentation, read 2026-11-23:

| Path | What EC2 promises | Source |
|---|---|---|
| `StopInstances`, default | "a graceful operating system (OS) shutdown". The force option acts "if the graceful shutdown fails to complete within the timeout period"; **the timeout is not stated** | [StopInstances API](https://docs.aws.amazon.com/AWSEC2/latest/APIReference/API_StopInstances.html), [stop methods](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/instance-stop-methods.html) |
| `StopInstances` with skip-OS-shutdown | No shutdown at all; default is off | same |
| Spot interruption, behaviour stop | A notice "two minutes before Amazon EC2 stops or terminates your Spot Instance", "on a best effort basis". **Nothing is said** about an OS shutdown at the end of the two minutes | [Spot interruption notices](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/spot-instance-termination-notices.html) |
| Rebalance recommendation | May come before the notice, or together with it; it is not an interruption | [Rebalance recommendations](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/rebalance-recommendations.html) |

On the box, systemd's default per-unit stop timeout is 90 s
(`systemctl show -p DefaultTimeoutStopUSec` prints `1min 30s`); the gateway
unit sets its own 45 s.

**Answer.** The only documented time is the 120 s spot notice. The design
therefore does not rely on the OS shutdown for anything it cares about: on
every path it controls, the gateway is stopped by systemd first and the
instance is stopped afterwards. The gateway's stop timeout must fit in the
spot budget:

```
120 s notice
 -  5 s poll interval (EC2 recommends checking every 5 s)
 -  2 s metadata request
 -  3 s status snapshot for the notification
 = 110 s ceiling for the gateway stop
```

The design budget is **60 s**, leaving 50 s of margin for a late notice. The
lifecycle module asserts `services.ahp-gateway.stopTimeoutSec <= 60` when the
watcher is enabled; the current 45 s fits. How long EC2 really waits for a
plain OS shutdown is unverified and can only be seen on a real instance (the
rehearsal, section 5).

## 2. Decisions

### 2.1 Layout

```
terraform/devbox/modules/devbox-lifecycle.nix   options, units, timers
terraform/devbox/lifecycle/
  gateway-status.sh    one /status request, shared by everything below
  autostop.sh          one auto-stop decision
  spot-watch.sh        the watcher loop (and --once for tests)
  notify.sh            boot-history line plus optional webhook post
  resume-summary.sh    the auto-resume line for the boot notification
  devbox-agents.sh     the box-side command the CLI runs over ssh
```

Every script takes its programs from `PATH`, which the module sets to store
paths. The tests put fakes first on `PATH`. `configuration.nix` imports the
module and replaces its inline `devbox-autostop` script with it; the timer
(every 10 min), the idle window file `/etc/devbox/autostop-after` and its
`off` value keep working as today.

### 2.2 Asking the gateway

`gateway-status.sh` reads `/etc/ahp-gateway/config.json` (written by the
gateway module: port and token file). No file means no gateway on this box;
the caller treats that as "not installed" and logs nothing.

- One `GET http://127.0.0.1:<port>/status`, 3 s limit.
- **The token** is read from the token file by root (auto-stop, watcher,
  boot notification) or by the user (`devbox-agents`). As the gateway reads
  it, one whole CRLF or LF line end is dropped and what is left must be
  printable ASCII without space (0x21 to 0x7e). A quote or a backslash is
  refused too, though the gateway accepts them, because either would change
  the curl config; this is the one way the check is narrower than the
  gateway's. Otherwise the request is not made and the result is
  "unreachable: token file unusable". It is given to curl as a config file
  on standard input (`-K -`), so it never appears on a command line, in the
  environment or in a log.
- **Only to the gateway.** The token file must be a regular file, not a
  link, not owned by root (the gateway never runs as root), with no group or
  other permission bits (0600 and 0400 both do), as the gateway itself
  requires; it is read with a size bound, so root cannot
  be pointed at another file, a FIFO or a device. The token is sent only
  when every listener on the port (`ss -Htlne`) is on `127.0.0.1` and
  belongs to the token file's owner. `ss` is read by column, never by
  matching text anywhere on the line: the cgroup path at the end of each
  line is chosen by the socket's owner and may contain spaces, so a match
  anywhere could be given a false address or owner. Column 6 is `uid:N`, or
  `ino:` for a socket owned by root, which counts as uid 0 and so never
  matches, as a root-owned token file is refused; anything else matches no
  owner. While the gateway is down, which is exactly when these
  checks run (after a spot stop, at boot, during a restart), another local
  user could otherwise bind the port and collect the token. Otherwise the
  result is "unreachable" with the reason.
- **Read as its owner.** The checks above are by path, so the owner could
  swap the file for a link to something only root may read between the
  checks and the read. Run as root, the file is therefore read with
  `setpriv` under the owner's account (uid and its primary gid, never the
  file's group), with no environment, standard input or standard error, and
  under a 3 s limit held by root outside the drop, in case a FIFO is swapped
  in. Its output goes to a file root made, never to a pipe, because the
  owner could open the reader's pipe through `/proc` and hold it, and root
  would wait. The answer is written by the listener's owner too, so it is
  capped at 64 KiB, parsed under a time limit, and refused unless it is
  exactly one JSON document (an empty answer or several are "unreachable"). curl is started with `-q` and `--noproxy '*'`, so root's
  curl config and proxy settings cannot send the header elsewhere.
- **Known limit.** The gateway could stop, and another user bind the port,
  in the moment between the listener check and curl's connect. Closing that
  needs a peer-credential check, which TCP on loopback does not give.
- Text from the answer never reaches a log line or a notice as it is: counts
  must be numbers, and words are reduced to letters.
- Results: `ok` with the JSON on standard output; `unreachable` with a reason
  (no answer, HTTP status, not JSON, no `turns` object). A build without a
  status endpoint, such as the public release, answers 404 and so counts as
  unreachable.

Classification, computed from the JSON:

| Result | Rule |
|---|---|
| busy | `turns.active - turns.waitingForUser > 0`, or `resume.pending > 0` |
| waiting | `turns.active > 0` and not busy: every active turn waits for a person |
| idle | `turns.active == 0` and nothing pending |

`resume` missing counts as nothing pending.

### 2.3 Auto-stop (`autostop.sh`)

Inputs: the idle window (`/etc/devbox/autostop-after`, default 4 h), the busy
cap (`/etc/devbox/autostop-busy-cap`, default 12 h, `off` disables it),
`who`, `ss`, the gateway status, and two markers in `/run/devbox-autostop/`:
`last-active` (the countdown, as today) and `busy-since`.

| # | Situation | Decision | Markers |
|---|---|---|---|
| 1 | Window is `off` | nothing | none |
| 2 | Someone logged in, or inbound :22 | active (login) | touch last-active, clear busy-since |
| 3 | Gateway busy, cap off or not reached | active (agent) | touch last-active, set busy-since if unset |
| 4 | Gateway busy, busy for at least the cap | **stop** (cap) with a notice | |
| 5 | Gateway waiting for a person | not activity | clear busy-since |
| 6 | Gateway unreachable | not activity, logged | clear busy-since |
| 7 | No gateway installed | not activity | clear busy-since |
| 8 | 5 to 7, window elapsed since last-active | **stop** (idle) | |
| 9 | 5 to 7, window not elapsed | nothing | |
| 10 | `stop-when-idle` requested (2.6), and the gateway answered idle or waiting, or no gateway is installed, or it has been unreachable without a break for 30 min | **stop** (requested), logins ignored | `unreachable-since` |

Row 10 never stops on an unreachable answer alone: right after boot, during
start-up repair or a restart after a crash, the gateway cannot yet report the
continuations it is about to start. Thirty minutes unreachable is "the
gateway is not coming back", and the box is not kept up for it. The rule is
in time, not polls, because each request also runs a poll at once.

"Continuously busy" means busy at every poll: one poll that is not busy
clears `busy-since`. A login clears it too, since the cap exists for boxes
nobody is watching. An attached editor does not: a laptop left awake with a
tunnel up must not keep a stuck agent running, and a tunnel over ssh already
counts as a login. A turn that waits for permission is not activity
(row 5), so a box left with a question on screen stops after the idle window
like before. The waiting turn is continued at the next start, once and only
within the gateway's maximum age for continuations; a second interruption, or
a start more than that age later, leaves it as an interrupted turn the person
continues by hand.

**Logging.** `/var/log/boot-history` gets a line in the existing
three-field format, `<time> | auto-stop | <decision>`, when the decision differs from
the previous poll's (kept in `/run/devbox-autostop/last`) and on every stop.
Logging every poll would write 144 lines a day and push the boot entries out
of the 20 shown at login. An unreachable gateway is therefore logged when it
starts being unreachable and again when the reason changes.

**Stopping**, in this order:

1. Log the stop line and send the notice (`notify.sh`, bounded to 10 s): why,
   and for the cap, how many turns were running.
2. `systemctl stop ahp-gateway.service`, bounded by the unit's own stop
   timeout plus 15 s. Its result and duration are logged. A failure does not
   prevent step 3: the OS shutdown tries again.
3. `aws ec2 stop-instances` on itself, as today. If it fails, the failure is
   logged and noticed **once**, and the gateway is started again, so the box
   is not left running with its work interrupted and nothing to continue it.
   The next try is a full idle window (or cap) later: `last-active` and
   `busy-since` restart and a requested stop is dropped. Trying again at the
   next poll would stop the gateway every 10 minutes, and each stop uses up
   the interrupted turns' one continuation. The gateway is not started again
   while the spot watcher has a current interruption notice: one that is
   not stale, not done, and whose action time is less than 5 min ago (the
   watcher's own limit, section 2.4). The time limit ends a notice that found
   the gateway already down, which the watcher never marks done.
4. The poll has a start timeout (the gateway's stop bound plus 15 s plus
   120 s), so one that hangs cannot hold off every later poll.

### 2.4 Spot watcher (`spot-watch.sh`, `devbox-spot-watch.service`)

A long-running service (`Restart=always`) that polls the instance metadata
service every 5 s with IMDSv2. The session token is requested with a 6 h
lifetime and renewed when it is 5 min from expiry or a request answers 401.
The base address is an environment variable so tests can point it at a fake.

| Metadata says | Action |
|---|---|
| `spot/instance-action` 404 and `events/recommendations/rebalance` 404 | nothing |
| a new `instance-action` (key: action and time) whose time is after this boot began and no more than 2 min in the past | log; take a status snapshot; start the gateway stop; post the notice with the action, its time, the session total and the running and waiting turn counts, concurrently with the stop; log the stop's result and duration |
| an `instance-action` whose time is before this boot began, or more than 2 min in the past when first seen | stale: log once, do not stop |
| a new rebalance recommendation (key: notice time) | log and post a notice; **do not stop** |
| metadata unreachable or unexpected answer | log when it becomes unreachable and when it recovers; keep polling |
| the watcher stopped the gateway, and the instance is still running 5 min after the action time, whether or not the notice is still shown | start the gateway again, log and notice "interruption did not happen"; the key is done and never stops the gateway again |

The same handling applies to `stop`, `hibernate` and `terminate`. A
terminated spot instance does not come back, but its `/home` volume does, so
a clean stop still matters.

A rebalance recommendation does not mean the instance will be interrupted;
it may come long before a notice or never be followed by one. Stopping the
gateway on it would interrupt work for nothing, so only the interruption
notice stops it.

**Stale notices.** EC2 says notices are best effort and documents at least
one metadata item that stays after the event with a time in the past. A
notice that outlives its event must not stop the gateway at the next boot,
which would interrupt the continuations that just started and use up their
one attempt. Hence the two time checks above; boot time is read from
`/proc/stat` (`btime`).

**State.** Per key, in `/run/devbox-spot-watch/` (0700, root): `seen`
(logged), `stopping` (written before `systemctl stop`), `stopped` (written
after it returned: `ours`, or `not running` when the gateway was already
down), and `noticed`. A watcher that dies between steps redoes only the steps
not marked; `systemctl stop` is safe to repeat, and a `stopping` marker with
the gateway already down still counts as `ours`, so the gateway is started
again if the interruption does not happen. The notice is posted until
`noticed` exists, independent of the stop.

**After hibernation** the instance resumes with `/run` and the boot time
intact, so the 5-minute rule starts the gateway again at once; the log and
notice say "resumed after hibernation", not "did not happen".

**Untrusted values.** Only `stop`, `hibernate` and `terminate` are accepted
as an action, and a time only in the characters of an ISO UTC time; anything
else is "not the expected JSON". The IMDS session token is checked the same
way and goes to curl on standard input.

### 2.5 Every stop path stops the gateway first

| Path | Who stops the gateway | Bound |
|---|---|---|
| Auto-stop (idle, cap, requested) | `autostop.sh`, before `stop-instances` | unit timeout + 15 s |
| `devbox down` (alias `stop`), `restart`, `reboot`, `terminate`, and `resize` for the instance it replaces | the CLI, through one shared guard (below), before the EC2 call | same |
| Spot interruption | the watcher, on the notice | 60 s budget (1.1) |
| `shutdown` on the box, console or API stop from elsewhere | systemd, in reverse dependency order | unit timeout 45 s |

The instances `spawn` and `resize` stop right after launching them, to attach
a volume, have run no agent and are not guarded.

**The CLI guard**, for each instance named, before any EC2 call:

1. Read the status over ssh (`devbox-agents status --json`). Busy: refuse and
   print the running and waiting counts. Unreadable: refuse and say why.
   `--force` goes ahead in both cases. With several instances, all are
   checked before any is touched, and one refusal stops the whole command.
2. Check that the EC2 call is allowed (`DryRun`), so a missing permission or
   expired credentials fail before the gateway is stopped.
3. Stop the gateway over ssh (`sudo systemctl stop ahp-gateway.service`,
   bounded). If ssh fails here and `--force` was given, go on.
4. Make the EC2 call. If it fails, start the gateway again over ssh.

`--hard` is the EC2 force stop for a wedged instance: it skips steps 1 to 3.
It replaces today's `--force` on `down` and `restart`, so the two commands
agree; this is a change in a user-visible flag and is called out in the CLI
pull request.

In the last row the gateway is stopped before `/home` is unmounted because
the gateway unit has `RequiresMountsFor=` on its state dir and the home
directory, which orders it after the mount when starting and so before it
when stopping. The tests check the generated unit for this ordering; that a
real shutdown honours it is unverified without a VM (section 4).

### 2.6 What the CLI needs on the box

`devbox-agents` is installed on `PATH` and runs as the user over the existing
ssh path:

- `devbox-agents status [--json]`: the classification, the session total and
  the turn counts from the status endpoint (it has no per-session list);
  non-zero exit when the gateway is unreachable.
- `devbox-agents resume status`: the `resume` object of the status endpoint,
  which is the running gateway's effective value and its source. It does not
  ask the gateway's command, because the gateway module's wrapper points that
  command at the client config in `/etc`, which has no `resume` key; a
  command that worked out the install default from that file would report
  `off` while the running gateway is on `continue`. DV-A and GW-R are told.
- `devbox-agents resume on|off`: runs the gateway's own `resume` command
  through the gateway module's wrapper, named by its absolute store path
  (`services.ahp-gateway.generated.commands`), never looked up on `PATH`,
  where a copy of the gateway the user installed could come first. It writes
  nothing itself. When the build or its wrapper does not offer `resume`, it
  says so and exits non-zero. It never starts the bare gateway command, which
  for the public release would start a second server.
- `devbox-agents stop-when-idle [cancel]`: writes or removes
  `/run/devbox-autostop/stop-when-idle` with `sudo` (the box's user is in
  `wheel` with passwordless sudo, which the CLI already relies on), then runs
  one auto-stop poll at once.

`devbox stop --when-idle` uses row 10.

### 2.7 Resume on boot

When the gateway is enabled, the lifecycle module sets, with `mkDefault`:

```nix
services.ahp-gateway.settings.resume = { onStart = "continue"; maxAttemptsPerTurn = 1; };
```

It does not set `AHP_GATEWAY_RESUME` in the unit, so a persisted
`devbox agents resume off` still wins over this install default. A build that
does not know `resume` ignores the key; whether resume is really available is
read from `/status`, never from the config being accepted. The gateway module
is not changed.

The boot notification (`devbox-slack-notify.service`) gains one line from
`resume-summary.sh`. It already runs once per boot after the network; it now
also runs after `ahp-gateway.service` (which, with the readiness check in the
gateway module's open change, means after the gateway is ready or has
failed) and waits up to 180 s for `/status` to report no pending
continuations. Its post goes through `notify.sh`, so the user's secrets file
is read for the one address it needs instead of being run as a shell script
by root. The unit gets `restartIfChanged = false`, so deploying this change
does not send a spurious "booted" post. It reports:

| `/status` | Line |
|---|---|
| `resume` present | `auto-resume: <policy> (<source>): continued N, failed F, skipped S` (`, still pending P` when the wait ran out) |
| answered, no `resume` | `auto-resume: not reported by this gateway build` |
| unreachable | `gateway: not answering (<reason>)` |
| no gateway | no line |

### 2.8 Options (`services.devbox-lifecycle`)

| Option | Default | Meaning |
|---|---|---|
| `enable` | `false` | Install auto-stop, the watcher and `devbox-agents` |
| `logFile` | `/var/log/boot-history` | Where decisions go |
| `webhookFile` | `null` | File with a `SLACK_WEBHOOK_URL=` line; read, never run |
| `autostop.defaultWindow` | `"4h"` | Used when the window file is absent |
| `autostop.defaultBusyCap` | `"12h"` | Used when the cap file is absent |
| `spotWatch.enable` | `true` | The watcher |
| `spotWatch.pollSeconds` | `5` | |
| `resumeOnBoot` | `true` | The settings in 2.7 |

There is no `user` option and no sudo rule of the module's own: `devbox-agents
stop-when-idle` uses `sudo`, which on this box's user is passwordless through
`wheel`. A box whose user is not in `wheel` needs a rule for that one command.

`configuration.nix` enables it and sets `webhookFile` to the same file the
boot notification already reads.

## 3. Options considered

| Topic | Rejected | Why |
|---|---|---|
| Busy signal | Count agent processes in the gateway's cgroup | A waiting turn keeps its process; it would count as busy, which is the wrong answer for a permission prompt |
| Busy signal | `ahp-acp-gateway status --json` | Needs the build to offer the command through the wrapper; the HTTP endpoint is the same data and works whatever is on `PATH` |
| Cap | No cap | A stuck agent would keep a paid instance up forever |
| Logging | A line every poll | Floods the history shown at login |
| Rebalance | Stop the gateway on a rebalance recommendation | Interrupts work that may never be interrupted |
| Watcher | Rely on the OS shutdown at the end of the notice | EC2 does not document one |
| Watcher | EventBridge rule outside the instance | Creates AWS resources and needs a path back into the box |
| Stop order | Stop the instance and let systemd stop the gateway | The OS shutdown's time is not documented |
| Resume toggle | Set `AHP_GATEWAY_RESUME=continue` in the unit | Would override the user's persisted `off` |
| `--when-idle` | Wait on the laptop until idle, then stop | The laptop has to stay up and connected |

## 4. Effect on the gateway contracts

None changed. The design reads the busy definition of the status endpoint
exactly as the gateway defines it, sets only the install default of the
resume switch, and calls the gateway's own `resume` command. It needs, from
the gateway: the status endpoint with `turns` and `resume`; a clean stop on
SIGTERM within its stop bound; the `resume` command offered through the
module's wrapper (`managementCommands`). Until a build has them, auto-stop
behaves as today plus logging, the watcher still stops the gateway in time,
and the boot line says the build does not report resume.

## 5. Migration and rollback

The module is enabled in `configuration.nix` and replaces the inline
auto-stop. The markers move from `/run/devbox-last-active` to
`/run/devbox-autostop/`; both are under `/run`, so the first poll after a
switch starts a fresh countdown, the same as after a boot. Rollback is the
previous generation; the window file is unchanged.

The rehearsal on a spawned spot clone (separate issue, with the user's
approval per run) is the only place the real EC2 timings can be seen.

## 6. Test plan

| Promise | Test | Kind |
|---|---|---|
| Every row of the auto-stop table, including the cap reached, the cap `off`, an unreachable gateway logged once and again when the reason changes, a bad token file | Go test runs `autostop.sh` with fakes for `who`, `ss`, `aws`, `systemctl`, `date` and a local HTTP server for `/status` | shell |
| The token reaches the gateway only as the `Authorization` header and is in no argument, log line or notice | Same harness: the server checks the header, the fakes record their arguments | shell |
| Stop order: notice, gateway stop, then `stop-instances`; a failed gateway stop still stops the instance; a failed `stop-instances` starts the gateway again | Fakes record the order | shell |
| Row 10: a requested stop does not happen on quick repeated unreachable polls, nor before 30 min unreachable after an answer, and does at 30 min; an attached client does not hold off the cap | Same harness | shell |
| Watcher: no notice; stop notice (gateway stopped once, notice posted once, not repeated after a watcher restart, redone when the watcher died before `stopped` was written); rebalance (notice, no stop); a notice dated before boot or more than 2 min in the past (no stop); metadata unreachable (logged once) and recovering; token renewal on 401; still running 5 min after the action time, with the notice still shown, starts the gateway once | Go test runs `spot-watch.sh --once` repeatedly, and once in its loop, against a fake metadata server, with boot time and clock from fakes | shell |
| CLI guard on `down`, `restart`, `reboot`, `terminate`, `resize`: refuses busy and unreadable, `--force` goes on, `--hard` skips ssh, a failed dry run stops before the gateway is touched, a failed EC2 call starts the gateway again, several ids are all checked first | Go tests with the ssh call and EC2 faked | Go |
| Resume summary: each row of 2.7, and the wait ending while still pending | Go test with the HTTP server | shell |
| `devbox-agents resume` never runs the bare gateway command | Go test with a fake wrapper that refuses | shell |
| Module evaluates; the units and timers have the intended settings; `resume.onStart` reaches the gateway's config and a user value wins; the 60 s assertion fires | `nix-instantiate` and assertions on the evaluated config | evaluation |
| The gateway unit is ordered against `/home` | Assertion on the evaluated unit | evaluation |
| Scripts pass shellcheck | `scripts/check.sh shell` | static |
| The system builds | `nixos-rebuild build`, no switch | by hand |

**Needs a VM or a real instance; unverified here** (this machine has no KVM):
a busy turn keeps a booted system up past a short idle window; a fake metadata
notice stops the gateway before the unit's timeout; a reboot continues an
interrupted turn exactly once; shutdown stops the gateway before `/home` is
unmounted. These are written as NixOS VM tests with a stub gateway, evaluated
locally, and run on a machine with KVM; continuing a real turn exactly once
needs the gateway build that implements resume.

## 7. Open points

0. The gateway module's open change adds its own failure notice
   (`failureNotice.*`, `notice.sh`) with the same webhook-file reading. This
   note keeps `notify.sh` as the one general poster; once both are merged,
   the gateway's notice can call it and the two `webhookFile` options can
   become one. The gateway's hourly check will also report a gateway the
   watcher stopped deliberately; that is true and is left as is.

1. The 5 min grace before restarting a gateway after a withdrawn notice is a
   guess; EC2 does not say whether a notice can be withdrawn.
2. On an on-demand instance the watcher only ever sees 404; it is cheap enough
   to leave on everywhere.
3. A stopped spot instance comes back on its own when capacity returns; an
   auto-stopped one does not. The notice says which happened.
