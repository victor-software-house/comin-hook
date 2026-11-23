{ lib, buildGoModule }:

buildGoModule {
  pname = "comin-hook";
  version = "0.0.0";

  src = lib.fileset.toSource {
    root = ../.;
    fileset = lib.fileset.unions [
      ../go.mod
      ../go.sum
      ../cmd
      ../internal
    ];
  };

  vendorHash = "sha256-OpfnzJXE0Cr2Zg5o55vhIPfTsJp+wR2SsSHUub1Jpy4=";

  subPackages = [ "cmd/comin-hook" ];

  # A static binary without file system paths, like the CI build.
  env.CGO_ENABLED = "0";
  flags = [ "-trimpath" ];
  ldflags = [
    "-s"
    "-w"
  ];

  # The tests run in the check phase.
  doCheck = true;

  meta = {
    description = "Trigger a comin fetch from CI with an OIDC token";
    mainProgram = "comin-hook";
    platforms = [
      "x86_64-linux"
      "aarch64-linux"
    ];
  };
}
