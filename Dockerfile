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
    ./cmd/paje

FROM node:24.4.1-alpine3.22

ARG CODEX_VERSION=0.144.5
RUN npm install --global "@openai/codex@${CODEX_VERSION}" \
    && npm cache clean --force \
    && codex --version

ARG PAJE_COMMIT
LABEL org.opencontainers.image.revision="${PAJE_COMMIT}" \
    org.opencontainers.image.source="https://github.com/araihu/paje" \
    io.araihu.paje.codex.version="${CODEX_VERSION}"

RUN apk add --no-cache ca-certificates git openssh-client \
    && addgroup -S -g 65532 paje \
    && adduser -S -D -H -u 65532 -G paje paje \
    && mkdir -p /workspace /run/paje \
    && chown 65532:65532 /workspace /run/paje

COPY --from=build /out/paje /usr/local/bin/paje

ENV HOME=/run/paje \
    PAJE_WORKSPACE_ROOT=/workspace \
    PAJE_RUN_ROOT=/workspace/runs \
    PAJE_ARTIFACT_ROOT=/workspace/artifacts \
    PAJE_RUNTIME_ROOT=/run/paje
WORKDIR /workspace
USER 65532:65532

ENTRYPOINT ["/usr/local/bin/paje"]
