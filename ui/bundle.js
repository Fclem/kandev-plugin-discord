// kandev-plugin-discord UI bundle: a hand-written, no-build ES module served
// verbatim from the package. It registers one `plugin-settings` card that
// helps an admin connect the Discord bot:
//   - lists Kandev workspaces with their IDs (needed for `workspace_guilds`),
//   - shows each configured workspace → server binding with its invite link,
//   - runs the backend `connection.test` admin action per binding.
//
// Settings themselves are edited in the schema form Kandev renders below this
// card. Saving restarts the plugin; "Test" re-reads the saved settings.
//
// Never import React here: use host.React / host.jsx (shared host instance).

export const PLUGIN_ID = "kandev-plugin-discord";

// Must match server/discord/permissions.go RequiredPermissions.
// BigInt: bit 51 (PIN_MESSAGES) exceeds 32-bit JS bitwise operators.
const REQUIRED_PERMISSION_BITS = [4n, 6n, 10n, 11n, 14n, 16n, 28n, 51n];

export function requiredPermissions() {
  return REQUIRED_PERMISSION_BITS.reduce((acc, bit) => acc | (1n << bit), 0n).toString();
}

export function inviteUrl(applicationId, guildId) {
  const q = new URLSearchParams({
    client_id: applicationId,
    scope: "bot applications.commands",
    permissions: requiredPermissions(),
  });
  if (guildId) {
    q.set("guild_id", guildId);
    q.set("disable_guild_select", "true");
  }
  return `https://discord.com/oauth2/authorize?${q.toString()}`;
}

const SNOWFLAKE = /^[0-9]{17,20}$/;

// Mirrors server/config.ParseBindings for display; the backend stays the
// authority and reports the same problems in connection.test.
export function parseBindings(raw) {
  const bindings = [];
  const problems = [];
  const seenWorkspaces = new Set();
  const seenGuilds = new Set();
  for (const part of String(raw || "").split(/[,\r\n]+/)) {
    const entry = part.trim();
    if (!entry) continue;
    const eq = entry.indexOf("=");
    const workspaceId = eq < 0 ? "" : entry.slice(0, eq).trim();
    const guildId = eq < 0 ? "" : entry.slice(eq + 1).trim();
    if (!workspaceId || !guildId) problems.push(`"${entry}" must look like <workspace id>=<server id>`);
    else if (!SNOWFLAKE.test(guildId)) problems.push(`server id "${guildId}" is not a Discord ID`);
    else if (seenWorkspaces.has(workspaceId)) problems.push(`workspace ${workspaceId} is bound twice`);
    else if (seenGuilds.has(guildId)) problems.push(`server ${guildId} is bound to two workspaces`);
    else {
      seenWorkspaces.add(workspaceId);
      seenGuilds.add(guildId);
      bindings.push({ workspaceId, guildId });
    }
  }
  return { bindings, problems };
}

const STATUS_MARK = { pass: "✓", fail: "✗", skip: "–" };
const STATUS_CLASS = { pass: "text-green-600", fail: "text-destructive", skip: "text-muted-foreground" };

