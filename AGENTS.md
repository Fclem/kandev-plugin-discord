# Plugin repository workflow

This repository owns only the `kandev-plugin-discord` Kandev runtime plugin.
Follow the parent project's create-kandev-plugin workflow and public authoring
guide (`kdlbs/kandev`: `.agents/skills/create-kandev-plugin/SKILL.md`,
`docs/public/plugins-authoring.md`).

The design lives in `docs/design/`:

- `spec.md` is the behavior contract.
- `plan.md` and the `task-*.md` files are the phased implementation.

Host tasks 01–04 and 15–17 belong to `kdlbs/kandev`. Do not implement them
here. Plugin tasks 05–14 belong to this repository.

Keep `id` synchronized across `manifest.yaml`, `go.mod`, `Makefile` `BIN`, and
`window.registerKandevPlugin(...)` in `ui/bundle.js`. Keep `version` equal in
`manifest.yaml` and `Makefile`. Declare only the capabilities and actions the
code exercises.

Go and frontend SDKs share version `0.1.0`
(`github.com/kdlbs/kandev/pluginsdk v0.1.0`, `@kdlbs/plugin-sdk` `0.1.0`).
Do not add SDK `replace` directives or sibling-source builds. Raise the SDK
and `min_kandev_version` together when adopting new Host APIs.

From the plugin root, with Go 1.26 and Node 22+:

```sh
npm ci --ignore-scripts
make sdk-check check-format vet test build
```

Packaging needs a Kandev checkout at `.kandev-tooling-ref` in
`.build/kandev-tooling` (see the README), then run `make verify-package-host` or
`make verify-package`. Smoke-test artifacts only in a disposable Kandev
instance and a test Discord server.

Never log config objects, the bot token, or message content. Never commit real
tokens.

Do not merge, publish releases, change catalog entries, or contact external
authors without an explicit instruction.
