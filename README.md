# devbox

A CLI for managing persistent NixOS spot instances on AWS. It handles the full lifecycle — listing, starting, stopping, resizing, DNS updates, spot price browsing, and spinning up fully-configured clones — all from your terminal.

## Why

Running a dev workstation as a persistent EC2 spot instance is cheap, but managing it requires juggling the AWS console, Terraform, and SSH. devbox wraps the common operations into single commands so you can resize your box, check spot prices, or spin up a clone without leaving the terminal.

## Install

```bash
go install github.com/emaland/devbox@latest
```

This puts the `devbox` binary in your `$GOPATH/bin` (or `$GOBIN`). Make sure that's on your `PATH`.

## Building from source

```bash
git clone git@github.com:emaland/devbox.git
cd devbox
go build -o devbox .

# Move to somewhere on your PATH
mv devbox ~/bin/        # or /usr/local/bin, etc.
```

Requires Go 1.21+. No runtime dependencies beyond AWS credentials.

## Configuration

devbox reads its config from `~/.config/devbox/default.json`. If the file doesn't exist, built-in defaults are used. Every field is optional — omit any field to keep the default.

```bash
mkdir -p ~/.config/devbox
```

Example config:

```json
{
  "dns_name": "dev.frob.io",
  "dns_zone": "frob.io.",
  "ssh_key_name": "dev-boxes",
  "ssh_key_path": "~/.ssh/dev-boxes.pem",
  "ssh_user": "emaland",
  "security_group": "dev-instance",
  "iam_profile": "dev-workstation-profile",
  "default_az": "us-east-2a",
  "default_type": "m6i.4xlarge",
  "default_max_price": "2.00",
  "spawn_name": "dev-workstation-tmp",
  "nixos_ami_owner": "427812963091",
  "nixos_ami_pattern": "nixos/24.11*"
}
```

### Config fields

| Field | Default | Description |
|-------|---------|-------------|
| `dns_name` | `dev.frob.io` | The DNS A record devbox manages |
| `dns_zone` | `frob.io.` | Route 53 hosted zone (trailing dot required) |
| `ssh_key_name` | `dev-boxes` | EC2 key pair name for launched instances |
| `ssh_key_path` | `~/.ssh/dev-boxes.pem` | Local path to the SSH private key |
| `ssh_user` | `emaland` | SSH username |
| `security_group` | `dev-instance` | EC2 security group name for spawned instances |
| `iam_profile` | `dev-workstation-profile` | IAM instance profile for spawned instances |
| `default_az` | `us-east-2a` | Default AZ for `spawn` |
| `default_type` | `m6i.4xlarge` | Default instance type for `spawn` |
| `default_max_price` | `2.00` | Default spot max price ($/hr) for `spawn` |
| `spawn_name` | `dev-workstation-tmp` | Default Name tag for `spawn` |
| `nixos_ami_owner` | `427812963091` | AWS account ID that owns the NixOS AMIs |
| `nixos_ami_pattern` | `nixos/24.11*` | Glob pattern for AMI name lookup |
| `dns_zone_id` | (looked up) | Route 53 hosted zone id rendered into the box's boot-time DNS updater. Leave unset: `nix-update`, `spawn` and `resize` look it up from `dns_zone`. Set it only if the lookup is not possible |
| `authorized_keys_file` | `~/.config/devbox/authorized_keys` | Local file of extra ssh public keys for the box, one per line (see "Machine-specific values" below) |
| `nix_local_module` | (none) | A NixOS module of your own, pushed to the box as `local.nix` beside the config (see "How the config reaches the box" below) |

## Infrastructure setup

The `devbox infra` command provisions all the AWS resources devbox depends on. It wraps Terraform so you don't have to touch `.tfvars` files or run terraform commands manually.

### Quick start

```bash
# Make sure your config is set up, then:
devbox infra
```

That's it. The command will:

