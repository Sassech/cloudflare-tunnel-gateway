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

# Static binary: no libc, no cgo — required for a scratch/distroless final
# stage and for a read-only root filesystem (REQ-7.2).
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
    -o /out/cf-watcher ./cmd/cf-watcher

# --- Final ----------------------------------------------------------------
# distroless/static: no shell, no package manager, ships ca-certificates
# (needed for TLS to the Cloudflare API) — unlike scratch, no manual CA
# bundle is required. Docker accepts a numeric USER with no /etc/passwd
# entry, so uid 1000 works even though distroless only ships root+nonroot.
FROM gcr.io/distroless/static-debian12

COPY --from=builder /out/cf-watcher /cf-watcher

# Non-root numeric UID (REQ-7.1), matching the project's established
# convention and the docker-compose group_add setup for docker.sock access.
USER 1000

ENTRYPOINT ["/cf-watcher"]
