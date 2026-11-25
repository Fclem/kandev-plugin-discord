---
id: "06-plugin-gateway-provisioning"
title: "Gateway lifecycle and guild provisioning"
status: pending
wave: 2
depends_on: ["05-plugin-bootstrap-config"]
plan: "plan.md"
requirements:
  - REQ-INTEGRATIONS-DISCORD-002
acceptance_criteria:
  - AC-INTEGRATIONS-DISCORD-002.1
  - AC-INTEGRATIONS-DISCORD-002.2
  - AC-INTEGRATIONS-DISCORD-002.3
system_design:
  - system-design.md
---

# Task 06: Gateway lifecycle and guild provisioning

Repository: `Fclem/kandev-plugin-discord`. Use `/tdd`.

- **Acceptance:**
  1. **Start.** `SetHost` starts the bridge once: read config once; with a
     valid config, open the gateway with intents
     `GUILDS | GUILD_MESSAGES | MESSAGE_CONTENT`. An invalid config leaves
     the bridge idle and is reported by `connection.status`.
  2. **Supervisor** (`server/gateway`): discordgo's own reconnect is off; the
     supervisor reopens with jittered exponential backoff (1 s → 2 min).
     Close codes `4004`, `4013`, `4014` (and `4010`–`4012`) stop it and record
     a fatal state with the code and a token-free reason. A REST 401 from
     `discordapi` stops it too.
  3. **Identify budget.** Each `READY` is recorded in `instance/""/identify_budget`
     (hourly buckets); before each open the supervisor refuses to connect
     when the last 24 h hold 900 or more identifies, and retries when the
     window frees.
  4. **Store** (`server/store`): typed records for the system design's data
     model (guild, channel, prompts, step approval, health, identify budget,
     `channel:<id>` reverse index) over `GetState/SetState/DeleteState/ListState`.
  5. **Provisioning** (`server/provision`) on `GUILD_CREATE` of a bound guild:
     creates or reuses the operator role, the optional viewer role, the
     `Kandev Tasks` and `Kandev Archive` categories with the overwrites from
     the system design's "Permissions", and `#kandev-status`; registers the
     guild commands only when `commands_hash` changes; stores
     `workspace/<id>/guild`. A second run makes zero create calls.
  6. **Membership.** On `READY`, each bound guild missing from the guild list
     is logged with its invite URL and recorded in the health record. Events
     from unbound guilds are ignored.
- **Verification:** `make check-format vet test` (from the plugin root, after `npm ci --ignore-scripts`; see the plan's "SDK gate" for why `make sdk-check` fails until SDK 0.5.0 ships); `go test ./server/gateway/... ./server/store/... ./server/provision/...`.
- **Files:** `server/gateway/*`, `server/store/*`, `server/provision/*`, `server/commands/defs.go`, `server/bridge/bridge.go`, `server/plugin.go`.
- **Dependencies:** task-05.
- **Risks:** the host may inject `Host` late (`Serve` retries for 30 s); there
  is no shutdown hook, so each process start re-identifies (budgeted).
- **Output contract:** summary, files changed, tests run, blockers, risks, and task/plan status update.

## Results

Pending.
