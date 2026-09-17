{
  config,
  lib,
  pkgs,
  ...
}:

let
  cfg = config.services.cccp;
in
{
  options.services.cccp = {
    enable = lib.mkEnableOption "cccp server";

    package = lib.mkOption {
      type = lib.types.package;
      description = "Package providing bin/cccp.";
    };

    addr = lib.mkOption {
      type = lib.types.str;
      default = ":12110";
      description = "Listen address (host:port).";
    };

    dbPath = lib.mkOption {
      type = lib.types.str;
      default = "/var/lib/cccp/store.sqlite";
      description = "SQLite database path.";
    };

    saeDir = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      description = "Directory of daily SAE dictionary pages served under /sae/daily/.";
    };

    saeCaptureURL = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      description = "Laguna capture service base URL for asynchronous SAE scoring.";
    };

    saeModelsDir = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      description = "Directory of SAE weights and original calibration, keyed by encoder SHA256.";
    };

    tokenFile = lib.mkOption {
      type = lib.types.path;
      description = ''
        Path to a file containing the bearer token. Read by systemd via
        LoadCredential so the token never lands in the world-readable
        /proc or the nix store.
      '';
    };

    pushTokenFile = lib.mkOption {
      type = lib.types.nullOr lib.types.path;
      default = null;
      description = ''
        Optional file containing a second bearer token accepted ONLY on
        the push endpoints (POST /ingest, PUT /blob/{sha}, POST
        /log/append). Hosts holding it can ship their own state but
        cannot read anything back. Read via LoadCredential like
        tokenFile.
      '';
    };

    openFirewall = lib.mkOption {
      type = lib.types.bool;
      default = false;
      description = "Open the listen port in the firewall.";
    };
  };

  config = lib.mkIf cfg.enable {
    users.groups.cccp = { };
    users.users.cccp = {
      isSystemUser = true;
      group = "cccp";
      home = "/var/lib/cccp";
      createHome = true;
    };

    systemd.services.cccp = {
      description = "cccp - cloud code config provider";
      wantedBy = [ "multi-user.target" ];
      after = [ "network.target" ];

      serviceConfig = {
        User = "cccp";
        Group = "cccp";
        StateDirectory = "cccp";
        LoadCredential = [
          "token:${toString cfg.tokenFile}"
        ]
        ++ lib.optional (cfg.pushTokenFile != null) "push-token:${toString cfg.pushTokenFile}";
        SyslogIdentifier = "cccp";
        ExecStart = ''
          ${pkgs.bash}/bin/bash -c 'CCCP_TOKEN="$(cat "$CREDENTIALS_DIRECTORY/token")" ${
            lib.optionalString (
              cfg.pushTokenFile != null
            ) ''CCCP_PUSH_TOKEN="$(cat "$CREDENTIALS_DIRECTORY/push-token")" ''
          }exec ${cfg.package}/bin/cccp serve --addr ${cfg.addr} --db ${cfg.dbPath}${
            lib.optionalString (cfg.saeDir != null) " --sae-dir ${cfg.saeDir}"
          }${lib.optionalString (cfg.saeCaptureURL != null) " --sae-capture-url ${cfg.saeCaptureURL}"}${
            lib.optionalString (cfg.saeModelsDir != null) " --sae-models-dir ${cfg.saeModelsDir}"
          }'
        '';
        Restart = "on-failure";
        RestartSec = 2;

        NoNewPrivileges = true;
        ProtectSystem = "strict";
        ReadWritePaths = lib.optional (cfg.saeDir != null) cfg.saeDir;
        ProtectHome = true;
        PrivateTmp = true;
        PrivateDevices = true;
        ProtectKernelTunables = true;
        ProtectKernelModules = true;
        ProtectControlGroups = true;
        RestrictAddressFamilies = [
          "AF_INET"
          "AF_INET6"
          "AF_UNIX"
        ];
        RestrictNamespaces = true;
        LockPersonality = true;
        MemoryDenyWriteExecute = true;
        SystemCallArchitectures = "native";
      };
    };

    networking.firewall = lib.mkIf cfg.openFirewall {
      allowedTCPPorts =
        let
          m = builtins.match "(.*):([0-9]+)" cfg.addr;
          port =
            if m == null then
              throw "services.cccp.addr must be host:port (got ${cfg.addr})"
            else
              lib.toInt (lib.elemAt m 1);
        in
        [ port ];
    };
  };
}
