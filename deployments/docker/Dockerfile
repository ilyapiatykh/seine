# Builder stage — produces statically linked binaries with version stamping.
#
# Build args:
#   VERSION    — embedded into the binary via -ldflags (defaults to "dev")
#   COMMIT     — git short SHA
#   BUILD_DATE — UTC timestamp
#
# The image is rebuilt cheaply because go.mod / go.sum are copied first,
# so vendored deps are cached on subsequent builds when only Go source
# changes.
FROM golang:1.25-alpine AS builder

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ENV CGO_ENABLED=0 GOOS=linux
RUN go build -trimpath \
    -ldflags "-s -w \
        -X github.com/ilyapiatykh/seine/internal/buildinfo.Version=${VERSION} \
        -X github.com/ilyapiatykh/seine/internal/buildinfo.Commit=${COMMIT} \
        -X github.com/ilyapiatykh/seine/internal/buildinfo.BuildDate=${BUILD_DATE}" \
    -o /out/seine-server ./cmd/seine-server && \
    go build -trimpath \
    -ldflags "-s -w \
        -X github.com/ilyapiatykh/seine/internal/buildinfo.Version=${VERSION} \
        -X github.com/ilyapiatykh/seine/internal/buildinfo.Commit=${COMMIT} \
        -X github.com/ilyapiatykh/seine/internal/buildinfo.BuildDate=${BUILD_DATE}" \
    -o /out/seine-agent ./cmd/seine-agent

# Runtime stage — Alpine with the small set of OS packages the agent needs:
# iptables for the hub firewall, iproute2 for diagnostic ip(8) inside the
# container, ca-certificates for HTTPS to git remotes.
FROM alpine:3.20 AS runtime

RUN apk add --no-cache ca-certificates iptables iproute2 tini

COPY --from=builder /out/seine-server /out/seine-agent /usr/local/bin/

ENTRYPOINT ["/sbin/tini", "--"]
