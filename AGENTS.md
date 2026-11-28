# Plugin repository workflow

This repository owns only the `kandev-plugin-discord` Kandev runtime plugin.
Follow the parent project's create-kandev-plugin workflow and public authoring
guide (`kdlbs/kandev`: `.agents/skills/create-kandev-plugin/SKILL.md`,
`docs/public/plugins-authoring.md`).

The design lives in `docs/design/`:

- `requirements.md` is the behavior contract and `system-design.md` the
  plugin design; both mirror `kdlbs/kandev`, which stays canonical.
- `host-controls-*.md` and the ADR describe the Host contract the plugin uses.
- `plan.md` and the `task-*.md` files are the phased implementation.

Host tasks 01–04 and 15–17 belong to `kdlbs/kandev` and are merged
(kdlbs/kandev#5438). Do not implement them here. Plugin tasks 05–14 belong to
this repository.

Keep `id` synchronized across `manifest.yaml`, `go.mod`, `Makefile` `BIN`, and
`window.registerKandevPlugin(...)` in `ui/bundle.js`. Keep `version` equal in
`manifest.yaml` and `Makefile`. Declare only the capabilities and actions the
code exercises.

SDK state: `go.mod` pins `github.com/kdlbs/kandev/pluginsdk v0.5.0` and
`package.json` pins `@kdlbs/plugin-sdk` `0.5.0`; `min_kandev_version` is
`0.104.0`, the first Kandev release with #5438. Raise both SDKs together and
keep `make sdk-check` passing. Do not add SDK `replace` directives or
sibling-source builds.

From the plugin root, with Go 1.26 and Node 22+:

```sh
npm ci --ignore-scripts
make check-format vet test sdk-check build
```

Packaging needs a Kandev checkout at `.kandev-tooling-ref` in
`.build/kandev-tooling` (see the README), then run
`make verify-package-host` or `make verify-package`.
Smoke-test artifacts only in a disposable Kandev instance and a test Discord
server.

Never log config objects, the bot token, or message content. Never commit real
tokens.

Do not merge, publish releases, change catalog entries, or contact external
authors without an explicit instruction.
