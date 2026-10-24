---
id: T-036
milestone: M2
status: done
depends_on: [T-010, T-012, T-013, T-014, T-020]
decision_blockers: []
owns: [internal/integrations/gitwebhook/, api/git-webhooks.yaml]
check: make check-go PKGS=./internal/integrations/gitwebhook/...
resources: [go]
acceptance: [forged_signature, duplicate_delivery, foreign_repository, exact_commit_trigger]
---
# T-036 — Native Git push trigger

## Goal
Start authorized runs from verified exact-commit Git push notifications.

## Contract
O-03 selects a native-first path: a server-side receive hook on a Git host the installation controls sends an authenticated push notification to Kodama. Ordinary HTTPS/token Git remains the transport; this task does not build a Git wire-protocol server. Git itself does not notify an unrelated control plane, and third-party hosts may need their own later webhook adapter.

Define one bounded hook payload/authentication/delivery contract, bind repository/project/definition/trigger settings and secret authority explicitly, and verify/fetch the exact accepted commit through the existing Git port. Authenticate installation of the trigger and every notification; a caller-supplied repository or tenant is not authority. GitHub and other provider adapters are later contributions behind the same trigger seam.

## Deliverables
Authenticated native receive-hook handler and example, delivery deduplication through the persistence seam, exact-commit trigger, safe branch/source filters, audit and API fragment. Use in-memory persistence and Git-port doubles for unit acceptance. T-038 owns the actual receive-hook/Git/admission join; no external provider adapter.

## Acceptance
`forged_signature`, `duplicate_delivery`, `foreign_repository`, and `exact_commit_trigger` cover AC-07 and PX-02 through authenticated requests and controlled port responses. Verify untrusted inputs never inherit privileged credentials and branch movement cannot substitute another commit. Real Git/persistence behavior is required at T-038.

## Out of scope
Git server/transport implementation, mandatory GitHub integration, universal provider abstraction, or treating hook payloads as trusted executable commands.
