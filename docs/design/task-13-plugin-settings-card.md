---
id: "13-plugin-settings-card"
title: "Quick-setup settings card with connection test and status"
status: in_progress
wave: 6
depends_on: ["06-plugin-gateway-provisioning"]
plan: "plan.md"
requirements:
  - REQ-INTEGRATIONS-DISCORD-009
acceptance_criteria:
  - AC-INTEGRATIONS-DISCORD-009.1
  - AC-INTEGRATIONS-DISCORD-009.2
  - AC-INTEGRATIONS-DISCORD-009.3
system_design:
  - system-design.md
---

# Task 13: Quick-setup settings card

Repository: `Fclem/kandev-plugin-discord`. Use `/mobile-parity` and `/tdd`.

- **Acceptance:**
  1. `ui/bundle.js` registers one `plugin-settings` card (above Kandev's
     schema form and the host's "Linked chat accounts" card): workspace IDs
     with "Copy line", bindings parsed with the backend rules, an invite link
     per binding, and a short note on the grants and `/link`.
  2. **Test connection** (`connection.test`) reports token, application,
     Message Content intent, membership, permissions, an invite URL when
     needed, and the gateway state from the cached health record
     (`4014` → "disallowed intent", `4004` → "token rejected"). Each binding
     row also shows the cached state from `connection.status`, which makes no
     Discord call. Neither result contains token material.
  3. `initialize`/`destroy` can run twice without duplicate registrations or
     pollers; the layout is usable at phone width (wrapping rows, ≥ 36 px
     touch targets).
- **Verification:** `make test` (node contract tests) and
  `make verify-package-host SDK_GATE=` with the tooling checkout.
- **Files:** `ui/bundle.js`, `tests/settings-card.test.js`, `server/health/*`, `server/plugin.go`, `manifest.yaml`.
- **Dependencies:** task-06.
- **Risks:** saving restarts the plugin; actions fail with 503 until it is up.
- **Output contract:** summary, files changed, tests run, blockers, risks, and task/plan status update.

## Results

Partial from the scaffold (`105108d`): card with workspace IDs, invite links,
and `connection.test`; backend checks for token, application, intent,
membership, and permissions. Remaining: gateway state, `connection.status`,
and a live-host visual check.
