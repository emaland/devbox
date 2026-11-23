# The AHP gateway as a system service (design note 030).
#
# A system unit with User=, so it can be ordered after the /home mount and
# other boot services can depend on it. A config switch never restarts it;
# the new build takes effect at the next restart or boot. The token is added
# to the gateway's config at each start and never enters the Nix store.
#
# The unit counts as started only after a readiness check: the ready path
# answers, the listener is this unit's own and loopback only, and a
# tokenless connection is refused. A failed unit, a unit that never started
# (/home missing) and a slow crash loop are reported in the box's log and to
# a webhook if one is configured.
#
# Values in `environment`, `settings` and providers' `env` are written to the
# world-readable Nix store and unit file: no secrets there.
{ config, lib, pkgs, utils, ... }:

let
  inherit (lib) mkOption mkIf types;
  cfg = config.services.ahp-gateway;

  account = config.users.users.${cfg.user};
  home = account.home;
  uid = toString account.uid;

  node = "${pkgs.nodejs_24}/bin/node";
  packageRoot = "${cfg.package}/lib/node_modules/ahp-acp-gateway";
  cli = "${packageRoot}/dist/src/cli.js";
  runtimeConfig = "/run/ahp-gateway/config.json";
  clientConfig = "ahp-gateway/config.json";
  database = "${cfg.stateDir}/ahp-acp-gateway.db";

  # Keys the module owns. `settings` may not set them.
  ownedKeys = [ "listener" "auth" "database" "providers" ];

  providerEntries = lib.mapAttrsToList
    (id: provider: {
      inherit id;
      inherit (provider) command args;
      displayName = if provider.displayName != null then provider.displayName else id;
      enabled = true;
    } // lib.optionalAttrs (provider.env != { }) { inherit (provider) env; })
    cfg.providers;

  overlay = lib.recursiveUpdate cfg.settings {
    version = 1;
    listener = {
      host = "127.0.0.1";
      inherit (cfg) port allowedOrigins;
      path = "/ahp";
      tlsTerminated = false;
      allowAnyOrigin = false;
    };
    database.path = database;
  };

  # The config the gateway starts from, minus the token. Built-in providers
  # are read from the package's own default config, so every one that is not
  # configured here is disabled, including any a newer build adds. A provider
  # that reuses a built-in id replaces that entry's program and arguments;
  # the gateway merges `env` key by key, so a built-in's own environment
  # stays unless set again.
  baseConfig = pkgs.runCommand "ahp-gateway-config.json"
    {
      nativeBuildInputs = [ pkgs.jq ];
      overlay = builtins.toJSON overlay;
      providers = builtins.toJSON providerEntries;
      passAsFile = [ "overlay" "providers" ];
    } ''
    jq --slurpfile mine "$providersPath" \
       --slurpfile defaults ${packageRoot}/ahp-acp-gateway.default.json '
      ($mine[0] | map(.id)) as $ids
      | .providers = (
          [ $defaults[0].providers[]
            | select(.id as $id | $ids | index($id) | not)
            | { id, enabled: false } ]
          + $mine[0])
    ' "$overlayPath" > $out
  '';

  # One store path whose closure is everything the running gateway uses. It
  # is in the unit's environment, which Nix treats as a garbage-collection
  # root while the process lives, so a gateway that outlives its generation
  # (a switch never restarts it) keeps its files.
  closure = pkgs.writeText "ahp-gateway-closure"
    (lib.concatStringsSep "\n" ([ "${cfg.package}" "${baseConfig}" node ]
      ++ map (provider: provider.command) providerEntries));

  # The unit's main process: run the start-up checks, then become the
  # gateway, whose command it is given as arguments. ExecStart therefore
  # names the gateway's own store path (`<launcher> <node> <package>/…/cli.js
  # …`), so tools of the same build can tell that the unit runs it.
  #
  # Prepare runs here rather than in ExecStartPre because systemd honours
  # RestartPreventExitStatus for the main process only; a refusal (exit 78)
  # must not be retried. The tools' PATH is given to prepare alone, so the
  # gateway and its agents keep the unit's PATH.
  prepareTools = lib.makeBinPath (with pkgs; [ jq iproute2 coreutils findutils gnused ]);
  # The launcher bounds prepare (a timeout exits 124, which is retried) and
  # creates a marker when it has finished: systemd starts the readiness check
  # as soon as the launcher starts, and that check starts its own deadline
  # only from the marker.
  launcher = pkgs.writeShellScript "ahp-gateway-launch" ''
    if [ "$#" -eq 0 ]; then
      echo "ahp-gateway-launch: no command to run" >&2
      exit 64
    fi
    marker="''${AHP_GATEWAY_PREPARED_MARKER:-}"
    [ -z "$marker" ] || ${pkgs.coreutils}/bin/rm -f "$marker"
    PATH=${prepareTools} ${pkgs.coreutils}/bin/timeout ${toString cfg.prepareTimeoutSec} ${pkgs.bash}/bin/sh ${../ahp-gateway/prepare.sh} || exit $?
    [ -z "$marker" ] || : >"$marker"
    exec "$@"
  '';

  # The whole start: prepare, then the wait for readiness, then a margin.
  startTimeout = cfg.prepareTimeoutSec + cfg.readyTimeoutSec + 15;
  preparedMarker = "/run/ahp-gateway/prepared";

  ready = pkgs.writeShellScript "ahp-gateway-ready" ''
    export PATH=${lib.makeBinPath (with pkgs; [ curl iproute2 coreutils gawk ])}
    exec ${pkgs.bash}/bin/sh ${../ahp-gateway/ready.sh}
  '';

  notice = pkgs.writeShellScript "ahp-gateway-notice" ''
    export PATH=${lib.makeBinPath (with pkgs; [ curl jq coreutils gnused ])}
    exec ${pkgs.bash}/bin/sh ${../ahp-gateway/notice.sh} "$@"
  '';

  noticeEnvironment = {
    AHP_NOTICE_LOG = cfg.failureNotice.logFile;
    AHP_NOTICE_WEBHOOK_FILE = if cfg.failureNotice.webhookFile == null then "" else cfg.failureNotice.webhookFile;
  };

  agentPath = lib.concatStringsSep ":" ([
    "${home}/.local/bin"
    "${home}/.nix-profile/bin"
    "/etc/profiles/per-user/${cfg.user}/bin"
    "/run/wrappers/bin"
    "/run/current-system/sw/bin"
  ] ++ [
    (lib.makeBinPath (with pkgs; [
      git bash coreutils findutils gnugrep gnused gawk gnumake gcc curl openssh nodejs_24 nix
    ] ++ cfg.extraPath))
  ]);

  commands = pkgs.writeTextFile {
    name = "ahp-acp-gateway-commands";
    destination = "/bin/ahp-acp-gateway";
    executable = true;
    text = ''
      #!${pkgs.runtimeShell}
      # On this box the gateway is a system service. Only commands that are
      # safe next to it are offered; the bare server command is not.
      case "''${1:-}" in
        ${lib.concatStringsSep " | " cfg.managementCommands}) ;;
        *)
          echo "ahp-acp-gateway: the gateway runs as the system service ahp-gateway.service here; available: ${lib.concatStringsSep ", " cfg.managementCommands}" >&2
          exit 2
          ;;
      esac
      export AHP_GATEWAY_STATE_DIR=${lib.escapeShellArg cfg.stateDir}
      export AHP_GATEWAY_CONFIG=/etc/${clientConfig}
      exec ${node} ${cli} "$@"
    '';
  };

  providerType = types.submodule {
    options = {
      command = mkOption {
        type = types.str;
        description = "Absolute Nix store path of the agent program.";
      };
      args = mkOption { type = types.listOf types.str; default = [ ]; };
      env = mkOption { type = types.attrsOf types.str; default = { }; };
      displayName = mkOption { type = types.nullOr types.str; default = null; };
    };
  };
