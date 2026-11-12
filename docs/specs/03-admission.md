# S03 — Pipeline definitions and immutable admission

Source: [core model](../../KODAMA.md#core-model), [ordered flow](../../KODAMA.md#ordered-flow), [push triggers](../../KODAMA.md#pipeline-push-triggers-and-userland-services). Owners: K01, P04, P08, P20. AC: 2, 11, 13, 14.

## 1. Authoring boundary

Pipelines have a stable ID, project-unique name, optimistic version, and exact YAML bytes. Only authorized API writes create/update them. Repository files can be posted by a client; the Shell never discovers or synchronizes definitions on push.

The Shell decodes YAML into the Kernel's typed definition. Kernel validates structural semantics without importing a YAML library. Decode rejects duplicate/unknown keys, malformed types, unsafe names/paths, unsupported tags/alias expansion, and bounded-size/depth violations. C01 fixes exact parser limits and literal policy with test vectors; implementations cannot silently coerce ambiguous YAML into different values.

| Location | Supported fields |
|---|---|
| Pipeline | `inputs`, literal `env`, ordered `actions`; optional `repository`, `service`, `on.push` |
| Container action | `name`, `image`, `commands`, literal `env`, `secrets`, `timeout`, optional `outputs` |
| Request action | `name`, `request`, `subject`, `timeout`, optional response `outputs` |

Exactly one action kind is present. Actions are nonempty and uniquely named; no condition, parallel action, dependency declaration, loop, join, skipped state, or dynamic request insertion is supported. Runtime requirements from the admitted model must be supported by the selected adapter; C01 records their exact supported encoding rather than inventing ad hoc YAML extensions.

Environment values are literals. Action values override pipeline defaults; neither can shadow `KODAMA_URL`, `KODAMA_EXECUTION`, `KODAMA_ACTION`, or `KODAMA_TASK`. Secret name `KODAMA_TOKEN` is reserved. Paths/slugs/keys cannot escape their materialization roots.

## 2. Subject references

A subject is a bounded object of literals or direct lookups:

```yaml
operation: git.ref.update
resource: {from: inputs.repository}
candidate: {from: build.outputs.candidate}
```

The reference object contains exactly `from`. Supported paths are `inputs.<declared-key>` and `<earlier-action>.outputs.<declared-key>`. Unknown, self, forward, malformed, and mixed literal/reference forms are refused. There is no interpolation, transformation, template expansion, or arbitrary JSON-path evaluation.

Admission verifies declarations and order. Activation resolves from immutable inputs and earlier **succeeded** outputs. Declaring an output does not prove it was submitted; missing runtime values fail the request before it is answerable. Resolution persists the immutable subject once and never rebinds it after a pipeline edit.

## 3. Admission sequence

1. Authenticate and authorize the start operation in the owning project.
2. Select an existing pipeline's exact current YAML, or authorized inline YAML for a task-free run.
3. Decode and validate declaration/input/reference/schema/runtime constraints.
4. Resolve image references to immutable digests through the registry port; reject unsupported/unresolvable images.
5. Resolve timeout durations from declarations or the configured default; all are finite and within 1 minute–30 days.
6. Construct a snapshot with exact YAML bytes, SHA-256 of those **bytes**, pinned images, input values, declared secret **names**, runtime requirements, and task conversation/references/metadata if present.
7. In the admission transaction, recheck current authority and relevant selection/task/resource versions; write queued execution/actions, task changes if any, receipt, audit, and events atomically.

Registry and other network calls run before the transaction. If versions changed, rebuild/revalidate rather than commit a mismatched definition. Action deadlines remain unset until activation. Secret values are never resolved or stored in the admission snapshot.

No definition blob store, pipeline version table, canonicalized YAML hash, mutable image reference, or Git-specific execution field is needed. Commits, refs, and artifact digests are ordinary values. Edits do not change an admitted run.

## 4. Push definitions

`repository` selects a registered resource and its trusted reporting service. `service` selects the automatic-start principal. These may be different service accounts. `on.push` contains exact refs; no patterns or input mappings. Declared inputs can only require values supplied from `repository`, `ref`, `commit`, `previous_commit`.

Configuration requires pipeline-management authority and Admin authority to bind/change the automatic principal. Invocation rechecks service enablement, scope, event-read, and current start authority. Trigger handling and immutable first selection are specified in S08.

## 5. Unit verification

| ID | Required cases |
|---|---|
| A-PARSE | Unknown/duplicate fields; mixed kinds; unsupported syntax; malformed YAML; bounded aliases/depth/bytes |
| A-REFERENCE | Every literal/direct form; unknown/self/forward reference; absent successful output |
| A-SNAPSHOT | Byte-distinct equivalent YAML yields distinct hashes; definition edits cannot mutate snapshots; image tags become digests |
| A-INPUT | Missing/extra/type-invalid inputs per frozen schema; task-free inline run; opaque non-Git values |
| A-TIME | Default and explicit boundary durations; invalid/infinite durations; no admission-time deadline |
| A-ENV | Override precedence; reserved variables/secrets; path traversal and invalid name rejection |
| A-ATOMIC | Fake registry failure and version/authority changes prevent admission; no secret values in snapshot |
| A-PUSH | Exact refs/input set; binding authority; no Git-to-pipeline synchronization |

Unit tests fake registry/store/authorization collaborators. Real digest lookup, transaction races, API parity, and trigger replay are verified after G2.
