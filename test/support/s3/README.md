# `test/support/s3` — isolated S3 prefixes for tests

Task T-015. Reuses the development fixture contract
([`dev/configuration.md`](../../../dev/configuration.md)) without changing it.

| Variable | Meaning |
| --- | --- |
| `KODAMA_S3_ENDPOINT` | Endpoint reachable from the test process (unset: the fixture is self-provisioned, see below) |
| `KODAMA_S3_BUCKET` | Fixture bucket; created when absent |
| `KODAMA_S3_PREFIX` | Scope prefix; every test appends a unique subprefix |
| `KODAMA_SECRETS_DIR` | Directory with `s3-access-key` and `s3-secret-key` (default `/secrets`) |
| `KODAMA_RESOURCE_PREFIX` | Task resource prefix (`t015` under `make test-task`) naming the subprefix |
| `KODAMA_REQUIRE_S3=1` | A fixture that is neither configured nor provisionable fails instead of skipping |
| `KODAMA_S3_PROVISION=0` | Disables the self-provisioned fixture |
| `DEV_PROVIDER` | Container provider (`podman` on every OS; explicit `docker` override for CI) |

`s3.New(t)` returns an `Env` with an SDK client (static credentials, SigV4,
`us-east-1`, path-style) and `Prefix`, a subprefix
`<KODAMA_S3_PREFIX><resource>-<random>/` owned by that test. `t.Cleanup` deletes
every object under exactly that subprefix; `CleanPrefix` refuses an empty prefix
or one without a trailing `/`, so cleanup can never reach a sibling or foreign
object. Nothing is created or deleted outside the subprefix except the bucket
itself when it does not exist.

LocalStack (the fixture) skips presigned-URL signature validation by default, and
only validates the access key/secret pair `test`/`test`. Tests that depend on
provider enforcement detect it and say so; see
[`internal/artifacts/README.md`](../../../internal/artifacts/README.md#provider-limitations).

## Self-provisioned fixture

`make test-task` runs with only the PostgreSQL and NATS fixtures, so the S3
variables are normally unset. `New` then starts (or reuses) one LocalStack
container named `<KODAMA_RESOURCE_PREFIX>-s3-localstack` (`t015-s3-localstack`
for T-015) from the digest-pinned image in `dev/compose.yaml`, published on an
ephemeral `127.0.0.1` port, with `S3_SKIP_SIGNATURE_VALIDATION=0` so presigned
expiry and method/key binding are enforced by the provider. Creation is
serialized across test processes with a lock file in the temp directory. The
bucket is `<prefix>-artifacts`, the scope prefix `<prefix>/`, and the
credentials LocalStack's fixed `test`/`test` development pair. Nothing outside
that container and the per-test subprefixes is created. The container is left
running for later runs; remove it with `podman rm -f t015-s3-localstack`. When no
container provider is available, tests skip (or fail under `KODAMA_REQUIRE_S3=1`).
