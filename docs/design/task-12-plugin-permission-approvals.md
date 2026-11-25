---
id: "12-plugin-permission-approvals"
title: "/link and approvals through linked-user receipts"
status: pending
wave: 6
depends_on: ["11-plugin-status-stop-commands", "09-plugin-outbound-mirroring", "15-host-delegated-human-responses", "16-host-step-approval-rpcs"]
plan: "plan.md"
requirements:
  - REQ-INTEGRATIONS-DISCORD-008
acceptance_criteria:
  - AC-INTEGRATIONS-DISCORD-008.1
  - AC-INTEGRATIONS-DISCORD-008.2
  - AC-INTEGRATIONS-DISCORD-008.3
  - AC-INTEGRATIONS-DISCORD-008.4
  - AC-INTEGRATIONS-DISCORD-008.5
system_design:
  - system-design.md
---

# Task 12: `/link` and approvals (permission requests and workflow steps)

Repository: `Fclem/kandev-plugin-discord`. Use `/tdd`.

- **Acceptance:**
  1. **`/link <code>`** (anyone in a bound guild, any channel):
     `LinkExternalIdentity{Provider: "discord", Subject: <user id>, DisplayName}`.
     Ephemeral replies never echo the code: `APPLIED` → "Linked to
     <Kandev user>"; `DENIED missing_capability_approval` → "a workspace
     manager must grant Discord approvals; your code stays valid"; other
     denials → "invalid or expired code".
  2. **Permission prompts** (sync, `permission_request.received.*`,
     reconcile): one message per pending permission interaction with one
     button per option (≤ 5 per row, ≤ 25; overflow → deep link), recorded
     in `task/<id>/interaction_prompts`; no duplicates. A pending
     clarification gets one notice with a deep link (not answerable from
     Discord). A prompt whose interaction resolved is edited to show
     `ResolvedBy` ("✅ Alice via discord") or "resolved elsewhere" and its
     buttons are removed.
  3. **Step prompts:** for each non-running session, `GetStepApproval`;
     `Awaiting` → one "Step *from* awaits approval → *to*" message with
     Approve, recorded in `task/<id>/step_approval`; cleared on `task.moved`
     or when no longer awaiting. Office stages (`Unimplemented`) post nothing.
  4. **Answering.** Buttons, `/approve [request]`, `/deny [request]`
     (operators only): read the exact revision (`GetInteraction` or
     `GetStepApproval`), request a receipt
     (`HumanResponseKindPermission` with the option, or
     `HumanResponseKindStepApproval` with the session as target), then
     `RespondPermission` or `ApproveStep`. `/approve` picks `allow_once`, then
     the first `allow_*`; `/deny` picks `reject_once`, then the first
     `reject_*`. With no argument, a single pending permission wins over the
     step; several pending → "choose one" with autocomplete; `/deny` never
     applies to steps. Autocomplete lists both kinds.
  5. **Outcomes.** Success edits the prompt with the actor and removes the
     buttons. `DENIED external_identity_not_linked` → deep link plus `/link`
     instructions; `linked_user_not_authorized` → "your Kandev account lacks
     session control"; other denials or `PermissionDenied` → "a Kandev
     workspace manager must grant Discord approvals
     (`host.v2.write:human_responses`)"; `CONFLICT`/`NOT_FOUND` or a resolved
     interaction → "already resolved" and the prompt is marked;
     `step_not_awaiting_approval` → "nothing awaiting approval";
     `Unimplemented`/`UNSUPPORTED` → "approve this stage in Kandev". Nothing
     changes in Kandev on any refusal.
- **Verification:** `make check-format vet test` (from the plugin root, after `npm ci --ignore-scripts`; see the plan's "SDK gate" for why `make sdk-check` fails until SDK 0.5.0 ships); `go test ./server/commands/... ./server/prompts/...`.
- **Files:** `server/commands/{link.go,approve.go,autocomplete.go,*_test.go}`, `server/prompts/*`, `server/kandev/*`, `server/store/store.go`, `manifest.yaml`.
- **Dependencies:** tasks 09, 11; host tasks 15 and 16 (merged).
- **Output contract:** summary, files changed, tests run, blockers, risks, and task/plan status update.

## Results

Pending.
