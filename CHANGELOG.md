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
- Design docs: spec, implementation plan, and task files.
