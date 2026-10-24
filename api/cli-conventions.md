# CLI conventions

Contribution contract for the `kodama` command line, implemented by T-017 in `internal/cli/root/`, `internal/cli/auth/` and `internal/cli/client/`. Normative behavior is in the [interface specification](../docs/specs/api-cli-and-integrations.md) (AC-03, AC-05, AC-10, IT-01) and the [HTTP contract](../docs/contracts/http-api.md); this file records the conventions the CLI foundation chose where those documents leave a gap. Where it says **proposed**, T-017 review and the integration owner (T-027) confirm the choice.

This is a foundation, not a release: it provides `auth login|logout` and local `context set|use|show|list` contributions and the transport that product commands (T-025, T-037 and later) reuse. It registers nothing in the central binary and adds no product command, route, migration or event.

Design baseline: [software design](../docs/specs/software-design.md) SD-01–SD-03 and SD-05–SD-08, decisions C-06, C-16/C-17 and O-14 (resolved), ADR 0006 and 0007. Conformance: planned-conforming, with the **proposed** items below awaiting confirmation.

## Authority

The server authorizes every request. A saved context, tenant or project selection is routing convenience, and an access token is an opaque credential: the CLI never parses token claims, never verifies signatures, and never infers a role. Issuer, audience, signature, expiry and subject verification and independent membership checks stay with the server (T-010). An ID token is never used as an API credential; login discards it.

## Flags

Every command accepts the common flags below through `root.BindFlags`. Flags may appear before, between or after positional arguments (`work update wi_1042 --if-version 1 --output json`); the runner separates them using the command's own flag definitions and parses them with the standard-library `flag` package before resolving the context, output format and timeout, so trailing common flags (`--tenant`, `--context`, `--non-interactive`, `--timeout`, `--output`) are honoured by the shared foundation. A bare `--` ends flag parsing and everything after it is a positional argument verbatim; a flag value is always the next token, even when it looks like a flag or `--`. Presence is preserved: an explicitly empty value (`--tenant ""`) fails validation (exit 2) instead of silently using a saved value.

| Flag | Meaning |
| --- | --- |
| `--context NAME` | Use a saved context instead of the selected one. |
| `--factory URL` | Factory origin (`https://host[:port]`, no path). |
| `--tenant ID`, `--project ID` | Scope. A project requires a tenant. |
| `--output human\|json` | Rendering. Default `human`. |
| `--credential-store keyring\|file` | Default `keyring`. `file` is an explicit opt-in to a `0600` file. |
| `--issuer`, `--client-id`, `--authorization-endpoint`, `--token-endpoint`, `--scopes a,b`, `--redirect-uri` | Authentication configuration (below). |
| `--non-interactive` | Never open a browser or prompt, including a keyring unlock prompt. |
| `--timeout DURATION` | Overall command bound; must be positive. |

`--if-version N` (sent as a strong quoted-integer `If-Match`) and `--idempotency-key KEY` are transport features of `client.Request` (`IfVersion`, `IdempotencyKey`). Product commands bind those two flags themselves, with the spellings used by the [CLI and HTTP walkthrough](../docs/examples/api/cli-and-http.md); the foundation does not own product commands.

## Contexts (proposed)

A saved context holds a factory, authentication configuration, and an optional tenant and project. It never holds a token.

- **Location:** `<user config dir>/kodama/contexts.json` (`root.DefaultContextPath`), injectable through `root.NewFileContextStore(path)`. The file is replaced atomically with restrictive permissions; read-modify-write is serialized with an adjacent `.lock` file. Contention fails with a visible `context_store_error`; an abandoned lock is never stolen automatically. Operator recovery: confirm no `kodama` process is running, then delete the `.lock` file.
- **Precedence:** explicit flag, then the `--context`-named or selected saved context. There is no environment-variable layer. Overrides apply to one invocation and are never written back.
- **Coherence:** changing the factory clears the inherited tenant and project; changing the tenant clears an inherited project unless `--project` is also given.
- **Commands:** `context set --context NAME [--select]` creates or updates; `context use [--context NAME] [--tenant ID] [--project ID]` selects and narrows scope without changing the factory or authentication; `context show` prints the effective context; `context list` prints saved contexts sorted by name.

## Authentication (proposed)

There is no factory-to-OIDC discovery route. Authentication configuration is injected through the flags above or a saved context. The development fixture (`dev/configuration.md`) uses public client `kodama-cli` and redirect `http://127.0.0.1:18999/callback`; its cluster-internal issuer is not rewritten to `localhost`, so a host-browser setup must configure an issuer the browser and the CLI both reach.

