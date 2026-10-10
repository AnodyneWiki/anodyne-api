{
  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";
    flake-utils.url = "github:numtide/flake-utils";
    systems.url = "github:nix-systems/triplet";
    flake-utils.inputs.systems.follows = "systems";
  };
  outputs =
    {
      self,
      nixpkgs,
      flake-utils,
      ...
    }:
    flake-utils.lib.eachDefaultSystem (
      system:
      let
        pkgs = import nixpkgs { inherit system; };
        anodyne-api = pkgs.buildGoModule {
          pname = "anodyne-api";
          version = "0.1.0";
          src = self.outPath;
          vendorHash = "sha256-wpv6Xs4d3l7SJq9mXsFTjnHzOF8oWANAl5Vab5gyVmE=";
        };
      in
      {
        formatter = pkgs.nixfmt-tree;
        packages = {
          default = anodyne-api;
          anodyne-api = anodyne-api;
        };
        devShells.default = pkgs.mkShell {
          packages = [
            anodyne-api
            pkgs.go
            pkgs.gopls
          ];
        };
      }
    )
    // {
      hydraJobs = {
        inherit (self) packages;
      };
    };
}
