---
id: "14-plugin-artifact-smoke-release"
title: "Artifact verification and disposable smoke"
status: blocked
wave: 7
depends_on: ["10-plugin-inbound-delivery", "12-plugin-permission-approvals", "13-plugin-settings-card", "04-host-plugin-docs"]
plan: "plan.md"
requirements:
  - REQ-INTEGRATIONS-DISCORD-010
acceptance_criteria:
  - AC-INTEGRATIONS-DISCORD-010.1
  - AC-INTEGRATIONS-DISCORD-010.2
  - AC-INTEGRATIONS-DISCORD-010.3
system_design:
  - system-design.md
---

# Task 14: Artifact verification and smoke

Repository: `Fclem/kandev-plugin-discord`. Follow `/create-kandev-plugin`
"Verify The Artifact". Publishing (tag, release, catalog entry) is out of
scope until the SDK gate clears and the user asks.

- **Acceptance:**
  1. `.kandev-tooling-ref` points at `83cecc587a8850f8da99dd3a77cf71d49297ab4c`
     (the commit whose `plugin-pack` and SDK match `go.mod`);
     `make verify-package-host SDK_GATE=` writes and verifies the archive.
  2. Install it into a **disposable** dev Kandev built from `kdlbs/kandev`
     `main` (≥ `83cecc58`, `KANDEV_MOCK_AGENT=true`, a fresh `~/.cache`-backed
     home), never the primary instance; grant the Host v2 capabilities in
     Settings > Plugins > Workspace access; confirm the plugin starts, the
     settings card renders, and both admin actions answer.
  3. With a **dedicated test bot and test guild** (never real credentials):
     lifecycle (create, rename, archive, reopen), downtime reconcile,
     inbound (queued, sent, queue full, non-operator ⛔, stale catch-up),
     `/status`, both `/stop` scopes, `/link`, approvals by button and by
     `/approve`, an unlinked user, a missing grant, mention suppression,
     `4004` and `4014`. Without a test token, record these as skipped.
  4. Upgrade (version bump + reinstall) keeps Host state; uninstall removes
     it and leaves Discord objects. README carries the setup guide and
     security notes.
- **Verification:** `make test`, `make verify-package-host SDK_GATE=`,
  `make verify-package SDK_GATE=`; record a per-scenario checklist below.
- **Files:** `README.md`, `CHANGELOG.md`, `.kandev-tooling-ref`, `docs/design/*`.
- **Dependencies:** tasks 04, 10, 12, 13.
- **Risks:** live Discord tests consume identifies; keep the run short.
- **Output contract:** summary, files changed, tests run, blockers, risks, and task/plan status update.

## Results

Status **blocked** on the live Discord smoke and the release gate; every
step reachable from this host passed (2026-11-25, linux-amd64).

**Commands** (from a copy of the plugin tree on the cache drive, so build
output stays off the repository disk; tooling worktree of `kdlbs/kandev` at
`83cecc58`):

```sh
make check-format vet test build SDK_GATE=            # pass (Go race tests, 6 UI tests, verifier tests)
make sdk-check                                        # FAIL by design: SDK record 0.5.0 vs pseudo-version
make verify-package-host SDK_GATE= KANDEV_TOOLING=…/apps/backend   # pass: manifest.yaml, server/plugin-linux-amd64, ui/bundle.js OK
make verify-package SDK_GATE= KANDEV_TOOLING=…/apps/backend        # pass: all five executables OK
```

Artifact: `kandev-plugin-discord-0.1.0.tar.gz` (62 MB, five platforms);
`plugin-pack` at `83cecc58` accepted the manifest (`api_write:
human_responses`, both admin actions).

**Disposable Kandev** built from `kdlbs/kandev` `main` `9ee282c2` (contains
`83cecc58`): `kandev __backend` with `KANDEV_HOME_DIR`, database, and `HOME`
under `~/.cache/tmp/kandev-smoke`, `KANDEV_E2E_MOCK=true`,
`KANDEV_MOCK_AGENT=true`, port 38990; web assets from `make build-web`.

| Scenario | Result |
|---|---|
| Install via `POST /api/plugins/install` | ✅ `active` |
| `connection.status` before settings | ✅ `not_configured` with the config errors, no token |
| `connection.test` before settings | ✅ config fail, later checks skipped |
| Save settings (`PATCH /api/plugins/:id`, fake token) → restart | ✅ plugin restarted; bridge `reconnecting` with backoff |
| `connection.test` with settings | ✅ config pass; token check reports the network error; `gateway` block present |
| Token never in logs | ✅ `grep` of the Kandev home for the fake token: no match outside the database |
| Capability grant (`PUT …/capability-approvals`) | ✅ 9 capabilities derived from the manifest, `active`, revision 1 |
| Link code (`POST …/identity-links/codes`) | ✅ issued (host-rendered card present on the settings page) |
| Settings card, desktop | ✅ renders above the schema form; bridge line; Test shows per-check lines |
| Settings card, 390 px phone viewport | ✅ wraps, no horizontal overflow |
| Upgrade 0.1.0 → 0.1.1 | ✅ `active`; config, grant (`active`, revision 2), and Host state row kept |
| Disable / enable | ✅ `disabled` → `active`; bridge restarts |
| Uninstall | ✅ 404 afterwards; `plugin_state` rows for the plugin: 0 |

**Not exercised (blocked):** every Discord-side scenario — provisioning in a
real guild, channel lifecycle, mirroring, inbound delivery and catch-up,
`/status`, `/stop`, `/link`, approvals by button and command, unlinked user,
missing grant from Discord, mention suppression, live `4004`/`4014`. This
host refuses TCP connections to `discord.com` and `gateway.discord.gg`
(`curl: (7) Failed to connect … Could not connect to server`), and no
dedicated test bot token was provided. These paths are covered by the Go
tests with fakes only. Run the checklist above plus the acceptance-3
scenarios on a host that can reach Discord, with a test bot and guild.

**Release:** not tagged or published; blocked on pluginsdk and
`@kdlbs/plugin-sdk` 0.5.0 and a Kandev release containing #5438 (plan "SDK
gate").
