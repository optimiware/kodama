# Deployment target profiles

A deployment target is selected by an installation-owned **target profile**. The
profile names the provider, the artifact kinds it accepts, the credentials an
environment must bind, the egress profiles it may use and the pinned driver that
performs provider-specific work. Environments store only the profile name and
their nonsecret target configuration; the server validates both at creation and
pins the profile digest and resolved values into the immutable snapshot.

`target-profiles.example.yaml` is an example, not an installed statement. Copy it
to the secrets directory as `target-profiles.yaml`, or set
`KODAMA_TARGET_PROFILES_FILE`. The file is strict YAML: unknown fields, duplicate
profiles, a missing `apply` command on an image driver, an unpinned driver image
or an invalid config pattern refuse startup.

The builtin `kubernetes-namespace` profile is always present. An environment with
no `targetProfile` keeps the legacy Kubernetes `namespace`/`kubeconfig` shape
and its existing delivery-tools scripts. An environment that selects a profile
uses `targetConfig` instead.

## Example

```sh
kodama environment create production \
  --kind production \
  --target-profile cloudflare-worker \
  --target-config accountId=0123456789abcdef0123456789abcdef \
  --target-config workerName=website-prod \
  --target-config route='optimi.com/*' \
  --secret CLOUDFLARE_API_TOKEN=secret://project/cloudflare-token@file
```

The Cloudflare API token is a project secret delivered as a file. Admission
pins the profile digest, target configuration and credential versions into the
snapshot; the server never calls Cloudflare itself. A profile driver must be a
digest-pinned image implementing `kodama-deploy/v1`; the runtime-side
materialization of the pinned target for that driver and a live Cloudflare
qualification are the next implementation step, not claimed here. A project may
still use an ordinary deploy script for the builtin Kubernetes profile.

## Kubernetes compatibility

Existing environments and deployments keep the legacy shape and the v1 snapshot
digest. New profile environments use a v2 snapshot digest that includes the
target profile, its resolved provider/artifact/egress values and the target
configuration. A profile change is explicit: an environment pins its profile
digest and refuses to resolve when the installed profile no longer matches.
