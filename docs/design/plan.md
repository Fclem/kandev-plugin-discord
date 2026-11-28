---
requirements:
  - REQ-INTEGRATIONS-DISCORD-001
  - REQ-INTEGRATIONS-DISCORD-002
  - REQ-INTEGRATIONS-DISCORD-003
  - REQ-INTEGRATIONS-DISCORD-004
  - REQ-INTEGRATIONS-DISCORD-005
  - REQ-INTEGRATIONS-DISCORD-006
  - REQ-INTEGRATIONS-DISCORD-007
  - REQ-INTEGRATIONS-DISCORD-008
  - REQ-INTEGRATIONS-DISCORD-009
  - REQ-INTEGRATIONS-DISCORD-010
  - REQ-PLUGINS-CHAT-BRIDGE-HOST-001
  - REQ-PLUGINS-CHAT-BRIDGE-HOST-002
  - REQ-PLUGINS-CHAT-BRIDGE-HOST-003
  - REQ-PLUGINS-CHAT-BRIDGE-HOST-004
  - REQ-PLUGINS-CHAT-BRIDGE-HOST-005
  - REQ-PLUGINS-CHAT-BRIDGE-HOST-006
  - REQ-PLUGINS-CHAT-BRIDGE-HOST-007
  - REQ-PLUGINS-CHAT-BRIDGE-HOST-008
system_design:
  - host-controls-system-design.md
  - system-design.md
created: 2026-11-21
updated: 2026-11-25
status: in_progress
---

# Implementation Plan: Discord Task Channels

