# OpenForge

Project-agnostic Bun/TypeScript SDLC task harness embedding `@opencode/sdk` **2.1.2** without an HTTP listener. Project configuration lives in the repository root [`openforge.json`](../../openforge.json).

## Usage

From the repository root:

```sh
make openforge-install
make check-openforge
bun scripts/openforge/src/cli.ts doctor
bun scripts/openforge/src/cli.ts validate
bun scripts/openforge/src/cli.ts ready
bun scripts/openforge/src/cli.ts run --dry-run
bun scripts/openforge/src/cli.ts telegram
```

`telegram` long-polls a private owner chat. Put `TELEGRAM_BOT_TOKEN`, `TELEGRAM_OWNER_ID`, and the provider key in the primary checkout `.env`; OpenForge loads that file without printing values.

See the [harness guide](../../docs/implementation/harness.md), [task index](../../docs/tasks/README.md), and [`openforge.json`](../../openforge.json).
