# syntax=docker/dockerfile:1

FROM golang:1.26.1-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/paje \
    ./cmd/paje

FROM alpine:3.22

RUN apk add --no-cache ca-certificates git openssh-client \
    && addgroup -S -g 65532 paje \
    && adduser -S -D -H -u 65532 -G paje paje \
    && mkdir -p /workspace \
    && chown 65532:65532 /workspace

COPY --from=build /out/paje /usr/local/bin/paje

ENV HOME=/tmp \
    PAJE_WORKSPACE_ROOT=/workspace
WORKDIR /workspace
USER 65532:65532

ENTRYPOINT ["/usr/local/bin/paje"]
