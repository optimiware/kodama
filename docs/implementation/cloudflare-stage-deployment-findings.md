# Cloudflare stage deployment findings

**Status:** observed while deploying the Optimi website through the `work/vercel-replacement` build.

**Goal:** one Kodama pipeline step builds the Next.js repository and deploys it to Cloudflare, with stage and production targets, without manual provider or cluster operations.

**Outcome at the time of writing:** the Worker and the `stage.website.optimi-lab.com` CNAME are live. The Kodama run has not succeeded: its first attempt failed with `startup_timeout` before the command started. The live site came from a manual validation deployment, so this is not yet a Kodama acceptance result.

## What worked

- Direct project-secret delivery to an ordinary container step.
- A digest-pinned container step can run arbitrary build and deploy tooling.
- The project, secret, work item, workspace, pipeline definition and run APIs are sufficient to express a single-step deployment.
- Cloudflare Workers custom domains work on the existing partial `optimi-lab.com` zone when the external DNS CNAME points at the per-host `*.cdn.cloudflare.net` target.

## Pain points

### P0: no executable Cloudflare target driver

The target-profile example names a `cloudflare-worker` profile, but its driver image is a placeholder and the runtime-side materialization is explicitly not implemented. A deployment that follows the target-profile documentation stops before it reaches Cloudflare.

**Effect:** the only working route is a hand-written container step that calls Wrangler itself. It has no Kodama deployment record, status, resource identity, cleanup or gate integration.

**Recommended change:** ship one reference driver image implementing `kodama-deploy/v1`, with Wrangler and an artifact download step, and install a real `target-profiles.yaml` example that passes `kodama validate` and a local smoke test.

### P0: build-to-deploy artifact handoff is missing

A build step can declare a file artifact, but a later step cannot obtain its bytes. `KODAMA_INPUT_*` exposes an artifact reference, not a downloaded bundle, and `v1alpha1` has no shared workspace. The result is that the requested two-step `build → deploy` pipeline cannot be made to work without an external bridge.

**Effect:** users either put build and deploy in one step, or invent an S3/registry handoff. Neither is the documented workflow.

**Recommended change:** either materialize a declared artifact into the consumer step at a fixed path, or enable the designed shared `/workspace` path for this use case. Add one end-to-end two-step fixture showing a build output consumed by a deploy step.

### P0: static CIDR-only egress is a poor fit for GitHub, npm and Cloudflare

The tenant policy accepts only IP blocks. GitHub, npm and Cloudflare use changing address sets, so the first deployment required a broad `0.0.0.0/0:443` live patch. That is both operationally fragile and a security regression.

**Effect:** every new external deployment requires a cluster-policy change, and the policy no longer expresses the intended destinations.

**Recommended change:** add named egress profiles with provider-aware destination sets or an approved egress proxy. At minimum document a supported HTTPS-proxy path for build/deploy steps and validate it in the installation preflight.

### P0: image startup timeout is opaque

The first run failed with `startup_timeout`. There were no command logs, no retained Job or Pod, and no image-pull event in the tenant namespace. The attempt never ran. A large pinned Node image is the leading hypothesis, but the result did not say whether the cause was scheduling, image pull, admission, or a missing secret.

**Effect:** the operator cannot tell whether to change the image, pre-pull it, increase the deadline, or fix a policy.

**Recommended change:** make the per-step or per-profile startup deadline configurable, surface the last Job condition/image-pull reason in the attempt, and retain a short diagnostic artifact for startup failures. A pre-pull helper for pinned builder images would also remove this class of failure.

### P1: automation identity onboarding is incomplete

The one-time tenant bootstrap creates one human administrator. There is no supported API/CLI flow to create a service identity and grant it `admin` or `engineer`. The deployment required a direct database membership insert.

**Effect:** unattended deployment cannot be onboarded through the product, and operators must bypass the authority model.

**Recommended change:** expose an operator-authorized service-principal flow, or document and support a bootstrap command that adds a second administrative binding without direct SQL.

### P1: first pipeline requires too much implicit context

