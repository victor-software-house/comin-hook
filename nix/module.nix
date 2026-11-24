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

  # Funnel proxies to the receiver at the same path as it serves on: tailscaled
  # strips the mount point from the request path before it proxies.
  funnelTarget = "http://${listen}${cfg.path}";
  tailscale = "${config.services.tailscale.package}/bin/tailscale";
  funnelArgs = [
    "--https=${toString cfg.funnel.port}"
    "--set-path=${cfg.path}"
  ];

  funnelPort = toString cfg.funnel.port;
  jq = lib.getExe pkgs.jq;
  sleep = "${pkgs.coreutils}/bin/sleep";

  # The jq programs are files, so the flake checks run them against fixtures.
  #
  # Funnel can start only once the node is on the tailnet. Before the first
  # network map, the node lacks the capabilities that the CLI reads, and the
  # CLI then exits 0 without changing anything. The wait ends before
  # TimeoutStartSec.
  funnelWait = pkgs.writeShellScript "comin-hook-funnel-wait" ''
    set -euo pipefail
    while [ "$SECONDS" -lt 40 ]; do
      if ${tailscale} status --json | ${jq} --exit-status --from-file ${./funnel-ready.jq} > /dev/null 2>&1; then
        exit 0
      fi
      ${sleep} 1
    done
    echo "tailscaled has not joined the tailnet yet" >&2
    exit 1
  '';

  # Funnel applies to a whole host and port, so it stops when the port serves
  # anything besides the receiver, which Funnel would publish too.
  funnelPreflight = pkgs.writeShellScript "comin-hook-funnel-preflight" ''
    set -euo pipefail
    problems=$(${tailscale} serve status --json | ${jq} --raw-output \
      --arg port ${funnelPort} \
      --arg path ${lib.escapeShellArg cfg.path} \
      --from-file ${./funnel-preflight.jq})
    if [ -n "$problems" ]; then
      echo "Funnel would publish more than the receiver on port ${funnelPort}:" >&2
      echo "$problems" >&2
      echo "Move the other handlers to another port, or set another services.comin-hook.funnel.port." >&2
      exit 1
    fi
  '';

  # The tailscale CLI exits 0 without changing anything when the node lacks the
  # funnel attribute, so a start succeeds only when the serve configuration
  # shows Funnel on for the port, with a proxy handler for the receiver.
  funnelCheck = pkgs.writeShellScript "comin-hook-funnel-check" ''
    set -euo pipefail
    if ! ${tailscale} serve status --json | ${jq} --exit-status \
      --arg port ${funnelPort} \
      --arg path ${lib.escapeShellArg cfg.path} \
      --arg target ${lib.escapeShellArg funnelTarget} \
      --from-file ${./funnel-check.jq} > /dev/null; then
      echo "Funnel is not on for port ${funnelPort} with a handler at ${cfg.path}" >&2
      echo "Check that the node has the funnel attribute and that HTTPS is enabled for the tailnet." >&2
      exit 1
    fi
  '';

  serveArgs = [
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
      type = lib.types.strMatching "(/([A-Za-z0-9_~-]|\\.[A-Za-z0-9_~-]|\\.\\.[A-Za-z0-9._~-])[A-Za-z0-9._~-]*)+";
      default = "/deploy";
      description = ''
        The one request path the receiver accepts.
        Each segment has letters, digits and the characters . _ ~ -, and is neither . nor ..
        The path has no trailing slash and no empty segment.
      '';
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
      default = config.services.comin.grpc.unix_socket_path;
      defaultText = lib.literalExpression "config.services.comin.grpc.unix_socket_path";
      description = "The path of the gRPC unix socket of comin.";
    };

    cominSocketGroup = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default =
        if config.services.comin.grpc.unix_socket_group == "" then
          null
        else
          config.services.comin.grpc.unix_socket_group;
      defaultText = lib.literalExpression ''
        the group of the comin socket, services.comin.grpc.unix_socket_group, or null when that is empty
      '';
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
      {
        assertion = cfg.funnel.enable -> config.services.tailscale.enable;
        message = "services.comin-hook.funnel.enable needs services.tailscale.enable.";
      }
    ];

    systemd.services.comin-hook = {
      description = "Make comin fetch when a CI job asks";
      wantedBy = [ "multi-user.target" ];
      wants = [ "network-online.target" ];
      after = [
        "network-online.target"
        "comin.service"
      ];

      serviceConfig = {
        ExecStart = utils.escapeSystemdExecArgs ([ exe ] ++ serveArgs);
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
      };
    };

    systemd.services.comin-hook-funnel = lib.mkIf cfg.funnel.enable {
      description = "Publish the comin-hook receiver with Tailscale Funnel";
      wantedBy = [ "multi-user.target" ];
      wants = [
        "tailscaled.service"
        "tailscaled-autoconnect.service"
      ];
      after = [
        "tailscaled.service"
        "tailscaled-autoconnect.service"
      ];
      partOf = [ "comin-hook.service" ];

      serviceConfig = {
        Type = "oneshot";
        RemainAfterExit = true;
        TimeoutStartSec = 60;
        # A start can fail before the node has its network map, and a oneshot
        # unit does not start again by itself.
        Restart = "on-failure";
        RestartSec = "10s";
        ExecStartPre = [
          funnelWait
          funnelPreflight
        ];
        ExecStart = utils.escapeSystemdExecArgs (
          [
            tailscale
            "funnel"
            "--bg"
            "--yes"
          ]
          ++ funnelArgs
          ++ [ funnelTarget ]
        );
        ExecStartPost = funnelCheck;
        # "serve ... off" removes the handler, and Funnel with it when no
        # handler remains on the port. Unlike "funnel ... off", it does not
        # check the funnel attribute first, so it works on a node that lost it.
        # "-" ignores a failure, so a stop with tailscaled down or with no
        # handler left leaves no failed unit.
        ExecStop =
          "-"
          + utils.escapeSystemdExecArgs (
            [
              tailscale
              "serve"
            ]
            ++ funnelArgs
            ++ [ "off" ]
          );
      };
    };
  };
}
