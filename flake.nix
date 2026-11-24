{
  description = "Trigger a comin fetch from CI with an OIDC token";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-26.05";

  # Only the module check evaluates a host with comin enabled.
  inputs.comin = {
    url = "github:nlewo/comin/4cc0f6a9764a0ac73ec248d1475a1372c19b4773";
    inputs.nixpkgs.follows = "nixpkgs";
  };

  outputs =
    {
      self,
      nixpkgs,
      comin,
    }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
      ];
      forSystems = nixpkgs.lib.genAttrs systems;
    in
    {
      packages = forSystems (
        system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
        in
        {
          # go.mod needs a newer Go than the default of nixos-26.05.
          default = pkgs.callPackage ./nix/package.nix {
            buildGoModule = pkgs.buildGoModule.override { go = pkgs.go_1_27; };
          };
        }
      );

      # Evaluates a host that runs the module with Funnel, comin and tailscaled
      # to its system derivation, with no build and no VM.
      checks = forSystems (
        system:
        let
          host = nixpkgs.lib.nixosSystem {
            inherit system;
            modules = [
              comin.nixosModules.comin
              self.nixosModules.default
              {
                networking.hostName = "example-host";
                system.stateVersion = "26.05";
                boot.loader.grub.enable = false;
                fileSystems."/" = {
                  device = "/dev/null";
                  fsType = "ext4";
                };

                services.tailscale.enable = true;
                services.comin = {
                  enable = true;
                  remotes = [
                    {
                      name = "origin";
                      url = "https://example.com/example-org/example-repo.git";
                    }
                  ];
                };
                services.comin-hook = {
                  enable = true;
                  audience = "https://example.com/comin-hook";
                  subjects = [ "repo:example-org/example-repo:ref:refs/heads/main" ];
                  funnel.enable = true;
                };
              }
            ];
          };
          pkgs = nixpkgs.legacyPackages.${system};
          inherit (nixpkgs) lib;

          # Runs a jq program of the Funnel unit on every fixture of its
          # directory, and compares the output with the .expected file.
          funnelPrograms =
            pkgs.runCommand "comin-hook-funnel-programs" { nativeBuildInputs = [ pkgs.jq ]; }
              ''
                failed=0
                count=0
                run() {
                  program=$1
                  dir=$2
                  shift 2
                  for fixture in "$dir"/*.json; do
                    expected="''${fixture%.json}.expected"
                    actual=$(jq --raw-output "$@" --from-file "$program" "$fixture")
                    count=$((count + 1))
                    if [ "$actual" != "$(cat "$expected")" ]; then
                      echo "FAIL $fixture: got '$actual', want '$(cat "$expected")'" >&2
                      failed=1
                    fi
                  done
                }
                run ${./nix/funnel-preflight.jq} ${./nix/fixtures/funnel-preflight} --arg port 443 --arg path /hook/x
                run ${./nix/funnel-check.jq} ${./nix/fixtures/funnel-check} \
                  --arg port 443 --arg path /hook/x --arg target 'http://[::1]:8371/hook/x'
                run ${./nix/funnel-ready.jq} ${./nix/fixtures/funnel-ready}
                echo "$count fixtures"
                [ "$count" -ge 20 ] && [ "$failed" = 0 ]
                touch $out
              '';

          # The path option takes only a clean absolute path.
          pathCheck = host.options.services.comin-hook.path.type.check;
          goodPaths = [
            "/deploy"
            "/a/b"
            "/.well-known/x"
            "/a.b"
            "/a~b_c-d"
            "/..a"
            "/.a"
            "/a."
          ];
          badPaths = [
            ""
            "/"
            "/a/"
            "//a"
            "/a//b"
            "/.."
            "/."
            "/a/../b"
            "/a/./b"
            "/a?b"
            "/a#b"
            "/a b"
            "/a%2e"
            "deploy"
          ];
        in
        assert lib.all pathCheck goodPaths;
        assert lib.all (path: !pathCheck path) badPaths;
        {
          module = pkgs.writeText "comin-hook-module-check" (
            builtins.unsafeDiscardStringContext host.config.system.build.toplevel.drvPath
          );
          funnel-programs = funnelPrograms;
        }
      );

      nixosModules.default = import ./nix/module.nix self;
    };
}