`kodama auth login`:

1. Requires a complete configuration, HTTPS provider endpoints (plain `http` only on loopback when the composition root injects the development exception), and a literal loopback `http` redirect such as `http://127.0.0.1:18999/callback` (`[::1]` also works; `localhost` is refused). Port `0` selects a free port, for providers that accept any loopback port.
2. Generates independent cryptographic `state` and PKCE verifier, uses S256, and opens the browser.
3. Accepts exactly one callback whose `state` matches. Wrong, missing or duplicated parameters, wrong path or method, and a foreign `Host` header are rejected without consuming the attempt or exchanging a code; a repeat after acceptance is refused; a callback after the attempt deadline is refused.
4. Exchanges the code once with `golang.org/x/oauth2` (public client, no secret, redirects not followed). An exchange with an unknown outcome is never retried: run `auth login` again.
5. Reports success only after the credential is persisted. On any failure the loopback listener is closed and any previously stored credential is untouched.

Login attempt states are `created → listening → callback accepted → exchanged → persisted`; every failure is terminal and the attempt cannot be reused. A login attempt is bounded (default five minutes).

Credentials: stored per credential key, a hash of factory and authentication configuration, so changing the factory, issuer, client, endpoints or scopes can never reuse another login's token. `--non-interactive` makes `auth login` fail with `interactive_required` (exit 2) before any listener or browser; commands that need a credential and have none fail with `not_authenticated` (exit 3) and never start a login. Expired credentials require an explicit `auth login`. Automatic refresh and provider logout/revocation are not implemented; `auth logout` deletes only the local credential, is idempotent, and reports `remoteRevocation: false`.

Storage: the OS keyring (`github.com/zalando/go-keyring`, service `kodama-cli`) by default. An unavailable keyring is an error (`credential_store_unavailable`, exit 1) with no fallback. `--credential-store file` stores `<user config dir>/kodama/credentials/<key>.json`; the directory must be a real directory with no group or other access and the file a regular `0600` file. A symlink, wrong type or loosened mode is rejected (`credential_store_insecure`) and never silently repaired; fix with `chmod 700` / `chmod 600` or delete the file and log in again. Writes go to a new `0600` file that is flushed and renamed over the target, so a failed write (`credential_persistence_failed`) leaves the previous credential intact. Prompting: with `--non-interactive`, token lookup and `auth logout` first ask the keyring (`auth.PromptProber`) whether it can run without a prompt, and fail with `credential_store_interaction_required` (exit 2) before any backend call when it cannot. On Windows the Credential Manager never prompts. On Linux and other platforms (for example macOS, where a locked keychain raises a dialog) the keyring cannot be shown to be prompt-free: go-keyring may still call `org.freedesktop.Secret.Prompt` on an item or during delete even when the collection looks unlocked, so `--non-interactive` refuses the OS keyring and requires `--credential-store file`. Unlock the keyring and run interactively, or opt in with `--credential-store file`; there is no fallback. Stores receive the policy through `auth.WithNonInteractive(ctx)`. Every keyring operation is bounded by the command context: `--timeout` or an interrupt returns exit 5 even while the backend is blocked on a prompt (an abandoned save may still complete, but login never reports success). Tokens, authorization codes and verifiers are never logged or rendered; `auth.Credential` formats as `redacted`.

## Output and exit codes

A successful command writes one newline-terminated document to stdout and nothing to stderr. With `--output json` it is deterministic JSON: object keys sorted, no HTML escaping, additive server fields preserved. A failed command writes only to stderr and nothing to stdout. JSON output is fully buffered, so a failure never leaves half a document.

JSON error envelope (stderr):

```json
{"error":{"code":"not_authenticated","message":"…","requestId":"","details":{},"status":0,"retryAfterSeconds":0}}
```

`status` and `retryAfterSeconds` appear only for remote failures that carry them. `requestId` is the server's request ID for remote failures and **empty for local failures**: the CLI never fabricates one. This differs deliberately from the HTTP error schema, where the ID is always present. Remote error codes are passed through unchanged, including codes the CLI has not seen.

| Exit | Meaning | Examples |
| --- | --- | --- |
| 0 | Success | |
| 1 | Remote or operation failure | 5xx, 429 and 503 (with retry guidance), credential store failure, malformed or oversized response, uncertain mutation |
| 2 | Usage or validation | bad flag, explicit empty value, 428 (a version or key was required and missing), `interactive_required` |
| 3 | Authentication or authorization | no credential, 401, 403, provider refused login |
| 4 | Conflict | 409, 412 |
| 5 | Timeout or interruption | `--timeout`, cancellation, login timeout |

