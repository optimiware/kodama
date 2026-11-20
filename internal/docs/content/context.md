# Shared Company and Project Context

Kodema separates **product documentation** (embedded in the `kodema` binary and served over MCP/CLI) from **company and project context** (stored in Git context repositories and governed by your tenant's `admin/conf/git.conf` rules).

## Context repository layout

Store shared Markdown knowledge in tenant and project context repositories:

- `<tenant>/context` — organization-wide architecture, domain glossary, coding standards, and shared skills.
- `<tenant>/<project>/context` — project-specific conventions and skills (later `--repo` entries override earlier ones on colliding skill names, e.g. `--repo context --repo <project>/context`).

Recommended layout inside each context repository:

```text
context/
├── README.md
├── conventions.md
├── docs/
│   └── architecture.md
└── skills/
    ├── build-and-test.md        # flat Markdown skill (slug = build-and-test)
    └── release-checklist/       # directory skill (slug = release-checklist)
        ├── SKILL.md
        └── references/
            └── policy.md
```

Rules enforced by `kodema context`:

- Skill slugs must be lowercase alphanumeric words joined by single hyphens (`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`, max 64 characters).
- Loaded files (skills under `skills/`, including supporting files inside directory skills, plus top-level `*.md` and `docs/**/*.md`) are bounded to 1 MiB per file and must not be symlinks.
- **No secrets or credentials:** every loaded skill file and Markdown doc is scanned for private keys, bearer headers, access keys, and credential assignments; any match refuses the pull before writing to the workspace. Declare secret names only.

## Pulling context in container steps (OpenCode, Claude Code, Codex)

When the installation configures a workload Git URL, `$KODEMA_GIT_URL` and Git authentication are already configured inside container actions. Pull shared context repos and materialize them into harness-discoverable skills in one command:

```sh
kodema context pull \
  --repo context \
  --repo myproject/context \
  --target agents \
  --agents-md AGENTS.md \
  --include-kodema-docs
```

What this does:

1. Clones each repository from `$KODEMA_GIT_URL/<repo>` (supports `<repo>@<ref>` to pin a branch, tag, or exact commit SHA).
2. Validates every skill and Markdown doc for symlink safety, size bounds, and secret leaks.
3. Writes skills as `<dest>/.agents/skills/<slug>/SKILL.md` (use `--target claude` for `.claude/skills`, `--target opencode` for `.opencode/skills`, or `--target all` for all three). Plain Markdown skills without YAML frontmatter automatically receive portable `name` and `description` frontmatter so harnesses advertise them.
4. Copies non-skill context Markdown docs into `<dest>/.kodema/context/<repo>/...`.
5. When `--agents-md <path>` is passed, updates a managed `<!-- kodema:context:start -->` block referencing pulled repos, resolved commits, docs, and skills without overwriting surrounding instructions.
6. When `--include-kodema-docs` is passed, also installs Kodema's embedded product docs as the `kodema` skill.

## Local validation and inspection

Validate a context repository locally (or in a CI pipeline) before merging changes:

```sh
kodema context list --dir ./path/to/context
kodema context show build-and-test --dir ./path/to/context
```

## Installing Kodema product docs as a standalone skill

On a workstation or in a container step without a company context repo:

```sh
kodema docs install --target agents     # or claude, opencode, all
```
