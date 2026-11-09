# Rename Project and Ecosystem to ProviderMux Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Rename `free-ai-router` across the entire project ecosystem (Go core daemon, DSH plugin, NixOS module and package, local configs, and GitHub repositories) to `providermux`.

**Architecture:** Systematic rename across four decoupled tiers:
1. Go daemon repository: module path (`providermux`), CLI directory (`cmd/providermux`), internal imports, XDG `AppName = "providermux"`, env variables (`PROVIDERMUX_*` with `FREE_AI_ROUTER_*` fallback), and GitHub repo rename (`kyomufs/free-ai-router` -> `kyomufs/providermux`).
2. DSH plugin repository: package name (`providermux-dshplugin`), env vars (`PROVIDERMUX_BASE`, `PROVIDERMUX_TOKEN`), GitHub repo rename (`kyomufs/free-ai-router-dshplugin` -> `kyomufs/providermux-dshplugin`), and installed profile node_modules sync.
3. Filesystem and local runtime: directory move `~/Projects/free-ai-router` -> `~/Projects/providermux`, `~/Projects/free-ai-router-dshplugin` -> `~/Projects/providermux-dshplugin`, config/state dir migration, and binary update to `~/.local/bin/providermux`.
4. NixOS flake (`~/.config/nixos-config`): module `modules/services/providermux.nix`, package `pkgs/providermux/package.nix`, host options `services.providermux.*`, verified via `./tools/ch/ch`.

**Tech Stack:** Go 1.24, Node.js / CommonJS, Nix / NixOS 26.05, GitHub CLI (`gh`).

## Global Constraints

- Go binary build requires `CGO_ENABLED=0` and reproducible `-ldflags "-X main.buildVersion=$(git rev-parse --short HEAD)"`.
- Config loader must support new `PROVIDERMUX_*` env variables while retaining fallback to `FREE_AI_ROUTER_*` so existing environments do not abruptly break.
- GitHub push rules: zero secrets in commits; push through authenticated `gh` or token with masked output.
- NixOS repository invariants: verification ladder `./tools/ch/ch` must be 100% green before any changes are committed or switched.

---

### Task 1: Rename Go Core Daemon (`free-ai-router` -> `providermux`)

**Files:**
- Modify: `go.mod` (module `free-ai-router` -> `providermux`)
- Move: `cmd/free-ai-router` -> `cmd/providermux`
- Modify: All Go source files with internal imports (`"free-ai-router/..."` -> `"providermux/..."`)
- Modify: `internal/config/config.go` (`AppName = "providermux"`, `Env* = "PROVIDERMUX_*"`, plus fallback to `FREE_AI_ROUTER_*`)
- Modify: `internal/config/config_test.go`, `cmd/providermux/main.go`, `cmd/providermux/flags.go`, `cmd/providermux/set.go`
- Modify: `nix/module.nix`, `nix/package.nix`, `install.sh`, `README.md`, `AGENTS.md`

- [ ] **Step 1: Update go.mod and internal package imports**
  Change `module free-ai-router` to `module providermux` in `go.mod`.
  Replace all `"free-ai-router/` imports with `"providermux/` across `*.go`.

- [ ] **Step 2: Rename `cmd/free-ai-router` to `cmd/providermux`**
  Move directory `cmd/free-ai-router` to `cmd/providermux`.
  Update CLI usage banners, binary name references, and help text.

- [ ] **Step 3: Update `AppName` and environment variables in `internal/config/config.go`**
  Change `AppName` to `"providermux"`.
  Define `EnvMode = "PROVIDERMUX_MODE"`, etc.
  In `applyEnv()`, check `PROVIDERMUX_*` first, falling back to `FREE_AI_ROUTER_*` if unset.

- [ ] **Step 4: Update documentation, nix expressions in repo, and tests**
  Update `nix/module.nix`, `nix/package.nix`, `README.md`, and `AGENTS.md`.
  Run formatting and test suite:
  `gofmt -l .` (must be empty)
  `CGO_ENABLED=0 go vet ./...`
  `CGO_ENABLED=0 go test ./... -count=1`

- [ ] **Step 5: Build binary and verify live**
  `CGO_ENABLED=0 go build -ldflags "-X main.buildVersion=$(git rev-parse --short HEAD)" -o ~/.local/bin/providermux ./cmd/providermux`
  Verify `~/.local/bin/providermux --version` and `~/.local/bin/providermux axes`.
  Create compatibility symlink `ln -sf ~/.local/bin/providermux ~/.local/bin/free-ai-router`.

- [ ] **Step 6: Commit changes to local git**
  `git add -A && git commit -m "feat: rename project to providermux"`

---

### Task 2: Rename GitHub Repositories and Local Project Directories

**Files / Operations:**
- Rename remote repository: `kyomufs/free-ai-router` -> `kyomufs/providermux` via `gh repo rename`
- Move local directory: `/home/kyomufs/Projects/free-ai-router` -> `/home/kyomufs/Projects/providermux`
- Rename remote repository: `kyomufs/free-ai-router-dshplugin` -> `kyomufs/providermux-dshplugin` via `gh repo rename`
- Move local directory: `/home/kyomufs/Projects/free-ai-router-dshplugin` -> `/home/kyomufs/Projects/providermux-dshplugin`

