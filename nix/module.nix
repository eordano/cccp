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

    tokenFile = lib.mkOption {
      type = lib.types.path;
      description = ''
        Path to a file containing the bearer token. Read by systemd via
        LoadCredential so the token never lands in the world-readable
        /proc or the nix store.
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
        LoadCredential = "token:${toString cfg.tokenFile}";
        # SyslogIdentifier overrides the default (argv[0], which would be
        # "bash" because we wrap the server to inject CCCP_TOKEN from the
        # systemd credential). Without this, journald tags every log line
        # "bash[pid]" instead of "cccp[pid]".
        SyslogIdentifier = "cccp";
        ExecStart = ''
          ${pkgs.bash}/bin/bash -c 'CCCP_TOKEN="$(cat "$CREDENTIALS_DIRECTORY/token")" exec ${cfg.package}/bin/cccp serve --addr ${cfg.addr} --db ${cfg.dbPath}'
        '';
        Restart = "on-failure";
        RestartSec = 2;

        # Hardening
        NoNewPrivileges = true;
        ProtectSystem = "strict";
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
          # Match "host:port" where host may contain colons (e.g. "[::1]:12110").
          # Greedy .* absorbs the host, leaving the trailing numeric port.
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
