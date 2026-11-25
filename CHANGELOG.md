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