1. Auto-detect your Route 53 hosted zone ID from the `dns_zone` in your config
2. Auto-detect your SSH public key (looks for `.pub` file next to your configured private key, then `~/.ssh/id_ed25519.pub`, then `~/.ssh/id_rsa.pub`)
3. Write `terraform.tfvars` with the detected values
4. Run `terraform init` (first time only), `validate`, and `plan`
5. Show you the plan and ask for confirmation
6. Run `terraform apply`

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--dns-zone-id` | auto-detected | Override the Route 53 zone ID |
| `--ssh-public-key` | auto-detected | Provide SSH public key directly |
| `--ssh-public-key-file` | — | Read SSH public key from a file |
| `--dir` | `./terraform` | Path to the terraform directory |
| `--auto-approve` | false | Skip the y/N confirmation prompt |

### What it creates

| Resource | Description |
|----------|-------------|
| **EC2 key pair** | SSH key pair (name from `ssh_key_name` config) |
| **Security group** | Allows inbound SSH (22/tcp) and Tailscale (41641/udp), all outbound. Attached to the default VPC |
| **IAM role + instance profile** | Grants instances permission to update Route 53 records so DNS stays correct after spot interruptions |
| **EBS volume** | 512 GiB gp3 persistent data volume (3000 IOPS, 250 MB/s). Has `prevent_destroy` enabled so it can't be accidentally deleted |

### NixOS system configuration

The Terraform directory also includes `configuration.nix`, which defines what runs on the instances:

- SSH with pubkey auth only (root login prohibited)
- Tailscale VPN with auto-connect on boot
- Docker enabled, with its data on the persistent `/home` volume, size-capped container logs and a weekly prune of unused images (volumes are never pruned)
- Nix builds use scratch space on `/home`, the store is garbage-collected automatically when the root disk runs low, and the system journal is capped at 1 GiB
- `/home` mounted from a persistent EBS volume (by label, so it works across instance types)
- Boot-time DNS update via Route 53
- Boot history logger — every boot appends instance metadata to `/var/log/boot-history`
- Auto-stop idle timer — instance self-stops after 4h idle by default (resets whenever a session is active), configurable via `devbox stop --after`
- home-manager switch on boot — applies latest home-manager config (supports remote flakes or local config)
- IMDS pinned to the primary interface via policy routing (a dedicated routing table ordered after the link is up), so Docker/Tailscale veth interfaces can't hijack the `169.254.169.254` route and break instance-metadata access
- Reboot-stable: `amazon-init` is detached from `multi-user.target` so it won't re-apply the bootstrap user-data and revert the system on reboot
- MOTD showing last 20 boot events on login
- Lingering enabled for `emaland`, so home-manager's systemd `--user` units start at boot and survive logout (see "Required secrets" below)
- System packages: git, curl, wget, htop, tmux, vim, jq, python3, emacs, gcc, make, awscli, home-manager

### How the config reaches the box

`configuration.nix` imports modules from `terraform/devbox/`, so the config is
pushed as a **set**: the rendered main file, that tree, and your own
`local.nix` when you have one. On the box each set is one directory under
`/etc/nixos/sets/`; `/etc/nixos/current` names the set in use and
`/etc/nixos/configuration.nix` is a link to `current/configuration.nix`.
Switching sets is a single rename, so an interrupted push leaves the previous
set in place. The set is mirrored to `~/.config/devbox/` on the persistent
volume, and restored from there at boot after an instance replacement.

- **`local.nix`** is for settings that are yours and not public: a package
  override, extra services. Point `nix_local_module` at it. When it is unset,
  a push keeps the `local.nix` already on the box (or in the saved copy, after
  an instance replacement). Once a box has had one, a push that cannot find it
  anywhere stops before switching instead of building a system without it.
  It is not a place for secrets: it is readable by every user on the box and
  its values end up in the Nix store. To stop using one, push an empty
  module (`{ }`) as `local.nix`; a push never removes it on its own.
- **Older CLI binaries.** A push carries a layout version. The box refuses a
  set older than the one it runs, so an old binary cannot push a config that
  lacks modules the box depends on. Binaries from before this check push a
  single file; update them before using a box that runs sets.
- `--file` must name a main file with a `devbox/` directory beside it.

### AHP gateway service

`terraform/devbox/modules/ahp-gateway.nix` runs the
[AHP gateway](https://github.com/formulahendry/ahp-acp-gateway) as the system
service `ahp-gateway.service` (design note `docs/design/notes/030-ahp-gateway-module.md`).
It is **off** in the template and is turned on in your `local.nix`, because
which agent to run is your choice. Before turning it on for the first time, run
the boot checklist in design note 030 section 5 once: what only a real boot
shows (ordering against `/home`, a reboot, a switch with a running gateway)
cannot be tested on a development machine.

```nix
{ pkgs, ... }: {
  services.ahp-gateway = {
    enable = true;
    user = "<you>";
    # Keep the built-in id `claude`, so sessions made before keep working.
    providers.claude = { command = "<store path of the agent adapter>"; args = [ ]; };
  };
}
```

The user needs a fixed `uid` and `linger = true` (the template's user has
both). A provider that reuses a built-in id takes over that entry's program and
arguments; any environment the built-in sets stays unless you set it again.

What it does: listens on `127.0.0.1` only, always with a token
(`<state dir>/token`, made on first start), with browser origins refused unless
listed in `allowedOrigins`. Agents must be programs in the Nix store; every
built-in launcher that downloads code is disabled. Before a different build
first opens the database it is backed up to `<state dir>/backups/`, and an
older build is refused. A config switch never restarts a running gateway; the
new build takes effect at the next restart or boot.

The unit counts as started only when the gateway answers, owns its port, listens
on loopback only and refuses a connection without a token. If it fails, if it
never starts (for example because `/home` did not mount), or if it keeps
restarting, a line goes to `/var/log/boot-history`, and to a webhook if
`services.ahp-gateway.failureNotice.webhookFile` names a file with a
`SLACK_WEBHOOK_URL=` line. That file is read, never run.

### Machine-specific values

This repository is public, so `configuration.nix` is a template. Anything that
identifies one person's machines or AWS account is a `@@MARKER@@` that the CLI
fills in when it pushes the config (`nix-update`, `spawn`, `resize`). The
rendered file exists only on the box.

| Marker | Filled from | If not set |
|--------|-------------|------------|
| `@@DNS_ZONE_ID@@` | `dns_zone_id`, or looked up from `dns_zone` | The command stops with an error before it changes anything if the lookup fails. With `dns_zone` empty too, the boot-time DNS update is skipped |
| `@@DNS_RECORD_NAME@@` | `dns_name` | The boot-time DNS update is skipped |
| `@@TAILSCALE_AUTH_KEY@@` | `tailscale_auth_key` | Tailscale auto-connect is skipped |
| `@@DATA_VOLUME_ID@@` | the attached data volume | The format-if-blank service is skipped |
| `@@EXTRA_AUTHORIZED_KEYS@@` | the file named by `authorized_keys_file` | No extra keys; only the EC2 key pair can log in |

Extra ssh public keys (a tablet, a second laptop) go in
`~/.config/devbox/authorized_keys` on the machine you run `devbox` from, one
`<type> <base64> [comment]` per line, separated by single spaces. Blank lines
and `#` comments are ignored. A line with options, control or non-ASCII
characters, or a quote, backslash, dollar sign or `@@` in the comment is
rejected, and the command stops before it touches any instance. Do not add keys
to `configuration.nix` itself.

