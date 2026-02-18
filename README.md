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

Requires Go 1.21+. The binary is fully static with no runtime dependencies beyond AWS credentials.

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

### AWS prerequisites

devbox expects these resources to already exist in your AWS account:

- An EC2 key pair matching `ssh_key_name`
- A security group matching `security_group`
- An IAM instance profile matching `iam_profile`
- A Route 53 hosted zone matching `dns_zone`
- Your AWS credentials must have permissions for EC2, Route 53, and IAM pass-role

The included Terraform configuration (`terraform/`) can create all of these for you — see [Terraform setup](#terraform-setup) below.

## Terraform setup

The `terraform/` directory contains a Terraform configuration that provisions the AWS infrastructure devbox depends on. It reads from the same `~/.config/devbox/default.json` config file as the CLI, so resource names (key pairs, security groups, IAM profiles) stay in sync automatically.

### What it creates

| Resource | Description |
|----------|-------------|
| **EC2 key pair** | SSH key pair (name from `ssh_key_name` config) |
| **Security group** | Allows inbound SSH (22/tcp) and Tailscale (41641/udp), all outbound. Attached to the default VPC |
| **IAM role + instance profile** | Grants instances permission to update Route 53 records so DNS stays correct after spot interruptions |
| **EBS volume** | 512 GiB gp3 persistent data volume (3000 IOPS, 250 MB/s). Has `prevent_destroy` enabled so it can't be accidentally deleted |

It also includes a `configuration.nix` that defines the NixOS system configuration for launched instances: SSH with pubkey auth, Tailscale VPN, Docker, a boot-time DNS updater service, and a dev toolchain (git, tmux, emacs, python3, awscli, home-manager, etc.).

### Variables

You need to provide two variables. Copy the example file and fill in your values:

```bash
cd terraform
cp terraform.tfvars.example terraform.tfvars
```

| Variable | Description |
|----------|-------------|
| `dns_zone_id` | Your Route 53 hosted zone ID (e.g. `ZXXXXXXXXXXXXXXXXXX`) |
| `ssh_public_key` | Your SSH public key (e.g. `ssh-ed25519 AAAA... you@host`) |

Everything else is pulled from `~/.config/devbox/default.json`.

### Usage

```bash
cd terraform
terraform init
terraform plan
terraform apply
```

After `apply` completes, the CLI's prerequisites are in place and you can start using `devbox spawn`, `devbox list`, etc.

## Usage

```
devbox <command> [args]
```

### Instance management

```bash
# List all spot instances
devbox list
devbox ls

# Start / stop / terminate instances
devbox start i-abc123
devbox stop i-abc123
devbox terminate i-abc123

# Reboot in-place (same host, no IP change)
devbox reboot i-abc123

# Full restart — stop then start (may get a new host/IP)
devbox restart i-abc123

# SSH into an instance
devbox ssh i-abc123
```

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
- **Resize** for on-demand instances uses `ModifyInstanceAttribute` between a stop/start cycle. For spot instances, it launches a replacement instance with the new type, confirms capacity, then swaps non-root EBS volumes and terminates the old instance.
- **Recover** combines `DescribeInstanceTypes` (for current specs/architecture), `fetchInstanceTypes` (for candidates), and `DescribeSpotPriceHistory` (filtered to the instance's AZ) to find alternatives with capacity, then optionally calls resize.
- **Volume** commands wrap the EC2 volume and snapshot APIs. `volume move` chains `CreateSnapshot` → `CopySnapshot` (cross-region) → `CreateVolume` to relocate a volume while preserving its type, IOPS, throughput, and tags.

## License

Do whatever you want with it.
