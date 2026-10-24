# `kodama pipeline` (T-025)

Command contribution for pipeline definitions: `validate`, `register`, `get`, `list`, over [`api/pipeline.yaml`](../../../api/pipeline.yaml) and the shared [`internal/pipeline`](../../pipeline/README.md) validator. Not yet registered in the executable (integration owner, T-027).

```go
c, err := pipeline.NewCommands(pipeline.Config{
    Validator:       v,                                              // *pipelinelib.Validator, built by the composition root
    Actor:           pipelinelib.Actor{ID: "local-cli", Type: pipelinelib.ActorUser}, // diagnostic identity, offline only
    DiagnosticScope: nil,                                            // optional fallback when no project is selected
})
```

## Commands

* `pipeline validate --file F|-` runs the shared `Validator` locally. No validation endpoint is contacted (it needs no factory and no credential). Success reports `valid`, the pipeline name and the canonical digest, byte-identical to `internal/pipeline`'s digest. A rejected document exits 2 with the validator's own bounded diagnostics (`details.diagnostics`, same codes, paths and messages as the server's 400/422).
* `pipeline register --file F|-` parses the file locally (syntax and structure only), then `POST`s the Pipeline document itself as JSON to `/pipeline-definitions`. A `.json` file is sent as written; YAML is converted structurally and checked to parse back to the identical document before anything is sent. The server validates authoritatively (graph, references, capabilities), allocates the version and returns it: 201 new version, 200 identical existing content. The CLI never allocates a version, ID or digest.
* `pipeline get ID`, `pipeline list [--name N] [--limit N] [--cursor C]`: reads, one page for lists.

## Validation context (assumption for T-014/T-025 review and the integration owner)

The shared validator needs a scope, an actor, capabilities and, for secret/environment/profile references, lookup policies. They are **injected**: scope comes from the selected tenant and project (or `DiagnosticScope`); the actor is `Config.Actor`; policies are whatever the `Validator` was built with. Nothing is inferred from a token and no permissive policy is installed. A self-contained M1 document validates with only the capability binding; a document that references a secret or environment with no policy fails closed (`validation_unavailable`, exit 1); a kind that is not enabled is `kind_disabled` (exit 2). The diagnostic actor never represents authenticated authority.

## Recovery

Registration is content-addressed. After a lost answer (outcome unknown) the advice is to repeat the identical `register`, or to read `pipeline list --name`; no idempotency key exists for this route. Unparseable documents and oversized files (`DefaultMaxFileBytes`, 1 MiB) fail with exit 2 before any request.

## Current assessment

Historical baseline `14f4132` (initial implementation); reassessed at `d683fe18a889b60b4da81fcab0d00ad1f15b7704` and amended in this task. Requirements AC-01, AC-05; decisions C-12, P-08. Evidence here is component evidence: contract-shaped HTTP fixtures plus the real runner, client and shared validators; it is not an assembled M1 certification, which belongs to the integration owner (T-026/T-027). Server modules own entities, authorization, transactions, history, audit, idempotency and every state transition; this package owns only flag parsing, bounded local input and rendering. Local validation reuses the shared parser/validator with an injected `pipeline.Config` (validator, diagnostic actor, optional diagnostic scope); a missing reference policy fails closed. Tests: `Test_pipeline_validation_parity`, `Test_pipeline_validation_fails_closed`, `Test_input_timeout_no_request`.

Bounded local input (files and redirected standard input) obeys the command context: a stalled pipe cannot outlast `--timeout` or an interrupt, the command exits 5 with empty stdout, nothing is submitted, and the outcome is not reported as uncertain because no mutation was attempted. The blocked read can never feed a later request: its bytes are discarded. Paths are type-checked (`stat`) before they are opened, so a FIFO is refused instead of blocking.

Assumption for T-014 and the integration owner: the injected validator context (validator, diagnostic actor, optional diagnostic scope) is the selected binding; no identity is derived from a token and the diagnostic actor grants nothing, because registration is validated again by the server.
