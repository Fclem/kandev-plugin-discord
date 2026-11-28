// Renders the settings card with a minimal hook runtime (useState,
// useEffect, useRef) and a fake Kandev host, so the card's behavior is
// tested without React or a browser.
import assert from "node:assert/strict";
import test from "node:test";

// The bundle registers itself once on import; the card factory it registers
// builds an independent card per initialize call.
let loaded;

function loadDefinition() {
  loaded ??= (async () => {
    let definition;
    Object.defineProperty(globalThis, "window", {
      configurable: true,
      value: {
        registerKandevPlugin(_id, d) {
          definition = d;
        },
      },
    });
    const mod = await import(new URL("../ui/bundle.js", import.meta.url));
    return { mod, definition };
  })();
  return loaded;
}

function flatten(children) {
  return children.flat(Infinity).filter((c) => c !== null && c !== undefined && c !== false);
}

// createRuntime returns React/jsx for the bundle and a renderer whose tree
// is plain { tag, props, children } nodes (strings for text).
function createRuntime() {
  const instances = new Map();
  let current = null;
  let hookIndex = 0;
  let dirty = false;
  let effects = [];

  const React = {
    useState(init) {
      const inst = current;
      const i = hookIndex++;
      if (!(i in inst.hooks)) inst.hooks[i] = { value: typeof init === "function" ? init() : init };
      const slot = inst.hooks[i];
      const set = (v) => {
        const next = typeof v === "function" ? v(slot.value) : v;
        if (!Object.is(next, slot.value)) {
          slot.value = next;
          dirty = true;
        }
      };
      return [slot.value, set];
    },
    useEffect(fn, deps) {
      const inst = current;
      const i = hookIndex++;
      const slot = inst.hooks[i] || (inst.hooks[i] = { deps: undefined, cleanup: undefined, ran: false });
      const changed = !slot.ran || !deps || deps.some((d, j) => !Object.is(d, slot.deps[j]));
      if (!changed) return;
      slot.ran = true;
      slot.deps = deps;
      effects.push(() => {
        if (typeof slot.cleanup === "function") slot.cleanup();
        slot.cleanup = fn();
      });
    },
    useRef(init) {
      const inst = current;
      const i = hookIndex++;
      if (!(i in inst.hooks)) inst.hooks[i] = { current: init };
      return inst.hooks[i];
    },
  };

  const jsx = (type, props, ...children) => ({ type, props: props || {}, children: flatten(children) });

  function render(el, path, seen) {
    if (el === null || el === undefined || el === false) return null;
    if (typeof el === "string" || typeof el === "number") return String(el);
    if (typeof el.type === "function") {
      const key = `${path}/${el.type.name}:${el.props.key ?? ""}`;
      let inst = instances.get(key);
      if (!inst) instances.set(key, (inst = { hooks: [] }));
      seen.add(key);
      const prev = [current, hookIndex];
      current = inst;
      hookIndex = 0;
      const out = el.type({ ...el.props, children: el.children });
      [current, hookIndex] = prev;
      return render(out, key, seen);
    }
    const children = el.children.map((c, i) => render(c, `${path}/${el.type}[${c?.props?.key ?? i}]`, seen));
    return { tag: el.type, props: el.props, children: children.filter((c) => c !== null) };
  }

  function cleanup(key) {
    for (const slot of instances.get(key).hooks) {
      if (slot && typeof slot.cleanup === "function") slot.cleanup();
    }
    instances.delete(key);
  }

  let root = null;
  let tree = null;
  function rerender() {
    dirty = false;
    const seen = new Set();
    tree = root ? render(root, "", seen) : null;
    for (const key of [...instances.keys()]) if (!seen.has(key)) cleanup(key);
    const run = effects;
    effects = [];
    run.forEach((e) => e());
  }

  return {
    React,
    jsx,
    mount(el) {
      root = el;
      rerender();
    },
    unmount() {
      root = null;
      rerender();
    },
    // settle lets pending promises resolve and re-renders until stable.
    async settle() {
      for (let idle = 0; idle < 3; ) {
        await new Promise((r) => setImmediate(r));
        if (dirty) {
          rerender();
          idle = 0;
        } else {
          idle++;
        }
      }
    },
    tree: () => tree,
  };
}

