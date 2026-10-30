# Cloudflare website example

This is the lean build-and-deploy project shape from the
[container-plugin plan](../../docs/implementation/container-plugins.md#92-website-project-two-step-build-and-cloudflare-deploy).

```text
build → deploy (Cloudflare Workers via Wrangler)
```

The publishing pipeline has no work item and no Environment or Deployment
entity. `status`, `rollback`, `reconcile` and `cleanup` are separate maintenance
definitions using the same state binding.

## Files

| Path | Purpose |
| --- | --- |
| `pipeline.yaml` | Proposed `kodama/v1alpha3` two-step publishing pipeline |
| `app/index.js` | Minimal Worker |
| `wrangler.jsonc` | Worker configuration |
| `ci/build-cloudflare.sh` | Produces the immutable bundle artifact |
| `maintenance/` | Status, rollback and cleanup definitions to register separately |

The `v1alpha3` example uses `artifactFiles` and `state` bindings. Those are
proposed runtime/plugin contracts in the implementation plan; the current
parser does not accept them until the container-plugin contract packages land.
Do not treat this document as executable on an older server.

The deploy image contains the pinned plugin and Wrangler. The API token is a
declared project secret; the account ID and Worker name are ordinary recipe
inputs. The build step has no deployment credential.

```sh
# build the example archive for inspection
./ci/build-cloudflare.sh /tmp/worker.tar.gz
```
