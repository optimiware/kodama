# Agent development example

This project shape is the development workflow from the
[container-plugin plan](../../docs/implementation/container-plugins.md#91-development-project-with-agents-and-review):

```text
plan agent → implement agent → tests → code review agent → security review agent → human gate
```

The example is a design fixture until the `container-plugins` implementation
packages and the named agent profiles are installed. It uses the existing
`run_shared` serialized execution model: reads cannot publish, and a reviewer's
report is an artifact outside the source tree.

```sh
# The server selects the profile versions; the document names only their IDs.
kodama pipeline register --file pipeline.yaml
kodama run create --definition develop-and-review --work-item <id>
```

The gate is a human decision over immutable evidence. An agent cannot approve,
complete the work item, or turn a prose claim into a passing reviewer result.
A changed candidate or execution configuration requires fresh applicable
evidence.

## Files

| Path | Purpose |
| --- | --- |
| `pipeline.yaml` | Plan, implement, test, two independent reviews and a human gate |
| `profiles/` | Installation-owned profile bindings (images, model/tool policy, limits) |
| `ci/test.sh` | The project test command used by the test step |
| `spec.md` | Example intent and acceptance criteria for the work item |

The profile JSON files are installation configuration, not a grant of runtime
authority. Use the profile registration contract of the installed Kodama
version; do not paste provider credentials into a pipeline document.
