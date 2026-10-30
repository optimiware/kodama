# sync-cursor

A deliberately non-deployment plugin that proves the generic state and artifact
contracts without provider vocabulary.

It performs one bounded fetch with the cursor stored under the authorized state
binding, writes the page to the declared output path, and CASes the next cursor
back to state. A second run starts from that cursor. The entrypoint uploads the
page artifact after the command; this plugin never talks to a provider.

```sh
sync-cursor \
  --source-url https://fixture.example.invalid/items \
  --state-binding cursor \
  --output /kodama/outputs/page.json
```

The source must return `200` and may return an `X-Next-Cursor` header. The
cursor value contains only the next cursor and a digest for diagnostics; large
pages belong in the artifact, not in state.
