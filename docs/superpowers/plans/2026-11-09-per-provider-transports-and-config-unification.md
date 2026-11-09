# Per-Provider Transports and Configuration Unification Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Enable per-provider transport routing (allowing Opencode Zen and Antigravity to use separate egress transports/proxies) and standardize secret/key configuration across providers.

**Architecture:** Extend `ProviderConfig` in `providermux` to support optional per-provider `transport` and `transport_settings` overriding the global transport. Update the gateway handler to dispatch requests through provider-specific round-trippers. Mirror the options in the NixOS module and standardize secret storage via `EnvironmentFile` (`keys.env`) and `apiKeyEnv`.

**Tech Stack:** Go 1.24+, Nix / NixOS 26.05 flake, systemd user services.

**Spec:** `docs/superpowers/plans/2026-11-09-per-provider-transports-and-config-unification.md`

## Global Constraints

- Go static binary build (`CGO_ENABLED=0`).
- No secrets in `/nix/store` or git trees (secrets reside exclusively in `keys.env` mode 0600).
- Backward compatibility: providers omitting `transport` seamlessly inherit the daemon's global `transport` and `transport_settings`.

## Review Focus

1. **Inheritance fallback**: A provider with no `transport` set in `config.json` must use the global transport, preserving existing behavior.
2. **Missing transport error handling**: Specifying an unknown or invalid transport for a provider must be caught at configuration validation (`Validate()`).
3. **Transport lifecycle**: Each instantiated transport must be properly started (`Start()`) and stopped (`Stop()`) on daemon lifecycle events without leaking sockets or background goroutines.
4. **Environment variable secret expansion**: Environment placeholders (`${NAME}`) in provider settings and `apiKeyEnv` must expand correctly regardless of whether the transport is per-provider or global.
5. **NixOS module drift**: Options declared in `nixos-config/modules/services/providermux.nix` must keep parity with standalone `nix/module.nix` in the repository.

---

### Task 1: Add Per-Provider Transport Configuration in `internal/config`

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go`

**Interfaces:**
- Consumes: `config.ProviderConfig`, `config.Config`
- Produces: `(p ProviderConfig) EffectiveTransport(global string) string`, `(p ProviderConfig) EffectiveTransportConfig(global map[string]any) map[string]any`

- [ ] **Step 1: Write failing tests for per-provider transport resolution**

Add tests to `internal/config/config_test.go`:
```go
func TestProviderEffectiveTransportFallback(t *testing.T) {
	p := ProviderConfig{}
	if got := p.EffectiveTransport("direct"); got != "direct" {
		t.Fatalf("EffectiveTransport = %q, want global 'direct'", got)
	}
}

