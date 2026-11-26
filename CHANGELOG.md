# Changelog

## [Unreleased]

### Added

- Scaffold from `kdlbs/kandev-plugin-template` (public plugin SDK 0.1.0).
- Settings schema: bot token, application ID, workspace → server bindings,
  operator and viewer roles, and channel lifecycle options.
- `connection.test` admin action: checks the bot token, the application ID
  match, the Message Content intent, server membership, and required
  permissions, and returns an invite URL.
- Settings card: workspace IDs, per-binding invite links, and a connection
  test.
- Discord setup guide in the README.
- Gateway bridge (bwmarrin/discordgo): intents `GUILDS | GUILD_MESSAGES |
  MESSAGE_CONTENT`, reconnect with backoff, stop on close codes 4004 and
  4010–4014 and on a REST 401, identify budget of 900 per 24 h.
- Guild provisioning: operator and viewer roles, `Kandev Tasks` and `Kandev
  Archive` categories with permission overwrites, `#kandev-status`, and guild
  slash commands registered only when they change.
- One channel per task: naming, topic with deep link, category rollover at
  50, rename debounce, archive or delete on close, reopen, retention,
  `capacity_blocked` at Discord's 500-channel limit, recreation after a
  manual delete; reconcile on connect and every five minutes.
- Kandev → Discord: agent messages after a cursor, opt-in user messages
  (own messages suppressed through `Message.Source`) and tool summaries,
  fence-aware chunking with a five-chunk cap, session status lines, and a
  pinned task card.
- Discord → Kandev: operator messages delivered with `Messages().Send` and
  ✅/🕒/⚠️/⛔ reactions, never twice; catch-up of messages sent while offline.
- `/status`, `/stop` (turn via `CancelTurn`, task via `StopTaskRun`), `/link`,
  `/approve`, `/deny`, permission and workflow-step buttons, and
  autocomplete. Approvals act as the linked Kandev user through
  human-response receipts; unlinked users get a deep link.
- `connection.status` admin action and the bridge state in the settings card.
- Design docs mirrored from kdlbs/kandev and re-planned for the merged Host
  contract (kdlbs/kandev#5438).

### Changed

- Builds against the unreleased pluginsdk pseudo-version
  `v0.4.1-0.20261123163015-83cecc587a88`; `make sdk-check` blocks release until
  pluginsdk and `@kdlbs/plugin-sdk` 0.5.0 and a Kandev release ship.

### Fixed (adversarial review round 1)

- Rebinding a workspace to another server moves its tasks to new channels;
  the old server receives nothing more.
- Changing the operator role takes access away from the former role.
- A stale step-approval button can no longer approve a later step.
- Discord messages are delivered in order, after any missed ones.
- Recovery: provisioning before READY, channels deleted while offline,
  lost Host writes during channel creation, transient role-lookup errors,
  partly posted long messages, and failed card pins.
- Gateway: fatal close codes after READY stop the bridge; every IDENTIFY
  counts against the budget; a REST 401 stops the gateway at once.
- Interaction acknowledgements never wait behind bot traffic, and error
  messages never contain interaction tokens.
- A message with a very long code-fence opener can no longer hang a task.

### Fixed (adversarial review round 2)

- No task output reaches a channel before its permissions reflect the
  current operator and viewer roles.
- A closing channel waits until its final answer posted.
- An Approve button only approves the exact transition and prompt it shows.
- A message that failed live delivery is retried, in order, before later
  ones; a catch-up interrupted at reconnect finishes on the next pass.
- Deleted task categories are recreated; provisioning re-runs every pass
  and no longer races category rollover.
- A channel created before a lost save is reused even after a rename, and
  never one belonging to another task.
- A long message interrupted by a crash or a lost reply is not posted twice,
  and a replacement channel receives it whole.
- Approval prompts keep their buttons recorded until Discord confirms the
  edit.
- An invalid-session re-identify can no longer exceed the identify budget.
- `/status` and other command replies stay within Discord's 2000
  characters.
- The settings card rejects the same bindings the backend rejects.
- `#kandev-status` announces connection, outages, and provisioning failures.
- Messages from the first second of a new channel's backlog are mirrored.

### Fixed (adversarial review round 3)

- A channel adopted after a lost save, or a guild whose re-provisioning
  failed after a role change, never shows output to the former role.
- A live message whose route could not be read is retried from history
  before later messages.
- Long messages carry an enforced nonce per part, and creations are never
  resent blindly by the Discord library, so a retry cannot duplicate a
  message, channel, or role nor mistake an earlier identical line for it.
- A closing channel waits for running sessions to finish their answer (up
  to 30 minutes).
- An Approve button refreshes when the approval state changes, so an old
  button cannot approve a later visit to the same step.
- A task closed while its channel creation was unrecorded archives that
  channel; archived channels lose their prompt buttons even after a failed
  edit.
- `#kandev-status` no longer announces a reconnect while the gateway is
  still down.
- Messages split at a code fence never exceed 2000 characters.

### Fixed (adversarial review round 4)

- A post that starts after a role change is published can no longer reach
  the former role.
- The gateway never resumes a session Discord invalidated; a stalled
  handshake times out and a token halt interrupts it; an IDENTIFY is only
  sent with budget reserved (an unwritable counter refuses).
- Channels created for tasks that were archived or deleted before the
  creation was recorded are closed reliably.
- A lost channel-index write is repaired and its messages caught up.
- Buttons and commands in an archived channel change nothing.
- An answer still being written when a turn starts or ends mid-sync is not
  posted early; a first live message is not mistaken for missed history.
- The close of a task with a stuck session waits at most 30 minutes from
  the first attempt, whatever else changes on the task.
- Approving one visit of a step never clears the next visit's prompt.
- Cards and prompts are not duplicated by a retry after a lost reply.
- `#kandev-status` keeps gateway outages and provisioning failures apart.
- Code blocks are never split across their delimiter line.
