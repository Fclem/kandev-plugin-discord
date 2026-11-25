---
id: "14-plugin-artifact-smoke-release"
title: "Artifact verification and disposable smoke"
status: pending
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

Pending.
