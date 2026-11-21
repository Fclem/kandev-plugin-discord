import assert from "node:assert/strict";
import test from "node:test";

let importCount = 0;

async function loadBundle() {
  let registered;
  Object.defineProperty(globalThis, "window", {
    configurable: true,
    value: {
      registerKandevPlugin(id, definition) {
        registered = { id, definition };
      },
    },
  });
  const mod = await import(new URL(`../ui/bundle.js?t=${importCount++}`, import.meta.url));
  return { mod, registered };
}

function fakeRegistry() {
  const components = [];
  return {
    components,
    registerComponent(slot, component) {
      components.push({ slot, component });
    },
  };
}

test("registers exactly one plugin-settings component under the manifest id", async () => {
  const { registered } = await loadBundle();
  assert.equal(registered.id, "kandev-plugin-discord");
  const registry = fakeRegistry();
  registered.definition.initialize(registry, { React: {}, jsx() {}, ui: {} });
  assert.deepEqual(registry.components.map((c) => c.slot), ["plugin-settings"]);
});

test("initialize/destroy can run twice without duplicate registrations", async () => {
  const { registered } = await loadBundle();
  for (let i = 0; i < 2; i++) {
    const registry = fakeRegistry();
    registered.definition.initialize(registry, { React: {}, jsx() {}, ui: {} });
    assert.equal(registry.components.length, 1);
    registered.definition.destroy();
  }
});

test("required permissions match the backend bitset", async () => {
  const { mod } = await loadBundle();
  // MANAGE_CHANNELS|ADD_REACTIONS|VIEW_CHANNEL|SEND_MESSAGES|EMBED_LINKS|
  // READ_MESSAGE_HISTORY|MANAGE_ROLES|PIN_MESSAGES, as in permissions.go.
  const expected = [4n, 6n, 10n, 11n, 14n, 16n, 28n, 51n].reduce((a, b) => a | (1n << b), 0n);
  assert.equal(mod.requiredPermissions(), expected.toString());
  assert.equal(BigInt(mod.requiredPermissions()) & 8n, 0n, "never requests ADMINISTRATOR");
});

test("invite URL preselects the bound server", async () => {
  const { mod } = await loadBundle();
  const url = new URL(mod.inviteUrl("123456789012345678", "223456789012345678"));
  assert.equal(url.searchParams.get("client_id"), "123456789012345678");
  assert.equal(url.searchParams.get("guild_id"), "223456789012345678");
  assert.equal(url.searchParams.get("scope"), "bot applications.commands");
});

test("parseBindings mirrors backend validation", async () => {
  const { mod } = await loadBundle();
  const ok = mod.parseBindings(" ws-a = 223456789012345678\r\nws-b=323456789012345678 ");
  assert.deepEqual(ok, {
    bindings: [
      { workspaceId: "ws-a", guildId: "223456789012345678" },
      { workspaceId: "ws-b", guildId: "323456789012345678" },
    ],
    problems: [],
  });
  const bad = mod.parseBindings("ws-a=general,ws-a=223456789012345678,ws-a=323456789012345678,ws-c=223456789012345678");
  assert.equal(bad.bindings.length, 1);
  assert.equal(bad.problems.length, 3);
});
