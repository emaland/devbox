{ config, pkgs, lib, modulesPath, ... }:

{
  imports = [ "${modulesPath}/virtualisation/amazon-image.nix" ];

  networking.hostName = "dev-workstation";
  networking.dhcpcd.extraConfig = "nohook hostname";

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
    openssh.authorizedKeys.keys = [
      # Add your SSH public key here
    ];
  };

  security.sudo.wheelNeedsPassword = false;

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
        # Wait for tailscaled to be ready
        sleep 2
        status=$(${pkgs.tailscale}/bin/tailscale status --json 2>/dev/null | ${pkgs.jq}/bin/jq -r '.BackendState // empty')
        if [ "$status" = "Running" ]; then
          echo "Tailscale already running"
          exit 0
        fi
        ${pkgs.tailscale}/bin/tailscale up \
          --auth-key=@@TAILSCALE_AUTH_KEY@@ \
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
      ExecStart = toString (pkgs.writeShellScript "update-route53" ''
        TOKEN=$(${pkgs.curl}/bin/curl -sX PUT \
          "http://169.254.169.254/latest/api/token" \
          -H "X-aws-ec2-metadata-token-ttl-seconds: 60")
        PUBLIC_IP=$(${pkgs.curl}/bin/curl -s \
          -H "X-aws-ec2-metadata-token: $TOKEN" \
          http://169.254.169.254/latest/meta-data/public-ipv4)

        # Update your Route53 zone — set the zone ID and record name
        # to match your terraform/main.tf dns_zone_id and dns_name.
        ZONE_ID="REPLACE_WITH_YOUR_ZONE_ID"
        RECORD_NAME="dev.frob.io"

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

  # ── Auto-stop service ──────────────────────────────────────────
  # Logs the auto-stop event and stops the instance via the EC2 API.
  systemd.services.devbox-autostop = {
    description = "Auto-stop this instance";
    serviceConfig = {
      Type      = "oneshot";
      ExecStart = toString (pkgs.writeShellScript "devbox-autostop" ''
        echo "$(date '+%Y-%m-%d %H:%M:%S') | auto-stop | timer expired" \
          >> /var/log/boot-history
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

  # ── Auto-stop scheduler ────────────────────────────────────────
  # Reads /etc/devbox/autostop-after (default 8h) and creates a
  # transient systemd timer to trigger devbox-autostop.service.
  systemd.services.devbox-schedule-autostop = {
    description = "Schedule auto-stop timer on boot";
    after       = [ "devbox-boot-log.service" ];
    wants       = [ "devbox-boot-log.service" ];
    wantedBy    = [ "multi-user.target" ];
    serviceConfig = {
      Type      = "oneshot";
      ExecStart = toString (pkgs.writeShellScript "devbox-schedule-autostop" ''
        # Cancel any previous transient timer
        ${pkgs.systemd}/bin/systemctl stop devbox-autostop-sched.timer 2>/dev/null || true

        TIMEOUT="8h"
        if [ -f /etc/devbox/autostop-after ]; then
          TIMEOUT=$(cat /etc/devbox/autostop-after)
        fi

        if [ "$TIMEOUT" = "off" ]; then
          echo "Auto-stop disabled"
          exit 0
        fi

        ${pkgs.systemd}/bin/systemd-run \
          --unit=devbox-autostop-sched \
          --on-active="$TIMEOUT" \
          ${pkgs.systemd}/bin/systemctl start devbox-autostop.service

        echo "Auto-stop scheduled in $TIMEOUT"
      '');
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
      Type      = "oneshot";
      User      = "emaland";
      ExecStart = toString (pkgs.writeShellScript "devbox-home-manager" ''
        export HOME=/home/emaland
        export PATH=${lib.makeBinPath [ pkgs.home-manager pkgs.nix pkgs.git pkgs.openssh ]}:$PATH

        FLAKE_FILE="$HOME/.config/devbox/home-flake"
        if [ -f "$FLAKE_FILE" ]; then
          FLAKE_URL=$(cat "$FLAKE_FILE")
          echo "Switching home-manager with flake: $FLAKE_URL"
          ${pkgs.home-manager}/bin/home-manager switch --flake "$FLAKE_URL"
        else
          echo "Switching home-manager with local config"
          ${pkgs.home-manager}/bin/home-manager switch
        fi
      '');
    };
  };

  # ── Docker ────────────────────────────────────────────────────────
  virtualisation.docker.enable = true;

  # ── Nix settings ──────────────────────────────────────────────────
  nix.settings = {
    experimental-features = [ "nix-command" "flakes" ];
    trusted-users         = [ "root" "emaland" ];
  };

  # ── System packages ──────────────────────────────────────────────
  environment.systemPackages = with pkgs; [
    git
    curl
    wget
    htop
    tmux
    vim
    jq
    python3
    emacs
    gcc
    gnumake
    awscli2
    home-manager
  ];

  system.stateVersion = "24.11";
}
