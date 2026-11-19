{
  description = "providermux — an OpenAI-compatible gateway aggregating free AI providers";

  # The project's own flake. It exists so that this repository can be built and
  # entered without a system configuration: `nix build .` for the binary,
  # `nix develop .` for the toolchain the package uses. A NixOS configuration
  # that consumes providermux does not have to use it — it can pin a commit and
  # import nix/package.nix directly — but this is what makes a flake input
  # (`inputs.providermux.url = "github:kyomufs/providermux"`) possible at all.
  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { self, nixpkgs }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" "x86_64-darwin" "aarch64-darwin" ];
      forAll = nixpkgs.lib.genAttrs systems;

      # The version the binary reports. A checkout that is not a clean commit
      # says so, rather than claiming a release it does not correspond to.
      version = self.shortRev or self.dirtyShortRev or "dev";
    in {
      packages = forAll (system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
          # `src = self` is the git-tracked tree only: an untracked scratch file
          # never reaches the build, so a local build matches what CI sees.
          providermux = pkgs.callPackage ./nix/package.nix {
            src = self;
            inherit version;
          };
        in {
          default = providermux;
          inherit providermux;
        });

      devShells = forAll (system:
        let pkgs = nixpkgs.legacyPackages.${system};
        in {
          default = pkgs.mkShell {
            packages = [ pkgs.go pkgs.gopls pkgs.gotools pkgs.golangci-lint ];
            # The build's own invariant, in the shell too: cgo stays off, so a
            # binary built here behaves like the packaged one.
            env.CGO_ENABLED = "0";
          };
        });

      # The module as a flake output: it imports the same nix/module.nix a
      # vendored setup would, and supplies the package built from THIS checkout —
      # so the binary reports the commit it came from instead of "dev". The
      # value is a default, so a module user can still point the service at a
      # package of their own.
      nixosModules.default = { pkgs, ... }: {
        imports = [ ./nix/module.nix ];
        services.providermux.package = nixpkgs.lib.mkDefault
          self.packages.${pkgs.stdenv.hostPlatform.system}.default;
      };
      nixosModules.providermux = self.nixosModules.default;

      formatter = forAll (system: nixpkgs.legacyPackages.${system}.nixpkgs-fmt);
    };
}
