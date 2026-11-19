# providermux — the NixOS module: the binary and the user unit, nothing else.
#
# WHAT THIS MODULE OWNS. The package on PATH, one systemd user unit, and the
# environment file that unit loads. That is the whole surface.
#
# WHAT IT DOES NOT OWN: ~/.config/providermux/config.json. The file belongs to
# the operator, because its values change far more often than the system does
# and because three things write it: the daemon's CLI, the daemon's web panel,
# and the daemon itself when it learns something about a provider.
#
# A module that rendered the file from its options and rewrote it on every
# activation would throw those writes away at the next switch, turn every
# setting into a rebuild, and put API keys into /nix/store. So the declaration
# stops at the process boundary and the daemon layers its configuration itself:
#
#   flags            (highest, one run)
#   environment      (PROVIDERMUX_*, one unit)
#   config.json      (base layer, the file the operator edits)
#
# This is also why ExecStart carries no --listen: a flag there would outrank the
# file forever, and changing the port would need a rebuild. The same reason
# keeps the timeouts, the axes and the provider sections out of the options.
#
# ADDING A PROVIDER needs no rebuild and no module edit: a section in
# config.json is enough. A provider's model catalog is never declared — it lives
# with the service and is learned by asking.
#
# SECRETS. The unit reads ONE environment file, `envFile`: PROVIDERMUX_TOKEN for
# server mode (the legacy FREE_AI_ROUTER_TOKEN is accepted too), and whatever
# each provider's api_key_env names. The file is read at start time, so its
# values never reach /nix/store; a name in config.json is not a secret, a value
# in the store would be. Keep the file 0600 with dotenv lines:
#
#   PROVIDERMUX_TOKEN=<token>
#   PROVIDERMUX_ANTIGRAVITY_KEY=<key>
#
# A sops secret fits the same slot: point envFile at the decrypted path and
# declare the secret in the usual way. The module does not depend on sops-nix.
#
# MODE. `local` listens on loopback and checks no token; `server` listens on a
# reachable address and refuses to start without one. Both live in config.json,
# not here: the address is a property of the host and of the run, and the file
# already records it where the panel can change it.
#
# THE UNIT IS A USER UNIT. The gateway is per-user: it listens on loopback, its
# state belongs in the user's home, and it needs no privileges at all. To have
# it up before login, enable lingering for the user:
#
#   users.users.<name>.linger = true;
{ config, lib, pkgs, ... }:
let
  cfg = config.services.providermux;
in
{
  options.services.providermux = {
    enable = lib.mkEnableOption "the providermux gateway for free AI providers";

    package = lib.mkOption {
      type = lib.types.package;
      default = pkgs.callPackage ./package.nix { };
      defaultText = lib.literalExpression "pkgs.callPackage ./package.nix { }";
      description = ''
        The gateway package to run.

        The default builds the source tree this module sits in, which is what a
        vendored module or a `nix build` wants. A flake input supplies its own
        package (with the real commit in `providermux version`) through
        `nixosModules.default`, and a local build can be tested against the live
        service by pointing this at it: the unit, the configuration and the
        state stay exactly as they are.
      '';
    };

    envFile = lib.mkOption {
      type = lib.types.str;
      default = "";
      example = "/home/you/.config/providermux/keys.env";
      description = ''
        Path to a dotenv file with the unit's secrets (`EnvironmentFile`):
        `PROVIDERMUX_TOKEN` for `mode = "server"`, and the variables the
        providers' `api_key_env` names.

        The value is a string, not a Nix path: a path literal would be copied
        into `/nix/store`, which is world-readable, and the secrets with it. The
        file itself is read when the daemon starts, so its contents stay out of
        the store.

        Empty leaves the unit without a token and without keys, which is the
        right answer for a loopback deployment that spends no account quota.
      '';
    };
  };

  config = lib.mkIf cfg.enable {
    # The binary goes into the system profile, so the CLI is on PATH from any
    # shell. There is deliberately no wrapper: the monitor is inside the binary.
    environment.systemPackages = [ cfg.package ];

    systemd.user.services.providermux = {
      description = "providermux gateway for free AI providers";
      wantedBy = [ "default.target" ];
      after = [ "network-online.target" ];
      wants = [ "network-online.target" ];

      # The XDG roots the daemon resolves its files from. %h rather than a
      # written-out home path: the unit is per-user, and it may run in a home
      # other than the first one declared.
      environment = {
        XDG_CONFIG_HOME = "%h/.config";
        XDG_STATE_HOME = "%h/.local/state";
      };

      serviceConfig = {
        # No --listen: the address belongs to the configuration file, and a flag
        # here would outrank it. Changing the address is providermux's business.
        ExecStart = "${cfg.package}/bin/providermux up";

        # Restart after a crash: the gateway is meant to be there.
        Restart = "on-failure";
        RestartSec = 5;

        # EnvironmentFile, not Environment=KEY=value: a value in Environment is
        # written into the unit file, which is a derivation — that is, into
        # /nix/store, exactly where a secret must not be.
        EnvironmentFile = lib.optional (cfg.envFile != "") cfg.envFile;

        # A private /tmp and a ban on new privileges: the daemon needs neither.
        # Cheap insurance, not a replacement for the access check.
        PrivateTmp = true;
        NoNewPrivileges = true;
      };
    };
  };
}