- [ ] **Step 1: Rename `free-ai-router` repo on GitHub**
  `gh repo rename providermux --repo kyomufs/free-ai-router --yes`
  Update git remote URL:
  `git remote set-url origin git@github.com:kyomufs/providermux.git`
  Push latest commits:
  `git push origin main`

- [ ] **Step 2: Rename directory `/home/kyomufs/Projects/free-ai-router`**
  Move `/home/kyomufs/Projects/free-ai-router` to `/home/kyomufs/Projects/providermux`.

- [ ] **Step 3: Rename `free-ai-router-dshplugin` repo on GitHub**
  `gh repo rename providermux-dshplugin --repo kyomufs/free-ai-router-dshplugin --yes`

- [ ] **Step 4: Rename directory `/home/kyomufs/Projects/free-ai-router-dshplugin`**
  Move `/home/kyomufs/Projects/free-ai-router-dshplugin` to `/home/kyomufs/Projects/providermux-dshplugin`.

---

### Task 3: Update DSH Plugin (`providermux-dshplugin`)

**Files:**
- Modify: `package.json` (name `providermux-dshplugin`, repository url `kyomufs/providermux-dshplugin`)
- Modify: `cordis.patch.yml` (id `providermux-dshplugin`, name `providermux-dshplugin`)
- Modify: `lib/daemon.cjs` (`PROVIDERMUX_BASE` with `FREE_AI_ROUTER_BASE` fallback, `PROVIDERMUX_TOKEN` with `FREE_AI_ROUTER_TOKEN` fallback, user-agent name)
- Modify: `lib/index.js`, `lib/adapter.cjs`, `README.md`, `AGENTS.md`, `test/smoke.cjs`
- Sync: `/home/kyomufs/.dsh/profiles/web/node_modules/providermux-dshplugin`
- Update: `/home/kyomufs/.dsh/profiles/web/package.json` and `cordis.patch.yml`

- [ ] **Step 1: Update plugin source code**
  Update `name`, `repository`, env vars (`PROVIDERMUX_BASE`, `PROVIDERMUX_TOKEN`), and docstrings.
  Update `test/smoke.cjs` to reflect new names.

- [ ] **Step 2: Run verification cycle**
  `for f in lib/*.cjs lib/*.js test/*.cjs; do node --check "$f" || echo "FAIL $f"; done`
  `node test/smoke.cjs` (must pass all tests)

- [ ] **Step 3: Commit and push plugin repo**
  `git add -A && git commit -m "feat: rename plugin to providermux-dshplugin"`
  `git push origin main`

- [ ] **Step 4: Update installed plugin in DSH web profile**
  Copy or reinstall into `/home/kyomufs/.dsh/profiles/web/node_modules/providermux-dshplugin`.
  Update `/home/kyomufs/.dsh/profiles/web/package.json` entry.
  Remove old node_modules directory `free-ai-router-dshplugin` or leave symlink if needed.

---

### Task 4: Update NixOS Configuration (`~/.config/nixos-config`)

**Files:**
- Move & Modify: `modules/services/free-ai-router.nix` -> `modules/services/providermux.nix`
- Move & Modify: `pkgs/free-ai-router/package.nix` -> `pkgs/providermux/package.nix`
- Modify: `overlays/packages.nix` (`providermux = final.callPackage ../pkgs/providermux/package.nix { };`)
- Modify: `hosts/asusfx506hc/default.nix`, `hosts/asusprimez590mplus/default.nix`, `hosts/aeza-vps/default.nix` (`services.providermux.*`)
- Modify: `modules/services/llm-server.nix`, `docs/llm-server.md` (comment references)

- [ ] **Step 1: Rename module and package files**
  Move `modules/services/free-ai-router.nix` to `modules/services/providermux.nix`.
  Move `pkgs/free-ai-router/package.nix` to `pkgs/providermux/package.nix`.
  Update options from `services.free-ai-router` to `services.providermux`.
  Update package name from `free-ai-router` to `providermux`.

- [ ] **Step 2: Update host declarations and package overlay**
  In `overlays/packages.nix`, export `providermux` (with alias `free-ai-router = final.providermux;` for safety).
  In `hosts/*/default.nix`, update `services.free-ai-router` blocks to `services.providermux`.

- [ ] **Step 3: Migrate config and state paths on host**
  Migrate `~/.config/free-ai-router` to `~/.config/providermux`.
  Migrate `~/.local/state/free-ai-router` to `~/.local/state/providermux`.
  Restart/switch user unit to `providermux.service`.

- [ ] **Step 4: Run NixOS verification ladder**
  Run `./tools/ch/ch` in `~/.config/nixos-config`.
  Verify all 6 gates pass (`OK`).
  Run `nixos-rebuild build --flake .#asusfx506hc`.

- [ ] **Step 5: Commit nixos-config changes**
  Review `git status --short` and `git diff --staged`.
  Commit: `feat(services): rename free-ai-router to providermux`.
