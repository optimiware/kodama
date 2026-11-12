# Repository conventions

These conventions describe how the example context repository is organised.

## Layout

- `conventions.md` records durable, project-wide rules.
- `skills/` contains reusable Markdown skills addressed by slug.
- Pipelines live outside the context repository and are posted through the API.

## Durable references

Handoffs reference durable values such as commits, refs and image digests.
Never reference workspace or temporary paths; those disappear when an execution
ends.

## Credentials

Declare secret names only. Values are resolved by the runtime and never stored in
knowledge, pipeline YAML, logs, events or receipts.
