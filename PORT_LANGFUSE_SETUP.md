# Bringing up litellm-proxy + self-hosted Langfuse on the NixOS devbox

This is a handoff prompt for another agent (or future me) to port the
litellm-proxy + Langfuse + Postgres setup built on macOS (2026-08-05) onto
this repo's NixOS dev box (`terraform/configuration.nix`). It records what
was actually tried, what broke, and why — read it before starting, since
several of the failures below look like configuration mistakes but are
actually load-bearing platform differences.

## What you're porting

A Docker Compose stack — `litellm-proxy` (bridges Claude Code's Anthropic
`/v1/messages` API to Fireworks/DeepInfra's OpenAI-only APIs) plus
self-hosted Langfuse (LLM observability: traces, spend logs) and its own
Postgres/Redis/ClickHouse/MinIO dependencies. Source of truth is the Mac's
`~/.config/home-manager` repo (same flake this devbox already pulls via
`devbox-home-manager` — search `configuration.nix` for `systemd.services.devbox-home-manager`):

- `dotfiles/langfuse/docker-compose.yml` — the whole stack
- `dotfiles/langfuse/init-litellm-db.sh` — Postgres init script (creates
  litellm's own database/role alongside Langfuse's, in the same container)
- `dotfiles/litellm/config.yaml` — model routing (Fireworks/DeepInfra) +
  Langfuse/Honeycomb callback config
- `home.nix` — `home.activation.langfuseFiles` copies those into
  `~/.config/langfuse/*` as real files rather than home-manager's usual
  symlinks-into-`/nix/store` (Docker Desktop's macOS VM doesn't traverse
  that symlink — plain Linux Docker likely doesn't have this problem, but
  keep the copy-based approach anyway since it's already proven and costs
  nothing), plus the `_claude_via_proxy`/`claude-fw`/`claude-di` shell
  aliases
- `dotfiles/claude/hooks/langfuse-stack-refcount.sh` +
  `dotfiles/claude/settings.json`'s `SessionStart`/`SessionEnd` hooks —
  reference-counted stack lifecycle, tied to how many Claude Code sessions
  are currently running

None of this needs rewriting for Linux — it's the same compose file, same
config, same hooks. What differs is entirely in `configuration.nix`: this
device is NixOS, not nix-darwin, so Docker's install/management path and
the sudo situation are both different (some of it *easier* than the Mac).

## Platform differences that matter (read before you start)

**Docker is already nix-native here — don't reach for Homebrew-cask-style
workarounds.** `configuration.nix` already has
`virtualisation.docker.enable = true` (search for `## ── Docker` in configuration.nix) and
`users.users.emaland.extraGroups = [ "wheel" "docker" ]`. On macOS, Docker
Desktop isn't a nix-darwin-native service — it had to be installed via a
Homebrew cask, and that hit a real wall: leftover root-owned files under
`/usr/local` from a prior manual install blocked brew's non-interactive
`darwin-rebuild` sudo, and `/usr/local` itself is SIP-protected so it
couldn't even be chowned away — needed three rounds of one-off `sudo
mkdir`/`chown` run by hand in an interactive terminal. **None of that
applies here.** `virtualisation.docker.enable` is a real NixOS module;
`nixos-rebuild switch` sets up the daemon directly, no separate cask/app
bundle, no `/usr/local` involved at all.

**Passwordless sudo already covers everything on this box** —
`security.sudo.wheelNeedsPassword = false` (search for it in configuration.nix).
On macOS, only `darwin-rebuild` itself has a passwordless sudoers rule (see
`darwin.nix`'s `environment.etc."sudoers.d/darwin-rebuild"`), so anything
brew shells out to that needs its *own* sudo (like linking binaries into
`/usr/local/bin`) hits an interactive password prompt with no TTY and just
fails. That whole class of problem doesn't exist here — if `nixos-rebuild
switch` needs to run a privileged step, it'll just run.

**litellm cannot run as a nix package — must be the official Docker
image.** This was tried and firmly ruled out on the Mac, for two
independent reasons, not one: (1) litellm's DB-backed features (spend
logs, virtual keys) run on Prisma, which needs to *write* a generated
client and fetch a platform-specific query-engine binary at `prisma
generate` time — impossible against the read-only nix store (confirmed
live: `PermissionError` writing into `/nix/store/.../prisma/schema.prisma`).
(2) Even redirecting the write target, `litellm[proxy]`'s Python deps do
*not* pull in the Prisma **CLI** — it's an npm package, not the
`prisma-client-py` Python bindings litellm imports — so `prisma generate`
fails with `prisma: not found` even from a writable directory. The fix
that actually worked: use `ghcr.io/berriai/litellm-database:main-stable`
(NOT the nixpkgs `litellm` package, NOT a custom-built image) — it bakes
in Prisma's CLI + query-engine binary at build time the way LiteLLM's own
official Dockerfile does. This is already reflected in
`docker-compose.yml`; don't try to "fix" it back to a nix package.

**Pin `main-stable`, not a specific version tag, until confirmed
otherwise.** `v1.89.0` (the version nixpkgs happened to have) has a live
bug: every `/v1/messages` response raises `[Non-Blocking]
LiteLLM.Success_Call Error: 1 validation error for AnthropicResponse`
inside litellm's own success-handler, which silently breaks *both*
spend-log DB writes and the Langfuse callback for every single request —
you get `200 OK` responses to the client, spend/logs stays empty, and
Langfuse traces never appear, with no obvious error surfaced anywhere
except that log line. `main-stable` doesn't hit this (verified: spend/logs
populated correctly, Langfuse traces landed with full content, after
switching). If you re-pin to a specific version later, verify against this
symptom first.

**Use Langfuse v3, not v4.** litellm's bundled Langfuse Python client is
v2.57.x (check via `docker exec <container> python3 -c 'import langfuse;
print(langfuse.__version__)'`), which speaks the v2/v3 ingestion API. Langfuse
v4 changed that API's shape; pointed at a v4 server, every ingestion call
was rejected with a generic `Bad request` (no useful detail) until
downgrading the `langfuse/langfuse` and `langfuse/langfuse-worker` images
from `:4` to `:3`. `docker-compose.yml` is already pinned this way — this
is the same "confusing symptom, non-obvious cause" trap as the
`main-stable` issue above, so don't assume the v4 tag is safe just because
Langfuse's own docs default to it.

**`~/.secrets` must be sourced explicitly wherever compose runs, including
inside hooks.** This bit us for real: the `SessionStart`/`SessionEnd` hook
script initially ran `docker compose up -d` without sourcing `~/.secrets`
first, so every generated password/key env var
(`REDIS_AUTH`/`POSTGRES_PASSWORD`/`LANGFUSE_INIT_*`/etc.) was empty. That
corrupted Redis's config file — `requirepass` and `--maxmemory-policy` got
concatenated into one malformed argument — and crash-looped it
indefinitely. Fixed in `langfuse-stack-refcount.sh` by sourcing
`~/.secrets` inside a subshell (`set -a; . ~/.secrets; set +a`) immediately
before every `docker compose` invocation. If you write any new script that
runs compose commands on this box, do the same — don't assume a shell
hook inherits an interactive login shell's profile sourcing.

## What actually needs to happen on this repo

1. **Generate this box's own secrets** — don't copy the Mac's `~/.secrets`
   values verbatim (new random passwords/keys are cheap and this avoids
   sharing secrets across machines for no reason). Same shape as what's in
   the Mac's `~/.secrets` — see `dotfiles/langfuse/docker-compose.yml`'s
   `${VAR}` references for the exact list (`POSTGRES_PASSWORD`,
   `LITELLM_POSTGRES_PASSWORD`, `CLICKHOUSE_PASSWORD`, `REDIS_AUTH`,
   `MINIO_ROOT_PASSWORD`, `ENCRYPTION_KEY` — `openssl rand -hex 32`,
   `NEXTAUTH_SECRET`, `SALT`, `LANGFUSE_INIT_PROJECT_PUBLIC_KEY` /
   `_SECRET_KEY`, `LANGFUSE_INIT_USER_EMAIL`/`_NAME`/`_PASSWORD`,
   `FIREWORKS_API_KEY`/`DEEPINFRA_API_KEY` if not already present,
   `HONEYCOMB_API_KEY` if you want tracing there too). `~/.secrets` is
   NOT nix-managed (per home.nix's own comment) — create it directly on
   this box, the same way it's done on macOS.

2. **Confirm `devbox-home-manager`'s boot-time switch actually deploys
   the new dotfiles** — `configuration.nix`'s `devbox-home-manager` service already runs
   `home-manager switch` on boot from this same flake, so
   `~/.config/langfuse/*` and `~/.claude/hooks/langfuse-stack-refcount.sh`
   should just appear once the flake is updated and the box reboots (or
   you run `home-manager switch` by hand over SSH to test without
   rebooting). Check the file-copy activation script
   (`home.activation.langfuseFiles` in `home.nix`) actually ran — look for
   real files (not broken symlinks) at `~/.config/langfuse/docker-compose.yml`.

3. **Bring the stack up manually first**, before trusting the hooks:
   ```bash
   set -a; source ~/.secrets; set +a
   cd ~/.config/langfuse && docker compose up -d
   ```
   Then verify the same way it was verified on macOS:
   ```bash
   curl -sf http://127.0.0.1:4111/health/liveliness   # "I'm alive!"
   curl -s http://127.0.0.1:4111/spend/logs -H "x-api-key: sk-llm-proxy-local"  # [] is fine pre-traffic
   ```

4. **Test `claude-fw`/`claude-di`** (aliases already in `home.nix`,
   should just work once the proxy is up):
   ```bash
   claude-fw -p "Say the word banana and nothing else."
   ```
   Confirm the response is correct AND that a new entry shows up in
   `/spend/logs` afterward — a `200 OK` with no spend-log entry is exactly
   the `AnthropicResponse` bug from above resurfacing; don't treat a
   correct-looking response alone as proof the pipeline is healthy.

5. **Test the session hooks** — start a fresh `claude` session (which
   should trigger `SessionStart` → stack comes up if it wasn't already),
   exit it (`SessionEnd` → stack goes down *only if* no other session is
   still running), and check `~/.cache/claude-langfuse-stack/hook.log` for
   what actually happened. If you have `tmux`/multiple panes open, test
   with two concurrent sessions to confirm the ref-count doesn't tear the
   stack down while one is still using it.

## What NOT to change

- Don't touch `dotfiles/langfuse/docker-compose.yml`'s image tags
  (`litellm-database:main-stable`, `langfuse:3`/`langfuse-worker:3`) without
  re-verifying against the specific bugs above first.
- Don't try to make `litellm` a nix package again — it's a settled dead end,
  not an oversight.
- Don't add a second Postgres instance for litellm — it shares Langfuse's
  `postgres` container via `init-litellm-db.sh`'s separate database/role,
  by design.

## Open questions for whoever does this

- Does this box's `security.sudo.wheelNeedsPassword = false` mean the
  `devbox-home-manager` boot service (which itself likely runs as
  root/system, not interactively) has a smoother path here than macOS did
  for `darwin-rebuild`'s brew step? Worth confirming there's no equivalent
  wrinkle before assuming this whole port is friction-free.
- Are Fireworks/DeepInfra/Honeycomb API keys already in this box's
  `~/.secrets` (they'd be needed for the `pi` coding agent's provider
  config, which `home.nix` already sets up independently)? If so, reuse
  those rather than minting new ones.