func TestProviderEffectiveTransportOverride(t *testing.T) {
	p := ProviderConfig{Transport: "custom"}
	if got := p.EffectiveTransport("direct"); got != "custom" {
		t.Fatalf("EffectiveTransport = %q, want 'custom'", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /home/kyomufs/Projects/providermux && CGO_ENABLED=0 go test ./internal/config -run TestProviderEffectiveTransport`
Expected: FAIL (field `Transport` undefined on `ProviderConfig`)

- [ ] **Step 3: Implement `Transport` and `TransportConfig` fields on `ProviderConfig`**

In `internal/config/config.go`:
1. Add to `ProviderConfig`:
   ```go
   Transport string `json:"transport,omitempty"`
   TransportConfig map[string]any `json:"transport_settings,omitempty"`
   ```
2. Add helper methods:
   ```go
   func (p ProviderConfig) EffectiveTransport(global string) string {
       if p.Transport != "" {
           return p.Transport
       }
       return global
   }

   func (p ProviderConfig) EffectiveTransportConfig(global map[string]any) map[string]any {
       if p.TransportConfig != nil {
           return p.TransportConfig
       }
       return global
   }
   ```
3. Update `c.Validate()` to validate `p.Transport` when present against `transport.Registry` or available transports.

- [ ] **Step 4: Run test to verify it passes**

Run: `cd /home/kyomufs/Projects/providermux && CGO_ENABLED=0 go test ./internal/config/...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/config/
git commit -m "feat(config): add per-provider transport and settings"
```

---

### Task 2: Implement Per-Provider Transport Dispatch in Gateway and Daemon

**Files:**
- Modify: `internal/gateway/gateway.go`
- Modify: `cmd/providermux/up.go`
- Test: `internal/gateway/gateway_test.go`

**Interfaces:**
- Consumes: `gateway.Config.Transport`, `provider.ID`
- Produces: `gateway.Config.TransportFor func(id provider.ID) RoundTripperFactory`

- [ ] **Step 1: Write failing test for per-provider transport routing in gateway**

In `internal/gateway/gateway_test.go`:
```go
func TestGatewayDispatchesPerProviderTransport(t *testing.T) {
	// Set up two mock transports and assert that Provider A routes to Transport A
	// and Provider B routes to Transport B.
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /home/kyomufs/Projects/providermux && CGO_ENABLED=0 go test ./internal/gateway -run TestGatewayDispatchesPerProviderTransport`
Expected: FAIL

- [ ] **Step 3: Implement `TransportFor` in `gateway.Handler` and daemon wiring**

1. In `internal/gateway/gateway.go`:
   - Add `TransportFor func(id provider.ID) RoundTripperFactory` to `gateway.Config` and `gateway.Handler`.
   - In `do(req *http.Request, p provider.Provider, model string)`:
     ```go
     if h.TransportFor != nil {
         if tf := h.TransportFor(p.ID()); tf != nil {
             if rt, err := tf.RoundTripper(); err == nil && rt != nil {
                 client.Transport = rt
             }
         }
     } else if h.Transport != nil {
         if rt, err := h.Transport.RoundTripper(); err == nil && rt != nil {
             client.Transport = rt
         }
     }
     ```
2. In `cmd/providermux/up.go`:
   - For each configured provider, build its designated `transport.Transport` instance using `pc.EffectiveTransport(cfg.Transport.Val)` and `pc.EffectiveTransportConfig(cfg.TransportConfig)`.
   - Start all instantiated transports on daemon startup (`Start(ctx)`) and stop on shutdown (`Stop()`).
   - Provide `TransportFor` closure to `gateway.New(...)`.

- [ ] **Step 4: Run gateway and cmd tests to verify they pass**

Run: `cd /home/kyomufs/Projects/providermux && CGO_ENABLED=0 go test ./...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/gateway/ cmd/providermux/
git commit -m "feat(gateway): support per-provider transport resolution"
```

---

### Task 3: Add Per-Provider Transport Options to NixOS Modules

**Files:**
- Modify: `Projects/providermux/nix/module.nix`
- Modify: `~/.config/nixos-config/modules/services/providermux.nix`

**Interfaces:**
- Consumes: `services.providermux.providers.<name>.transport`, `services.providermux.providers.<name>.transportSettings`
- Produces: `config.json` containing `providers.<name>.transport` and `providers.<name>.transport_settings`

- [ ] **Step 1: Add `transport` and `transportSettings` to providerSubmodule**

In both `module.nix` files:
```nix
transport = myLib.withDesc "Transport identifier for this provider (defaults to global transport)" (
  lib.mkOption {
    type = lib.types.nullOr lib.types.str;
    default = null;
    example = "direct";
  }
);

transportSettings = myLib.withDesc "Settings for the provider transport" (
  lib.mkOption {
    type = lib.types.attrsOf lib.types.anything;
    default = { };
    example = { family = "v4"; };
  }
);
```

- [ ] **Step 2: Update `configFile` generation in `providermux.nix`**

In `lib.filterAttrs` under `providers = lib.mapAttrs`:
```nix
transport = p.transport;
transport_settings = p.transportSettings;
```

- [ ] **Step 3: Verify NixOS evaluation**

Run: `nix eval /home/kyomufs/.config/nixos-config#nixosConfigurations.asusfx506hc.config.services.providermux.providers.opencode`
Expected: evaluation succeeds without errors.

- [ ] **Step 4: Commit module changes**

```bash
git -C /home/kyomufs/Projects/providermux commit -am "feat(nix): expose provider transport options in module"
git -C /home/kyomufs/.config/nixos-config commit -am "feat(services/providermux): expose provider transport options"
```

---

### Task 4: Unify Key and Secret Management in NixOS Host Configuration

**Files:**
- Modify: `~/.config/nixos-config/hosts/asusfx506hc/default.nix`
- Modify: `~/.config/providermux/keys.env`

**Goal:** Eliminate plaintext secret leaks in `/nix/store` and bring `opencode` to the exact same declarative pattern as `antigravity`.

- [ ] **Step 1: Move OpenCode key to `keys.env`**

In `~/.config/providermux/keys.env` (permissions 0600):
```env
PROVIDERMUX_ANTIGRAVITY_KEY=...
PROVIDERMUX_OPENCODE_KEY=oc_sk_...
```

- [ ] **Step 2: Update `hosts/asusfx506hc/default.nix` to use `apiKeyEnv`**

Replace:
```nix
      providers.opencode = {
        type = "opencode";
        settings.keys = [
          "oc_sk_e27acfdcef63_ZSNwPc_MLh0_ChIOTKjox7GN6WC33Ia2"
        ];
      };
```
With uniform configuration:
```nix
      providers.opencode = {
        type = "opencode";
        credential = "anonymous"; # или "account" при наличии баланса Console
        apiKeyEnv = "PROVIDERMUX_OPENCODE_KEY";
      };
```

- [ ] **Step 3: Verify and switch**

Run:
```bash
nh os switch /home/kyomufs/.config/nixos-config
```
Verify `~/.config/providermux/config.json` contains no plaintext secrets and specifies clean env references.

- [ ] **Step 4: Commit host configuration**

```bash
git -C /home/kyomufs/.config/nixos-config commit -am "refactor(hosts/asusfx506hc): unify providermux keys via apiKeyEnv"
```
