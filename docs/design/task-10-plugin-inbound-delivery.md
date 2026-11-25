---
id: "10-plugin-inbound-delivery"
title: "Discord → Kandev delivery with authorization"
status: pending
wave: 5
depends_on: ["08-plugin-reconciler", "09-plugin-outbound-mirroring"]
plan: "plan.md"
requirements:
  - REQ-INTEGRATIONS-DISCORD-006
acceptance_criteria:
  - AC-INTEGRATIONS-DISCORD-006.1
  - AC-INTEGRATIONS-DISCORD-006.2
  - AC-INTEGRATIONS-DISCORD-006.3
system_design:
  - system-design.md
---

# Task 10: Discord → Kandev delivery

Repository: `Fclem/kandev-plugin-discord`. Use `/tdd`.

- **Acceptance:**
  1. **Filter.** `MESSAGE_CREATE` is dropped unless it comes from a bound
     guild, an `active` mapped channel, a non-bot, non-webhook author, and
     has content. `authz.IsOperator(roles, userID)` (operator role or
     `allowed_user_ids`) gates delivery; a non-operator gets one ⛔ and
     nothing reaches Kandev.
  2. **Delivery.** `Messages().Send(taskID, "", "<display name> (Discord): <content>")`
     runs on the task's queue. `queued` → 🕒; `sent`/`started` → ✅;
     `ResourceExhausted` → ⚠️ + "queue full"; `FailedPrecondition` → ⚠️ +
     "task or session no longer active"; other errors → ⚠️ + a short reason.
     The message ID is written as `inbound_inflight` before `Send` and the
     cursor advances after the outcome, so a crash never resends: an
     in-flight ID found at catch-up gets ⚠️ "delivery uncertain".
  3. **Catch-up** on `READY` for each active channel: messages after
     `inbound_cursor`, oldest first; younger than
     `inbound_catchup_max_age_minutes` → the same path; older → ⚠️ and
     "not delivered — the bridge was offline".
- **Verification:** `make check-format vet test` (from the plugin root, after `npm ci --ignore-scripts`; see the plan's "SDK gate" for why `make sdk-check` fails until SDK 0.5.0 ships); `go test ./server/inbound/... ./server/authz/...`.
- **Files:** `server/inbound/{handler.go,catchup.go,*_test.go}`, `server/authz/*`, `server/bridge/*`.
- **Dependencies:** tasks 08 and 09.
- **Inputs:** `apps/backend/internal/backendapp/adapters_plugin_messenger.go`
  and `adapters_plugin_exact_message.go` (error codes) in `kdlbs/kandev`.
- **Output contract:** summary, files changed, tests run, blockers, risks, and task/plan status update.

## Results

Pending.
