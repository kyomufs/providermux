# providermux — the build recipe, as one derivation.
#
# ONE RECIPE, TWO ENTRY POINTS. The flake builds this file from a checkout
# (`nix build .`); a NixOS module builds it from a fetched source tree. Neither
# owns the source or the version — the caller passes `src` and `version` — so
# the same file describes a working tree, a release tag and a pinned commit
# without being edited three different ways.
#
# The only field that lives here is `vendorHash`: it belongs to the Go
# dependencies and changes when go.mod changes, not when the project's own code
# changes. When it moves, the failing build prints the replacement:
#
#   nix build . 2>&1 | grep 'got:'
{ lib
, buildGoModule
, src ? ../.
, version ? "dev"
}:

buildGoModule {
  pname = "providermux";
  inherit src version;

  # A static binary: the unit needs neither LD_LIBRARY_PATH nor a wrapper, and
  # the build works on a machine with no C toolchain at all.
  env.CGO_ENABLED = "0";

  # The version goes into the binary. Without it `version` answers "unknown",
  # and a bug report becomes a guessing game about what exactly is running.
  ldflags = [ "-s" "-w" "-X" "main.buildVersion=${version}" ];

  # Bubble Tea and its transitive dependencies, vendored. The hash is mandatory:
  # without it the build would reach the network, which a Nix build must not do.
  vendorHash = "sha256-SD4lb6oox77QZzzTEix7eEFUO+uM4oYwZrQR+vaoGC8=";

  # The test suite is hermetic — no test reaches the network — so it runs here,
  # and a broken tree fails at build time rather than on the machine that
  # installed it.
  doCheck = true;

  meta = {
    description = "OpenAI-compatible gateway aggregating free AI providers";
    homepage = "https://github.com/kyomufs/providermux";
    license = lib.licenses.mit;
    mainProgram = "providermux";
    platforms = lib.platforms.unix;
  };
}
