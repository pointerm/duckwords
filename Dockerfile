ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown

# Keep the compiler and standard library aligned with go.mod and CI. The digest
# pins the multi-platform image index while --platform selects the native builder.
FROM --platform=$BUILDPLATFORM golang:1.26.6-alpine3.23@sha256:5978cc992ad5ef96a7469713c8af849c1433824761ce3be2c56381403cd8d9a3 AS build

WORKDIR /src

# Dependency metadata changes less often than source, so this layer remains
# reusable without carrying the module cache into the runtime image.
COPY go.mod go.sum ./
RUN go mod download && go mod verify

COPY cmd/ ./cmd/
COPY internal/ ./internal/

ARG TARGETOS
ARG TARGETARCH
ARG VERSION
ARG COMMIT
ARG BUILD_DATE

# A static binary lets the runtime contain no libc, package manager, or shell.
# -buildvcs=false makes the explicitly supplied release metadata authoritative.
RUN CGO_ENABLED=0 GOFIPS140=off GOFLAGS= GOWORK=off GOENV=off GOEXPERIMENT= \
    GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
    go build \
    -mod=readonly \
    -trimpath \
    -buildvcs=false \
    -ldflags="-s -w -buildid= -X github.com/pointerm/duckwords/internal/buildinfo.version=${VERSION} -X github.com/pointerm/duckwords/internal/buildinfo.commit=${COMMIT} -X github.com/pointerm/duckwords/internal/buildinfo.buildDate=${BUILD_DATE}" \
    -o /duckwords \
    ./cmd/duckwords

# The fixture image is deliberately a compiled Go test binary. It exercises the
# exact CLI and production composition through injected offline transports, while
# the production binary contains no fixture switch or endpoint override.
FROM build AS fixture-build

ARG TARGETOS
ARG TARGETARCH

RUN CGO_ENABLED=0 GOFIPS140=off GOFLAGS= GOWORK=off GOENV=off GOEXPERIMENT= \
    GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
    go test -c \
    -mod=readonly \
    -trimpath \
    -buildvcs=false \
    -ldflags="-s -w -buildid=" \
    -o /duckwords-fixture \
    ./cmd/duckwords

FROM scratch AS runtime-base

# HTTPS is required for approved live runs and remote assignment inputs. Copying
# only the trusted root bundle and required redistribution notice keeps both final
# images minimal, compliant, and shell-free.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY THIRD_PARTY_NOTICES.md /THIRD_PARTY_NOTICES.md

ENV SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt
WORKDIR /workspace

# Numeric IDs work in scratch without adding passwd/group files. DuckWords writes
# only stdout/stderr, so this identity is compatible with a read-only rootfs.
USER 65532:65532

FROM runtime-base AS fixture

COPY --from=fixture-build /duckwords-fixture /duckwords-fixture
ENV DUCKWORDS_OFFLINE_FIXTURE_PROCESS=1
ENTRYPOINT ["/duckwords-fixture"]

FROM runtime-base AS runtime

ARG VERSION
ARG COMMIT
ARG BUILD_DATE

LABEL org.opencontainers.image.title="DuckWords" \
      org.opencontainers.image.description="Deterministic Reddit comment word-frequency CLI" \
      org.opencontainers.image.source="https://github.com/pointerm/duckwords" \
      org.opencontainers.image.version="$VERSION" \
      org.opencontainers.image.revision="$COMMIT" \
      org.opencontainers.image.created="$BUILD_DATE"

COPY --from=build /duckwords /duckwords

# Exec form preserves flags, signals, exit status, and stdout/stderr separation.
ENTRYPOINT ["/duckwords"]
