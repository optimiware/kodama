# Scaffold configuration contract

T-001 owns the process/bootstrap names below. They are consumed by the scaffold and its fixtures. Later modules request additions through the shared-file/integration owner; this is not a complete production server configuration schema.

## Process environment

| Name | Type / default | Validation |
| --- | --- | --- |
| `KODAMA_HTTP_ADDR` | String, `:8080` | `host:port`; numeric port 0–65535; port 0 is useful for tests |
| `KODAMA_LOG_LEVEL` | slog level, `INFO` | Go slog textual levels, case-insensitive, including supported numeric offsets |
| `KODAMA_SHUTDOWN_TIMEOUT` | Go duration, `10s` | Greater than zero, at most `1m` |

An unset or empty environment setting selects its default. Command parsing uses Go's standard-library `flag` package. Server flags `--http-addr`, `--log-level`, and `--shutdown-timeout` override their corresponding environment settings; precedence is **flags → environment → defaults**. Explicit empty flag values are rejected. There is no configuration-file layer. `kodama --help` and `kodama server --help` print help to stdout, even when environment settings are invalid.

Invalid configuration/usage exits 2; listen/serve failures exit 1; help and graceful shutdown exit 0. Logs go to stderr as JSON. `SIGINT`/`SIGTERM` marks readiness false and drains requests up to the shutdown timeout.

## Development controls
### Core secrets configuration

The assembled server reads configuration document version 2 through `KODAMA_CONFIG_FILE`. Its `secrets` section selects `adapter: keyfile | kms`; see the [chart contract](../deploy/charts/kodama/README.md#kms-selection). The scaffold-only process settings above retain their existing flag/environment behavior.

For environment-only test startup set `KODAMA_SECRETS_ADAPTER=keyfile`; it defaults to current `v1` at `$KODAMA_SECRETS_DIR/master-key`. `KODAMA_SECRETS_CONFIG` supplies the complete secrets object as strict JSON (duration values in nanoseconds); it replaces the document's secrets object, including entire retained-key maps. `KODAMA_SECRETS_ADAPTER` overrides its adapter. Old `KODAMA_KMS_*` names are rejected with a migration message. Production requires `kms`; cloud provider names are `aws`, `gcp`, `scw`.


| Name | Default | Contract |
| --- | --- | --- |
| `DEV_PROVIDER` | `podman` on every OS | Explicit `docker` override for Docker/CI; no runtime auto-selection |
| `DEV_SCOPE` | `t-001-local` | 2–31 lowercase letters/digits/hyphens; starts with letter, ends alphanumeric |

Make variables become explicit arguments to `dev/cmd/devctl`, which also reads them from the environment when invoked directly. Go and Make 3.81 are the minimum host prerequisites; the development tooling is Go. Go, kind, kubectl, the kind node image, client libraries, actionlint, and the additional Git package have exact locks; Compose and the Containerfile pin the fixture images by digest.

## Generated Pod endpoint inventory

`dev/.state/environments/<scope>/endpoints.json` is a JSON object of string values. The same values are injected into the smoke Pod:

| Name | Value or purpose |
| --- | --- |
| `KODAMA_TEST_SCOPE` | Assigned development scope |
| `KODAMA_SERVER_URL` | `http://kodama.kodama-deps.svc.cluster.local:8080` |
| `KODAMA_NATS_URL` | `nats://nats.kodama-deps.svc.cluster.local:4222` |
| `KODAMA_S3_ENDPOINT` | `http://localstack.kodama-deps.svc.cluster.local:4566` |
| `KODAMA_S3_BUCKET` | `kodama-<scope>` |
| `KODAMA_S3_PREFIX` | `<scope>/`; probes append a unique suffix |
| `KODAMA_GIT_URL` | `https://git.kodama-deps.svc.cluster.local:8443/fixture.git` |
| `KODAMA_OIDC_ISSUER` | `http://oauthsonas.kodama-deps.svc.cluster.local:8181` |
| `KODAMA_OIDC_AUDIENCE` | `https://kodama.example.test/api` |
| `KODAMA_REGISTRY_URL` | `http://registry.kodama-deps.svc.cluster.local:5000` |
| `KODAMA_REGISTRY_SOURCE` | `<scope>/scaffold`; source manifest tag `dev` |

These are development endpoints. The product's repository ownership, credentials, and authorization remain module responsibilities. S3 uses SigV4, `us-east-1`, path-style URLs, and static development credentials. LocalStack does not establish production IAM enforcement. HTTP is confined to this local fixture profile; Git specifically exercises HTTPS and CA verification.

## Secret-file contract

Local files are under `dev/.state/environments/<scope>/fixtures/` (directory 0755, files 0644, inside a 0700 scope directory). The directory is mounted read-only at `/secrets/` in the Compose containers, and selected files are mounted at `/secrets/` in the probe Pod. Text values have no required trailing newline. The probe never receives the PostgreSQL administrator password, Git TLS private key, or master key.

| File | Format / purpose |
| --- | --- |
| `database-runtime-url` | PostgreSQL URI with `kodama_runtime` identity and database-specific scope |
| `database-migration-url` | PostgreSQL URI with `kodama_migration` owner identity |
| `nats-token` | Random hexadecimal development token |
| `s3-access-key` | LocalStack test access key `test` |
| `s3-secret-key` | Random hexadecimal fixture secret |
| `git-read-token`, `git-write-token` | Independent random hexadecimal tokens, sent as HTTP Basic passwords over TLS |
| `ca.crt` | PEM-encoded development CA certificate |
| `tls.crt`, `tls.key` | Git service certificate and PKCS#8 EC private key; one-year development certificate |
| `master-key` | Exactly 32 random bytes, kept outside PostgreSQL; the development KEK for T-012's keyfile adapter. It wraps scope keys and does not encrypt secret values directly |

`postgres-password`, `migration-password`, `runtime-password`, `init.sql`, `nats.conf`, and `oauthsonas.yaml` are container-bootstrap files that never reach the probe. Stable generated credentials are reused on repeated setup. Deleting credential files while keeping dependency volumes is not a credential-rotation procedure; recreate that development scope instead.

## oauthsonas fixture

The image comes from [optimiware/oauthsonas](https://github.com/optimiware/oauthsonas), version 1.4.0, source commit recorded in [versions.lock.json](versions.lock.json). The build does not depend on a sibling checkout.

- Public client ID: `kodama-cli`; S256 PKCE required by oauthsonas.
- Redirect URI: `http://127.0.0.1:18999/callback`.
- Post-logout URI / allowed origin: `http://127.0.0.1:18999/` / `http://127.0.0.1:18999`.
- Persona: `engineer`; subject: `oauthsonas|engineer`; role claim: `engineer`.
- Access tokens: factory audience above; ID tokens: client audience.
- Token/code/refresh lifetimes: `15m` / `5m` / `8h`.
- Container: one instance, deliberate `OAUTHSONAS_ALLOW_NON_LOOPBACK=true` because Pods reach it through a Service rather than loopback.

Persona role claims are test inputs, not factory membership records. oauthsonas uses an in-memory picker/login hint; implementing and validating factory identity/membership belongs to T-010.
