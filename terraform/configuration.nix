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
