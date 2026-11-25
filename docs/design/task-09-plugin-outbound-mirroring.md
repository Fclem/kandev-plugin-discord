---
id: "09-plugin-outbound-mirroring"
title: "Kandev → Discord mirroring"
status: pending
wave: 4
depends_on: ["07-plugin-channel-lifecycle", "17-host-message-source"]
plan: "plan.md"
requirements:
  - REQ-INTEGRATIONS-DISCORD-005
acceptance_criteria:
  - AC-INTEGRATIONS-DISCORD-005.1
  - AC-INTEGRATIONS-DISCORD-005.2
  - AC-INTEGRATIONS-DISCORD-005.3
  - AC-INTEGRATIONS-DISCORD-005.4
system_design:
  - system-design.md
---

# Task 09: Kandev → Discord mirroring

Repository: `Fclem/kandev-plugin-discord`. Use `/tdd`.

- **Acceptance:**
  1. **Triggers and cursor.** `turn.completed`, `task_session.state_changed`,
     reconcile, and a live-update timer (every `live_update_interval_seconds`
     while a session runs) pull `Messages().List` with
     `TaskIDs=[task]`, `Since=outbound_cursor.created_at`, paginated. IDs
     already posted at the cursor timestamp are skipped. Each message is
     posted in order and the cursor is persisted after it. While a session
     runs, its newest agent message is held back (rows can still grow).
  2. **Filter.** Agent `message`/`content` → posted. User messages → posted
     only with `mirror_user_messages` and `Source != "plugin:kandev-plugin-discord"`.
     Tool types (`tool_*`) → a one-line summary only with `post_tool_activity`.
     `thinking` and everything else → never.
  3. **Rendering.** Split at 2000 characters on line boundaries, closing and
     reopening code fences across chunks; at most 5 chunks, then
     "… truncated — open in Kandev"; every send uses `allowed_mentions.parse=[]`.
  4. **Status and card.** Session transitions post one line (started, turn
     complete, completed, failed, stopped), tracked per session in the
     channel record. One task card (title, identifier, state, step, sessions,
     queue depth, link) is sent and pinned once and edited only when its
     content changes, at most every 10 s.
- **Verification:** `make check-format vet test` (from the plugin root, after `npm ci --ignore-scripts`; see the plan's "SDK gate" for why `make sdk-check` fails until SDK 0.5.0 ships); `go test ./server/outbound/...`.
- **Files:** `server/outbound/{puller.go,chunk.go,render.go,status.go,card.go,*_test.go}`.
- **Dependencies:** task-07; task-17 (`Message.Source`, merged).
- **Risks:** messages sharing one timestamp are tie-broken by the posted-ID set.
- **Output contract:** summary, files changed, tests run, blockers, risks, and task/plan status update.

## Results

Pending.
