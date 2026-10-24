# Kodama release image.
#
# Multi-stage build of the one static binary the chart runs:
#   * build: Go 1.27.2 compiles cmd/kodama (server, migrate, bootstrap, step);
#   * runtime: Debian bookworm-slim with git (the server shells out to it),
#     ca-certificates (system trust for PostgreSQL, NATS, S3, Git, OIDC, KMS),
#     and cp (the chart's default entrypoint injector command).
#
# The binary is /usr/local/bin/kodama and the entrypoint, so chart args
# ("server", "--assembly", "core" and "migrate") append to it. The process
# runs as uid 65532 with a read-only root; writable scratch is the /tmp mount.
#
# Image tags are semver git tags. This tree commits 0.0.1 in VERSION; the
# main-branch workflow publishes that tag and then increments the patch.
# Keep the golang image tag aligned with dev/versions.lock.json.
#
#   podman build -f Containerfile -t localhost/kodama:0.0.1 .

ARG BUILDPLATFORM
FROM --platform=${BUILDPLATFORM} docker.io/library/golang:1.27.2-bookworm@sha256:5cf287a799e6b94384bad13d16b14904c531f51ba65792237e122ce42b392f61 AS build

ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN set -eu; \
    os="${TARGETOS:-linux}"; \
    arch="${TARGETARCH:-$(go env GOARCH)}"; \
    CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" \
    go build -mod=readonly -trimpath -buildvcs=false \
    -ldflags '-s -w -buildid=' \
    -o /out/kodama ./cmd/kodama

FROM docker.io/library/debian:bookworm-slim@sha256:7ff6ba31527b9e336162fa061746c366c4b2032f4d2cff2078bea43f78efe3f8

ARG SNAPSHOT=20261020T213255Z
ARG VERSION=0.0.0
ARG REVISION=unknown

RUN set -eu; \
    printf 'Types: deb\nURIs: http://snapshot.debian.org/archive/debian/%s/\nSuites: bookworm bookworm-updates\nComponents: main\nSigned-By: /usr/share/keyrings/debian-archive-keyring.gpg\n\nTypes: deb\nURIs: http://snapshot.debian.org/archive/debian-security/%s/\nSuites: bookworm-security\nComponents: main\nSigned-By: /usr/share/keyrings/debian-archive-keyring.gpg\n' "$SNAPSHOT" "$SNAPSHOT" > /etc/apt/sources.list.d/debian.sources; \
    echo 'Acquire::Check-Valid-Until "false";' > /etc/apt/apt.conf.d/99snapshot; \
    apt-get update; \
    apt-get install -y --no-install-recommends git ca-certificates; \
    rm -rf /var/lib/apt/lists/* /var/cache/apt /var/log/apt /var/log/dpkg.log /var/log/alternatives.log /var/cache/ldconfig/aux-cache

COPY --from=build --chmod=0755 /out/kodama /usr/local/bin/kodama

LABEL org.opencontainers.image.title="kodama" \
      org.opencontainers.image.description="Kodama control plane: server, migrate, and the static step binary" \
      org.opencontainers.image.source="https://github.com/optimiware/kodama" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}" \
      io.kodama.binary="/usr/local/bin/kodama"

ENV HOME=/tmp \
    TZ=UTC
USER 65532:65532
WORKDIR /tmp
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/kodama"]
CMD ["server"]