This is the plugin repository's copy of
[`kdlbs/kandev` `docs/plans/discord-plugin/plan.md`](https://github.com/kdlbs/kandev/blob/9ee282c20a36b71d1d8ebae797865741bdc94a38/docs/plans/discord-plugin/plan.md),
re-planned on 2026-11-25 against the **merged** Host contract
([kdlbs/kandev#5438](https://github.com/kdlbs/kandev/pull/5438), merge commit
`83cecc587a8850f8da99dd3a77cf71d49297ab4c`, 2026-11-23). Upstream stays
canonical for the requirements and the Host tasks; this copy owns the plugin
tasks 05–14.

| Document | Content |
|---|---|
| [`requirements.md`](requirements.md) | REQ-INTEGRATIONS-DISCORD-001…010 (behavior contract) |
| [`system-design.md`](system-design.md) | Plugin design, data model, failure modes, scenarios |
| [`host-controls-requirements.md`](host-controls-requirements.md) | REQ-PLUGINS-CHAT-BRIDGE-HOST-001…008 (Host contract) |
| [`host-controls-system-design.md`](host-controls-system-design.md) | Host design of linking, receipts, `CancelTurn`, step approval |
| [`adr-2026-11-22-plugin-external-chat-task-control.md`](adr-2026-11-22-plugin-external-chat-task-control.md) | Decision record (#5420) |

## Overview

There are two deliverables.

- **(A) Host API changes in `kdlbs/kandev` — done.** Decided on
  [#5420](https://github.com/kdlbs/kandev/issues/5420#issuecomment-7083766483)
  and merged in #5438:
  - `CancelTurn` (exact execution command, fenced like `StopTaskRun`);
  - account linking (`LinkExternalIdentity`) and linked-user receipts
    (`RequestHumanResponseReceipt`) behind `host.v2.write:human_responses`;
  - `GetStepApproval`/`ApproveStep`;
  - `Message.Source`;
  - docs, including the `raw_content` correction (the payload field stays).
- **(B) The plugin
  [`Fclem/kandev-plugin-discord`](https://github.com/Fclem/kandev-plugin-discord).**
  Tasks 05–14 below.

### What changed in this re-plan

The plugin tasks were written before #5420 was decided. They now follow the
merged contract:

- **Linking, not attestation.** A Kandev user generates a link code on the
  plugin settings page (host-rendered "Linked chat accounts" card) and runs
  `/link <code>`; the plugin relays it with `LinkExternalIdentity`. Every
  approval first requests a receipt for the clicking Discord user
  (`Provider: "discord"`, `Subject: <user id>`). An unlinked user gets a deep
  link and `/link` instructions instead of an effect.
- **Exact signatures** from `docs/public/plugins-authoring.md` at `83cecc58`
  (see "Host contract used").
- **Echo suppression** uses `Message.Source`; the plugin never subscribes to
  `message.*`.
- **SDK gate** below.

## SDK gate

| Item | State on 2026-11-28 |
|---|---|
| `pluginsdk` with #5438 | `pluginsdk/v0.5.0` (commit `15852b70`), pinned in `go.mod`. |
| `@kdlbs/plugin-sdk` | `0.5.0`, pinned in `package.json`. |
| Kandev release with #5438 | `v0.104.0` (commit `15852b70`, contains `83cecc58`): `min_kandev_version: "0.104.0"`, the floor `kandev-sdk-check@v0.5.0` reports. |

The gate is cleared: `make sdk-check` passes and runs before every build and
package target. `.kandev-tooling-ref` points at `15852b70`. Until 2026-11-28
the plugin pinned the pseudo-version
`v0.4.1-0.20261123163015-83cecc587a88`, and `make sdk-check` failed by
design.

## Host contract used (verified at `83cecc58`)

| Need | Host surface | Manifest | Workspace grant |
|---|---|---|---|
| Task list/get with archive and step | `Tasks().List/Get` (`TaskFilter.IncludeArchived`, `Task.ArchivedAt`, `Task.WorkflowStepID`) | `api_read: tasks` | — |
| Sessions | `Sessions().List` | `api_read: sessions` | — |
| Outbound content | `Messages().List` (sanitized, `Message.Source`) | `api_read: messages` | — |
| Inbound delivery | `Messages().Send(ctx, taskID, "", text)` → `queued`/`sent`/`started`; gRPC `ResourceExhausted` = queue full, `FailedPrecondition` = task or session inactive, `Aborted` = changed or claimed | `api_write: messages` | — |
| Pending prompts | `pluginsdk.Interactions(host).ListPending/Get` (`Interaction.ResolvedBy`) | `api_read: interactions` | — |
| Step names | `Workflows().ListSteps` | `api_read: workflows` | — |
| Capability context | `pluginsdk.HostV2(host).GetCapabilityContext(ctx, workspaceID)` → `ApprovalRevision`, `ManifestDigest` | — | — |
| Execution IDs and session versions | `pluginsdk.HostExactQueries(host).ListSessions` (`ExactSessionObservation.ExecutionID`, `.ResourceVersion`) | `api_read: sessions` | `host.v2.read:sessions` |
| Interaction revision | `HostExactQueries(host).GetInteraction` (`ResourceVersion`) | `api_read: interactions` | `host.v2.read:interactions` |
| `/stop scope:task` | `HostExecutionCommands(host).StopTaskRun(ctx, ExactTaskExecutionCommand)` | `api_write: execution` | `host.v2.write:execution` |
| `/stop` (turn) | `HostExecutionCommands(host).CancelTurn(ctx, ExactTaskExecutionCommand)`; `NO_CHANGE` when no turn runs | `api_write: execution` | `host.v2.write:execution` |
| Step approval read | `HostExecutionCommands(host).GetStepApproval(ctx, ExactStepApprovalQuery)`; Office stage → gRPC `Unimplemented` | `api_read: sessions` | `host.v2.read:sessions` |
| Step approval | `HostExecutionCommands(host).ApproveStep(ctx, ExactStepApprovalCommand)`; `CONFLICT` `step_not_awaiting_approval` / `step_approval_changed`, `UNSUPPORTED` for Office | `api_write: execution` | `host.v2.write:execution` |
| `/link` | `HostHumanResponseDelegation(host).LinkExternalIdentity(ctx, ExternalIdentityLinkRequest)`; `DENIED missing_capability_approval` keeps the code valid | `api_write: human_responses` | `host.v2.write:human_responses` in any workspace |
| Receipts | `HostHumanResponseDelegation(host).RequestHumanResponseReceipt(ctx, HumanResponseReceiptRequest)`; `DENIED external_identity_not_linked` / `linked_user_not_authorized`; 5 min, single use, in memory | `api_write: human_responses` | `host.v2.write:human_responses` |
| Permission answer | `HostExactInteractionCommands(host).RespondPermission(ctx, ExactPermissionResponse)` | `api_write: interactions` | `host.v2.write:interactions` |
| Settings card | Manifest `actions` `connection.test`, `connection.status` (`access: admin`, `scope: workspace`) | `min_kandev_version ≥ 0.91.1` | — |

Management claim fence: the plugin sends an empty `ManagementInstanceKey` and
`ExpectedClaimGeneration: 0` (never-claimed task). The SDK has no claim read,
so a task claimed by another owner, or released after a claim, returns
`CONFLICT`; `/stop` reports it as "the task is managed elsewhere". Recorded as
a Host gap (see Risks).

## Architecture (plugin)

```
kandev events ──OnEvent──▶ bridge.HandleEvent ──▶ per-task scheduler (coalesced sync)
                                                     ├▶ channels   (create / rename / close / reopen / retention)
                                                     ├▶ outbound   (Messages.List after cursor → chunk → send; status lines; card)
                                                     └▶ prompts    (Interactions.ListPending, GetStepApproval → buttons)
Discord gateway ─MESSAGE_CREATE───▶ inbound  (authz → dedupe → Messages.Send → reaction)   [per-task queue]
               ─INTERACTION_CREATE▶ commands (/link /status /stop /approve /deny, buttons, autocomplete)
               ─READY/GUILD_CREATE▶ provision → reconcile → inbound catch-up
               ─CHANNEL_DELETE────▶ record → deleted
reconciler (READY + every 5 min): bindings → tasks (IncludeArchived) → kick every task sync → orphan records
admin actions: connection.test (live) / connection.status (cached) ◀── settings card
store: Host state (plugin-namespaced JSON)
```

| Package | Responsibility |
|---|---|
| `server/config` | Parse and validate every `config_schema` field |
| `server/discordapi` | `API` interface, discordgo REST implementation (401 halt, 40 req/s budget, context), call-recording `Fake` |
| `server/gateway` | Supervisor around the discordgo websocket: intents, fatal close codes, identify budget, jittered reconnect |
| `server/store` | Typed Host-state records and the channel reverse index; in-memory fake `StateHost` |
| `server/kandev` | Narrow facade over `pluginsdk.Host` and its v2 extensions; fake for tests |
| `server/provision` | Idempotent guild provisioning and command registration |
| `server/channels` | Naming, topic, category rollover, lifecycle state machine |
| `server/outbound` | Cursor puller, filter, fence-aware chunker, status lines, task card |
| `server/prompts` | Permission and step-approval prompt posting and clearing |
| `server/inbound` | Message delivery, reactions, catch-up |
| `server/authz` | Operator check |
| `server/commands` | Command definitions, `/link`, `/status`, `/stop`, `/approve`, `/deny`, buttons, autocomplete |
| `server/bridge` | Wiring: scheduler, event dispatch, gateway handlers, reconcile loop, health record |
| `server/health` | `connection.test` checks and the cached `connection.status` |

Cross-cutting rules:

- **Idempotency.** Event IDs go through an in-memory LRU. Every Discord
  mutation checks the stored record first. Outbound posting is cursor-driven.
  Prompts are reconciled against `Interactions()` and `GetStepApproval`.
- **Concurrency.** One goroutine per task key runs that task's work in order
  (sync, inbound messages, button presses), because Host state has no
  compare-and-swap. Guild provisioning is serialized per guild.
- **Contexts.** Event and action handlers pass the Host's handler context to
  Host calls. Gateway-driven work has none and uses a background context.
- **Logging.** Never log config, the token, or message content; log IDs and
  outcomes only.

## Frontend

No Kandev SPA change. `ui/bundle.js` registers one `plugin-settings` card above
Kandev's schema form and the host-rendered "Linked chat accounts" card. It
shows workspace IDs, per-binding invite links, **Test connection**
(`connection.test`), and the cached gateway state (`connection.status`).

## Tests

| What (spec scenario) | File | How |
|---|---|---|
| Config parse, defaults, validation | `server/config/config_test.go` | Table-driven |
| Fake records calls; 401 halts; error mapping | `server/discordapi/*_test.go` | `httptest` Discord |
| Fatal close codes, identify budget, reconnect | `server/gateway/gateway_test.go` | Fake connector, fake clock |
| Provisioning idempotency | `server/provision/provision_test.go` | Fake `discordapi` |
| Naming, rename debounce, rollover, capacity, close/reopen/retention | `server/channels/*_test.go` | Fake `discordapi`, fixed clock |
| Reconcile: downtime, archive, orphan, no-op second run | `server/bridge/*_test.go` | Fake Host plus fake Discord |
| Outbound order, cursor, echo suppression, chunking, mentions, card | `server/outbound/*_test.go` | Fake Host messages |
| Inbound authz, reactions per outcome, dedupe, catch-up age | `server/inbound/*_test.go` | Fake `Send` |
| `/status`, `/stop` scopes and outcomes | `server/commands/*_test.go` | Fake exact commands |
| `/link`, approvals: linked, unlinked, missing grant, stale, Office | `server/commands/*_test.go`, `server/prompts/*_test.go` | Fake delegation and interactions |
| Admin actions; no secrets in output | `server/health/health_test.go`, `server/plugin_test.go`, `tests/settings-card.test.js` | Fake Discord; node contract tests |

## E2E

There are no Kandev SPA changes and no Playwright specs. Plugin acceptance is
the artifact smoke in task-14 on a disposable dev Kandev built from `main`
(≥ `83cecc58`, `KANDEV_MOCK_AGENT=true`) with a dedicated test bot and guild.

## Implementation waves

Default execution is sequential.

```
Wave 1:
- [x] [task-01-host-control-rpcs-adr](task-01-host-control-rpcs-adr.md)  (#5438)
- [x] [task-05-plugin-bootstrap-config](task-05-plugin-bootstrap-config.md)

Wave 2:
- [x] [task-02-host-control-rpcs](task-02-host-control-rpcs.md)  (CancelTurn, #5438)
- [x] ~~[task-03-host-plugin-event-sanitization](task-03-host-plugin-event-sanitization.md)~~  (cancelled; docs corrected in #5438)
- [x] [task-17-host-message-source](task-17-host-message-source.md)  (#5438)
- [x] [task-06-plugin-gateway-provisioning](task-06-plugin-gateway-provisioning.md)

Wave 3:
- [x] [task-15-host-delegated-human-responses](task-15-host-delegated-human-responses.md)  (#5438)
- [x] [task-07-plugin-channel-lifecycle](task-07-plugin-channel-lifecycle.md)

Wave 4:
- [x] [task-16-host-step-approval-rpcs](task-16-host-step-approval-rpcs.md)  (#5438)
- [x] [task-08-plugin-reconciler](task-08-plugin-reconciler.md)
- [x] [task-09-plugin-outbound-mirroring](task-09-plugin-outbound-mirroring.md)

Wave 5:
- [x] [task-04-host-plugin-docs](task-04-host-plugin-docs.md)  (#5438)
- [x] [task-10-plugin-inbound-delivery](task-10-plugin-inbound-delivery.md)
- [x] [task-11-plugin-status-stop-commands](task-11-plugin-status-stop-commands.md)

Wave 6:
- [x] [task-12-plugin-permission-approvals](task-12-plugin-permission-approvals.md)  (/link, permissions, steps)
- [x] [task-13-plugin-settings-card](task-13-plugin-settings-card.md)

Wave 7:
- [ ] [task-14-plugin-artifact-smoke-release](task-14-plugin-artifact-smoke-release.md)  (blocked: live Discord smoke)
```

## Verification Results

- Tasks 05–13: done; `go test -race ./server/...` and the UI contract tests
  pass (see each task's `## Results`).
- Task 14: packages build and verify for all five platforms; install,
  settings, grants, link codes, upgrade, disable/enable, and uninstall pass
  on a disposable Kandev from `main`. Live Discord scenarios are blocked:
  this host cannot reach Discord and no test bot was provided.
- `make sdk-check` passes since 2026-11-28 (SDK 0.5.0, Kandev 0.104.0).
- Adversarial review round 1 (20 findings) fixed: rebinding retires old
  channels; operator-role changes converge overwrites; step buttons bind
  the displayed step; message intake is ordered per channel with catch-up
  first; provisioning waits for the bot identity and is retried; channels
  deleted offline (or answering Unknown Channel) are recreated with their
  prompts; the chunker always terminates; post-READY fatal close codes and
  every IDENTIFY (op 9 included) are observed through discordgo's logger
  (loopback-gateway tests); a REST 401 wakes the supervisor; interaction
  callbacks skip the shared budget; the scheduler is created once; category
  rollover is serialized; channel creation persists an intent and adopts on
  retry; unknown authority does not advance the inbound cursor; terminal
  tasks flush before closing; message parts checkpoint; transport errors
  are URL-redacted; failed pins retry.
- Adversarial review round 2 (16 findings) fixed: nothing posts to a
  channel until its overwrites match the current roles, and a closing
  channel waits for its final output; step buttons bind the transition and
  the posted prompt; a failed live delivery or an unfinished READY catch-up
  is retried from the saved cursor before later messages; provisioning
  shares the category lock and re-runs each reconcile, and deleted
  categories are replaced; creation adopts by the persisted request (name,
  topic, time, unowned); outbound parts save a send intent and look for an
  interrupted part before resending; partial progress is per channel;
  prompt buttons stay recorded until their edit succeeds; an op 9
  re-identify beyond the budget is refused before it is sent; `/status`
  and command replies fit 2000 characters; the settings card and backend
  share a bindings fixture; `#kandev-status` gets connect, outage, and
  provisioning notices.
- Adversarial review round 3 (11 findings) fixed: an adopted channel keeps
  the RolesKey of its creation (Converge gates it); provisioning stores a
  role change before later steps and the bridge reloads it on failure; a
  failed route read retries the channel from history; outbound parts use
  `enforce_nonce` instead of a content lookup, and creations disable
  discordgo's 502 resend; a closing channel waits for running sessions
  (bounded); the step prompt's button rotates with the approval's
  ResourceVersion; an unrecorded creation of a closed task is adopted and
  closed; archived channels resolve their prompts with retries; outage
  notices are not cleared by REST provisioning; the chunker reserves the
  closing fence of a line that opens one.
- Adversarial review round 4 (15 findings) fixed: posts hold a read fence
  that role publication takes for writing; every gateway attempt is a fresh
  session (no resume of invalidated sessions) with an abortable, bounded
  handshake; identifies are reserved before sending, released when unsent;
  abandoned creations are adopted and saved before closing, and a creation
  inventory covers tasks deleted meanwhile; the reverse index is verified
  each sync and repaired with a catch-up; buttons and mutating commands
  need the active channel; sessions are read before and after messages;
  catch-up stops before the live message that triggered it; the close hold
  is bounded from a persisted first attempt; `markStep` matches the
  ResourceVersion; cards and prompts carry nonces; outage and provisioning
  notices are tracked apart; over-long fence lines are never split.
- Adversarial review round 5 (5 findings) fixed: the creation inventory
  keeps a vanished task until its channel is deleted or closed and
  indexed; the handshake is abandoned (not awaited) after an abort and an
  aborted session never dials (discordgo deadlocks in Open on an op 7
  before READY); a 401 from gateway discovery halts; a card returned by a
  retried post with older contents is edited; status lines carry nonces.
- Adversarial review round 6 (2 findings) fixed: a status-line batch
  advances its nonce counter only once fully posted, so a partly posted
  batch retries with the same nonces; failed and capacity-blocked
  creations settle the creation inventory.
- Adversarial review round 7 (3 findings, first round on Opus 5.5) fixed:
  a message arriving while a bound workspace is unprovisioned marks its
  channel for catch-up; status-line nonces are keyed by session, not batch
  position; a 403 on an active channel marks it `failed` with one
  `#kandev-status` note and no retries until the plugin restarts, when the
  same channel is tried again (never a second one).
- Adversarial review round 8 (3 findings) fixed: a channel failed for
  lost access (posting, channel edits, or reading history) is reactivated
  at each retry point — process start or a successful provisioning, which
  every reconcile repeats and which fails while the bot is out of the
  guild — whether or not its task still wants it, so a closed task's
  channel is archived; `#kandev-status` gets one note per outage and one
  when access returns.
- Adversarial review round 9 (3 findings) fixed: losing access marks the
  channel for catch-up, so messages posted meanwhile are delivered once
  access returns in the same process; a 403 mid-catch-up keeps the cursor
  the catch-up saved; `AccessLostAt` is cleared with the channel (deleted,
  recreated, archived, reopened).
- Adversarial review round 10 (2 findings) fixed: a channel recovered
  while a session runs is marked recovered; prompt and step nonces include
  the channel's reopen count, so prompts closed on archive are posted
  again with live buttons after a quick reopen.
- Adversarial review round 11 (3 findings) fixed: an invalid session closes
  the connection and reconnects with growing backoff (IDENTIFYs at least
  6 s apart) instead of re-identifying in place; unreadable settings are
  retried and reported on the card; a stale message from a non-operator
  gets ⛔ during catch-up.
- Adversarial review round 12: no findings.
- Adversarial review round 13 (2 findings) fixed: the retention deadline
  follows the current `archive_retention_days` for channels already
  archived; the "deleted in Discord" note fires only for channels Discord
  deleted, not for policy deletes.
- Adversarial review round 14 (2 findings) fixed: duration settings are
  bounded (retention ≤ 36500 days, catch-up ≤ 525600 minutes) and an
  overlong retention keeps the channel instead of overflowing its deadline;
  a parent read error other than NotFound skips the subtask's rename and
  topic update.

## Risks

- **SDK and release gate.** Cleared (see "SDK gate"); `make sdk-check` runs
  in CI and before every build.
- **Background calls and workspace collaboration.** Gateway-driven Host calls
  (inbound chat, commands, reconcile) have no Kandev handler context. On an
  instance with `workspace_collaboration` enabled, conversation-domain calls
  then return `PermissionDenied` ("Conversation caller context" in the
  authoring guide). The plugin reports the error; it cannot work around it.
  Host gap.
- **Management claims.** No plugin read exposes the task's claim generation,
  so `/stop` assumes an unclaimed task. Host gap.
- **Discord policy.** Further restrictions on bots or privileged intents would
  break inbound chat. Discord sits behind `discordapi`.
- **Channel cap.** Retention, deletion, and rollover mitigate it.
- **Authority.** Guild role admins control chat and `/stop`. Approvals act
  only for a linked Kandev user with `session.control`, need the workspace
  grant, and are audited.
- **Event loss.** Cursor pulls and the 5-minute reconcile bound the delay.
- **Identify budget.** A persisted counter refuses to connect after 900
  identifies in 24 h. A reservation is written before every IDENTIFY and
  taken back when the attempt fails before sending it; an unwritable
  counter refuses. IDENTIFYs are at least 6 s apart, an invalid session
  (op 9) closes the connection instead of re-identifying on it, and the
  backoff only resets after a connection stayed up for a minute.
- **No gateway resume.** Unlike the system design's "resume with
  `resume_gateway_url`", each connection attempt uses a fresh discordgo
  session and IDENTIFYs: discordgo keeps a session Discord may have
  invalidated (close 4007/4009) and offers no reset, and the READY
  reconcile already recovers missed events. Each reconnect costs one
  identify from the budget. A handshake that discordgo wedges (an op 7
  RECONNECT before READY deadlocks its Open) is abandoned after the
  handshake timeout, leaving one blocked goroutine per occurrence.
- **Duplicate posts.** Message parts, status lines, cards, and prompts carry an enforced
  nonce, so a retry within Discord's nonce window (a few minutes) never
  duplicates them. A crash between Discord accepting a post and the record
  write, followed by an outage longer than that window, can post that one
  item twice. Accepted: Discord offers no durable idempotency, and the
  alternative (matching earlier posts by content) can drop messages.
- **Streaming rows.** An agent message row can grow while its turn runs, so a
  mid-turn pull holds back the newest agent message of a session that was
  running (or changed state) in the session reads before and after the
  message read.

## Open Questions

None.
