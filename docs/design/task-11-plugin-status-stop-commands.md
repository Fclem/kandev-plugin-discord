---
id: "11-plugin-status-stop-commands"
title: "/status and /stop commands"
status: done
wave: 5
depends_on: ["06-plugin-gateway-provisioning", "02-host-control-rpcs"]
plan: "plan.md"
requirements:
  - REQ-INTEGRATIONS-DISCORD-007
acceptance_criteria:
  - AC-INTEGRATIONS-DISCORD-007.1
  - AC-INTEGRATIONS-DISCORD-007.2
  - AC-INTEGRATIONS-DISCORD-007.3
  - AC-INTEGRATIONS-DISCORD-007.4
system_design:
  - system-design.md
---

# Task 11: `/status` and `/stop`

Repository: `Fclem/kandev-plugin-discord`. Use `/tdd`.

- **Acceptance:**
  1. **Deferral and placement.** Commands and buttons are deferred
     ephemerally before any Host call; autocomplete answers directly. Outside
     a mapped task channel, task commands reply "Commands work only in task
     channels."
  2. **`/status`** (anyone): title, identifier, task state, workflow step
     name (`Workflows().ListSteps`), each session's state, queue count
     (cached from `message.queue.status_changed`), pending interaction count,
     and the deep link.
  3. **`/stop [scope:turn|task]`** (operators; others get a refusal):
     execution IDs and session versions come from
     `HostExactQueries.ListSessions`; `turn` (default) calls `CancelTurn`,
     `task` calls `StopTaskRun` for each session with an execution, with
     `ApprovalRevision`/`ManifestDigest` from `GetCapabilityContext`, an
     empty management key, generation 0, and a per-interaction idempotency
     key.
  4. **Outcomes.** `APPLIED`/`ALREADY_APPLIED` → "stopped"; `NO_CHANGE` →
     "nothing running"; `CONFLICT` → "the session changed (or the task is
     managed elsewhere); try again"; `DENIED` or gRPC `PermissionDenied` →
     "a Kandev workspace manager must grant execution control
     (`host.v2.write:execution`, `host.v2.read:sessions`)"; missing extension
     or `UNSUPPORTED` → "this Kandev version does not support it".
- **Verification:** `make check-format vet test` (from the plugin root, after `npm ci --ignore-scripts`; see the plan's "SDK gate" for why `make sdk-check` fails until SDK 0.5.0 ships); `go test ./server/commands/...`.
- **Files:** `server/commands/{defs.go,router.go,status.go,stop.go,reply.go,*_test.go}`, `server/kandev/*`.
- **Dependencies:** task-06; task-02 (`CancelTurn`, merged).
- **Output contract:** summary, files changed, tests run, blockers, risks, and task/plan status update.

## Results

- **Router** (`server/commands/router.go`): unbound guild → immediate
  ephemeral reply; commands are deferred ephemerally before any Kandev
  call and answered by editing the deferred response; task commands
  resolve the channel through the reverse index, otherwise "Commands work
  only in Kandev task channels."
- **`/status`** (anyone): title and identifier, state (+ archived), step
  name, each session's state, cached queue depth, pending interaction
  count, deep link.
- **`/stop [scope]`** (operators): execution IDs and session versions from
  `ExactSessions`; `turn` → `CancelTurn` for running sessions, `task` →
  `StopTaskRun` for every session with an execution; capability revision
  and manifest digest from `GetCapabilityContext`; empty management key,
  generation 0; idempotency key `discord:stop:<scope>:<interaction>:<session>`.
  Outcomes: applied, `NO_CHANGE` → "Nothing is running.", `CONFLICT`,
  `DENIED`/`PermissionDenied` → grant message, `UNSUPPORTED`/
  `Unimplemented` → unsupported.
- **Bridge.** `INTERACTION_CREATE` → router (LRU dedupe); the scheduler
  now runs queued jobs even after cancellation so waiters never hang.
- **Tests:** `go test -race ./server/...` passed (deferral first and
  ephemeral, placement, unbound guild, status content, turn fences, task
  scope, refusal, every outcome, missing grant).
