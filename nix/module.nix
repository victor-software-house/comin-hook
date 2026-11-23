self:
{
  config,
  lib,
  pkgs,
  utils,
  ...
}:
let
  cfg = config.services.comin-hook;

  listen =
    if lib.hasInfix ":" cfg.listenAddress then
      "[${cfg.listenAddress}]:${toString cfg.port}"
    else
      "${cfg.listenAddress}:${toString cfg.port}";

  exe = lib.getExe cfg.package;

  # Flags of publish and unpublish, which proxy to the receiver.
  funnelArgs = [
    "--listen"
    listen
    "--path"
    cfg.path
    "--funnel-port"
    (toString cfg.funnel.port)
  ];

  serveArgs =
    [
      "serve"
      "--listen"
      listen
      "--path"
      cfg.path
      "--issuer"
      cfg.issuer
      "--audience"
      cfg.audience
      "--comin-socket"
      cfg.cominSocket
    ]
    ++ lib.concatMap (subject: [
      "--subject"
      subject
    ]) cfg.subjects
    ++ lib.concatLists (
      lib.mapAttrsToList (key: value: [
        "--claim"
        "${key}=${value}"
      ]) cfg.claims
    );

  # "+" runs the command as root, outside the sandbox of the unit.
  # "-" lets the stop succeed when tailscaled is already down.
  command = prefix: args: "${prefix}${utils.escapeSystemdExecArgs ([ exe ] ++ args)}";
in
{
  options.services.comin-hook = {
    enable = lib.mkEnableOption "the comin-hook receiver, which makes comin fetch when a CI job asks";

    package = lib.mkOption {
      type = lib.types.package;
      default = self.packages.${pkgs.stdenv.hostPlatform.system}.default;
      defaultText = lib.literalExpression "comin-hook.packages.${pkgs.stdenv.hostPlatform.system}.default";
      description = "The comin-hook package to run.";
    };

    listenAddress = lib.mkOption {
      type = lib.types.str;
      default = "127.0.0.1";
      description = "The address the receiver listens on.";
    };

    port = lib.mkOption {
      type = lib.types.port;
      default = 8371;
      description = "The TCP port the receiver listens on.";
    };

    path = lib.mkOption {
      type = lib.types.strMatching "/.*";
      default = "/deploy";
      description = "The one request path the receiver accepts.";
    };

    issuer = lib.mkOption {
      type = lib.types.str;
      default = "https://token.actions.githubusercontent.com";
      description = "The OIDC issuer of the CI tokens. It must be an https URL.";
    };

    audience = lib.mkOption {
      type = lib.types.str;
      example = "https://example.com/comin-hook";
      description = "The audience that the CI tokens must carry.";
    };

    subjects = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [ ];
      example = [ "repo:example-org/example-repo:ref:refs/heads/main" ];
      description = "The sub claims that may trigger a fetch, matched exactly. At least one is required.";
    };

    claims = lib.mkOption {
      type = lib.types.attrsOf lib.types.str;
      default = { };
      example = {
        event_name = "push";
        ref = "refs/heads/main";
      };
      description = "Extra string claims that a token must carry with exactly these values.";
    };

    cominSocket = lib.mkOption {
      type = lib.types.path;
      default = "/run/comin-grpc.sock";
      description = "The path of the gRPC unix socket of comin.";
    };

    cominSocketGroup = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      example = "comin";
      description = ''
        A group to add to the receiver's supplementary groups, when the group of the comin socket limits access to it.
        With null, the receiver joins no extra group.
      '';
    };

    funnel = {
      enable = lib.mkEnableOption "publishing the receiver on the internet through the Tailscale Funnel of the local tailscaled";

      port = lib.mkOption {
        type = lib.types.enum [
          443
          8443
          10000
        ];
        default = 443;
        description = "The HTTPS port of Tailscale Funnel. Funnel allows only these three.";
      };
    };
  };

  config = lib.mkIf cfg.enable {
    assertions = [
      {
        assertion = cfg.subjects != [ ];
        message = "services.comin-hook.subjects needs at least one subject.";
      }
    ];

    systemd.services.comin-hook = {
      description = "Make comin fetch when a CI job asks";
      wantedBy = [ "multi-user.target" ];
      wants = [ "network-online.target" ];
      after = [
        "network-online.target"
        "comin.service"
      ] ++ lib.optional cfg.funnel.enable "tailscaled.service";

      serviceConfig = {
        ExecStart = command "" serveArgs;
        Restart = "on-failure";
        RestartSec = "5s";

        DynamicUser = true;
        SupplementaryGroups = lib.optional (cfg.cominSocketGroup != null) cfg.cominSocketGroup;

        CapabilityBoundingSet = "";
        AmbientCapabilities = "";
        LockPersonality = true;
        MemoryDenyWriteExecute = true;
        NoNewPrivileges = true;
        PrivateDevices = true;
        PrivateTmp = true;
        ProcSubset = "pid";
        ProtectClock = true;
        ProtectControlGroups = true;
        ProtectHome = true;
        ProtectHostname = true;
        ProtectKernelLogs = true;
        ProtectKernelModules = true;
        ProtectKernelTunables = true;
        ProtectProc = "invisible";
        ProtectSystem = "strict";
        RemoveIPC = true;
        RestrictAddressFamilies = [
          "AF_INET"
          "AF_INET6"
          "AF_UNIX"
        ];
        RestrictNamespaces = true;
        RestrictRealtime = true;
        RestrictSUIDSGID = true;
        SystemCallArchitectures = "native";
        SystemCallFilter = [
          "@system-service"
          "~@privileged"
        ];
        UMask = "0077";
      }
      // lib.optionalAttrs cfg.funnel.enable {
        ExecStartPre = command "+" ([ "publish" ] ++ funnelArgs);
        ExecStopPost = command "-+" ([ "unpublish" ] ++ funnelArgs);
      };
    };
  };
}
