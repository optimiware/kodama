# Releasing Kodama

This runbook describes how a Kodama version is built, versioned and published.

- Workflows: [`release.yml`](../../.github/workflows/release.yml) and
  [`ci.yml`](../../.github/workflows/ci.yml)
- Build scripts: [`scripts/dist`](../../scripts/dist) and
  [`scripts/image`](../../scripts/image)
- Changes and known limitations per version: [CHANGELOG.md](../../CHANGELOG.md)

## Versions

The [`VERSION`](../../VERSION) file holds the one release version of the tree.
`TestReleaseVersionIsConsistent` (`internal/installation`) fails `make check`
unless both charts, the stack chart's dependency, the default image tags and
the default tag of `scripts/lib/deploy.sh` carry the same value.

`0.1.0` is the first public release.

`kodama version` and `kodactl version` print the version and the source
revision of the binary:

```text
kodama 0.1.0 (revision <40 hex>, go1.27.3, linux/amd64)
```

`make build`, `make dist` and `scripts/image` stamp both values. A plain
`go build` reports version `dev`.

## What a release publishes

| Artifact | Where |
|---|---|
| `kodama` image | `ghcr.io/optimiware/kodama/kodama:<version>`; the release notes give the digest |
| Charts `kodama` and `kodama-stack` | `oci://ghcr.io/optimiware/kodama/charts`, and attached to the GitHub release |
| `kodactl` for linux and darwin, amd64 and arm64 | `kodactl_<version>_<os>_<arch>.tar.gz` attached to the GitHub release |
| `SHA256SUMS` | attached to the GitHub release; covers the archives and chart packages |

The packages are named after this repository because
`ghcr.io/optimiware/kodama` holds the images of the archived v1 repository.
The image is built for linux/amd64 only. Nothing is signed and no SBOM is
produced. The GHCR packages are public, like the repository.

## Cut a release

1. Set the version: edit `VERSION`, the `version` and `appVersion` of both
   `Chart.yaml` files, the `kodama` dependency version in
   `deploy/charts/kodama-stack/Chart.yaml`, the `image.tag` defaults of both
   `values.yaml` files, the default tag in `scripts/lib/deploy.sh` and the
   version in the README quick start and the Podman quickstart.
   `go test ./internal/installation/ -run TestReleaseVersionIsConsistent`
   names any place that was missed.
2. Add the version's section to `CHANGELOG.md`.
3. Push `master` and wait for CI. Its `artifacts` job builds every release
   artifact, including the image, and pushes nothing.
4. Tag the commit and push the tag:

   ```sh
   git tag -a v<version> -m "Kodama <version>" <commit>
   git push origin v<version>
   ```

The tag starts the release workflow. It runs the CI jobs again, refuses a tag
that differs from `v<VERSION>` or a commit that is not on `master`, and then
publishes. A `0.0.x` version or a version with a `-suffix` is marked as a
pre-release on GitHub.

To rehearse without publishing, start the workflow by hand on a branch:

```sh
gh workflow run release.yml --ref master
```

## Use a release

```sh
# kodactl
gh release download v<version> --pattern 'kodactl_*_linux_amd64.tar.gz' --pattern SHA256SUMS
sha256sum --check --ignore-missing SHA256SUMS

# image, pinned by the digest from the release notes
export KODAMA_IMAGE_REPOSITORY=ghcr.io/optimiware/kodama/kodama
export KODAMA_IMAGE_DIGEST=sha256:<64 hex>

# chart
helm pull oci://ghcr.io/optimiware/kodama/charts/kodama-stack --version <version>
```

`scripts/deploy` deploys the chart of the checkout, not a published one; see
[production-deployment.md](production-deployment.md) §7. The package is public,
so no image pull secret is needed.

## If the release workflow fails

- Before the "Push the image" step nothing is published. Fix `master`, delete
  the tag (`git push origin :refs/tags/v<version>`), and tag the fixed commit.
- After it, re-run the failed job when the failure was transient: the image
  and chart pushes replace the same version and an existing GitHub release
  only gets its files replaced.
- Never move the tag of a version that someone has already deployed. Publish
  the fix as the next patch version.
