# providermux

A gateway that puts several free AI providers behind one OpenAI-compatible
endpoint. One address for every client, one place where credentials, key
rotation and egress are decided, and a web panel for the settings you actually
change.

```
client ──▶ providermux ──▶ provider
           (request shape, keys, transport)
```

Each provider is spoken to the way its own client speaks to it. Adding one is a
section in `config.json`, not a patch to the code. The gateway is a single static
binary with a built-in terminal monitor, and it can tell you what it is doing:
which credential is in play, whether keys are rotating, which address the traffic
left from.

## Contents

- [Install](#install)
- [Quick start](#quick-start)
- [Configure](#configure)
- [Run as a service](#run-as-a-service)
- [Commands](#commands)
- [Files and ports](#files-and-ports)
- [Debugging](#debugging)
- [Providers](#providers)
- [License](#license)

## Install

### NixOS

The repository is a flake: it exports the package, a development shell and a
NixOS module. The module provides the binary and a systemd user unit, and
nothing else — the configuration file stays yours, so a `switch` never touches
your settings.

```nix
# flake.nix
{
  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
  inputs.providermux.url = "github:kyomufs/providermux";

  outputs = { self, nixpkgs, providermux, ... }: {
    nixosConfigurations.myhost = nixpkgs.lib.nixosSystem {
      system = "x86_64-linux";
      modules = [
        ./configuration.nix
        providermux.nixosModules.default
      ];
    };
  };
}
```

```nix
# configuration.nix
{ ... }:

{
  services.providermux.enable = true;
}
```

```sh
sudo nixos-rebuild switch --flake .#myhost
```

`nix flake update providermux` moves the binary and the module together. The
binary lands in `environment.systemPackages` and a user unit is installed; it
starts with your session. To have it start at boot without logging in, enable
lingering for the user:

```nix
users.users.myuser.linger = true;
```

If you do not want the module, install the flake directly:

```sh
nix profile install github:kyomufs/providermux
```

### Any other distribution

The binary is static: no C library versions, no runtime dependencies.

```sh
git clone https://github.com/kyomufs/providermux
cd providermux
./install.sh              # build and install into ~/.local/bin
./install.sh --unit       # also write the systemd user unit
```

```sh
PREFIX=/opt/providermux ./install.sh   # a different prefix, still no sudo
./install.sh --no-build                # install an existing ./providermux
```

`install.sh` deliberately does not touch `/usr`, does not write into a Nix
profile, refuses to install over a binary that Nix owns, and never edits an
existing `config.json`.

Without a Nix install and without the script, the whole thing is two commands:

```sh
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o ~/.local/bin/providermux ./cmd/providermux
providermux systemd --install      # writes the unit, prints the next two commands
```

`CGO_ENABLED=0` is what makes the result static, and it is why the unit needs
neither `LD_LIBRARY_PATH` nor a wrapper.

### From a checkout

```sh
nix build .            # ./result/bin/providermux
nix develop .          # go, gopls, golangci-lint, CGO off
```

## Quick start

A gateway with no provider refuses to start — it would have nothing to serve.
So the first run is a configuration file:

```sh
mkdir -p ~/.config/providermux
cat > ~/.config/providermux/config.json <<'JSON'
{
  "providers": { "opencode": { "type": "opencode" } }
}
JSON

providermux up
```

```sh
curl http://127.0.0.1:8787/v1/models
```

That is a loopback gateway on `127.0.0.1:8787`, presenting no credential to the
provider and spending the free tier's allowance against your address.

The web panel is the shortest path to everything else:

```
http://127.0.0.1:8787/
```

Point any OpenAI client at it:

```sh
export OPENAI_BASE_URL=http://127.0.0.1:8787/v1
export OPENAI_API_KEY=anything     # local mode checks no token
```

## Configure

Three layers, each overriding the one below:

```
flags  →  environment variables  →  config.json
```

Every setting has all three. `providermux axes` prints the complete table —
value type, default, and the environment variable — generated from the same
source the validation uses, so it cannot go stale.

### Changing a value

```sh
# Adopt a value: writes config.json, survives a restart.
providermux cred anonymous
providermux rotate off
providermux family v4

# Try a value: writes nothing, applies to this run.
providermux up --rotate off --family v4
```

The one-axis commands write the value even when the result could not run, and
that is deliberate: a configuration is allowed to be incomplete while it is being
assembled. `credential account` on a keyless configuration is the case to know
about — it is accepted, it is written, and the daemon refuses to start until a
key appears:

```
error: config: provider "opencode": credential "account" requires a key, and none
is configured: set api_key, api_key_env, or settings.keys — or switch the
credential to "anonymous"
```

### The axes that matter

| Axis | Values | Default | What it decides |
|---|---|---|---|
| `mode` | `local` \| `server` | `local` | loopback without a token, or a reachable listener that requires one |
| `listen` | address | `127.0.0.1:8787` | where the gateway listens |
| `token` | string | — | the token for `server` (env only, in practice) |
| `credential` | `anonymous` \| `account` | `anonymous` | whether a key is presented to the provider |
| `key_rotation` | `on` \| `off` | `on` | spend another key on a refusal that names the key |
| `family` | `auto` \| `v4` \| `v6` | `auto` | the address family the traffic leaves from |
| `transport` | `direct` \| `proxy` | `direct` | how traffic reaches the provider |
| `upstream` | address | — | default address for providers that do not name their own |
| `egress_url` | URL | the CDN trace document | where the gateway asks which address it leaves from |

The timeouts (`first_event_timeout`, `idle_timeout`, `reasoning_idle_timeout`,
default `30s`, `120s`, `300s`) decide how long silence is tolerated before a
stream is abandoned. `provider.*` sets the same axes per provider.

### Repeating a request

By default one client request is one provider request. `retry` raises that
budget, and a repeat is only possible where the client cannot answer the failure
itself — a connection that died before the provider answered, or an exhausted
free-lane allowance.

```json
{
  "retry": { "attempts": 3, "delay": "500ms" },
  "rotation": {
    "controller": "http://127.0.0.1:9091",
    "group": "egress",
    "probe_url": "https://cp.cloudflare.com/cdn-cgi/trace",
    "probe_count": 6,
    "transport": "proxy"
  },
  "transport": "proxy",
  "transport_settings": { "url": "http://127.0.0.1:7891" }
}
```

`attempts` is the TOTAL number of upstream requests (`1` disables retrying),
`delay` grows linearly: the second attempt waits one delay, the third two.

The wait is not decoration and it is not only for overloaded edges: the free lane
counts a **burst** as well. One address refused a dozen requests in two minutes
and answered the same address again a minute later, so a new exit alone does not
answer every refusal. Both levers run on a quota refusal — the wait, then the
move — and the wait belongs to the client: it ends the moment the client does.

The free lane counts its allowance **per source address** as well, so a repeat
from the same address cannot succeed while that address is spent. `rotation` names the proxy controller whose
selector group is the exit; on a quota refusal the gateway asks it for a live
node (`probe_count` candidates, each probed through the controller), switches
the group, re-reads its own address, and repeats. Without `rotation` a quota
refusal reaches the client verbatim — the gateway does not repeat a counter that
did not move.

`rotation.transport` names which transport the selector stands behind, and it
defaults to the deployment's own. It matters when transports are split between
providers — one provider on the host's routing, another through the proxy: a
refusal from a provider that does not travel the group's path cannot be answered
by moving it, so that refusal is passed on verbatim and no rotation is spent on
it. Each provider also observes the address of its OWN path, since the address
describes the path and not the host.

The address is read from the trace document at `egress_url`, through the same
path the requests take, so a rotation is visible as a change of that value. The
default endpoint is `https://cp.cloudflare.com/cdn-cgi/trace` and not the
service's own literal: measured on this uplink, `https://1.1.1.1/cdn-cgi/trace`
stopped answering while the document behind the name kept answering, and an
address that cannot be observed is a rotation that cannot decide.

A rotation answers ONE refusal; it is not a new home. Once an answer arrives on
the rotated exit, the gateway puts the group back on its first member — the
profile keeps the direct exit there — so a spent node is not kept for hours after
the address it replaced has recovered. That return runs after the client already
has its answer, and a controller that refuses it only costs a log line.

A key refusal, a malformed request and a content refusal are never repeated: no
other address changes those answers.

### Providers

```json
{
  "providers": {
    "opencode": { "type": "opencode" },
    "antigravity": {
      "type": "antigravity",
      "credential": "account",
      "api_key_env": "PROVIDERMUX_ANTIGRAVITY_KEY",
      "settings": { "project": "you-project" }
    }
  }
}
```

| Field | What it is |
|---|---|
| `type` | `opencode`, `antigravity`, or `stub` for a local end-to-end check |
| `credential` | overrides the global credential for this provider |
| `key_rotation` | overrides the global rotation for this provider |
| `api_key` | the key, inline. Fine for a scratch setup, readable by anyone who can read the file |
| `api_key_env` | the name of the environment variable that holds the key — the better place for a secret |
| `settings.keys` | additional keys, which is where the panel writes what you add there |
| `settings` | provider-specific values; each provider documents its own |

Which service accepts what is documented next to the code:
[`docs/providers/opencode_zen.md`](docs/providers/opencode_zen.md) and
[`docs/providers/antigravity.md`](docs/providers/antigravity.md).

### Keys

A key can arrive three ways, and all three survive a restart:

| Route | Survives a restart | Where the secret lives |
|---|---|---|
| the panel (`http://127.0.0.1:8787/`) or `POST /_control/keys/add` | yes — written into `settings.keys` | `config.json` |
| `"api_key": "sk-..."` in the section | yes | `config.json` |
| `"api_key_env": "NAME"` plus the variable in the unit's environment | yes | the environment file, never in `config.json` |

A key added from the panel is written into the provider's `settings.keys`; a key
that came from the environment is never copied into the file.

`key_rotation` decides what happens on a refusal that names the key: `on` takes
another key from the pool, `off` passes the refusal to the client. Under
`credential = "anonymous"` it is ignored — no key is presented there, so there is
nothing to replace.

### Server mode

`local` is loopback-only and checks no token. Switching to `server` makes the
token mandatory, because a tokenless listener on a reachable address spends your
quota on behalf of whoever finds the port:

```json
{
  "mode": "server",
  "listen": "0.0.0.0:8787",
  "token": "a long random string",
  "providers": { "opencode": { "type": "opencode" } }
}
```

Clients then send `Authorization: Bearer <token>`, on `/v1/*` and `/_control/*`
alike. Prefer a token in the environment over the file:

```
PROVIDERMUX_TOKEN=<token>
```

## Run as a service

On NixOS the module does this for you. Everywhere else:

```sh
providermux systemd --install       # writes ~/.config/systemd/user/providermux.service
systemctl --user daemon-reload
systemctl --user enable --now providermux
```

Secrets go into a `EnvironmentFile`, read by systemd at start time rather than
written into the unit:

```ini
[Service]
EnvironmentFile=%h/.config/providermux/env
```

```
PROVIDERMUX_TOKEN=<token>
PROVIDERMUX_ANTIGRAVITY_KEY=<key>
```

The gateway is a user unit because it is a per-user gateway: loopback, state in
the home, no privileges.

## Commands

| Command | What it does |
|---|---|
| `up [flags]` | start the daemon |
| `monitor` | the built-in terminal monitor |
| `status [--json]` | mode, address, providers, counters |
| `stop` | stop the daemon |
| `models [--json]` | the model catalog the gateway serves |
| `stats [--json]` | counters by day |
| `keys [--json]` | the key pool |
| `logs [-n N]` | the log |
| `data [clear]` | what lies on disk, and clearing it |
| `config [--json]` | the effective configuration, with the source of every value |
| `axes [--json]` | every setting, its values and its defaults |
| `systemd [--install]` | print the user unit, or write it |
| `version` | the version |
| `mode` \| `cred` \| `rotate` \| `family` | one axis: read it, or write it |

`providermux axes` is the reference for startup flags too: every axis lists the
flag and the environment variable next to it. `--config`, `--state`,
`--print-config` and `--detach` are the ones that concern the run rather than a
setting.

## Files and ports

| Path | What |
|---|---|
| `~/.config/providermux/config.json` | the configuration |
| `~/.local/state/providermux/state.json` | counters and per-address history |
| `~/.local/state/providermux/app.log` | the log |

All three follow `XDG_CONFIG_HOME` and `XDG_STATE_HOME`.

The gateway serves `/v1/*` for clients and `/_control/*` for the panel and the
CLI, on `127.0.0.1:8787` by default — loopback only.

`providermux data` prints the files with their sizes; `providermux data clear`
empties the log or the counters. It never touches the configuration: deleting
the file that describes the deployment is not maintenance.

## Debugging

### See the merged result without starting anything

```sh
$ providermux up --rotate off --family v4 --print-config
family                  v4                           (flag)
key_rotation            off                          (flag)
credential              anonymous                    (default)
...
file:      /home/you/.config/providermux/config.json
state:     /home/you/.local/state/providermux/state.json
```

`providermux config` shows the same for the values in force, with their source.

### A second daemon on a spare port

The system daemon keeps serving `:8787`; the experiment runs beside it with its
own configuration, its own state and its own port:

```sh
mkdir -p ~/pmux-debug
cat > ~/pmux-debug/config.json <<'JSON'
{ "credential": "anonymous", "providers": { "opencode": { "type": "opencode" } } }
JSON

providermux up --config ~/pmux-debug/config.json --state ~/pmux-debug/state.json \
  --listen 127.0.0.1:8799 &
curl http://127.0.0.1:8799/v1/models
providermux stop --config ~/pmux-debug/config.json --state ~/pmux-debug/state.json
```

`--config` and `--state` go together. With `--config` alone the second daemon
reads the experiment's settings but writes the system daemon's `state.json`, and
the two then disagree about what happened.

`--detach` is not a shell detach: it starts the daemon and keeps holding the
terminal for as long as the daemon runs. Backgrounding with `&` is what gives the
prompt back.

### Override the unit for one run

```sh
systemctl --user edit providermux
```

```ini
[Service]
ExecStart=
ExecStart=/run/current-system/sw/bin/providermux up --family v4 --rotate off
```

Then `systemctl --user daemon-reload && systemctl --user restart providermux`.
Removing the drop-in and restarting undoes it; nothing in the declaration changed.

### A local build against the real service

```sh
CGO_ENABLED=0 go build -trimpath -o /tmp/providermux ./cmd/providermux
```

Use it on a spare port first (above). To swap the service's binary on NixOS,
point the module at your own package — the unit, the configuration and the state
stay exactly as they are:

```nix
services.providermux.package = pkgs.callPackage /path/to/providermux/nix/package.nix { };
```

`nix/package.nix` takes `src` and `version`, so the same recipe builds a working
tree, a tag or a pinned commit:

```nix
pkgs.callPackage ./nix/package.nix {
  src = pkgs.fetchFromGitHub {
    owner = "kyomufs";
    repo = "providermux";
    rev = "v1.7.2";
    hash = "...";
  };
  version = "1.7.2";
}
```

### Tests

```sh
go test ./...
nix build .            # runs the same suite hermetically, with vendored deps
```

## Providers

| Type | Credential | Notes |
|---|---|---|
| `opencode` | anonymous or account | the free lane identifies the caller by egress address; a key belongs to the account lane |
| `antigravity` | account | one Google account per section; a pool of them is multi-account failover |
| `stub` | — | answers locally; for end-to-end checks of the gateway itself |

## License

MIT
