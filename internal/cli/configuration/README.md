# `kodama variable` and `kodama secret` (T-025)

Command contributions over [`api/configuration.yaml`](../../../api/configuration.yaml). Not yet registered in the executable (integration owner, T-027).

```go
root.NewRunner(cfg, configuration.NewVariableCommands(), configuration.NewSecretCommands(), ...)
```

## Variables

`variable create NAME (--value S | --value-json J) [--description D] --idempotency-key K`, `variable get ID`, `variable list`, `variable update ID --if-version N [--value|--value-json] [--description]`, `variable delete ID --if-version N`.

A bare `--value` is always a string; booleans and integers need the explicit `--value-json true|42|"text"`. Integers keep their literal spelling; the CLI never guesses a type, and the server owns ranges. The resource `version` (ETag, for `--if-version`) and the `valueVersion` (what runs pin) are labelled separately.

## Secrets

`secret create NAME --kind K --value-file F|- --idempotency-key K`, `secret get ID`, `secret list`, `secret versions ID`, `secret rotate ID --if-version N --value-file F|- --idempotency-key K`, `secret revoke ID --version V [--reason R]`, `secret delete ID --if-version N`.

* Secret bytes come **only** from a regular file readable by its owner alone (no group or other bits; symlinks, directories and devices are refused) or from explicitly redirected standard input (`--value-file -`). A terminal or device on standard input is refused: the CLI never prompts, never echoes. There is no argument, environment variable or temporary-file path for a value; `--value` does not exist for secrets.
* The value must be 1 to 131072 bytes, is preserved exactly (no trimming, no encoding assumptions) and is sent as standard padded base64 in the request body. The buffer is cleared after encoding.
* Errors name the problem, never the content. Responses are management metadata only; the retention and consumer-handling reports of `delete` and `revoke` are shown as returned (`backupRetention: unresolved`, consumer actions pending, "revocation cannot recall delivered values").
* The merged API requires `Idempotency-Key` for create and rotate and `If-Match` for rotate and delete; `revoke` takes the **value** version in the path and no precondition. The CLI follows that exactly (the walkthrough's `secret create` omits the key; that example predates the API).

## Recovery

Keyed requests are never resent automatically. After an uncertain outcome (lost answer, gateway page, deadline), repeat the identical command with the identical `--idempotency-key` (the same value file), or reread with `get`. A changed value under a used key conflicts; a revoked permission still fails a replay. Stale `--if-version` (412, exit 4) advises rereading. Revoke is idempotent: repeating it returns the same accounting.

## Current assessment

Historical baseline `14f4132` (initial implementation); reassessed at `d683fe18a889b60b4da81fcab0d00ad1f15b7704` and amended in this task. Requirements AC-01–AC-06, VS-04. Evidence here is component evidence: contract-shaped HTTP fixtures plus the real runner, client and shared validators; it is not an assembled M1 certification, which belongs to the integration owner (T-026/T-027). Server modules own entities, authorization, transactions, history, audit, idempotency and every state transition; this package owns only flag parsing, bounded local input and rendering. Acceptance: `Test_secret_stdin_no_echo` (byte-exact binary stdin; no plaintext or base64 in output or persisted CLI files across success, conflict, unavailable and uncertain answers), `Test_configuration_input_safety`, `Test_input_timeout_no_request`.

Bounded local input (files and redirected standard input) obeys the command context: a stalled pipe cannot outlast `--timeout` or an interrupt, the command exits 5 with empty stdout, nothing is submitted, and the outcome is not reported as uncertain because no mutation was attempted. The blocked read can never feed a later request: its bytes are discarded. Paths are type-checked (`stat`) before they are opened, so a FIFO is refused instead of blocking. A secret read that is cancelled clears whatever it had buffered. Protected-file permissions (no group/other bits) are checked on the path and again on the opened file, closing the check/open gap; symbolic links, devices and terminals are refused.

Assumptions for confirmation: the flag spellings `--value`, `--value-json`, `--value-file -`, `--version` and the mandatory `--idempotency-key` (the proposed walkthrough omits the key that the executable configuration contract requires; the CLI keeps the stricter contract).
