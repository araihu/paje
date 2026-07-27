# syntax=docker/dockerfile:1

FROM golang:1.26.1-alpine AS revision

ARG PAJE_COMMIT
RUN printf '%s\n' "${PAJE_COMMIT}" | grep -Eq '^[0-9a-f]{40}$' \
    || { printf '%s\n' 'PAJE_COMMIT must be a full 40-character lowercase hexadecimal Git commit' >&2; exit 1; }

FROM revision AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/paje \
    ./cmd/paje \
    && CGO_ENABLED=0 go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/paje-leaf-gateway \
    ./cmd/paje-leaf-gateway

FROM alpine:3.22

ARG CA_CERTIFICATES_PACKAGE_VERSION=20260611-r0
ARG GIT_PACKAGE_VERSION=2.49.1-r0
ARG OPENSSH_PACKAGE_VERSION=10.0_p1-r10
RUN apk add --no-cache \
    "ca-certificates=${CA_CERTIFICATES_PACKAGE_VERSION}" \
    "git=${GIT_PACKAGE_VERSION}" \
    "openssh-client-default=${OPENSSH_PACKAGE_VERSION}"

ARG PAJE_COMMIT
LABEL org.opencontainers.image.revision="${PAJE_COMMIT}" \
    org.opencontainers.image.source="https://github.com/araihu/paje" \
    io.araihu.paje.git.version="2.49.1" \
    io.araihu.paje.openssh.version="10.0_p1-r10"

RUN addgroup -S -g 65532 paje \
    && adduser -S -D -H -u 65532 -G paje paje \
    && mkdir -p /workspace /run/paje /run/paje-leaf /var/lib/paje-leaf \
    && chown 65532:65532 /workspace /run/paje /run/paje-leaf /var/lib/paje-leaf

COPY --from=build /out/paje /usr/local/bin/paje
COPY --from=build /out/paje-leaf-gateway /usr/local/bin/paje-leaf-gateway

ENV HOME=/run/paje \
    PAJE_WORKSPACE_ROOT=/workspace \
    PAJE_RUN_ROOT=/workspace/runs \
    PAJE_ARTIFACT_ROOT=/workspace/artifacts \
    PAJE_RUNTIME_ROOT=/run/paje
WORKDIR /workspace
USER 65532:65532

ENTRYPOINT ["/usr/local/bin/paje"]
