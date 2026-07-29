{ config, pkgs, lib, modulesPath, ... }:

{
  imports = [ "${modulesPath}/virtualisation/amazon-image.nix" ];

  networking.hostName = "dev-workstation";
  networking.dhcpcd.extraConfig = "nohook hostname";

  # ── Stop amazon-init from clobbering us on every reboot ────────────
  # The amazon-image module's amazon-init.service unconditionally copies
  # the EC2 user_data over /etc/nixos/configuration.nix and runs
  # `nixos-rebuild switch` on EVERY boot — there is no "already applied"
  # guard. Because devbox uses a tiny SSH-only *stub* as user_data (the
  # full config is >16 KB, the user_data limit, so it's pushed over SSH
  # instead), letting amazon-init run on a reboot reverts the system to
  # that stub: it overwrites this file, drops the emaland user, and
  # unmounts /home. amazon-init is only needed for the first-boot SSH
  # bootstrap; once this hardened config is active we never want it to
  # run again. Detaching it from multi-user.target leaves the unit
  # present (runnable by hand) but stops it from starting at boot.
  systemd.services.amazon-init.wantedBy = lib.mkForce [ ];

  # ── Pin the transient hostname ────────────────────────────────────
  # networking.hostName sets the *static* hostname (/etc/hostname), but on
  # a fresh instance amazon-init's first-boot stub rebuild (and/or DHCP)
  # sets the *transient* hostname to the EC2 default (ip-172-31-x-x), which
  # overrides the static one for `hostname`/uname until something resets
  # it — and nothing did, so the box kept showing ip-172-31-…. This pins
  # the transient hostname back to the static one on every boot. (plain
  # `hostname` works on NixOS; `hostnamectl set-hostname` is blocked here.)
  systemd.services.devbox-fix-hostname = {
    description = "Pin transient hostname to ${config.networking.hostName}";
    after       = [ "network-online.target" ];
    wants       = [ "network-online.target" ];
    wantedBy    = [ "multi-user.target" ];
    serviceConfig = {
      Type            = "oneshot";
      RemainAfterExit = true;
      ExecStart       = "${pkgs.nettools}/bin/hostname ${config.networking.hostName}";
    };
  };

  # Ensure IMDS is always reachable via the primary interface.
  # Docker/Tailscale veth interfaces can steal the 169.254.0.0/16
  # link-local route from the main table. A policy routing rule with
  # high priority (low number) forces IMDS traffic to a dedicated
  # routing table that only has the ens5 route — immune to other
  # interfaces adding/removing routes in the main table.
  #
  # This must run AFTER the primary interface is up. The old approach
  # (networking.localCommands) ran inside network-setup before ens5 had
  # carrier, so `ip route replace ... dev ens5` failed with "Device for
  # nexthop is not up", left table 100 EMPTY, and the boot was degraded.
  # With an empty table 100 the rule falls through to the main table,
  # where Docker's veth 169.254.0.0/16 route wins — silently breaking
  # IMDS (and update-route53, auto-stop, etc.) once containers start.
  # A oneshot ordered after network-online.target, waiting for the link
  # and tolerating a transient failure, fixes this. Once table 100 holds
  # the IMDS route, the rule keeps IMDS pinned to ens5 no matter what
  # Docker/Tailscale later do to the main table.
  systemd.services.devbox-imds-route = {
    description = "Pin IMDS (169.254.169.254) to the primary interface";
    after       = [ "network-online.target" ];
    wants       = [ "network-online.target" ];
    wantedBy    = [ "multi-user.target" ];
    serviceConfig = {
      Type            = "oneshot";
      RemainAfterExit = true;
      ExecStart = toString (pkgs.writeShellScript "devbox-imds-route" ''
        set -u
        IF=ens5
        # Wait for the primary interface to come up (carrier present).
        for _ in $(seq 1 30); do
          if ${pkgs.iproute2}/bin/ip link show "$IF" up 2>/dev/null | ${pkgs.gnugrep}/bin/grep -q "state UP"; then
            break
          fi
          sleep 1
        done
        ${pkgs.iproute2}/bin/ip rule add to 169.254.169.254 lookup 100 priority 100 2>/dev/null || true
        # Retry the route a few times in case carrier is still settling.
        for _ in $(seq 1 10); do
          if ${pkgs.iproute2}/bin/ip route replace 169.254.169.254 dev "$IF" table 100; then
            exit 0
          fi
          sleep 1
        done
        echo "warning: could not pin IMDS route to $IF" >&2
        exit 0
      '');
    };
  };

  # ── Initialize the data volume on first use (SAFE, exact match) ────
  # A freshly-created EBS volume has no filesystem, so /home would not
  # persist. This service formats ONLY the disk whose EBS volume-id
  # matches @@DATA_VOLUME_ID@@ (rendered into this config by the devbox
  # CLI when it attaches the data volume), and only when that disk is
  # blank.
  #
  # Safety contract — it can never touch any other disk:
  #   * It identifies the target by NVMe serial == the exact volume id,
  #     so an ephemeral/instance-store NVMe or any other EBS volume is
  #     never a candidate (no "blank disk" guessing).
  #   * If a home-data filesystem already exists, it no-ops.
  #   * If the matched disk has ANY filesystem/partition signature, it
  #     refuses and leaves it untouched.
  #   * If the marker is unrendered or no volume id is set, it no-ops.
  systemd.services.devbox-format-home = {
    description = "Initialize the home-data EBS volume if blank (exact volume-id match)";
    before      = [ "home.mount" ];
    requiredBy  = [ "home.mount" ];
    unitConfig.DefaultDependencies = false;
    serviceConfig = {
      Type            = "oneshot";
      RemainAfterExit = true;
      ExecStart = toString (pkgs.writeShellScript "devbox-format-home" ''
        set -u

        VOLID="@@DATA_VOLUME_ID@@"
        case "$VOLID" in
          ""|@@*@@) echo "no data volume id configured; skipping format"; exit 0 ;;
        esac
        # EBS exposes the volume id (minus the dash) as the NVMe serial.
        WANT="''${VOLID//-/}"

        # Already formatted → done (normal case for an existing volume).
        if ${pkgs.util-linux}/bin/blkid -L home-data >/dev/null 2>&1; then
          echo "home-data filesystem already exists; nothing to do"
          exit 0
        fi

        # Find the whole disk whose NVMe serial equals our volume id.
        target=""
        for dev in $(${pkgs.util-linux}/bin/lsblk -dn -o NAME,TYPE | ${pkgs.gawk}/bin/awk '$2=="disk"{print $1}'); do
          serial=$(${pkgs.util-linux}/bin/lsblk -dn -o SERIAL "/dev/$dev" 2>/dev/null | ${pkgs.coreutils}/bin/tr -d '[:space:]')
          if [ "$serial" = "$WANT" ]; then target="$dev"; break; fi
        done

        if [ -z "$target" ]; then
          echo "data volume $VOLID not found among attached disks (serial match); skipping"
          exit 0
        fi

        # Never reformat a disk that already holds data.
        if ${pkgs.util-linux}/bin/blkid "/dev/$target" >/dev/null 2>&1; then
          echo "/dev/$target ($VOLID) already has a filesystem signature; leaving untouched"
          exit 0
        fi
        children=$(${pkgs.util-linux}/bin/lsblk -n -o NAME "/dev/$target" | ${pkgs.coreutils}/bin/wc -l)
        if [ "$children" -gt 1 ]; then
          echo "/dev/$target ($VOLID) has partitions; leaving untouched"
          exit 0
        fi

        echo "Formatting blank data volume $VOLID (/dev/$target) as ext4 (label home-data)"
        ${pkgs.e2fsprogs}/bin/mkfs.ext4 -L home-data "/dev/$target"
      '');
    };
  };

  # ── Filesystem ────────────────────────────────────────────────────
  # Mount the persistent EBS volume by label so it works across
  # instance types (device paths vary on Nitro).
  fileSystems."/home" = {
    device  = "/dev/disk/by-label/home-data";
    fsType  = "ext4";
    options = [ "defaults" "nofail" ];
  };

  # ── Users ─────────────────────────────────────────────────────────
  users.users.emaland = {
    isNormalUser = true;
    uid          = 1001;
    extraGroups  = [ "wheel" "docker" ];
    # home-manager installs systemd --user units (e.g. llm-proxy, which bridges
    # Claude Code to OpenAI-only model providers). Without lingering those only
    # run while an interactive login session is open, so they never start at
    # boot and die on logout.
    linger       = true;
    # Extra authorized keys, in addition to the EC2 key-pair key that
    # devbox-fetch-ssh-key installs into ~/.ssh/authorized_keys each boot.
    # These are written to /etc/ssh/authorized_keys.d/emaland — a separate
    # file sshd also checks — so the fetch service never clobbers them.
    openssh.authorizedKeys.keys = [
      "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAz5otg2dmDpJbCO75OlSZZPrP1vIRCv1sVNWeOovsMR emaland@ipad"
    ];
  };

  security.sudo.wheelNeedsPassword = false;

  # ── Run generic dynamically-linked binaries (nix-ld) ──────────────
  # Prebuilt Linux binaries installed outside Nix — notably Claude Code's
  # native self-updating binary in ~/.local/share/claude — expect a
  # loader at /lib64/ld-linux-x86-64.so.2. NixOS ships only a stub there
  # that prints "cannot run dynamically linked executables". nix-ld
  # replaces it with a real loader plus a library search path, so those
  # binaries just work.
  programs.nix-ld.enable = true;
  programs.nix-ld.libraries = with pkgs; [
    stdenv.cc.cc.lib
    zlib
    openssl
  ];

  # ── Sync configuration.nix from the persistent /home volume ────
  # On instance replacement (resize, spawn) the root volume is new but
  # /home survives, so this copies the saved config back to /etc/nixos
  # for the next rebuild.
  #
  # It deliberately does NOT run a nested `nixos-rebuild switch`: doing
  # so collided with boot- and switch-time activation (the nested switch
  # raced the real one and failed, leaving the box half-configured — no
  # /home mount, missing users — and `nixos-rebuild` isn't even on a
  # service's PATH, so it exited 127). Activation is handled by the
  # devbox config push (spawn/resize) or a manual `nixos-rebuild`. This
  # service only syncs the file and can never fail the boot.
  systemd.services.devbox-restore-nixos-config = {
    description = "Sync configuration.nix from the persistent volume";
    after       = [ "home.mount" ];
    before      = [ "devbox-fetch-ssh-key.service" "update-route53.service" "devbox-claude.service" ];
    wantedBy    = [ "multi-user.target" ];
    serviceConfig = {
      Type            = "oneshot";
      RemainAfterExit = true;
      ExecStart = toString (pkgs.writeShellScript "devbox-restore-nixos-config" ''
        SAVED="/home/emaland/.config/devbox/configuration.nix"
        TARGET="/etc/nixos/configuration.nix"
        if [ -r "$SAVED" ] && ! ${pkgs.diffutils}/bin/cmp -s "$SAVED" "$TARGET" 2>/dev/null; then
          echo "Syncing $TARGET from the persistent volume"
          ${pkgs.coreutils}/bin/cp "$SAVED" "$TARGET" || true
        else
          echo "configuration.nix already in sync (or no readable saved copy)"
        fi
      '');
    };
  };

  # ── Fetch EC2 SSH key for emaland ──────────────────────────────
  # Pulls the EC2 key pair's public key from instance metadata on
  # every boot and installs it as emaland's authorized_keys.
  systemd.services.devbox-fetch-ssh-key = {
    description = "Fetch EC2 SSH key for emaland user";
    after       = [ "home.mount" "network-online.target" ];
    wants       = [ "network-online.target" ];
    wantedBy    = [ "multi-user.target" ];
    serviceConfig = {
      Type      = "oneshot";
      ExecStart = toString (pkgs.writeShellScript "devbox-fetch-ssh-key" ''
        TOKEN=$(${pkgs.curl}/bin/curl -sX PUT \
          "http://169.254.169.254/latest/api/token" \
          -H "X-aws-ec2-metadata-token-ttl-seconds: 60")
        PUBKEY=$(${pkgs.curl}/bin/curl -s \
          -H "X-aws-ec2-metadata-token: $TOKEN" \
          http://169.254.169.254/latest/meta-data/public-keys/0/openssh-key)

        if [ -z "$PUBKEY" ]; then
          echo "No EC2 public key found in metadata, skipping"
          exit 0
        fi

        SSH_DIR=/home/emaland/.ssh
        mkdir -p "$SSH_DIR"
        echo "$PUBKEY" > "$SSH_DIR/authorized_keys"
        chmod 700 "$SSH_DIR"
        chmod 600 "$SSH_DIR/authorized_keys"
        chown -R emaland:users "$SSH_DIR"
        echo "Installed EC2 public key for emaland"
      '');
    };
  };

  # ── SSH ───────────────────────────────────────────────────────────
  services.openssh = {
    enable = true;
    settings = {
      PermitRootLogin         = "prohibit-password";
      PasswordAuthentication  = false;
    };
  };

  # ── Tailscale ─────────────────────────────────────────────────────
  services.tailscale.enable = true;

  systemd.services.tailscale-autoconnect = {
    description = "Automatic Tailscale login";
    after       = [ "network-online.target" "tailscaled.service" ];
    wants       = [ "network-online.target" "tailscaled.service" ];
    wantedBy    = [ "multi-user.target" ];
    serviceConfig = {
      Type      = "oneshot";
      ExecStart = toString (pkgs.writeShellScript "tailscale-up" ''
        KEY="@@TAILSCALE_AUTH_KEY@@"
        # No auth key configured (unset, or marker left unrendered) →
        # skip rather than calling `tailscale up --auth-key=` with a
        # bogus value, which fails auth on every boot.
        case "$KEY" in
          ""|@@*@@) echo "no tailscale auth key configured; skipping tailscale up"; exit 0 ;;
        esac
        # Wait for tailscaled to be ready
        sleep 2
        status=$(${pkgs.tailscale}/bin/tailscale status --json 2>/dev/null | ${pkgs.jq}/bin/jq -r '.BackendState // empty')
        if [ "$status" = "Running" ]; then
          echo "Tailscale already running"
          exit 0
        fi
        ${pkgs.tailscale}/bin/tailscale up \
          --auth-key="$KEY" \
          --ssh \
          --hostname=dev-workstation
      '');
    };
  };

  # ── Route53 DNS updater ───────────────────────────────────────────
  # Updates the DNS A record on every boot so it stays correct after
  # spot interruption/restart cycles. The hosted zone ID and record
  # name are baked in via the IAM policy — the instance role only has
  # permission to update the specific zone.
  systemd.services.update-route53 = {
    description = "Update Route53 A record on boot";
    after       = [ "network-online.target" ];
    wants       = [ "network-online.target" ];
    wantedBy    = [ "multi-user.target" ];
    serviceConfig = {
      Type      = "oneshot";
      # Stay active after running so nixos-rebuild switch doesn't re-run this on
      # every config deploy. Re-running mid-switch races the IMDS policy route
      # (which switch-to-configuration is also restarting), so it burns its full
      # ~4min IMDS-credential retry and stalls the whole switch. The public IP
      # only changes on a real boot, where this runs fresh anyway.
      RemainAfterExit = true;
      ExecStart = toString (pkgs.writeShellScript "update-route53" ''
        # Wait for IMDS credentials to become available (IAM role
        # propagation can lag behind network-online.target).
        for i in $(seq 1 12); do
          TOKEN=$(${pkgs.curl}/bin/curl -sX PUT \
            "http://169.254.169.254/latest/api/token" \
            -H "X-aws-ec2-metadata-token-ttl-seconds: 60")
          if ${pkgs.curl}/bin/curl -sf \
            -H "X-aws-ec2-metadata-token: $TOKEN" \
            http://169.254.169.254/latest/meta-data/iam/security-credentials/ >/dev/null 2>&1; then
            break
          fi
          echo "Waiting for IMDS credentials (attempt $i/12)..."
          sleep 5
        done

        TOKEN=$(${pkgs.curl}/bin/curl -sX PUT \
          "http://169.254.169.254/latest/api/token" \
          -H "X-aws-ec2-metadata-token-ttl-seconds: 60")
        PUBLIC_IP=$(${pkgs.curl}/bin/curl -s \
          -H "X-aws-ec2-metadata-token: $TOKEN" \
          http://169.254.169.254/latest/meta-data/public-ipv4)

        ZONE_ID="@@DNS_ZONE_ID@@"
        RECORD_NAME="@@DNS_RECORD_NAME@@"

        ${pkgs.awscli2}/bin/aws route53 change-resource-record-sets \
          --hosted-zone-id "$ZONE_ID" \
          --change-batch "{
            \"Changes\": [{
              \"Action\": \"UPSERT\",
              \"ResourceRecordSet\": {
                \"Name\": \"$RECORD_NAME\",
                \"Type\": \"A\",
                \"TTL\": 60,
                \"ResourceRecords\": [{\"Value\": \"$PUBLIC_IP\"}]
              }
            }]
          }"
      '');
    };
  };

  # ── Boot history logger ──────────────────────────────────────────
  # Appends a line to /var/log/boot-history on every boot with
  # instance metadata and current auto-stop setting.
  systemd.services.devbox-boot-log = {
    description = "Log boot event to /var/log/boot-history";
    after       = [ "network-online.target" ];
    wants       = [ "network-online.target" ];
    wantedBy    = [ "multi-user.target" ];
    serviceConfig = {
      Type      = "oneshot";
      # Stay active after running so nixos-rebuild switch doesn't re-log a
      # spurious "boot" line to /var/log/boot-history on every config deploy.
      RemainAfterExit = true;
      ExecStart = toString (pkgs.writeShellScript "devbox-boot-log" ''
        TOKEN=$(${pkgs.curl}/bin/curl -sX PUT \
          "http://169.254.169.254/latest/api/token" \
          -H "X-aws-ec2-metadata-token-ttl-seconds: 60")
        ITYPE=$(${pkgs.curl}/bin/curl -s \
          -H "X-aws-ec2-metadata-token: $TOKEN" \
          http://169.254.169.254/latest/meta-data/instance-type)
        AZ=$(${pkgs.curl}/bin/curl -s \
          -H "X-aws-ec2-metadata-token: $TOKEN" \
          http://169.254.169.254/latest/meta-data/placement/availability-zone)
        PIP=$(${pkgs.curl}/bin/curl -s \
          -H "X-aws-ec2-metadata-token: $TOKEN" \
          http://169.254.169.254/latest/meta-data/public-ipv4 || echo "n/a")
        AUTOSTOP="8h"
        if [ -f /etc/devbox/autostop-after ]; then
          AUTOSTOP=$(cat /etc/devbox/autostop-after)
        fi
        echo "$(date '+%Y-%m-%d %H:%M:%S') | boot | ''${ITYPE:-unknown} | ''${AZ:-unknown} | ''${PIP:-n/a} | auto-stop: $AUTOSTOP" \
          >> /var/log/boot-history
      '');
    };
  };

  # ── Slack boot notification ─────────────────────────────────────
  # On every boot, posts the instance type / AZ / IP and current spot
  # price to Slack. The webhook URL is read from /home/emaland/.secrets
  # (export SLACK_WEBHOOK_URL=...) so it never lives in this repo; if it
  # is unset the service logs and no-ops. Needs ec2:DescribeSpotPriceHistory
  # on the instance role (granted by the ec2-selfmanage policy).
  systemd.services.devbox-slack-notify = {
    description = "Notify Slack that the devbox booted (with cost)";
    after       = [ "network-online.target" "update-route53.service" ];
    wants       = [ "network-online.target" ];
    wantedBy    = [ "multi-user.target" ];
    serviceConfig = {
      Type      = "oneshot";
      # Stay "active (exited)" after running so nixos-rebuild switch does not
      # re-trigger this on every config deploy — only a real reboot (fresh unit
      # state) fires it again. Without this, each `devbox nix-update` produced a
      # spurious "devbox booted" Slack alert.
      RemainAfterExit = true;
      ExecStart = toString (pkgs.writeShellScript "devbox-slack-notify" ''
        SECRETS=/home/emaland/.secrets
        [ -f "$SECRETS" ] && . "$SECRETS"
        if [ -z "''${SLACK_WEBHOOK_URL:-}" ]; then
          echo "SLACK_WEBHOOK_URL not set in $SECRETS; skipping Slack notify"
          exit 0
        fi

        TOKEN=$(${pkgs.curl}/bin/curl -sX PUT \
          "http://169.254.169.254/latest/api/token" \
          -H "X-aws-ec2-metadata-token-ttl-seconds: 60")
        imds() { ${pkgs.curl}/bin/curl -s -H "X-aws-ec2-metadata-token: $TOKEN" \
          "http://169.254.169.254/latest/meta-data/$1"; }
        ITYPE=$(imds instance-type)
        AZ=$(imds placement/availability-zone)
        REGION=''${AZ%?}

        # NB: no --max-items — it enables the CLI paginator, which prints the
        # NextToken ("None") as a second output line and corrupts $PRICE.
        # --start-time now bounds the result to the current price point.
        PRICE=$(${pkgs.awscli2}/bin/aws ec2 describe-spot-price-history \
          --region "$REGION" --instance-types "$ITYPE" \
          --availability-zone "$AZ" --product-descriptions "Linux/UNIX" \
          --start-time "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
          --query 'SpotPriceHistory[0].SpotPrice' \
          --output text 2>/dev/null | head -n1)
        [ "$PRICE" = "None" ] && PRICE=""

        if [ -n "$PRICE" ]; then
          DAILY=$(${pkgs.gawk}/bin/awk "BEGIN{printf \"%.2f\", $PRICE*24}")
          COST=":  \$$PRICE/hr (~\$$DAILY/day at 24h)"
        else
          COST=" (spot price unavailable)"
        fi

        MSG=":computer: *devbox booted* — \`$(${pkgs.nettools}/bin/hostname)\`
• type: \`$ITYPE\`  ($AZ)
• spot$COST"

        PAYLOAD=$(${pkgs.jq}/bin/jq -n --arg t "$MSG" '{text:$t}')
        ${pkgs.curl}/bin/curl -s -X POST -H 'Content-type: application/json' \
          --data "$PAYLOAD" "$SLACK_WEBHOOK_URL" >/dev/null \
          && echo "posted boot notification to Slack" \
          || echo "warning: Slack POST failed" >&2
      '');
    };
  };

  # ── Boot history MOTD ───────────────────────────────────────────
  # Shows last 20 boot-history entries on interactive login.
  environment.etc."profile.d/boot-history.sh" = {
    text = ''
      if [ -f /var/log/boot-history ]; then
        echo "=== Boot history ==="
        tail -n 20 /var/log/boot-history
        echo ""
      fi
    '';
    mode = "0644";
  };

  environment.etc."profile.d/devbox-env.sh" = {
    text = ''
      export LETTA_BASE_URL=http://localhost:8283
      export LETTA_SERVER_TOKEN=foo
      export LETTA_API_KEY=foo
    '';
    mode = "0644";
  };

  # ── Auto-stop (idle timer) ─────────────────────────────────────
  # A true idle timer that RESETS whenever the box is in use. The
  # devbox-autostop.timer below polls this every 10 min; each poll, if a user
  # is active, it touches the last-active marker (resetting the countdown).
  # The box stops only after the full idle window with no activity — default
  # 4h, overridable via /etc/devbox/autostop-after ("off" disables).
  systemd.services.devbox-autostop = {
    description = "Stop this instance after it has been idle";
    serviceConfig = {
      Type      = "oneshot";
      ExecStart = toString (pkgs.writeShellScript "devbox-autostop" ''
        LOG=/var/log/boot-history
        STATE=/run/devbox-last-active
        stamp() { ${pkgs.coreutils}/bin/date '+%Y-%m-%d %H:%M:%S'; }

        # Idle window (resettable via /etc/devbox/autostop-after; "off" disables).
        WINDOW="4h"
        [ -f /etc/devbox/autostop-after ] && WINDOW=$(${pkgs.coreutils}/bin/cat /etc/devbox/autostop-after)
        [ "$WINDOW" = "off" ] && exit 0
        num=$(printf '%s' "$WINDOW" | ${pkgs.coreutils}/bin/tr -cd '0-9'); [ -z "$num" ] && num=4
        case "$WINDOW" in
          *d*) SECS=$((num * 86400));;
          *h*) SECS=$((num * 3600));;
          *min*|*m*) SECS=$((num * 60));;
          *s*) SECS=$num;;
          *) SECS=$((num * 3600));;
        esac

        # First poll of the boot seeds the marker so the window counts from now.
        [ -f "$STATE" ] || ${pkgs.coreutils}/bin/touch "$STATE"

        # Active? Reset the idle countdown and stop here. "Active" = a logged-in
        # session (who/utmp; covers SSH and Tailscale SSH) or an established
        # inbound :22 connection.
        if ${pkgs.coreutils}/bin/who 2>/dev/null | ${pkgs.gnugrep}/bin/grep -q . \
           || ${pkgs.iproute2}/bin/ss -tnH state established 2>/dev/null | ${pkgs.gnugrep}/bin/grep -qE ':22[[:space:]]'; then
          ${pkgs.coreutils}/bin/touch "$STATE"
          exit 0
        fi

        # Idle: stop only once the full window has elapsed since last activity.
        NOW=$(${pkgs.coreutils}/bin/date +%s)
        LAST=$(${pkgs.coreutils}/bin/stat -c %Y "$STATE" 2>/dev/null || echo "$NOW")
        IDLE=$((NOW - LAST))
        [ "$IDLE" -lt "$SECS" ] && exit 0

        echo "$(stamp) | auto-stop | idle ''${IDLE}s >= ''${SECS}s, stopping" >> "$LOG"
        TOKEN=$(${pkgs.curl}/bin/curl -sX PUT \
          "http://169.254.169.254/latest/api/token" \
          -H "X-aws-ec2-metadata-token-ttl-seconds: 60")
        IID=$(${pkgs.curl}/bin/curl -s \
          -H "X-aws-ec2-metadata-token: $TOKEN" \
          http://169.254.169.254/latest/meta-data/instance-id)
        AZ=$(${pkgs.curl}/bin/curl -s \
          -H "X-aws-ec2-metadata-token: $TOKEN" \
          http://169.254.169.254/latest/meta-data/placement/availability-zone)
        REGION=''${AZ%?}
        ${pkgs.awscli2}/bin/aws ec2 stop-instances \
          --region "$REGION" --instance-ids "$IID"
      '');
    };
  };

  # ── Auto-stop poll timer ───────────────────────────────────────
  # Runs devbox-autostop every 10 min (the idle-reset granularity). The idle
  # window itself is 4h by default — see devbox-autostop above.
  systemd.timers.devbox-autostop = {
    description = "Poll for idle auto-stop";
    wantedBy    = [ "timers.target" ];
    timerConfig = {
      OnBootSec       = "10min";
      OnUnitActiveSec = "10min";
      Unit            = "devbox-autostop.service";
    };
  };

  # ── home-manager switch on boot ─────────────────────────────────
  # Applies the latest home-manager configuration for emaland on
  # every boot. Supports a remote flake URL or traditional config.
  systemd.services.devbox-home-manager = {
    description = "Run home-manager switch on boot";
    after       = [ "home.mount" "network-online.target" ];
    wants       = [ "network-online.target" ];
    wantedBy    = [ "multi-user.target" ];
    serviceConfig = {
      Type            = "oneshot";
      User            = "emaland";
      # Bound the boot-time switch so a slow/large rebuild can't run
      # forever (it previously ran with no limit). 60min gives a fresh
      # instance type enough headroom for a full first-time package build
      # even with the local cache reuse above; Wants=multi-user.target is a
      # soft dependency, so this was never actually gating boot completion.
      TimeoutStartSec = "60min";
      ExecStart = toString (pkgs.writeShellScript "devbox-home-manager" ''
        export HOME=/home/emaland
        export PATH=${lib.makeBinPath [ pkgs.home-manager pkgs.nix pkgs.git pkgs.openssh pkgs.util-linux ]}:$PATH

        # Only run against the persistent /home volume. If it isn't
        # mounted (e.g. the data volume failed to attach/mount), skip:
        # running home-manager with an ephemeral HOME corrupts the nix
        # profile (the ~/.nix-profile → /tmp/fake breakage) and wastes a
        # heavy rebuild that won't persist.
        if ! mountpoint -q /home; then
          echo "/home is not a mountpoint; skipping home-manager switch"
          exit 0
        fi

        # Reuse the same local binary cache the system-level nixos-rebuild
        # switch substitutes from (populated onto the persistent /home volume
        # after that switch). Without this, home-manager — a separate
        # profile — re-fetches every package from the network on a fresh
        # instance even when the system closure just proved it's all sitting
        # right here, which is slow enough to hit TimeoutStartSec above.
        OPT=""
        if [ -d /home/.nix-cache ]; then
          OPT="--option extra-substituters file:///home/.nix-cache --option extra-trusted-substituters file:///home/.nix-cache --option require-sigs false"
          echo "devbox: substituting from local nix cache /home/.nix-cache"
        fi

        FLAKE_FILE="$HOME/.config/devbox/home-flake"
        if [ -f "$FLAKE_FILE" ]; then
          FLAKE_URL=$(cat "$FLAKE_FILE")
          echo "Switching home-manager with flake: $FLAKE_URL"
          ${pkgs.home-manager}/bin/home-manager switch --flake "$FLAKE_URL" $OPT
        else
          echo "Switching home-manager with local config"
          ${pkgs.home-manager}/bin/home-manager switch $OPT
        fi
      '');
    };
  };

  # ── Claude Code autostart ────────────────────────────────────────
  # Starts a tmux session running Claude Code on boot.
  # Attach with: tmux attach -t claude
  systemd.services.devbox-claude = {
    description = "Start Claude Code in tmux";
    after       = [ "home.mount" "network-online.target" "devbox-home-manager.service" ];
    wants       = [ "network-online.target" "devbox-home-manager.service" ];
    wantedBy    = [ "multi-user.target" ];
    serviceConfig = {
      Type            = "oneshot";
      RemainAfterExit = true;
      User            = "emaland";
      ExecStart = toString (pkgs.writeShellScript "devbox-claude" ''
        export HOME=/home/emaland
        export PATH=${lib.makeBinPath [ pkgs.tmux pkgs.git pkgs.curl pkgs.openssh pkgs.nodejs ]}:/etc/profiles/per-user/emaland/bin:$PATH
        PROJECT_DIR="$HOME/scratch/git/ions"

        if [ ! -d "$PROJECT_DIR" ]; then
          echo "Project directory $PROJECT_DIR does not exist, skipping"
          exit 0
        fi

        # Start the tmux server so the user's tmux config loads — this is
        # what re-arms tmux-continuum's periodic session saving.
        ${pkgs.tmux}/bin/tmux start-server 2>/dev/null || true

        # Restore the tmux sessions/windows/panes saved before the reboot.
        # tmux-continuum's own auto-restore only triggers when a client
        # attaches, so on a headless boot we invoke tmux-resurrect's restore
        # explicitly. Check system config (/etc/tmux.conf, written by the
        # programs.tmux NixOS module) then user config as a fallback.
        RES_TMUX=""
        for TMUX_CONF in /etc/tmux.conf "$HOME/.config/tmux/tmux.conf" "$HOME/.tmux.conf"; do
          if [ -f "$TMUX_CONF" ]; then
            RES_TMUX=$(${pkgs.gnugrep}/bin/grep -oE \
              '/nix/store/[^ ]*resurrect[^ ]*/resurrect\.tmux' "$TMUX_CONF" | head -1)
            [ -n "$RES_TMUX" ] && break
          fi
        done
        if [ -n "$RES_TMUX" ]; then
          RESTORE="$(dirname "$RES_TMUX")/scripts/restore.sh"
          if [ -x "$RESTORE" ]; then
            ${pkgs.tmux}/bin/tmux run-shell "$RESTORE"
            sleep 3
            echo "Restored saved tmux sessions"
          fi
        fi

        # Ensure a dedicated claude session (recreate so it always resumes the
        # latest conversation with --continue). Separate from restored sessions.
        ${pkgs.tmux}/bin/tmux kill-session -t claude 2>/dev/null || true
        ${pkgs.tmux}/bin/tmux new-session -d -s claude -c "$PROJECT_DIR" \
          "/etc/profiles/per-user/emaland/bin/claude --dangerously-skip-permissions --continue 'continue from where you left off'"

        echo "Claude Code started in tmux session 'claude' at $PROJECT_DIR"
      '');
    };
  };

  # ── tmux with session save/restore ───────────────────────────────
  # resurrect saves sessions manually (prefix + Ctrl-s) and on shutdown.
  # continuum auto-saves every 15 minutes and auto-restores on server start.
  programs.tmux = {
    enable  = true;
    plugins = with pkgs.tmuxPlugins; [
      sensible
      resurrect
      continuum
    ];
    extraConfig = ''
      set -g @resurrect-capture-pane-contents 'on'
      set -g @continuum-restore 'on'
      set -g @continuum-save-interval '15'
    '';
  };

  # ── SSM Agent ─────────────────────────────────────────────────────
  services.amazon-ssm-agent.enable = true;

  # ── Docker ────────────────────────────────────────────────────────
  virtualisation.docker.enable = true;

  # ── Nix settings ──────────────────────────────────────────────────
  nix.settings = {
    experimental-features = [ "nix-command" "flakes" ];
    trusted-users         = [ "root" "emaland" ];
  };

  # ── Nix garbage collection ─────────────────────────────────────
  nix.gc = {
    automatic = true;
    dates     = "daily";
    options   = "--delete-older-than 7d";
  };

  # ── System packages ──────────────────────────────────────────────
  environment.systemPackages = with pkgs; [
    git
    curl
    wget
    htop
    vim
    jq
    python3
    emacs
    gcc
    gnumake
    awscli2
    home-manager
    ripgrep
  ];

  system.stateVersion = "24.11";
}