A push installs exactly the keys in that file and removes any other extra key
from the box. `nix-update`, `spawn` and `resize` print how many keys they will
install, and warn when it is none. If a key used to be written directly in
`configuration.nix`, put it in that file **before** the next `devbox
nix-update`.

Docker and the Nix daemon keep data on `/home` and do not start when `/home` is
not mounted. On a box that already has images under `/var/lib/docker`, moving
the data root leaves them behind on the root disk; remove them by hand once the
new location is in use.

### Required secrets

Some home-manager units read credentials from `/home/emaland/.secrets`, which is
provisioned out of band (this repo does not create or copy it). If a key is
missing the box still boots normally — the failure is deferred to first use.

| Key | Needed by | Symptom if missing |
| --- | --- | --- |
| `FIREWORKS_API_KEY` | `llm-proxy` user service (backs the `claude-fw` / `claude-fw-flash` aliases) | Service starts and passes health checks; Fireworks requests fail auth at request time |
| `DEEPINFRA_API_KEY` | `llm-proxy` user service (backs the `claude-di` / `claude-di-flash` aliases) | Same, for DeepInfra requests |

`llm-proxy` translates Claude Code's Anthropic `/v1/messages` protocol to the
OpenAI-only APIs those providers serve, so the aliases cannot work without it.
It binds to loopback only, since it holds these provider keys and has no auth of
its own. To check a box:

