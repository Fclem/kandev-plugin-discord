# kandev-plugin-discord

A [Kandev](https://github.com/kdlbs/kandev) runtime plugin that bridges Kandev
tasks and Discord:

- **One Discord text channel per Kandev task**, created when the task is
  created and archived (or deleted) when the task is archived.
- **Two-way chat.** Agent messages and status updates are posted to the
  task's channel. Messages that operators post in the channel are delivered to
  the task as user messages, following Kandev's queue and running-turn rules.
- **Slash commands** `/status`, `/stop`, `/approve`, and `/deny`.

> **Status: early scaffold (v0.1.0).**
>
> | Working today | Still to come |
> |---|---|
> | Settings and validation | The channel bridge |
> | The settings-page connection test (bot token, application, Message Content intent, server membership, permissions) | Slash commands |
>
> The design and phased plan are in [`docs/design/`](docs/design/). The
> `/stop` (turn) and `/approve` commands depend on Kandev Host API additions
> proposed upstream; see the design spec, section "Host API additions
> requested".

## Requirements

- A Kandev instance at the version in `manifest.yaml`
  (`min_kandev_version`) or newer, where you are an **admin**: plugin
  settings are admin-only.
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

Every line should show ✓.

### 9. Decide who may control tasks from Discord

Members with the **Kandev Operator** role (configurable), or whose user IDs
appear in **Extra operator user IDs**, can message tasks and run `/stop` and
`/approve`. Everyone else can only read channels they can see. Anyone who can
assign that role in your server can control the bound workspace's tasks, so
keep `Manage Roles` limited to people you trust.

## Security notes

- The token and server admin rights are equivalent to task control in the
  bound Kandev workspace.
- Agent output leaves Kandev and is posted to Discord, a third-party service.
  Agent thinking is never posted, and tool-call summaries are off by default.
- Bot messages disable mention parsing, so agent text cannot ping
  `@everyone`.
- The plugin exposes no public webhook. Discord traffic uses an outbound
  gateway connection, and the settings card uses a Kandev admin-only action.

## Development

Requirements: Go 1.26 and Node 22+.

```sh
npm ci --ignore-scripts
make sdk-check check-format vet test build
```

Packaging needs a Kandev source checkout for the backend-owned
`plugin-pack` tool, pinned by `.kandev-tooling-ref`:

```sh
git clone --filter=blob:none --no-checkout https://github.com/kdlbs/kandev.git .build/kandev-tooling
git -C .build/kandev-tooling sparse-checkout set apps/backend pluginsdk
git -C .build/kandev-tooling checkout "$(cat .kandev-tooling-ref)"
make verify-package-host   # host platform only
make verify-package        # all five platforms
```

Install the archive into a **disposable** Kandev instance and a test Discord
server, never your primary ones. Bump `version` in both `manifest.yaml` and
`Makefile` before reinstalling, because Kandev rejects reinstalling the same
version.

Layout:

```
manifest.yaml        plugin contract (id, actions, config schema)
server/              Go backend (pluginsdk v0.1.0)
  config/            settings parsing and validation
  discord/           minimal Discord REST client, permissions, invite URL
  health/            connection test
ui/bundle.js         settings card (no build step)
tests/               UI contract tests (node --test)
docs/design/         spec, implementation plan, and task files
scripts/             package and release verifiers
```

## License

MIT, see [LICENSE](LICENSE).
