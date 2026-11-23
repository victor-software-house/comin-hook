{
  description = "Trigger a comin fetch from CI with an OIDC token";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-26.05";

  outputs =
    { self, nixpkgs }:
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

      nixosModules.default = import ./nix/module.nix self;
    };
}
