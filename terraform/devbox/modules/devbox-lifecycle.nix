# Devbox lifecycle (design note 031): auto-stop that sees agents, the spot
# interruption watcher, the box-side `devbox-agents` command, and auto-resume
# as the install default of the gateway.
#
# The scripts are in ../lifecycle and take every program from PATH, which is
# set here to store paths; the tests run the same scripts with fakes first on
# PATH.
{ config, lib, pkgs, ... }:

let
  inherit (lib) mkOption mkIf mkDefault types;
  cfg = config.services.devbox-lifecycle;
  gateway = config.services.ahp-gateway;

  scripts = ../lifecycle;

  tools = [
    pkgs.coreutils
    pkgs.gnused
    pkgs.curl
    pkgs.jq
    pkgs.iproute2
    pkgs.util-linux # setpriv: the token file is read with its owner's rights
    pkgs.awscli2
    config.systemd.package
  ];

  environment = {
    DEVBOX_LIFECYCLE_LIB = "${scripts}";
    DEVBOX_LOG = cfg.logFile;
    DEVBOX_WEBHOOK_FILE = if cfg.webhookFile == null then "" else cfg.webhookFile;
    DEVBOX_GATEWAY_UNIT = "ahp-gateway.service";
    DEVBOX_GATEWAY_STOP_TIMEOUT = toString (gateway.stopTimeoutSec + 15);
    DEVBOX_AUTOSTOP_STATE = "/run/devbox-autostop";
    DEVBOX_SPOT_STATE = "/run/devbox-spot-watch";
  };

  hasResume = gateway.enable && lib.elem "resume" gateway.managementCommands;

  devboxAgents = pkgs.writeShellScriptBin "devbox-agents" ''
    export PATH=${lib.makeBinPath tools}:/run/wrappers/bin:$PATH
    export DEVBOX_LIFECYCLE_LIB=${scripts}
    export DEVBOX_AUTOSTOP_STATE=${environment.DEVBOX_AUTOSTOP_STATE}
    export DEVBOX_GATEWAY_HAS_RESUME=${if hasResume then "1" else "0"}
    # By store path: a copy of the gateway earlier on the user's PATH is
    # never the one run.
    export DEVBOX_GATEWAY_COMMAND=${if hasResume then "${gateway.generated.commands}/bin/ahp-acp-gateway" else ""}
    exec ${pkgs.bash}/bin/sh ${scripts}/devbox-agents.sh "$@"
  '';

  resumeSummary = pkgs.writeShellScript "devbox-resume-summary" ''
    export PATH=${lib.makeBinPath tools}
    export DEVBOX_LIFECYCLE_LIB=${scripts}
    exec ${pkgs.bash}/bin/sh ${scripts}/resume-summary.sh
  '';

  notify = pkgs.writeShellScript "devbox-notify" ''
    export PATH=${lib.makeBinPath tools}
    export DEVBOX_WEBHOOK_FILE=${lib.escapeShellArg environment.DEVBOX_WEBHOOK_FILE}
    exec ${pkgs.bash}/bin/sh ${scripts}/notify.sh "$@"
  '';
in
{
  options.services.devbox-lifecycle = {
    enable = mkOption {
      type = types.bool;
      default = false;
      description = "Auto-stop that sees agents, the spot watcher and devbox-agents.";
    };
    logFile = mkOption {
      type = types.str;
      default = "/var/log/boot-history";
      description = "Where each decision is written.";
    };
    webhookFile = mkOption {
      type = types.nullOr types.str;
      default = null;
      description = "Optional file with a SLACK_WEBHOOK_URL=... line. Read, never run.";
    };
    autostop = {
      defaultWindow = mkOption {
        type = types.str;
        default = "4h";
        description = "Idle window when /etc/devbox/autostop-after is absent.";
      };
      defaultBusyCap = mkOption {
        type = types.str;
        default = "12h";
        description = "Busy cap when /etc/devbox/autostop-busy-cap is absent; \"off\" disables it.";
      };
    };
    spotWatch = {
      enable = mkOption { type = types.bool; default = true; description = "Watch for spot interruption notices."; };
      pollSeconds = mkOption { type = types.ints.positive; default = 5; };
    };
    resumeOnBoot = mkOption {
      type = types.bool;
      default = true;
      description = "Make auto-resume the gateway's install default (resume.onStart = continue, one attempt per turn).";
    };

    generated = mkOption {
      internal = true;
      readOnly = true;
      type = types.attrs;
      description = "Generated files, for tests and the boot notification.";
    };
  };

  config = mkIf cfg.enable {
    assertions = [
      {
        # Note 031 section 1.1: the gateway's stop must fit in the spot
        # notice with margin.
        assertion = !(cfg.spotWatch.enable && gateway.enable) || gateway.stopTimeoutSec <= 60;
        message = "services.devbox-lifecycle: services.ahp-gateway.stopTimeoutSec must be at most 60 s to fit in a spot interruption notice.";
      }
    ];

    services.devbox-lifecycle.generated = { inherit devboxAgents resumeSummary notify scripts hasResume; };

    # The install default only: a persisted `devbox agents resume off` wins,
    # because the unit does not set AHP_GATEWAY_RESUME.
    services.ahp-gateway.settings = mkIf (cfg.resumeOnBoot && gateway.enable) {
      resume = {
        onStart = mkDefault "continue";
        maxAttemptsPerTurn = mkDefault 1;
      };
    };

    environment.systemPackages = [ devboxAgents ];

    systemd.services.devbox-autostop = {
      description = "Stop this instance after it has been idle";
      path = tools;
      environment = environment // {
        DEVBOX_AUTOSTOP_DEFAULT_WINDOW = cfg.autostop.defaultWindow;
        DEVBOX_AUTOSTOP_DEFAULT_CAP = cfg.autostop.defaultBusyCap;
      };
      serviceConfig = {
        Type = "oneshot";
        ExecStart = "${pkgs.bash}/bin/sh ${scripts}/autostop.sh";
        # A poll that hangs must not hold off every later one: the gateway
        # stop (its timeout + 15 s), the notices and stop-instances fit.
        TimeoutStartSec = gateway.stopTimeoutSec + 15 + 120;
      };
    };

    # The poll, every 10 min. The window itself is in the script.
    systemd.timers.devbox-autostop = {
      description = "Poll for idle auto-stop";
      wantedBy = [ "timers.target" ];
      timerConfig = {
        OnBootSec = "10min";
        OnUnitActiveSec = "10min";
        Unit = "devbox-autostop.service";
      };
    };

    systemd.services.devbox-spot-watch = mkIf cfg.spotWatch.enable {
      description = "Watch for spot interruption notices and stop the gateway in time";
      wantedBy = [ "multi-user.target" ];
      after = [ "network-online.target" "devbox-imds-route.service" ];
      wants = [ "network-online.target" ];
      path = tools;
      environment = environment // {
        DEVBOX_SPOT_STATE = "/run/devbox-spot-watch";
        DEVBOX_SPOT_POLL_SECONDS = toString cfg.spotWatch.pollSeconds;
      };
      serviceConfig = {
        Type = "simple";
        ExecStart = "${pkgs.bash}/bin/sh ${scripts}/spot-watch.sh";
        Restart = "always";
        RestartSec = 5;
      };
    };
  };
}