Cancellation: a command whose context is already cancelled or expired exits 5 before any side effect. Context and credential file adapters check cancellation before taking a lock or creating files and again immediately before the committing rename or removal, so a cancelled `context set|use` or file-backed `auth logout` leaves persisted state unchanged and writes only to stderr. A commit that has already happened is not rolled back or reported as cancelled. `ServiceConfig.LoginTimeout` (default five minutes) bounds the whole login attempt (browser launch, callback wait, exchange and credential persistence), not just the callback wait. A login that times out while a keyring save is in flight exits 5 with `outcomeUncertain: true` and login-specific `recovery` text in `details` (JSON) and an `outcome unknown:` line (human): the abandoned save may still complete and install or replace the credential for that factory, so run `auth login` again to replace it or `auth logout` to remove it. An uncertain code exchange carries its own guidance (the code may be consumed; run `auth login` again). This guidance never reuses the resource-mutation text (`idempotency-key`, reread the resource), because no resource was mutated. Login never reports success after a timeout.

## Transport

`client.New` takes the factory, an injected `AccessTokenSource` and an HTTP client. It addresses only the factory origin under `/api/v1`; arbitrary absolute URLs, path escapes and redirects are refused so a bearer token cannot reach another destination. Scope is built by validated path construction (`/tenants/{id}/projects/{id}/…`) from the explicit `client.Scope`. Responses are size-bounded and must be complete JSON where a document is expected; additive fields and unknown error codes are accepted. A response's `ETag`, request ID, `Retry-After` and idempotency replay indication are preserved in `client.Response`.

Path segments are escaped exactly once (`a b` is sent as `a%20b` and decoded by the server as `a b`; `/` inside a segment stays `%2F`).

Retry restraint: the transport never retries and never alters a mutation's body, version or key, and it stops `net/http` from silently resending a mutation after a lost answer on a reused connection (it clears `GetBody` and, for a keyed bodyless mutation, sends the key under an upper-case header name that the transport does not recognise as a replay marker). After a transport failure, timeout, cancellation or malformed (non-JSON or truncated) answer to a mutation, the error carries `outcomeUncertain: true` and a `recovery` string in `details` in JSON mode, and an `outcome unknown:` line in human mode, independently of the exit code (a timed-out mutation exits 5 and is still uncertain). Timeout classification covers any wrapped `net.Error` with `Timeout() == true` (for example a stalled TLS handshake under the HTTP client's `TLSHandshakeTimeout`) as well as `context.DeadlineExceeded`: exit 5, code `timeout`. Recover by rereading the resource, or by replaying the identical request with the identical `--idempotency-key`. A 412 means reread and decide again; a 428 means the command omitted a required version or key.

## Integration instructions for composition (T-027)

Exported surfaces:

- `root.NewRunner(root.RunnerConfig{…}, contributions…)`, `root.NewContextCommands()`, `root.NewFileContextStore(path)`, `root.BindFlags`, `root.ResolveContext`, `root.Classify`, `root.WriteSuccess`/`WriteFailure`, and `root.Failure` for commands choosing their own exit code. Contributions supply `root.Command{Name, Need, Bind}`; the runner resolves the effective context and calls `Invocation.Client()`.
- `auth.NewService(auth.ServiceConfig{…})` (a `root.TokenResolver`), `auth.NewCommands(svc)`, `auth.NewKeyringStore`, `auth.NewFileStore`, and the consumer-owned ports `auth.CredentialStore` and `auth.BrowserOpener`.
- `client.New`, `client.Request`, `client.Response`, `client.RemoteError`, `client.TransportError` and the consumer-owned `client.AccessTokenSource`.

The composition root builds one `Runner` with `root.NewContextCommands()` and `auth.NewCommands(svc)` plus product contributions, passes `svc` as `RunnerConfig.Tokens`, and maps the returned exit code to `os.Exit`. Nothing in these packages calls `os.Exit` or writes to the process streams directly.

## Checks

```sh
go test -race -count=1 ./internal/cli/...
make test-task TASK=T-017
KODAMA_TEST_OS_KEYRING=1 go test -run Test_keyring_adapter_roundtrip ./internal/cli/auth/   # opt-in, real OS keyring
```

Tests named for the acceptance identifiers: `Test_pkce_state`, `Test_context_override`, `Test_credential_storage`, `Test_exit_json_contract`. They use local `httptest` servers, temporary files, a fake keyring and per-test loopback ports; none needs PostgreSQL, NATS, S3 or Git. The real-keyring test is skipped unless opted in and is recorded separately from headless acceptance.
