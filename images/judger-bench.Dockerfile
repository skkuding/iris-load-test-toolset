# syntax=docker/dockerfile:1
#
# Digest-pinned direct Judger benchmark image.
#
# The Judger v1.0.0-alpha.4 release publishes no checksums, so the amd64
# artifact digest is pinned here and verified during the build. judger-bench
# re-checks the same digest at run time via --judger-sha256 before it executes
# any user code.
#
# alpha.4 requires root (it creates cgroups), derives
# /sys/fs/cgroup/sandbox-<CONTAINER_ID> from the CONTAINER_ID environment
# variable, and never accepts an explicit cgroup parent. The benchmark must
# therefore run this image inside a delegated cgroup subtree and pass the
# delegated parent to judger-bench. Samples whose reported cgroup falls outside
# that parent are rejected as containment failures, never silently accepted.

ARG JUDGER_VERSION=v1.0.0-alpha.4
ARG JUDGER_AMD64_SHA256=2c9a4da817e06f49daabe6b437f81d10f78b42be517d2e345ab4f868a4189103

FROM golang:1.25 AS gobuild
WORKDIR /src
COPY go.mod ./
COPY cmd/judger-bench ./cmd/judger-bench
COPY internal/artifact ./internal/artifact
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/judger-bench ./cmd/judger-bench

FROM ubuntu:24.04
ARG JUDGER_VERSION
ARG JUDGER_AMD64_SHA256

# gcc/g++ match the production Iris image toolchain so the compile operation
# uses the same compiler generation and production-equivalent flags.
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl gcc g++ \
 && rm -rf /var/lib/apt/lists/*

RUN mkdir -p /app/sandbox \
 && curl -fsSL \
      "https://github.com/skkuding/Judger/releases/download/${JUDGER_VERSION}/libjudger-amd64.so" \
      -o /app/sandbox/libjudger.so \
 && echo "${JUDGER_AMD64_SHA256}  /app/sandbox/libjudger.so" | sha256sum -c - \
 && chmod 0750 /app/sandbox/libjudger.so

COPY --from=gobuild /out/judger-bench /usr/local/bin/judger-bench

ENV LIBJUDGER_PATH=/app/sandbox/libjudger.so
ENTRYPOINT ["/usr/local/bin/judger-bench"]
