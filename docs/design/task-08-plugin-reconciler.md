---
id: "08-plugin-reconciler"
title: "Reconciler and per-task scheduler"
status: pending
wave: 4
depends_on: ["07-plugin-channel-lifecycle"]
plan: "plan.md"
requirements:
  - REQ-INTEGRATIONS-DISCORD-004
acceptance_criteria:
  - AC-INTEGRATIONS-DISCORD-004.1
  - AC-INTEGRATIONS-DISCORD-004.2
  - AC-INTEGRATIONS-DISCORD-004.3
system_design:
  - system-design.md
---

# Task 08: Reconciler

Repository: `Fclem/kandev-plugin-discord`. Use `/tdd`.

- **Acceptance:**
  1. **Scheduler.** Work for one task runs on one goroutine in order; sync
     requests coalesce. Event IDs are deduplicated by an LRU. Events are
     triggers: the handler extracts `task_id` (or maps `session_id`) and
     kicks a sync.
  2. **Reconcile** on `READY` and every 5 minutes per bound workspace: list
     tasks with `IncludeArchived` (and `IncludeEphemeral` when configured),
     following `PageInfo.NextCursor`, and sync each; channel records whose
     task is missing are closed as deleted; `capacity_blocked` and `deleted`
     records of open tasks are retried; retention deletes are applied.
  3. **Prompts.** The same sync posts prompts for pending interactions that
     have none and marks vanished ones resolved (task-12 provides the prompt
     renderer).
  4. **No-op second run.** A second reconcile with unchanged fakes makes no
     Discord mutation.
- **Verification:** `make check-format vet test` (from the plugin root, after `npm ci --ignore-scripts`; see the plan's "SDK gate" for why `make sdk-check` fails until SDK 0.5.0 ships); `go test ./server/bridge/...`.
- **Files:** `server/bridge/{bridge.go,scheduler.go,reconcile.go,events.go,*_test.go}`, `server/plugin.go`.
- **Dependencies:** task-07.
- **Output contract:** summary, files changed, tests run, blockers, risks, and task/plan status update.

## Results

Pending.
