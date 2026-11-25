---
id: "05-plugin-bootstrap-config"
title: "Plugin config parsing and Discord API seam"
status: in_progress
wave: 1
depends_on: []
plan: "plan.md"
requirements:
  - REQ-INTEGRATIONS-DISCORD-001
acceptance_criteria:
  - AC-INTEGRATIONS-DISCORD-001.1
  - AC-INTEGRATIONS-DISCORD-001.2
  - AC-INTEGRATIONS-DISCORD-001.3
system_design:
  - system-design.md
---

# Task 05: Config parsing and Discord API seam

Repository: `Fclem/kandev-plugin-discord`. Use `/create-kandev-plugin` and
`/tdd`.

- **Acceptance:**
  1. **Manifest** (`manifest.yaml`): `api_version: 2`; capabilities
     `events` (the subjects in the system design's manifest summary),
     `api_read: [tasks, sessions, messages, interactions, workflows]`,
     `api_write: [messages, execution, interactions, human_responses]`,
     `state: true`; admin actions `connection.test` and `connection.status`
     (`scope: workspace`); no webhooks; the full `config_schema`.
     `min_kandev_version` stays at the template floor until the SDK gate in
     the plan is cleared; the manifest comment says so.
  2. **SDK.** `go.mod` pins `github.com/kdlbs/kandev/pluginsdk
     v0.4.1-0.20261123163015-83cecc587a88` without a `replace`; the
     `Makefile` exposes `SDK_GATE` so local builds work while
     `make sdk-check` blocks release.
  3. **Config** (`server/config`): parses every schema field with its
     default; `workspace_guilds` uses comma or newline separators, is
     one-to-one, and needs snowflake guild IDs; `allowed_user_ids` must be
     snowflakes; `kandev_base_url` must be an absolute `http(s)` URL; enums
     and non-negative integers are validated; errors never contain the token.
  4. **Discord seam** (`server/discordapi`): one `API` interface for users,
     applications, guilds, roles, channels, messages, reactions, pins,
     commands, and interaction responses; a discordgo-backed `Client` that
     passes the context, stops every call after a REST 401 (`ErrHalted`),
     and keeps a global budget of 40 requests/s; a call-recording `Fake`
     with in-memory guild state and injectable errors. The old
     `server/discord` package is folded into it.
- **Verification:** `make check-format vet test` (from the plugin root, after `npm ci --ignore-scripts`; see the plan's "SDK gate" for why `make sdk-check` fails until SDK 0.5.0 ships); `go test ./server/config/... ./server/discordapi/...`.
- **Files:** `manifest.yaml`, `go.mod`, `go.sum`, `Makefile`, `.kandev-tooling-ref`, `server/config/*`, `server/discordapi/{api.go,client.go,fake.go,permissions.go,*_test.go}`, `server/health/*` (import move), `server/plugin.go`.
- **Dependencies:** none.
- **Risks:** `bwmarrin/discordgo` v0.29.0 (BSD-3-Clause) is the only new
  dependency; `gorilla/websocket` comes with it.
- **Output contract:** summary, files changed, tests run, blockers, risks, and task/plan status update.

## Results

Pending (scaffold commit `105108d` delivered the manifest identity, the
`connection.test` action, the config schema, and binding parsing).