function nodes(tree, pred, out = []) {
  if (!tree || typeof tree === "string") return out;
  if (pred(tree)) out.push(tree);
  tree.children.forEach((c) => nodes(c, pred, out));
  return out;
}

function text(node) {
  if (!node) return "";
  if (typeof node === "string") return node;
  return node.children.map(text).join("");
}

const byTag = (tag) => (n) => n.tag === tag;
const button = (tree, label) => nodes(tree, (n) => n.tag === "Button" && text(n) === label);

const ui = Object.fromEntries(["Button", "Card", "CardContent", "CardDescription", "CardHeader", "CardTitle"].map((n) => [n, n]));

// fakeHost answers /config and the two admin actions, and records calls.
function fakeHost(rt, { config = {}, configStatus = 200, workspaces = [], status = {}, test: testAction } = {}) {
  const calls = { fetch: [], actions: [], toasts: [] };
  let listeners = [];
  let state = { workspaces: { items: workspaces } };
  const host = {
    React: rt.React,
    jsx: rt.jsx,
    ui,
    store: {
      getState: () => state,
      subscribe(fn) {
        listeners.push(fn);
        return () => {
          listeners = listeners.filter((l) => l !== fn);
        };
      },
    },
    api: {
      async fetch(path, { signal }) {
        calls.fetch.push({ path, signal });
        return { ok: configStatus === 200, status: configStatus, json: async () => ({ config }) };
      },
      invokeAction(key, context, { signal }) {
        calls.actions.push({ key, context, signal });
        if (key === "connection.status") {
          const s = status[context.workspaceId];
          return s instanceof Error ? Promise.reject(s) : Promise.resolve(s ?? null);
        }
        return testAction(context);
      },
    },
    toast: {
      success: (m) => calls.toasts.push(["success", m]),
      error: (m) => calls.toasts.push(["error", m]),
    },
  };
  const setWorkspaces = (items) => {
    state = { workspaces: { items } };
    listeners.forEach((l) => l());
  };
  return { host, calls, setWorkspaces, listeners: () => listeners.length };
}

async function mountCard(options) {
  const rt = createRuntime();
  const { mod, definition } = await loadDefinition();
  const fake = fakeHost(rt, options);
  let Card;
  definition.initialize({ registerComponent: (_slot, c) => (Card = c) }, fake.host);
  rt.mount(rt.jsx(Card, null));
  await rt.settle();
  return { rt, mod, ...fake };
}

const WS = "ws-1";
const GUILD = "223456789012345678";
const APP = "123456789012345678";

test("lists each binding with its workspace name, invite link, and cached bridge state", async () => {
  const { rt, mod, calls } = await mountCard({
    config: { application_id: APP, workspace_guilds: `${WS}=${GUILD}` },
    workspaces: [{ id: WS, name: "Platform" }, { id: "ws-2", name: "Docs" }],
    status: { [WS]: { state: "connected", bot_member: true, provisioned: true } },
  });
  const tree = rt.tree();
  assert.deepEqual(calls.fetch.map((c) => c.path), ["/config"]);
  assert.deepEqual(
    calls.actions.map((a) => [a.key, a.context.workspaceId]),
    [["connection.status", WS]],
  );
  const all = text(tree);
  assert.match(all, /Platform/);
  assert.match(all, new RegExp(`Discord server ${GUILD}`));
  assert.match(all, /Bridge connected; server provisioned\./);
  const invite = nodes(tree, (n) => n.tag === "a" && text(n) === "Invite link");
  assert.equal(invite.length, 1);
  assert.equal(invite[0].props.href, mod.inviteUrl(APP, GUILD));
  assert.equal(invite[0].props.rel, "noopener noreferrer");
  assert.equal(nodes(tree, byTag("code")).map(text).join(","), `${WS},ws-2`, "every workspace ID is listed");
});

test("shows binding problems and the empty state", async () => {
  const { rt } = await mountCard({ config: { workspace_guilds: "" } });
  const all = text(rt.tree());
  assert.match(all, /workspace_guilds needs at least one/);
  assert.match(all, /No workspace is bound to a Discord server yet\./);
});

