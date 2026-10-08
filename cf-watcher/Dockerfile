# syntax=docker/dockerfile:1

# --- Builder ------------------------------------------------------------
# go.mod requires go >= 1.25.0 (auto-bumped when github.com/docker/docker
# was added — its module graph needs a newer toolchain than 1.23).
FROM golang:1.25-alpine AS builder

WORKDIR /src

# Cache module downloads separately from source changes.
COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ ./cmd/
COPY internal/ ./internal/

# Static binaries: no libc, no cgo — required for a scratch/distroless final
# stage and for a read-only root filesystem. healthcheck is a second, tiny
# binary (see cmd/healthcheck) used by HEALTHCHECK below, since the final
# image has no shell/curl/wget to run a normal check command.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
    -o /out/cf-watcher ./cmd/cf-watcher
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
    -o /out/healthcheck ./cmd/healthcheck

# Mount point for the shared tunnel-token volume (distroless has no shell to
# mkdir in the final stage). See the COPY --chown below.
RUN mkdir -p /out/run/cloudflared

# --- Final ----------------------------------------------------------------
# distroless/static: no shell, no package manager, ships ca-certificates
# (needed for TLS to the Cloudflare API) — unlike scratch, no manual CA
# bundle is required. Docker accepts a numeric USER with no /etc/passwd
# entry, so uid 1000 works even though distroless only ships root+nonroot.
#
# Pinned by digest (docker:S6596): base image updates are deliberate,
# reviewable changes, never silent drift on rebuild.
FROM gcr.io/distroless/static-debian12@sha256:597c2b4bc7f353100af9b8b06bb4f126c4a45f9d8175e25d4f01f965d5d94396

COPY --from=builder /out/cf-watcher /cf-watcher
COPY --from=builder /out/healthcheck /healthcheck

# /run/cloudflared is where cf-watcher writes the tunnel connector token
# that cloudflared reads (--token-file). docker-compose mounts a named
# volume here; on first use Docker copies this directory's ownership and
# mode into the empty volume, which is what makes it writable by uid 1000
# despite read_only: true on the container's root filesystem.
#
# Ownership 1000:65532 / mode 0750: cf-watcher (uid 1000) writes; the
# official cloudflared image runs as 65532:65532 (distroless "nonroot"), so
# group 65532 grants it read/traverse access and nothing else gets any.
COPY --from=builder --chown=1000:65532 --chmod=0750 /out/run/cloudflared /run/cloudflared

# Non-root numeric UID, matching the project's established convention and
# the docker-compose group_add setup for docker.sock access. Primary GID
# 65532 (cloudflared's group) makes the token file cf-watcher creates
# group-readable by cloudflared with mode 0640 — see internal/tokenfile.
USER 1000:65532

# /healthcheck reads HEALTH_ADDR from the container env (same variable
# cf-watcher itself reads) and probes /healthz.
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD ["/healthcheck"]

ENTRYPOINT ["/cf-watcher"]
