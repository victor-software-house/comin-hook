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
        in
        {
          module = nixpkgs.legacyPackages.${system}.writeText "comin-hook-module-check" (
            builtins.unsafeDiscardStringContext host.config.system.build.toplevel.drvPath
          );
        }
      );

      nixosModules.default = import ./nix/module.nix self;
    };
}