test("binding problems are listed and invalid lines get no row", async () => {
  const { rt, calls } = await mountCard({ config: { application_id: APP, workspace_guilds: `${WS}=nope` } });
  assert.match(text(rt.tree()), /server id "nope" is not a Discord ID/);
  assert.equal(calls.actions.length, 0);
  assert.equal(button(rt.tree(), "Test connection").length, 0);
});

test("a failed settings read is shown and Reload retries", async () => {
  const opts = { configStatus: 500 };
  const { rt, calls } = await mountCard(opts);
  assert.match(text(rt.tree()), /reading settings failed \(HTTP 500\)/);

  button(rt.tree(), "Reload saved settings")[0].props.onClick();
  await rt.settle();
  assert.equal(calls.fetch.length, 2, "reload fetches again");
});

test("without an application ID there is no invite link", async () => {
  const { rt } = await mountCard({ config: { workspace_guilds: `${WS}=${GUILD}` } });
  assert.equal(nodes(rt.tree(), (n) => n.tag === "a" && text(n) === "Invite link").length, 0);
  assert.match(text(rt.tree()), /Bridge has not reported yet/, "no cached state yet");
  assert.match(text(rt.tree()), new RegExp(WS), "an unknown workspace shows its ID");
});

test("Test connection shows the checks and refreshes the bridge state", async () => {
  let resolveTest;
  const { rt, calls } = await mountCard({
    config: { application_id: APP, workspace_guilds: `${WS}=${GUILD}` },
    status: { [WS]: new Error("not yet") },
    test: () => new Promise((r) => (resolveTest = r)),
  });
  assert.match(text(rt.tree()), /Bridge has not reported yet/, "a failed status read falls back");

  button(rt.tree(), "Test connection")[0].props.onClick();
  await rt.settle();
  const testing = button(rt.tree(), "Testing…");
  assert.equal(testing.length, 1);
  assert.equal(testing[0].props.disabled, true, "one test at a time");
  assert.deepEqual(calls.actions.at(-1).context, { workspaceId: WS });

  resolveTest({
    ok: false,
    invite_url: "https://discord.com/oauth2/authorize?client_id=1",
    checks: [
      { id: "bot_token", status: "pass", detail: "valid" },
      { id: "guild_member", status: "fail", detail: "the bot is not in the server" },
      { id: "intent", status: "skip", detail: "skipped" },
    ],
    gateway: { state: "connected", bot_member: false },
  });
  await rt.settle();
  const tree = rt.tree();
  const all = text(tree);
  assert.match(all, /Some checks failed:/);
  assert.match(all, /bot token: valid/);
  assert.match(all, /guild member: the bot is not in the server/);
  assert.deepEqual(
    nodes(tree, (n) => n.tag === "span" && n.props["aria-label"]).map((n) => [n.props["aria-label"], text(n)]),
    [["pass", "✓"], ["fail", "✗"], ["skip", "–"]],
  );
  assert.equal(nodes(tree, (n) => n.tag === "a" && text(n) === "Invite the bot to this server")[0].props.href, "https://discord.com/oauth2/authorize?client_id=1");
  assert.match(all, /Bridge connected, but the bot is not in this server\./, "the test result updates the bridge state");
  assert.equal(button(tree, "Test connection")[0].props.disabled, false);
});

test("a passing test needs no invite link", async () => {
  const { rt } = await mountCard({
    config: { workspace_guilds: `${WS}=${GUILD}` },
    test: async () => ({ ok: true, checks: [{ id: "bot_token", status: "pass", detail: "valid" }] }),
  });
  button(rt.tree(), "Test connection")[0].props.onClick();
  await rt.settle();
  const all = text(rt.tree());
  assert.match(all, /Connected: every check passed\./);
  assert.equal(nodes(rt.tree(), (n) => n.tag === "a" && text(n) === "Invite the bot to this server").length, 0);
});

test("a failed test shows its error and a restarted test aborts the first", async () => {
  const signals = [];
  const { rt } = await mountCard({
    config: { workspace_guilds: `${WS}=${GUILD}` },
    test: () => Promise.reject(new Error("HTTP 503")),
  });
  button(rt.tree(), "Test connection")[0].props.onClick();
  await rt.settle();
  assert.match(text(rt.tree()), /HTTP 503/);

  const { rt: rt2, calls } = await mountCard({
    config: { workspace_guilds: `${WS}=${GUILD}` },
    test: () => new Promise(() => {}),
  });
  const run = button(rt2.tree(), "Test connection")[0].props.onClick;
  run();
  run();
  await rt2.settle();
  for (const a of calls.actions.filter((a) => a.key === "connection.test")) signals.push(a.signal);
  assert.equal(signals.length, 2);
  assert.equal(signals[0].aborted, true, "the superseded test is aborted");
  assert.equal(signals[1].aborted, false);

  rt2.unmount();
  assert.equal(signals[1].aborted, true, "unmounting aborts the running test");
});

