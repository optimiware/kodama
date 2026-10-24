---
id: T-051
milestone: M4
status: deferred
depends_on: [T-050]
decision_blockers: []
owns: [docs/user/analyst.md, test/e2e/platform-analyst/]
---
# T-051 — Analyst input and review

## Goal
Make specifications and business acceptance usable without a CLI.

## Contract
Remain deferred with the optional Platform-only UI under O-15. Consume Platform's agreed shell and unchanged public work/gate APIs through T-050's upstream handoff; this task does not own a local `web/` tree or the sibling checkout. Display candidate/content/deployment identity before approval and preserve conflict handling on concurrent edits.

## Deliverables
Kodama-side analyst workflow/API handoff, specification/criteria editing, preview/evidence review, reject/rework flow, explicit completion, accessible business navigation requirements and qualified upstream UI usability evidence.

## Acceptance
`analyst_spec_to_review`, `stale_approval_ui`, `rejection_to_rework`, and `human_completion_ui` verify WM-02/WM-07 and DQ-03–DQ-06/DQ-11/DQ-13.

## Out of scope
New completion-policy engines or hiding current evidence identity behind an unconditional approve button.