function makeSettingsCard(host) {
  const React = host.React;
  const h = host.jsx;
  const { Button, Card, CardContent, CardDescription, CardHeader, CardTitle } = host.ui;

  function useWorkspaces() {
    const read = () => (host.store.getState().workspaces?.items || []).map((w) => ({ id: w.id, name: w.name }));
    const [workspaces, setWorkspaces] = React.useState(read);
    React.useEffect(() => host.store.subscribe(() => setWorkspaces(read())), []);
    return workspaces;
  }

  async function loadConfig(signal) {
    const res = await host.api.fetch("/config", { signal });
    if (!res.ok) throw new Error(`reading settings failed (HTTP ${res.status})`);
    const body = await res.json();
    return body.config || {};
  }

  function CheckList({ result }) {
    return h(
      "ul",
      { className: "mt-2 space-y-1 text-xs" },
      result.checks.map((c) =>
        h(
          "li",
          { key: c.id, className: "flex gap-2" },
          h("span", { className: STATUS_CLASS[c.status] || "", "aria-label": c.status }, STATUS_MARK[c.status] || "?"),
          h("span", null, h("span", { className: "font-medium" }, c.id.replace(/_/g, " ")), ": ", c.detail),
        ),
      ),
      result.invite_url
        ? h(
            "li",
            { key: "invite" },
            h("a", { href: result.invite_url, target: "_blank", rel: "noopener noreferrer", className: "underline" }, "Invite the bot to this server"),
          )
        : null,
    );
  }

  function BindingRow({ binding, workspaceName, applicationId }) {
    const [state, setState] = React.useState({ running: false, result: null, error: null });
    const abortRef = React.useRef(null);
    React.useEffect(() => () => abortRef.current && abortRef.current.abort(), []);

    const runTest = async () => {
      if (abortRef.current) abortRef.current.abort();
      const controller = new AbortController();
      abortRef.current = controller;
      setState({ running: true, result: null, error: null });
      try {
        const result = await host.api.invokeAction(
          "connection.test",
          { workspaceId: binding.workspaceId },
          { signal: controller.signal },
        );
        setState({ running: false, result, error: null });
      } catch (err) {
        if (controller.signal.aborted) return;
        setState({ running: false, result: null, error: err && err.message ? err.message : String(err) });
      }
    };

    return h(
      "div",
      { className: "rounded-md border p-3" },
      h(
        "div",
        { className: "flex flex-wrap items-center justify-between gap-2" },
        h(
          "div",
          { className: "min-w-0 text-sm" },
          h("div", { className: "font-medium" }, workspaceName || binding.workspaceId),
          h("div", { className: "text-muted-foreground text-xs break-all" }, `Discord server ${binding.guildId}`),
        ),
        h(
          "div",
          { className: "flex gap-2" },
          applicationId
            ? h(
                "a",
                {
                  href: inviteUrl(applicationId, binding.guildId),
                  target: "_blank",
                  rel: "noopener noreferrer",
                  className: "inline-flex min-h-9 items-center text-xs underline",
                },
                "Invite link",
              )
            : null,
          h(
            Button,
            { type: "button", size: "sm", variant: "outline", disabled: state.running, onClick: runTest },
            state.running ? "Testing…" : "Test connection",
          ),
        ),
      ),
      state.error ? h("p", { className: "text-destructive mt-2 text-xs" }, state.error) : null,
      state.result
        ? h(
            "div",
            null,
            h(
              "p",
              { className: state.result.ok ? "mt-2 text-xs text-green-600" : "text-destructive mt-2 text-xs" },
              state.result.ok ? "Connected: every check passed." : "Some checks failed:",
            ),
            h(CheckList, { result: state.result }),
          )
        : null,
    );
  }

  return function DiscordSettingsCard() {
    const workspaces = useWorkspaces();
    const [config, setConfig] = React.useState(null);
    const [loadError, setLoadError] = React.useState(null);
    const [reloadKey, setReloadKey] = React.useState(0);

    React.useEffect(() => {
      const controller = new AbortController();
      loadConfig(controller.signal)
        .then((c) => {
          setConfig(c);
          setLoadError(null);
        })
        .catch((err) => {
          if (!controller.signal.aborted) setLoadError(err.message || String(err));
        });
      return () => controller.abort();
    }, [reloadKey]);

    const { bindings, problems } = parseBindings(config && config.workspace_guilds);
    const nameOf = (id) => (workspaces.find((w) => w.id === id) || {}).name;
    const copy = (text) =>
      navigator.clipboard
        .writeText(text)
        .then(() => host.toast.success("Copied"))
        .catch(() => host.toast.error("Copy failed"));

    return h(
      Card,
      { className: "mb-4" },
      h(
        CardHeader,
        null,
        h(CardTitle, null, "Discord connection"),
        h(
          CardDescription,
          null,
          "1. Create a Discord server and a bot (see the plugin README). ",
          "2. Fill in Bot token, Application ID and one workspace=server line per workspace below, then save. ",
          "3. Invite the bot and test each binding here.",
        ),
      ),
      h(
        CardContent,
        { className: "space-y-3" },
        loadError ? h("p", { className: "text-destructive text-xs" }, loadError) : null,
        problems.length
          ? h("ul", { className: "text-destructive list-disc pl-5 text-xs" }, problems.map((p) => h("li", { key: p }, p)))
          : null,
        bindings.length === 0 && config
          ? h("p", { className: "text-muted-foreground text-xs" }, "No workspace is bound to a Discord server yet.")
          : null,
        bindings.map((b) =>
          h(BindingRow, {
            key: `${b.workspaceId}=${b.guildId}`,
            binding: b,
            workspaceName: nameOf(b.workspaceId),
            applicationId: config && config.application_id,
          }),
        ),
        h(
          "details",
          { className: "text-xs" },
          h("summary", { className: "cursor-pointer min-h-9" }, "Workspace IDs for workspace_guilds"),
          h(
            "ul",
            { className: "mt-2 space-y-1" },
            workspaces.map((w) =>
              h(
                "li",
                { key: w.id, className: "flex flex-wrap items-center gap-2" },
                h("span", { className: "font-medium" }, w.name),
                h("code", { className: "break-all" }, w.id),
                h(Button, { type: "button", size: "sm", variant: "ghost", onClick: () => copy(`${w.id}=`) }, "Copy line"),
              ),
            ),
          ),
        ),
        h(
          Button,
          { type: "button", size: "sm", variant: "ghost", onClick: () => setReloadKey((k) => k + 1) },
          "Reload saved settings",
        ),
      ),
    );
  };
}

window.registerKandevPlugin(PLUGIN_ID, {
  initialize(registry, host) {
    registry.registerComponent("plugin-settings", makeSettingsCard(host));
  },
  destroy() {
    // No module-level state, timers, or subscriptions: components clean up
    // their own effects, and the host unregisters the slot component.
  },
});
