# Git Service and Access Rules

Kodema's optional Git companion (`kodema git serve`) is an independent Userland service that enforces access rules via Kodema's `/api/v1/verify` contract before accepting any read or push.

## Repository naming conventions

Tenant-relative repository paths:

```text
<tenant>/admin                  # access rules (conf/git.conf), Admin-managed
<tenant>/context                # company-wide Markdown docs, conventions, skills
<tenant>/<project>/context      # project-specific Markdown docs and skills
<tenant>/<project>/<component>  # application source repositories
<tenant>/<project>/deploy       # deployment / GitOps repositories
```

Inside a container action, repositories are addressed as `$KODEMA_GIT_URL/<repository>` (tenant-relative).

## Access rules (`admin/conf/git.conf`)

Tenant Admins manage access rules by editing `conf/git.conf` in the tenant's `admin` repository and pushing. Rules apply immediately on the next request.

```text
# <tenant>/admin -> conf/git.conf

repo admin
    RW                              = @admin

repo shop/web
    option resource                 = git-shop-web
    R                               = @all
    RW+  refs/heads/work/           = @engineer @execution
    RW   refs/heads/main            = @execution
    option approve.refs/heads/main  = reviewer

repo shop/deploy
    option resource                 = git-shop-deploy
    R                               = @operator @execution
    RW+  refs/heads/release/        = @execution
    RW   refs/heads/main            = @execution
    option approve.refs/heads/main  = operator
```

### Evaluation semantics

- Permissions:
  - `R` — read (clone/fetch)
  - `RW` — create or fast-forward ref
  - `RW+` — create, fast-forward, rewind, or delete ref
  - `-` — explicit deny
- **First-match wins** in declaration order: the first rule whose ref and principal match and whose permission covers the attempt allows; a matching `-` denies; no match refuses.
- Refs match **exact** names, or by **prefix** when ending in `/` (for example `refs/heads/work/`). No regular expressions. Omitting a ref covers all refs.
- Principals: `@admin`, `@engineer`, `@reviewer`, `@operator`, `@viewer`, `@execution` (running execution in scope), `@all` (every principal in scope), or explicit service-account IDs. There is no implicit Admin bypass.
- `option resource = <resource-id>` binds the repository stanza to its registered Kodema resource ID. Always preserve it when editing `conf/git.conf`.

### Approval-guarded refs (`option approve.<ref> = <role>`)

When `option approve.<ref> = <role>` is set:

- The `pre-receive` hook requires a **passed** `request: role/<role>` in the **same execution** that is pushing.
- That passed request's immutable subject must match `operation: git.ref.update`, `resource`, exact `ref`, `expected_base` (empty or all-zero OID for an unborn ref), and `candidate` commit OID.
- Humans and service accounts carry no execution approval evidence, so they cannot push directly to approval-guarded refs. To land workstation changes, push to a `work/` branch and run an integration pipeline with a review gate.

## Workstation Git setup

Configure a workstation to authenticate with your `kodema login` session (never storing tokens in a keychain):

```sh
kodema git configure --origin https://<kodema-git-host> --global
```

Inside container actions, Kodema configures the credential helper and CA automatically whenever `KODEMA_GIT_URL` is set.
