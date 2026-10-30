# 6. Knowledge base and blueprints

## Git-native Knowledge Base

Instead of storing skills, prompts, blueprints, and engineering standards in custom database tables, Kodama uses its **native Git service** and a Gerrit-style path convention ([ADR 0013](../adr/0013-two-layer-architecture-and-domain-simplification.md)):

```text
<tenant>/context                     # Tenant-wide guidelines, security policies, global skills, blueprints
<tenant>/<project>/context           # Project architecture, domain glossary, component map, conventions
<tenant>/<project>/<component-a>     # Component source repo + component-specific Markdown context
<tenant>/<project>/<component-b>     # Component source repo + component-specific Markdown context
<tenant>/<project>/deploy            # Deployment manifests, infrastructure configs, promotion pipelines
```

### How it works

1. **Automatic project association:** Any repository created or pushed under `<tenant>/<project>/*` is automatically associated with `<project>`. `<tenant>/context` is scoped to the tenant and readable by all projects in that tenant.
2. **Standard Markdown & Git:** Engineers and agents read and update knowledge using standard Git commits, branches, and merges or via the Kodama MCP / API endpoints.
3. **Mounted or queried by agents:**
   - **Mode A (External harnesses like `opencode`, `claude-code`, `pi`):** The pipeline step or workspace preparation checks out `<tenant>/context` and `<tenant>/<project>/context` alongside the component repository (for example under `/workspace/.context/`), so the harness reads them as ordinary local Markdown files.
   - **Mode B (Native `kodama agent`):** Can read `/workspace/.context/` directly or query knowledge-base files, skills, and blueprints via native tools and the external MCP proxy.

## Recommended repository layout for `context` repos

```text
context/
├── README.md                # High-level overview and index
├── architecture.md          # System boundaries, data ownership, and service map
├── conventions.md           # Coding style, testing rules, error handling
├── blueprints/              # Stack templates and starter guides (e.g., go-postgres.md)
└── skills/                  # Reusable agent skill instructions (plan, review, guardrail, release)
```

## Updating knowledge as part of the SDLC

Because the knowledge base is a Git repository in the project space, agents and engineers can propose updates to `<tenant>/<project>/context` as part of normal task execution when architecture or conventions evolve.
