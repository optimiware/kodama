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
the default tag of `scripts/lib/deploy.sh` carry the same value. Set the
version with `make release-prepare VERSION=<version>`, never by hand.

A version has one of three forms:

| Form | Example | Meaning |
|---|---|---|
| Release | `0.2.0` | Published. The README and the Podman quickstart install it, and the changelog has its section. |
| Release candidate | `0.2.0-rc.1` | Published as a pre-release; `latest` does not move. The documents keep naming the last release. |
| Development | `0.3.0-dev` | The state of `master` between releases. It is never published. |

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
| `kodama` image (`linux/amd64`, `linux/arm64`) | `ghcr.io/optimiware/kodama/kodama:<version>` (and `:latest` for non-prerelease versions); the release notes give the manifest index digest |
| Charts `kodama` and `kodama-stack` | `oci://ghcr.io/optimiware/kodama/charts`, and attached to the GitHub release |
| `kodactl` for linux and darwin, amd64 and arm64 | `kodactl_<version>_<os>_<arch>.tar.gz` attached to the GitHub release |
| `SHA256SUMS` | attached to the GitHub release; covers the archives and chart packages |

The packages are named after this repository because
`ghcr.io/optimiware/kodama` holds the images of the archived v1 repository.
The image is published as a multi-architecture OCI manifest index covering
`linux/amd64` and `linux/arm64`; non-prerelease versions (`PRERELEASE=false`)
also push the `:latest` tag pointing to the same manifest digest. Nothing is
signed and no SBOM is produced. The GHCR packages are public, like the
repository.

## Cut a release

1. Prepare the version on a branch and open a pull request:

   ```sh
   make release-prepare VERSION=0.2.0
   ```

   It rewrites every file that carries the version. For a release it also
   updates the version the README and the Podman quickstart install, and
   renames the changelog's `Unreleased` section to the version. Review that
   section before you commit.
2. Merge the pull request. The change of `VERSION` on `master` starts the
   release workflow. It runs the CI jobs, builds every artifact, installs the
   image with the Podman bundle and runs a pipeline
   ([`scripts/release-smoke`](../../scripts/release-smoke)), and only then
   publishes and creates the tag `v<version>` on that commit.
3. Set the next development version the same way, for example
   `make release-prepare VERSION=0.3.0-dev`, and merge it. Builds and charts
   from `master` then no longer carry the released version.

For a large change, publish a release candidate first (`0.2.0-rc.1`), try it,
then prepare the release.

A released version is never replaced. The workflow refuses a version whose
GitHub release exists, and the repository rules in
[`.github/rulesets`](../../.github/rulesets) refuse deleting or moving a `v*`
tag and rewriting `master`. They are applied once per repository:

```sh
gh api repos/optimiware/kodama/rulesets --method POST --input .github/rulesets/release-tags.json
gh api repos/optimiware/kodama/rulesets --method POST --input .github/rulesets/master.json
```

To rehearse without publishing, start the workflow by hand on any branch. It
builds everything, runs the installation check and publishes nothing:

```sh
gh workflow run release.yml --ref <branch>
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

The GitHub release is created last, so it marks a complete publication.

- Before the "Push the image" step nothing is published. Fix `master`, then
  publish the same version by hand:

  ```sh
  gh workflow run release.yml --ref master -f publish=true
  ```

- After it, when the failure was transient, re-run the failed job. While the
  GitHub release does not exist, the image and chart pushes replace the
  incomplete publication of the same version.
- Once the GitHub release exists the version is final. Publish a fix as the
  next patch version.
