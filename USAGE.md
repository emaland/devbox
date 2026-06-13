# devbox usage

A single binary for managing a persistent NixOS dev workstation running as an
EC2 spot instance.

## Mental model

There is **one devbox** (a persistent spot instance) and **one data volume**
(mounted at `/home`, survives instance replacement). Most commands auto-target
"the box," so you rarely pass an instance ID. The box boots **fully configured
in one phase** — the real `configuration.nix` is rendered into EC2 user-data, so
no post-boot SSH push is needed.

## Daily lifecycle — the front door

```bash
devbox up            # bring the box up (idempotent)
devbox status        # state, public IP, attached volume, live spot price
devbox down          # stop it (data volume preserved)
devbox ssh           # shell in (auto-detects the box)
```

`up` is idempotent — run it whenever you want the box ready:

| Situation              | What `up` does                                             |
| ---------------------- | ---------------------------------------------------------- |
| No box yet             | ensures the data volume exists → launches → attaches it    |
| Box stopped            | starts it + refreshes DNS                                  |
| Box already running    | prints status                                              |
| `up i-…` (explicit ID) | starts exactly those instances                             |

`down` stops the box (volume kept; bring it back with `up`). It can also
schedule an auto-stop timer over SSH:

```bash
devbox down --after 4h     # stop automatically in 4h
devbox down --after off    # disable the auto-stop timer
```

### Aliases (old names still work)

`start` → `up` · `stop` → `down` · `list` / `ls` → `status`. The canonical
names are `up` / `down` / `status`.

## Moving the box across instance types (carries the volume)

```bash
devbox resize m6i.2xlarge          # auto-targets the box
devbox resize i-0abc… m6i.2xlarge  # explicit instance
devbox resize --resume             # continue an interrupted resize
```

For spot instances this is a **resumable state machine**: launch the
replacement → confirm capacity → stop → swap the data volume over → terminate
the old one → start → update DNS. Progress is recorded via `devbox-resize-*`
tags on the new instance; if it's interrupted mid-swap, `--resume` picks up from
the last completed step (no orphaned volumes). The old root volume's size/type
is carried over.

```bash
devbox recover     # spot interrupted? find alternative types with capacity in the same AZ
```

## State

```bash
devbox restart     # stop + start (new host, same volume)
devbox reboot      # in-place reboot (same host/IP)
devbox terminate   # terminate + cancel the spot request
```

## Volumes

```bash
devbox volume ls                       # lists volumes + which instance each is attached to
devbox volume create --size 512        # name defaults to dev-data-volume
devbox volume attach <vol> <instance>
devbox volume detach <vol>
devbox volume snapshot <vol>
devbox volume snapshots
devbox volume destroy <vol>
devbox volume move <vol> <region>      # snapshot → cross-region copy → recreate
```

Volumes are referenced by ID (`vol-…`) or by `Name` tag.

## Config

```bash
devbox config                          # show every setting + its source (file vs default)
devbox config set default_type m6i.2xlarge
devbox config path
```

Config lives at `~/.config/devbox/default.json`. Keys: `dns_name`, `dns_zone`,
`dns_zone_id`, `ssh_key_name`, `ssh_key_path`, `ssh_user`, `security_group`,
`iam_profile`, `default_az`, `default_type`, `default_max_price`, `spawn_name`,
`nixos_ami_owner`, `nixos_ami_pattern`, `tailscale_auth_key`, `data_volume_name`,
`data_volume_size`. Any unset key uses a built-in default.

## Setup / plumbing

```bash
devbox infra        # one-time: terraform (key pair, security group, IAM, EBS volume)
devbox nix-update   # push/apply configuration.nix edits to the box (renders @@MARKER@@s first)
devbox spawn        # launch a clone (--from / --volume / --type / --az / --name)
devbox dns          # point the Route53 A record at the box
devbox setup-dns    # install a boot-time DNS updater on the box
devbox search       # browse spot prices by vCPU / memory / arch / GPU
devbox prices       # current spot prices for active request types
devbox bids         # active spot request max prices
devbox rebid        # cancel + recreate a spot request at a new max price
```

## Zero → running

```bash
devbox infra      # once, ever — creates SG / IAM / key pair / data volume
devbox up         # creates+attaches the volume, launches the box, boots configured
devbox ssh        # you're in

# later:
devbox down                 # stop (keep the volume)
devbox up                   # back, same /home
devbox resize m6i.4xlarge   # grow/shrink — the volume comes along
```

## How the data volume is initialized (safety note)

On first boot of a *fresh* (unformatted) data volume, a NixOS service formats it
as ext4 labelled `home-data`. It only ever touches the disk whose **EBS
volume-id matches** the one the CLI designated (matched by NVMe serial), and
only when that disk is blank — so it can never format an ephemeral/instance-store
NVMe or any other volume. The volume-id is templated into the config by the CLI
when it attaches the volume.
