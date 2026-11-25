# kandev-plugin-discord

A [Kandev](https://github.com/kdlbs/kandev) runtime plugin that bridges Kandev
tasks and Discord:

- **One Discord text channel per Kandev task**, created when the task is
  created and archived (or deleted) when the task is archived or reaches
  the configured close state.
- **Two-way chat.** Agent messages, status lines, and a pinned task card are
  posted to the task's channel. Messages that operators post in the channel
  are delivered to the task as user messages, following Kandev's queue and
  running-turn rules.
- **Slash commands** `/status`, `/stop`, `/approve`, `/deny`, and `/link`,
  plus buttons on permission requests and workflow review steps. Approvals
  act as the Kandev user who linked their Discord account.

> **Status: feature-complete, not released (v0.1.0).** The plugin uses Host
> APIs merged in [kdlbs/kandev#5438](https://github.com/kdlbs/kandev/pull/5438)
> (`CancelTurn`, linked accounts, step approval, `Message.Source`). No Kandev
> release and no published plugin SDK (0.5.0) contain them yet, so this
> repository builds against an SDK pseudo-version and runs only on a Kandev
> built from `main` (≥ `83cecc58`). See
> [`docs/design/plan.md`](docs/design/plan.md) "SDK gate".

## Requirements

- A Kandev instance that contains kdlbs/kandev#5438 (today: a build from
  `main`), where you are an **admin**: plugin settings are admin-only.
- A Kandev workspace manager, to grant the plugin's Host access (step 10).
- A Discord account that can create servers and applications.

## Discord setup, step by step

### 1. Create (or pick) the Discord server

Discord no longer lets bots create servers (the Create Guild API was removed
for apps on 2025-07-15), so you create it yourself:

1. In the Discord app, click **+** (Add a Server) in the server list, then
   **Create My Own** → **For me and my friends** (or a club or community).
2. Name it, for example `Kandev – <workspace name>`, and click **Create**.

Use one server per Kandev workspace: a workspace binds to exactly one server,
and a server serves exactly one workspace.

### 2. Turn on Developer Mode to copy IDs

In Discord, open **User Settings → Advanced** and enable **Developer Mode**.
You can now right-click a server icon and choose **Copy Server ID**.

### 3. Create the application and its bot

1. Open the [Discord Developer Portal](https://discord.com/developers/applications)
   and click **New Application**. Name it (for example `Kandev`), accept the
   terms, and click **Create**.
2. On **General Information**, copy the **Application ID**. This is the
   plugin's `Application ID` setting.
3. Open the **Bot** page:
   - Click **Reset Token**, confirm, and copy the token. This is the plugin's
     `Bot token` setting. Discord shows the token only once, and anyone who
     has it controls the bot, so never paste it in chat or commit it.
   - Turn **off** **Public Bot**, so only you can add it to servers.
   - Leave **Requires OAuth2 Code Grant** off.

### 4. Enable the Message Content intent

Still on the **Bot** page, under **Privileged Gateway Intents**, enable
**Message Content Intent** and click **Save Changes**.

Without it, Discord withholds the text of channel messages and the bot cannot
relay them to Kandev. Bots in fewer than 100 servers only need this toggle.
Discord requires verification and an intent review only at 100 or more
servers, and a bot dedicated to one Kandev instance stays far below that.

### 5. Invite the bot with the needed permissions

Use this URL, replacing `<APPLICATION_ID>` (the settings page also shows a
ready-made link for each bound server):

```
https://discord.com/oauth2/authorize?client_id=<APPLICATION_ID>&scope=bot%20applications.commands&permissions=2251800082205776
```

Open it, choose your server, keep every listed permission checked, and click
**Authorize**.

| Permission | Why |
|---|---|
| Manage Channels | Create, rename, and archive one channel per task, plus the categories |
| Manage Roles | Create the `Kandev Operator` role and set channel permission overwrites |
| View Channels, Send Messages, Read Message History | Read and post in task channels |
| Add Reactions | Acknowledge delivered (✅), queued (🕒), or rejected (⚠️/⛔) messages |
| Embed Links | Task cards and links |
| Pin Messages | Pin the task card |

The scope `applications.commands` lets the bot register its slash commands.
The bot never asks for **Administrator**.

If the bot's role sits below roles it must manage, open **Server Settings →
Roles** and drag its role above the `Kandev Operator` role.

### 6. Install the plugin in Kandev

1. Download `kandev-plugin-discord-<version>.tar.gz` from this repository's
   Releases, or build it (see [Development](#development)).
2. In Kandev, open **Settings → Plugins → Install** and upload the archive.

### 7. Put the token and IDs into the plugin settings

Open **Settings → Plugins → Discord**:

| Setting | Value |
|---|---|
| **Bot token** | The token from step 3. It is stored in Kandev's encrypted vault and masked everywhere except inside the plugin process. |
| **Application ID** | From step 3. |
| **Workspace → server bindings** | One `<kandev workspace id>=<discord server id>` line per workspace, for example `2bca24ab-c6c5-4b78-9671-2e3404fa9871=1234567890123456789`. The settings card lists your workspaces with a **Copy line** button that copies `<workspace id>=`, and the server ID comes from step 2. |

Click **Save**. Saving restarts the plugin.

### 8. Test the connection

At the top of the same settings page, the **Discord connection** card shows
each binding. Click **Test connection**. It checks:

- the bot token (a 401 means it was reset or mistyped);
- that the token belongs to the configured application;
- the Message Content intent (step 4);
- that the bot is a member of the bound server; if not, the card shows the
  invite link;
- the required permissions; missing ones are listed by name.

Every line should show ✓. Below the button, each binding also shows the
bridge's cached state (connected and provisioned, or why it stopped, for
example `4014` when the Message Content intent is off). Once connected, the
plugin creates the `Kandev Operator` role, the `Kandev Tasks` and `Kandev
Archive` categories, a `#kandev-status` channel, and the slash commands, then
one channel per open task.

### 9. Decide who may control tasks from Discord

Members with the **Kandev Operator** role (configurable), or whose user IDs
appear in **Extra operator user IDs**, can message tasks and run `/stop`,
`/approve`, and `/deny`. Everyone else can only read channels they can see,
and `/status` works for anyone in a task channel. Anyone who can assign that
role in your server can chat with and stop the bound workspace's tasks, so
keep `Manage Roles` limited to people you trust.

### 10. Grant Host access (workspace manager)

In Kandev, open **Settings → Plugins → Discord → Workspace access** for each
bound workspace and approve the requested capabilities:

| Grant | Enables |
|---|---|
| `host.v2.read:sessions`, `host.v2.write:execution` | `/stop` (turn or task) and step approvals |
| `host.v2.read:interactions`, `host.v2.write:interactions` | Answering permission requests |
| `host.v2.write:human_responses` | Account linking and approvals by linked users |

Without them, chat and `/status` still work; `/stop` and approvals reply that a
workspace manager must grant access, and nothing changes in Kandev.

### 11. Link your Discord account (each approver)

Approvals from Discord always act as a real Kandev user. Each person who wants
to approve from Discord:

1. Opens **Settings → Plugins → Discord → Linked chat accounts** in Kandev and
   generates a code (valid ten minutes, single use).
2. Runs `/link <code>` in any channel of the bound Discord server.

The reply confirms the Kandev user. The user or an admin can revoke the link
on the same card. An operator who is not linked gets a link to the request in
Kandev instead of an action. The linked Kandev user also needs session control
in the workspace.

## Using it

| In a task channel | Effect |
|---|---|
| Post a message (operator) | Delivered to the task: ✅ sent or started, 🕒 queued behind the running turn, ⚠️ rejected with a reason, ⛔ not an operator |
| `/status` | Task state, workflow step, sessions, queued messages, pending requests, link |
| `/stop` (`scope: turn`, default) | Cancels the current turn; the session stays open |
| `/stop scope: task` | Stops every session's run |
| Buttons on a 🔐 permission request, `/approve`, `/deny` | Answers the request as your linked Kandev user |
| **Approve** on a ⏸️ step message, or `/approve` | Approves the workflow review step like Kandev's Approve button |

Messages posted while the bridge was offline are delivered when it reconnects
if they are younger than **Deliver missed Discord messages up to (minutes)**;
older ones get ⚠️ and are not delivered. Clarification questions are not
answerable from Discord; the channel links to them in Kandev.

## Security notes

- The token and server admin rights are equivalent to chat and `/stop`
  control over the bound Kandev workspace.
- Approvals and answers act only for a linked Kandev user with session
  control, need the workspace grant, and are audited in Kandev as "<user> via
  discord".
- Agent output leaves Kandev and is posted to Discord, a third-party service.
  Agent thinking is never posted, and tool-call summaries are off by default.
- Bot messages disable mention parsing, so agent text cannot ping
  `@everyone`, roles, or users.
- The plugin exposes no public webhook. Discord traffic uses an outbound
  gateway connection, and the settings card uses Kandev admin-only actions.
- The plugin never logs the token, its settings, or message content.

## Known limitations

- Gateway-driven work (chat, commands, reconcile) has no Kandev request
  context. On an instance with `workspace_collaboration` enabled, Kandev
  denies those calls; the plugin reports the error.
- The plugin cannot read a task's management claim, so `/stop` on a task
  claimed by another plugin answers "managed elsewhere".

## Development

Requirements: Go 1.26 and Node 22+.

```sh
npm ci --ignore-scripts
make check-format vet test build SDK_GATE=
make sdk-check   # fails until pluginsdk 0.5.0 is published (release gate)
```

`SDK_GATE=` skips the SDK compatibility check for local builds while
`go.mod` pins the unreleased pluginsdk pseudo-version; CI and the release
workflow always run `make sdk-check`.

Packaging needs a Kandev source checkout for the backend-owned
`plugin-pack` tool, pinned by `.kandev-tooling-ref`:

```sh
git clone --filter=blob:none --no-checkout https://github.com/kdlbs/kandev.git .build/kandev-tooling
git -C .build/kandev-tooling sparse-checkout set apps/backend pluginsdk
git -C .build/kandev-tooling checkout "$(cat .kandev-tooling-ref)"
make verify-package-host SDK_GATE=   # host platform only
make verify-package SDK_GATE=        # all five platforms
```

Install the archive into a **disposable** Kandev instance and a test Discord
server, never your primary ones. Bump `version` in both `manifest.yaml` and
`Makefile` before reinstalling, because Kandev rejects reinstalling the same
version.

Layout:

```
manifest.yaml        plugin contract (id, capabilities, actions, config schema)
server/              Go backend (pluginsdk pseudo-version of 0.5.0)
  bridge/            wiring: per-task scheduler, events, gateway handlers, reconcile
  config/            settings parsing and validation
  discordapi/        Discord REST seam (discordgo), permissions, fake
  gateway/           gateway supervisor: close codes, identify budget
  store/             Host state records
  kandev/            Host facade and fake
  provision/         roles, categories, status channel, commands
  channels/          task channel lifecycle
  outbound/          Kandev → Discord mirroring and task card
  inbound/           Discord → Kandev delivery
  prompts/           permission and step approval prompts
  commands/          slash commands, buttons, autocomplete
  authz/             operator checks
  health/            connection test and cached status
ui/bundle.js         settings card (no build step)
tests/               UI contract tests (node --test)
docs/design/         requirements, system design, plan, and task files
scripts/             package and release verifiers
```

## License

MIT, see [LICENSE](LICENSE).
