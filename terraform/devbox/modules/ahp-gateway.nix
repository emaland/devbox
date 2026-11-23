# The AHP gateway as a system service (design note 030).
#
# A system unit with User=, so it can be ordered after the /home mount and
# other boot services can depend on it. A config switch never restarts it;
# the new build takes effect at the next restart or boot. The token is added
# to the gateway's config at each start and never enters the Nix store.
#
# Not in this file yet: the readiness check, the failure notice. Until they
# exist the template keeps `enable = false`.
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

  prepare = pkgs.writeShellScript "ahp-gateway-prepare" ''
    export PATH=${lib.makeBinPath [ pkgs.jq pkgs.iproute2 pkgs.coreutils pkgs.gnused ]}
    exec ${pkgs.bash}/bin/sh ${../ahp-gateway/prepare.sh}
  '';

  startTimeout = cfg.readyTimeoutSec + 15;

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
    readyPath = mkOption { type = types.str; default = "/healthz"; };
    noRetryExitStatus = mkOption { type = types.listOf types.int; default = [ ]; };
    managementCommands = mkOption { type = types.listOf types.str; default = [ ]; };
    allowDowngrade = mkOption { type = types.bool; default = false; };
    environment = mkOption { type = types.attrsOf types.str; default = { }; };
    extraPath = mkOption { type = types.listOf types.package; default = [ ]; };
    readyTimeoutSec = mkOption { type = types.ints.positive; default = 60; };
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
    ];

    services.ahp-gateway.generated = { inherit baseConfig closure; };

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
      };

      unitConfig = {
        RequiresMountsFor = [ cfg.stateDir home ];
        StartLimitBurst = 5;
        # Five starts that each spend a full start timeout in prepare and
        # another waiting for readiness, then stop and wait, must fit inside
        # the window, or the limit is never reached.
        StartLimitIntervalSec = 6 * (2 * startTimeout + cfg.stopTimeoutSec + 2);
      };

      serviceConfig = {
        User = cfg.user;
        Group = account.group;
        WorkingDirectory = home;
        RuntimeDirectory = "ahp-gateway";
        RuntimeDirectoryMode = "0700";
        UnsetEnvironment = "CLAUDECODE";
        ExecStartPre = "${prepare}";
        ExecStart = "${node} ${cli} --config ${runtimeConfig}";
        Restart = "on-failure";
        RestartSec = 2;
        KillMode = "mixed";
        OOMPolicy = "continue";
        TimeoutStartSec = startTimeout;
        TimeoutStopSec = cfg.stopTimeoutSec;
      } // lib.optionalAttrs (cfg.noRetryExitStatus != [ ]) {
        RestartPreventExitStatus = map toString cfg.noRetryExitStatus;
      };
    };
  };
}