test("a non-Error rejection is shown as text", async () => {
  const { rt } = await mountCard({
    config: { workspace_guilds: `${WS}=${GUILD}` },
    test: () => Promise.reject("offline"),
  });
  button(rt.tree(), "Test connection")[0].props.onClick();
  await rt.settle();
  assert.match(text(rt.tree()), /offline/);
});

test("Copy line copies a workspace_guilds prefix", async () => {
  const written = [];
  let fail = false;
  Object.defineProperty(globalThis.navigator, "clipboard", {
    configurable: true,
    value: { writeText: (t) => (fail ? Promise.reject(new Error("denied")) : (written.push(t), Promise.resolve())) },
  });
  const { rt, calls } = await mountCard({ config: {}, workspaces: [{ id: WS, name: "Platform" }] });
  button(rt.tree(), "Copy line")[0].props.onClick();
  await rt.settle();
  assert.deepEqual(written, [`${WS}=`]);
  assert.deepEqual(calls.toasts, [["success", "Copied"]]);

  fail = true;
  button(rt.tree(), "Copy line")[0].props.onClick();
  await rt.settle();
  assert.deepEqual(calls.toasts.at(-1), ["error", "Copy failed"]);
});

test("workspace names follow the store and unmount cleans up", async () => {
  const { rt, calls, setWorkspaces, listeners } = await mountCard({
    config: { workspace_guilds: `${WS}=${GUILD}` },
    workspaces: [],
  });
  assert.equal(listeners(), 1);
  setWorkspaces([{ id: WS, name: "Renamed" }]);
  await rt.settle();
  assert.match(text(rt.tree()), /Renamed/);

  rt.unmount();
  assert.equal(listeners(), 0, "the store subscription ends");
  assert.ok(calls.fetch.every((c) => c.signal.aborted), "the settings read is aborted");
  assert.ok(calls.actions.every((a) => a.signal.aborted), "the status read is aborted");
});

test("gatewaySummary covers every state", async () => {
  const { mod } = await loadDefinition();
  assert.deepEqual(mod.gatewaySummary({ state: "connected", bot_member: true, error: "provisioning failed: 503" }), {
    tone: "error",
    text: "Bridge connected; provisioning failed: 503",
  });
  assert.deepEqual(mod.gatewaySummary({ state: "connected", bot_member: true }), {
    tone: "warn",
    text: "Bridge connected; provisioning the server…",
  });
  assert.deepEqual(mod.gatewaySummary({ state: "reconnecting", detail: "close 1006" }), {
    tone: "warn",
    text: "Bridge reconnecting — close 1006",
  });
  assert.deepEqual(mod.gatewaySummary({ state: "token_rejected" }), { tone: "error", text: "Bridge token rejected" });
  assert.equal(mod.inviteUrl(APP, "").includes("guild_id"), false, "no server preselected");
  assert.deepEqual(mod.parseBindings(undefined).bindings, []);
  assert.equal(mod.gatewaySummary({ state: "fatal", close_code: 4014 }).tone, "error");
  assert.equal(mod.gatewaySummary({ state: "fatal", close_code: 4004 }).tone, "error");
  assert.match(mod.gatewaySummary({ state: "fatal", close_code: 4013 }).text, /^Bridge fatal/);
});

test("destroy leaves registered cards working", async () => {
  const { definition } = await loadDefinition();
  const rt = createRuntime();
  const fake = fakeHost(rt, { config: { workspace_guilds: `${WS}=${GUILD}` } });
  let Card;
  definition.initialize({ registerComponent: (_slot, c) => (Card = c) }, fake.host);
  definition.destroy();
  rt.mount(rt.jsx(Card, null));
  await rt.settle();
  assert.match(text(rt.tree()), /Discord server 223456789012345678/, "no module state was torn down");
});
