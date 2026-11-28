---
id: "07-plugin-channel-lifecycle"
title: "Task channel create, rename, close, reopen, retention"
status: done
wave: 3
depends_on: ["06-plugin-gateway-provisioning"]
plan: "plan.md"
requirements:
  - REQ-INTEGRATIONS-DISCORD-003
acceptance_criteria:
  - AC-INTEGRATIONS-DISCORD-003.1
  - AC-INTEGRATIONS-DISCORD-003.2
  - AC-INTEGRATIONS-DISCORD-003.3
  - AC-INTEGRATIONS-DISCORD-003.4
system_design:
  - system-design.md
---

# Task 07: Task channel lifecycle

Repository: `Fclem/kandev-plugin-discord`. Use `/tdd`.

- **Acceptance:**
  1. **Desired state.** A task wants an open channel unless it is archived
     (`ArchivedAt`), deleted (`Get` → `NotFound`), ephemeral (unless
     `include_ephemeral_tasks`), or terminal (`COMPLETED`/`CANCELLED`) with
     `close_trigger: terminal`.
  2. **Create.** Name `<identifier>-<slug(title)>`, lowercase `[a-z0-9-]`,
     ≤ 100 characters (fallback `task-<id prefix>`); topic with title,
     identifier, parent identifier, and deep link `<kandev_base_url>/t/<task id>`;
     record and `channel:<id>` index written; `inbound_cursor` starts at the
     channel ID. A duplicate trigger creates nothing. Error `30013` marks the
     record `capacity_blocked` and posts one note to `#kandev-status`; 403/404
     mark it `failed` with `last_error`.
  3. **Rollover.** A category with 50 channels is skipped; when all are full
     the next one (`Kandev Tasks 2`, …, same overwrites) is created and stored.
  4. **Rename.** A title change renames the channel and topic at most once per
     10 minutes; a deferred retry applies the latest title.
  5. **Close/reopen/retention.** `archive` moves the channel to an archive
     category with send denied and sets `delete_after` when
     `archive_retention_days > 0`; `delete` deletes it. A reopened task moves
     back with the task overwrites. Past `delete_after`, the channel is
     deleted. `CHANNEL_DELETE` marks the record `deleted`.
- **Verification:** `make check-format vet test` (from the plugin root, after `npm ci --ignore-scripts`; see the plan's "SDK gate" for why `make sdk-check` fails until SDK 0.5.0 ships); `go test ./server/channels/...`.
- **Files:** `server/channels/{naming.go,lifecycle.go,*_test.go}`, `server/store/store.go`, `server/bridge/*`.
- **Dependencies:** task-06.
- **Risks:** the rename limit is undocumented; discordgo honors 429
  `retry_after` in addition to the debounce.
- **Output contract:** summary, files changed, tests run, blockers, risks, and task/plan status update.

## Results

- **`server/channels`.** `Name` (NFKD slug, ASCII `[a-z0-9-]`, ≤ 100,
  `task-<id>` fallback), `Topic` (title · identifier · parent · link,
  ≤ 1024), `WantOpen`, and `Manager.Sync` implementing the state machine:
  create (task overwrites, inbound cursor = channel ID, outbound cursor =
  task creation but at most 10 minutes back), capacity (`30013` →
  `capacity_blocked`, one `#kandev-status` note, retried by later syncs),
  403/404 → `failed` (retried once per process), category rollover at 50
  (`Kandev Tasks 2`, saved to the guild record), rename/topic debounce
  (10 min, deferred retry, latest title), archive (archive category,
  read-only overwrites, `delete_after`), delete action, reopen, retention
  delete, terminal close trigger, and recreation with a note after a
  manual delete.
- **Bridge.** Per-task `Kick` (coalesced, on the task queue), delayed
  kicks (`kickAfter`), `syncTask`, `HandleEvent` (event-ID LRU, `task_id`
  or session lookup), `CHANNEL_DELETE` → record `deleted` and index
  removed; `OnEvent` forwards to the bridge.
- **Tests:** `go test -race ./server/...` passed (`channels`: naming,
  topic, create-once, backlog cursor, capacity note once, failed retry per
  process, rollover, rename debounce, archive/reopen/retention,
  delete/terminal, recreation; `bridge`: event-driven create/dedupe/archive,
  channel delete).