in
{
  options.services.ahp-gateway = {
    enable = mkOption {
      type = types.bool;
      default = false;
      description = "Run the AHP gateway as a system service.";
    };
    package = mkOption {
      type = types.package;
      default = pkgs.callPackage ../pkgs/ahp-acp-gateway.nix { };
      description = "The gateway build. The default is the public release.";
    };
    user = mkOption {
      type = types.str;
      description = "Account the gateway and its agents run as.";
    };
    port = mkOption { type = types.port; default = 8765; description = "Loopback port."; };
    stateDir = mkOption {
      type = types.str;
      default = "${home}/.ahp-acp-gateway";
      defaultText = "<home>/.ahp-acp-gateway";
      description = "Database, token and backups. Must be on persistent storage.";
    };
    tokenFile = mkOption {
      type = types.str;
      default = "${cfg.stateDir}/token";
      defaultText = "<stateDir>/token";
      description = "Bearer token, created on first start if missing.";
    };
    allowedOrigins = mkOption {
      type = types.listOf types.str;
      default = [ ];
      description = "Browser origins allowed to connect.";
    };
    providers = mkOption {
      type = types.attrsOf providerType;
      default = { };
      description = "Agent providers by id. A provider that keeps a built-in id keeps that id's existing sessions working.";
    };
    settings = mkOption {
      type = types.attrsOf types.anything;
      default = { };
      description = "Extra keys merged into the gateway config. Keys the module owns are refused.";
    };
    readyPath = mkOption {
      type = types.str;
      default = "/healthz";
      description = "HTTP path that answers 200 when the gateway is ready.";
    };
    failureNotice = {
      logFile = mkOption {
        type = types.str;
        default = "/var/log/boot-history";
        description = "File a line is appended to when the gateway fails or is not running.";
      };
      webhookFile = mkOption {
        type = types.nullOr types.str;
        default = null;
        description = "Optional file with a SLACK_WEBHOOK_URL=... line. Read, never run.";
      };
      restartsBeforeNotice = mkOption {
        type = types.ints.unsigned;
        default = 3;
        description = "Restarts between two hourly checks that count as a crash loop.";
      };
    };
    noRetryExitStatus = mkOption {
      type = types.listOf types.int;
      default = [ ];
      description = "Exit statuses of the gateway that mean restarting will not help, besides 78, which is always included.";
    };
    managementCommands = mkOption { type = types.listOf types.str; default = [ ]; };
    allowDowngrade = mkOption { type = types.bool; default = false; };
    environment = mkOption {
      type = types.attrsOf types.str;
      default = { };
      description = "Extra variables for the gateway and its agents. Stored in the Nix store: no secrets.";
    };
    extraPath = mkOption { type = types.listOf types.package; default = [ ]; };
    readyTimeoutSec = mkOption {
      type = types.ints.positive;
      default = 60;
      description = "Seconds the gateway has to become ready once the start-up checks have finished.";
    };
    prepareTimeoutSec = mkOption {
      type = types.ints.positive;
      default = 300;
      description = "Seconds the start-up checks may take, a database backup included.";
    };
    stopTimeoutSec = mkOption { type = types.ints.positive; default = 45; };

    generated = mkOption {
      internal = true;
      readOnly = true;
      type = types.attrs;
      description = "Generated files, for tests.";
    };
  };

  config = mkIf cfg.enable {
    assertions = [
      {
        assertion = cfg.providers != { };
        message = "services.ahp-gateway: configure at least one provider; there is no default agent.";
      }
      {
        assertion = lib.all (key: !(cfg.settings ? ${key})) ownedKeys;
        message = "services.ahp-gateway.settings may not set ${lib.concatStringsSep ", " ownedKeys}; the module owns them.";
      }
      {
        assertion = lib.all (p: lib.hasPrefix "${builtins.storeDir}/" p.command) (lib.attrValues cfg.providers);
        message = "services.ahp-gateway.providers: every command must be a Nix store path, so nothing is downloaded or looked up when a session starts.";
      }
      {
        assertion = lib.hasPrefix "/" cfg.stateDir && lib.hasPrefix "/" cfg.tokenFile;
        message = "services.ahp-gateway: stateDir and tokenFile must be absolute paths.";
      }
      {
        assertion = account.uid != null;
        message = "services.ahp-gateway: users.users.${cfg.user}.uid must be set.";
      }
      {
        # Without linger the user's runtime directory goes away at the last
        # logout, under the running gateway and its agents.
        # null (the default) means "not managed", which is not enough here.
        assertion = account.linger == true;
        message = "services.ahp-gateway: users.users.${cfg.user}.linger must be true.";
      }
      {
        assertion = lib.all (name: builtins.match "[a-z][a-z0-9-]*" name != null) cfg.managementCommands;
        message = "services.ahp-gateway.managementCommands must be plain subcommand names (lower-case letters, digits, -).";
      }
    ];

    services.ahp-gateway.generated = { inherit baseConfig closure launcher; };

    environment.etc.${clientConfig}.text = builtins.toJSON {
      managedBy = "system";
      service = "ahp-gateway.service";
      inherit (cfg) port stateDir tokenFile;
      path = "/ahp";
    };

    environment.systemPackages = lib.optional (cfg.managementCommands != [ ]) commands;

    systemd.services.ahp-gateway = {
      description = "AHP gateway (agent sessions for editors)";
      wantedBy = [ "multi-user.target" ];
      wants = [ "network-online.target" "user@${uid}.service" ];
      after = [ "network-online.target" "user@${uid}.service" ];
      restartIfChanged = false;

      environment = cfg.environment // {
        PATH = lib.mkForce agentPath;
        HOME = home;
        SHELL = utils.toShellPath account.shell;
        XDG_RUNTIME_DIR = "/run/user/${uid}";
        NIX_PATH = lib.concatStringsSep ":" config.nix.nixPath;
        AHP_GATEWAY_STATE_DIR = cfg.stateDir;
        AHP_GATEWAY_CLOSURE = "${closure}";
        AHP_GATEWAY_PORT = toString cfg.port;
        AHP_GATEWAY_TOKEN_FILE = cfg.tokenFile;
        AHP_GATEWAY_BASE_CONFIG = "${baseConfig}";
        AHP_GATEWAY_RUNTIME_CONFIG = runtimeConfig;
        AHP_GATEWAY_BUILD_NAME = cfg.package.pname or cfg.package.name;
        AHP_GATEWAY_BUILD_VERSION = cfg.package.version or "unknown";
        AHP_GATEWAY_BUILD_PATH = "${cfg.package}";
        AHP_GATEWAY_ALLOW_DOWNGRADE = if cfg.allowDowngrade then "1" else "0";
        AHP_GATEWAY_READY_PATH = cfg.readyPath;
        AHP_GATEWAY_READY_TIMEOUT = toString cfg.readyTimeoutSec;
        AHP_GATEWAY_PREPARED_MARKER = preparedMarker;
        AHP_GATEWAY_PREPARE_TIMEOUT = toString cfg.prepareTimeoutSec;
      };

      onFailure = [ "ahp-gateway-failed.service" ];

      unitConfig = {
        RequiresMountsFor = [ cfg.stateDir home ];
        StartLimitBurst = 5;
        # Five starts that each spend the whole start timeout (prepare and the
        # ready wait), then stop and wait, must fit inside the window, or the
        # limit is never reached.
        StartLimitIntervalSec = 6 * (startTimeout + cfg.stopTimeoutSec + 2);
      };

      serviceConfig = {
        User = cfg.user;
        Group = account.group;
        WorkingDirectory = home;
        RuntimeDirectory = "ahp-gateway";
        RuntimeDirectoryMode = "0700";
        UnsetEnvironment = "CLAUDECODE";
        ExecStart = "${launcher} ${node} ${cli} --config ${runtimeConfig}";
        ExecStartPost = "${ready}";
        Restart = "on-failure";
        RestartSec = 2;
        KillMode = "mixed";
        OOMPolicy = "continue";
        TimeoutStartSec = startTimeout;
        TimeoutStopSec = cfg.stopTimeoutSec;
        # 78: the start-up checks refused; restarting will not help.
        RestartPreventExitStatus = lib.unique ([ "78" ] ++ map toString cfg.noRetryExitStatus);
      };
    };

    # Neither of these needs /home: "the gateway never started because /home
    # never mounted" is one of the cases they report.
    systemd.services.ahp-gateway-failed = {
      description = "Report that the AHP gateway failed";
      environment = noticeEnvironment;
      serviceConfig = {
        Type = "oneshot";
        # A oneshot has no start timeout by default; a hung notice would
        # block the next one.
        TimeoutStartSec = 60;
        ExecStart = "${notice} 'ahp-gateway.service failed; see journalctl -u ahp-gateway'";
      };
    };

    systemd.services.ahp-gateway-check = {
      description = "Check that the AHP gateway is running and not crash-looping";
      path = [ config.systemd.package pkgs.coreutils pkgs.gnused ];
      environment = noticeEnvironment // {
        AHP_CHECK_UNIT = "ahp-gateway.service";
        AHP_CHECK_STATE = "/run/ahp-gateway-check/restarts";
        AHP_CHECK_RESTARTS = toString cfg.failureNotice.restartsBeforeNotice;
        AHP_NOTICE_SCRIPT = "${notice}";
      };
      serviceConfig = {
        Type = "oneshot";
        RuntimeDirectory = "ahp-gateway-check";
        RuntimeDirectoryPreserve = "yes";
        TimeoutStartSec = 60;
        ExecStart = "${pkgs.bash}/bin/sh ${../ahp-gateway/check.sh}";
      };
    };

    systemd.timers.ahp-gateway-check = {
      wantedBy = [ "timers.target" ];
      timerConfig = {
        OnBootSec = "5min";
        OnUnitActiveSec = "1h";
        Unit = "ahp-gateway-check.service";
      };
    };
  };
}