```bash
systemctl --user status llm-proxy
journalctl --user -u llm-proxy -e     # warns on startup if ~/.secrets is unreadable
```

### Manual terraform (if you prefer)

If you'd rather run terraform yourself:

```bash
cd terraform
cp terraform.tfvars.example terraform.tfvars
# Edit terraform.tfvars with your dns_zone_id and ssh_public_key
terraform init
terraform plan
terraform apply
```

## Usage

```
devbox <command> [flags]
```

Use `devbox --help` for a full list of commands, or `devbox <command> --help` for flags on any command.

### Instance management

```bash
# List all spot instances
devbox list
devbox ls

# Start / stop / terminate instances (stop and terminate are refused while an
# agent is working; see "Unattended agent work")
devbox start i-abc123
devbox stop i-abc123
devbox terminate i-abc123

# Reboot in-place (same host, no IP change)
devbox reboot i-abc123

# Full restart — stop then start (may get a new host/IP)
devbox restart i-abc123

# SSH into an instance (auto-detects if only one is running)
devbox ssh
devbox ssh i-abc123
```

### Auto-stop timer

Instances auto-stop after 4 hours idle by default. The box counts as in use while someone is logged in (or has an inbound ssh connection) **or an agent on the AHP gateway is working**; see [Unattended agent work](#unattended-agent-work). You can change the window on a running instance:

```bash
# Set auto-stop to 2 hours idle
devbox stop --after 2h
devbox stop --after 2h i-abc123

# Disable auto-stop
devbox stop --after off

# Stop now, unless an agent is working
devbox stop i-abc123
```

When `--after` is used without an instance ID, devbox auto-detects the single running instance. The window counts from each boot.

### Unattended agent work

With the AHP gateway enabled you can start a long agent task, disconnect, and walk away. Design note `docs/design/notes/031-devbox-lifecycle.md` has the details.

- **Auto-stop waits for agents.** Every 10 minutes the box asks the gateway whether an agent is working. A working agent counts as activity. A turn that is **waiting for you** (a permission prompt) does not: that box stops after the idle window, and the turn asks again when it comes back.
- **The busy cap.** A box that has been continuously busy for 12 hours with nobody logged in is stopped anyway, with a notice, in case an agent is stuck. Change it on the box in `/etc/devbox/autostop-busy-cap` (`8h`, `2d`, or `off`).
- **Each decision is logged** in `/var/log/boot-history`, which you see at login. A line is written when the decision changes and on every stop.
- **Stopping is clean.** Every stop the box or the CLI makes stops the gateway through systemd first, so it can record where each turn was.

```bash
devbox agents                    # sessions, turns running, turns waiting for you
devbox status --agents           # the instance list with an AGENTS column
devbox stop                      # refused while an agent is working; shows what is running
devbox stop --when-idle          # stop once no agent is working (the box checks)
devbox stop --force              # stop anyway; the gateway is still stopped cleanly
devbox stop --hard               # EC2 force stop of a wedged box; no clean shutdown
```

`restart`, `reboot`, `resize` and `terminate` check the agents and stop the gateway the same way, and take `--force` too.

> **Changed flag.** `devbox stop --force` and `devbox restart --force` used to mean an EC2 force stop. That is now `--hard`. `--force` now means "even if an agent is working".

### What happens on a spot interruption

AWS gives a spot instance a two-minute notice before it stops it. `devbox-spot-watch` checks for the notice every 5 seconds. When one arrives it:

1. writes a line to `/var/log/boot-history`;
2. sends a notification with the session and turn counts (to the same Slack webhook as the boot notification, if one is set);
3. stops the gateway cleanly, which takes well under the two minutes.

A rebalance recommendation (an early warning that may never be followed by an interruption) is logged and notified, but the gateway keeps running. If the interruption never happens, the gateway is started again five minutes after the announced time.

The data volume survives. A persistent spot request starts the instance again when capacity returns. An auto-stopped box stays stopped until `devbox up`.

### Auto-resume after a stop

When the box comes back, the gateway continues, **once**, each turn that was interrupted by the stop. The agent is told the host restarted and to check whether its last command or edit finished. The boot notification says how many turns were continued. A turn that was waiting for your permission asks again.

```bash
devbox agents resume status     # on or off, and where the setting comes from
devbox agents resume off        # stop continuing turns; also cancels any not yet started
devbox agents resume on
```

Auto-resume is on by default on the devbox. The setting is kept on the box and survives reboots and config pushes. To make "off" the box's default instead, set `services.devbox-lifecycle.resumeOnBoot = false;` in your `local.nix`. `devbox agents resume` needs a gateway build that offers the `resume` command (listed in `services.ahp-gateway.managementCommands`). Otherwise it says the build does not offer it.

### VS Code and Zed against the devbox

The gateway listens only on the devbox's loopback. Reach it through an ssh port forward:

```bash
devbox tunnel                    # 127.0.0.1:18765 here → the gateway on the devbox
devbox tunnel --port 19000
```

`devbox tunnel` copies the gateway's token to `~/.config/devbox/gateway-token` (mode 0600; it is never printed), then prints the editor settings and holds the forward open until Ctrl-C:

- **VS Code:** add the devbox to `chat.remoteAgentHosts` with address `127.0.0.1:18765/ahp`. Put the content of the token file in `connectionToken`, and turn on `chat.remoteAgentHostsEnabled`. Open the Agents window and pick the devbox host.
  - **If you use Settings Sync**, first add `chat.remoteAgentHosts` to `settings.sync.ignoredSettings`. Otherwise the token is uploaded with your settings to every machine you sign in on.
- **Zed:** point the AHP bridge at `ws://127.0.0.1:18765/ahp`, with the token file `~/.config/devbox/gateway-token`.

**Who can reach the tunnel.**
- While the tunnel is up, every user and program on this machine can connect to `127.0.0.1:18765`. The token is the only barrier, and whoever has it can run agents on the devbox as you.
- Do not run `devbox tunnel` on a shared machine.
- Turn off your editor's auto-connect while no tunnel is up. Otherwise the editor sends the token to whatever holds that port at the time.
- If the token on the devbox changes, run `devbox tunnel` again to copy it.

The folders you open must exist on the devbox, because the agent runs there. Sessions belong to the gateway, not to the editor: close the editor or the tunnel mid-turn, and the turn keeps running.

An open tunnel is an ssh connection, so the devbox counts as in use and does not auto-stop while it is open.

### DNS

```bash
# Point dns_name from config at an instance's public IP
devbox dns i-abc123

# Point a specific DNS name at an instance instead
devbox dns i-abc123 staging.frob.io

# Install a systemd service that updates DNS on every boot
devbox setup-dns i-abc123
```

The `dns` command updates a Route 53 A record (TTL 60s) in the hosted zone specified by `dns_zone`. When called without a DNS name argument, it uses `dns_name` from your config. When called with a second argument, it uses that name instead — useful for pointing multiple records at different instances.

The `setup-dns` command SSHes into the instance and installs a oneshot systemd service that runs on every boot, queries the instance metadata for its current public IP, and updates the Route 53 record. This is a safety net so DNS stays correct after spot interruption/restart cycles without manual intervention.

### Spot management

```bash
# Show current spot request bids
devbox bids

# Show current spot market prices for your active request types
devbox prices

# Cancel a spot request and re-create it with a new max price
devbox rebid sir-abc123 0.50
```

### Search spot prices

Browse spot prices across instance types by hardware specs:

```bash
# Default search: 8+ vCPUs, 16+ GiB memory, x86_64, sorted by price
devbox search

# Look up specific instance types
devbox search m6i.4xlarge m6i.8xlarge

# Filter by specs
devbox search --min-vcpu 32 --min-mem 64 --max-price 1.00

# GPU instances only
devbox search --gpu

# ARM instances in a specific AZ
devbox search --arch arm64 --az us-east-2a

# Sort by memory, show top 50
devbox search --sort mem --limit 50
```

**Flags:**

| Flag | Default | Description |
|------|---------|-------------|
| `--min-vcpu` | 8 | Minimum vCPUs |
| `--min-mem` | 16 | Minimum memory (GiB) |
| `--max-price` | 0 | Max spot price $/hr (0 = no limit) |
| `--arch` | x86_64 | CPU architecture (`x86_64` or `arm64`) |
| `--gpu` | false | Require GPU |
| `--az` | (all) | Filter by availability zone |
| `--sort` | price | Sort by: `price`, `vcpu`, `mem` |
| `--limit` | 20 | Max rows to display |

### Resize an instance

Change an instance's type without leaving the terminal. devbox stops the instance, changes the type, restarts it, and updates DNS:

```bash
devbox resize i-abc123 m6i.8xlarge
```

For on-demand instances, this does a simple stop → modify type → start. For spot instances (which don't support in-place type changes), it launches a new instance with the new type first, confirms it's running, then stops it, moves non-root EBS volumes from the old instance, terminates the old instance, and starts the new one with volumes attached. The new instance is only created after confirming spot capacity — if the launch fails, the old instance and its volumes remain untouched.

### Recover a stuck instance

When a spot instance can't start due to `InsufficientInstanceCapacity`, the `recover` command finds alternative instance types with available spot capacity in the same AZ (since EBS volumes are AZ-locked):

```bash
# Show alternative instance types with spot capacity
devbox recover i-abc123

# Auto-pick the cheapest alternative and resize
devbox recover --yes i-abc123

# Override minimum specs
devbox recover --min-vcpu 16 --min-mem 64 i-abc123

# Set a price cap
devbox recover --max-price 0.50 i-abc123
```

The command describes the instance, determines its specs and architecture, searches for compatible types (>=50% of current vCPUs and memory, same architecture), fetches spot prices filtered to the instance's AZ, and displays candidates sorted by price. With `--yes`, it automatically resizes to the cheapest option.

**Flags:**

| Flag | Default | Description |
|------|---------|-------------|
| `--min-vcpu` | 50% of current | Minimum vCPUs |
| `--min-mem` | 50% of current | Minimum memory (GiB) |
| `--max-price` | from config | Max spot price $/hr (0 = no limit) |
| `--yes` | false | Auto-pick cheapest candidate and resize |

### Spawn a clone

Spin up a new spot instance with the same NixOS config as your primary box. The new instance gets its own root volume but does NOT attach the primary's data EBS volume:

```bash
# Use defaults from config
devbox spawn

# Override instance type and AZ
devbox spawn --type m6i.8xlarge --az us-east-2b

# Clone user_data from a specific instance
devbox spawn --from i-abc123

# Custom name and price cap
devbox spawn --name my-test-box --max-price 0.50
```

**Flags:**

| Flag | Default | Description |
|------|---------|-------------|
| `--type` | from config | Instance type |
| `--az` | from config | Availability zone |
| `--name` | from config | Name tag |
| `--max-price` | from config | Spot max price $/hr |
| `--from` | auto-detected | Instance ID to clone user_data from |

When `--from` is omitted, devbox auto-detects the source: if exactly one running/stopped spot instance exists, it uses that. If there are multiple, it asks you to specify.

### Volume management

Manage EBS volumes — list, create, attach/detach, snapshot, and move across regions:

```bash
# List all EBS volumes
devbox volume ls

# Create a new volume
devbox volume create
devbox volume create --size 1024 --type gp3 --iops 6000 --az us-east-2b --name my-data

# Attach / detach
devbox volume attach vol-abc123 i-def456
devbox volume attach --device /dev/xvdg vol-abc123 i-def456
devbox volume detach vol-abc123
devbox volume detach --force vol-abc123

# Snapshots
devbox volume snapshot vol-abc123
devbox volume snapshot --name "before-upgrade" vol-abc123
devbox volume snapshots

# Delete a volume (must be detached)
devbox volume destroy vol-abc123

# Move a volume to another region (snapshot → copy → create)
devbox volume move vol-abc123 us-west-2
devbox volume move --az us-west-2b --cleanup vol-abc123 us-west-2
```

Volumes can be specified by ID (`vol-xxx`) or by Name tag.

**`volume create` flags:**

| Flag | Default | Description |
|------|---------|-------------|
| `--size` | 512 | Volume size in GiB |
| `--type` | gp3 | Volume type |
| `--iops` | 3000 | IOPS |
| `--throughput` | 250 | Throughput MB/s |
| `--az` | from config | Availability zone |
| `--name` | dev-data-volume | Name tag |

**`volume move` flags:**

| Flag | Default | Description |
|------|---------|-------------|
| `--az` | `<region>a` | Target AZ |
| `--cleanup` | false | Delete intermediate snapshots after move |

## How it works

devbox talks directly to the AWS API using the Go SDK v2. There's no local state — it discovers everything from AWS on each run:

- **Instance management** uses the EC2 `DescribeInstances`, `StartInstances`, `StopInstances`, `RebootInstances`, and `TerminateInstances` APIs. `restart` chains stop + wait + start for a full host migration.
- **DNS** uses Route 53 `ChangeResourceRecordSets` to upsert an A record.
- **Search** paginates `DescribeInstanceTypes` (filtered to spot-capable, current-gen) then fetches `DescribeSpotPriceHistory` and joins the results.
- **Spawn** discovers the AMI, security group, and subnet from AWS, fetches `user_data` from the source instance, and calls `RunInstances` with persistent spot + stop-on-interruption.
- **Config delivery is two-phase.** The full `configuration.nix` exceeds EC2's 16 KB user-data limit, so a new box boots a tiny SSH-only stub via user-data; the CLI then renders the real config (substituting `@@MARKER@@` placeholders for the DNS zone, data-volume id, etc.) and pushes it over SSH with `nixos-rebuild switch`. The rendered config is saved to `/home` so it survives instance replacement, and `amazon-init` (which would otherwise re-apply the stub user-data on every boot and revert the system) is detached from `multi-user.target` once the real config is active — so a reboot keeps the configured system instead of reverting to the stub.
- **Resize** for on-demand instances uses `ModifyInstanceAttribute` between a stop/start cycle. For spot instances, it launches a replacement instance with the new type, confirms capacity, then swaps non-root EBS volumes and terminates the old instance.
- **Recover** combines `DescribeInstanceTypes` (for current specs/architecture), `fetchInstanceTypes` (for candidates), and `DescribeSpotPriceHistory` (filtered to the instance's AZ) to find alternatives with capacity, then optionally calls resize.
- **Volume** commands wrap the EC2 volume and snapshot APIs. `volume move` chains `CreateSnapshot` → `CopySnapshot` (cross-region) → `CreateVolume` to relocate a volume while preserving its type, IOPS, throughput, and tags.

## License

Do whatever you want with it.
