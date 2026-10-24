# Kodama release image.
#
# Multi-stage build of the one static binary the chart runs:
#   * build: Go 1.27.2 compiles cmd/kodama (server, migrate, bootstrap, step);
#   * runtime: Debian trixie-slim with git (the server shells out to it),
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
FROM --platform=${BUILDPLATFORM} docker.io/library/golang:1.27.2-trixie@sha256:47667934874913a449b96faa7ef4777fa285d9ec29126ee80ed61c45b7cccf0e AS build

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

FROM docker.io/library/debian:trixie-slim@sha256:62bd423aeee912639ca8ee681c2b762cb3e25137e0a92b20be455898a9f034ad

ARG VERSION=0.0.0
ARG REVISION=unknown

RUN set -eu; \
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