A first run needs, in order: a project with a repository binding, a work item with specification and acceptance criteria, `backlog → ready → active` transitions, workspace provisioning, a source commit that belongs to the workspace history, a registered definition, a secret binding and a keyed run request. Most of these preconditions are only discovered from error responses.

**Effect:** a simple first deployment takes many calls and deep knowledge of the lifecycle. It is easy to leave a run stuck in `running` with a failed attempt.

**Recommended change:** add a `kodama project bootstrap` / `kodama run quickstart` command or a documented one-command example that performs the complete preflight and reports every missing prerequisite before creating any state. Include a minimal website-to-Cloudflare example in the installation guide.

### P1: provider account/zone preflight is missing

The Cloudflare zone for `optimi-lab.com` lives in the `optimi testenv` account, not the main account. The first Worker was deployed to the wrong account. The token was valid for both accounts, so only the later custom-domain call exposed the mismatch. Custom domains also require a per-host CNAME target that was not discoverable until after the Worker existed.

**Effect:** deployment failures look like permission failures, and the operator must discover account, zone and DNS targets manually.

**Recommended change:** make account/zone/token ownership an explicit target-profile validation step, and have the driver return the exact DNS records the operator must create (or create them when the zone is Cloudflare-managed).

### P1: secret-to-environment naming is easy to get wrong

Project secret names are lowercase slug names, while the environment variable is a separate binding. The requested `CLOUDFLARE_TESTENV` is not a valid secret name, and the installation had no such token. The working shape was a lowercase secret reference bound to `CLOUDFLARE_API_TOKEN`.

**Effect:** users naturally try to put the environment-variable name in the secret reference or expect the secret name to become the variable name.

**Recommended change:** document a canonical example, validate the mapping at definition registration, and support a `secretFiles`-backed Wrangler auth file in the reference driver.

### P2: environment-pinned steps and ordinary secret bindings have surprising precedence

A step that pins an environment receives the environment's credentials rather than its own declared secrets. A writer step that declares a secret also cannot publish source. Both are reasonable safeguards, but neither is obvious from the pipeline examples.

**Recommended change:** add explicit validation diagnostics and a “deployment step” example that distinguishes project secrets, environment credentials and writer restrictions.

### P2: run observability is incomplete during startup failures

A run can remain `running` with a failed attempt and `failureReason: startup_timeout`, while the run and logs endpoints return partial or proxy-timeout responses. That makes it hard to tell whether to cancel, retry, or wait.

**Recommended change:** terminalize or expose a clear waiting reason when every attempt has failed; keep startup diagnostics for the attempt; make `run get` responsive while a step is running.

## Minimal path to 100%

1. **One executable target:** install a real Cloudflare driver and a passing target-profile example.
2. **One artifact bridge:** materialize a build output into the deploy step, or officially enable the shared workspace for the two-step case.
3. **One egress profile:** a supported npm/GitHub/Cloudflare HTTPS path that does not require `0.0.0.0/0`.
4. **One startup diagnostic:** a configurable image startup deadline plus the last pull/scheduling reason on the attempt.
5. **One automation identity flow:** a supported service-principal bootstrap with `engineer` and `admin` roles.
6. **One quickstart:** a single documented command/API sequence that takes a project URL, pipeline definition, secret references and environment inputs and produces a run, preflighting the work item and workspace requirements first.
7. **One provider preflight:** account/zone/token validation and the exact DNS records required for the custom hostname.

## Acceptance checklist

- [ ] A clean Kodama installation can add the Cloudflare target from committed configuration.
- [ ] A build step produces an artifact that a separate deploy step consumes.
- [ ] The deploy step runs without a manual Wrangler invocation or direct database/cluster changes.
- [ ] The run reaches `succeeded` and the attempt has command logs.
- [ ] The stage hostname returns HTTP 200 from the pipeline’s own verification.
- [ ] The production target is a separate profile/environment with separate credentials and DNS.
- [ ] A failed image pull or startup displays an actionable reason and retains diagnostics.
