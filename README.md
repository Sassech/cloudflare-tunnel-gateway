# Cloudflare Tunnel Gateway

Docker Compose stack that exposes your services via Cloudflare Tunnel with automatic ingress and optional DNS management.

## Services

- **cloudflared** — Cloudflare Tunnel connector (token mode, token read from a shared volume)
- **cf-watcher** — Go sidecar (~26MB) that provisions this host's tunnel, watches Docker containers with `cf.*` labels, keeps the tunnel ingress + DNS CNAMEs in sync, cleans up orphaned DNS records, and exposes health/metrics/reload endpoints — see [Operations](#operations)

Zero-touch per VPS: no tunnel to create in the dashboard, no connector token to copy. On boot cf-watcher reuses or creates this host's tunnel, hands its token to cloudflared, and from then on label a container → cf-watcher detects it → updates the tunnel.

## Prerequisites (one-time, in Cloudflare)

Done once per Cloudflare account, not per VPS.

1. **Create an API token** at [dash.cloudflare.com/profile/api-tokens](https://dash.cloudflare.com/profile/api-tokens) → _Create Token_ → _Custom token_, with:
   - **Account > Cloudflare Tunnel > Edit** — always required (list/create tunnels, fetch the connector token, manage ingress).
   - **Zone > DNS > Edit** — only if any container uses `cf.dns=true`; scope it to the zone you set in `CF_ZONE_ID`.
2. **Get your Account ID** — Dashboard → any domain → Overview → right sidebar → _Account ID_.
3. **Get your Zone ID** (only for `cf.dns=true`) — Dashboard → your domain → Overview → right sidebar → _Zone ID_.

## Quick start

On each VPS:

```bash
cp .env.example .env
# Fill in CF_API_TOKEN, CF_ACCOUNT_ID, DOCKER_GID (and CF_ZONE_ID if using cf.dns=true)

docker compose up -d
```

What happens on first boot:

1. cf-watcher looks up a live tunnel named `CF_TUNNEL_NAME` (default `cf-watcher-<host hostname>`) in the account, and creates it (remotely managed) if it doesn't exist.
2. It fetches the tunnel's connector token and writes it to the shared `cloudflared-token` volume (`/run/cloudflared/token`).
3. Only then does cf-watcher report healthy — cloudflared waits for that (`depends_on: condition: service_healthy`) and starts with `--token-file`.
4. cf-watcher pushes the ingress for all labeled containers and reconciles DNS.

Restarts are idempotent: the same tunnel is found by name and reused; the token file is only rewritten if it changed.

### Where each value comes from

| Variable        | Where to find it                                                                           |
| --------------- | ------------------------------------------------------------------------------------------ |
| `CF_API_TOKEN`  | Created in [Prerequisites](#prerequisites-one-time-in-cloudflare)                           |
| `CF_ACCOUNT_ID` | Dashboard > right sidebar > Account ID                                                     |
| `CF_ZONE_ID`    | Dashboard > your domain > Overview > Zone ID (right sidebar) — only if using `cf.dns=true` |
| `DOCKER_GID`    | `getent group docker \| cut -d: -f3` on the host                                           |

### Migrating from a manually created tunnel

Older setups set `CLOUDFLARED_TOKEN` and `CF_TUNNEL_ID`. `CLOUDFLARED_TOKEN` is no longer read — delete it. Keeping `CF_TUNNEL_ID` keeps using that existing tunnel (cf-watcher fetches its token itself). Removing it switches to auto-provisioning, which creates a **new** tunnel: delete the old tunnel and its CNAMEs (they point at the old tunnel ID and cf-watcher won't touch them) before or after switching.

## Configuration

Full reference of every environment variable `cf-watcher` reads (see `.env.example`):

| Variable            | Required                                       | Default                  | Description                                                                                                  |
| ------------------- | ---------------------------------------------- | ------------------------ | ------------------------------------------------------------------------------------------------------------ |
| `CF_API_TOKEN`      | yes                                            | —                        | Cloudflare API token — see [Prerequisites](#prerequisites-one-time-in-cloudflare)                            |
| `CF_ACCOUNT_ID`     | yes                                            | —                        | Cloudflare account ID (owns every VPS's tunnel)                                                              |
| `DOCKER_GID`        | yes (Compose only)                             | —                        | Host `docker` group GID, for non-root `docker.sock` access                                                   |
| `CF_ZONE_ID`        | only if `cf.dns=true` is used on any container | —                        | Zone ID for DNS CNAME management                                                                             |
| `CF_TUNNEL_NAME`    | no                                             | `cf-watcher-<hostname>`  | Tunnel to reuse or create. **Must be unique per VPS**                                                        |
| `CF_TUNNEL_ID`      | no                                             | — (provision by name)    | Pin an existing tunnel UUID instead of provisioning by name (`CF_TUNNEL_NAME` is then ignored)               |
| `TUNNEL_TOKEN_FILE` | no                                             | `/run/cloudflared/token` | Where the connector token is written; must match the shared volume and cloudflared's `--token-file`          |
| `DOCKER_NETWORK`    | no                                             | `cloudflared-network`    | Network cf-watcher checks labeled containers against (warns if mismatched)                                   |
| `LOG_LEVEL`         | no                                             | `INFO`                   | `DEBUG` \| `INFO` \| `WARNING` \| `ERROR` — unrecognized values fall back to `INFO`                          |
| `DEBOUNCE_SECONDS`  | no                                             | `2.0`                    | Debounce window before a tunnel PUT after a burst of container events                                        |
| `HEALTH_ADDR`       | no                                             | `:9090`                  | Listen address for `/healthz`, `/metrics`, `/reload`. `off` disables the server (breaks cloudflared ordering) |
| `RELOAD_TOKEN`      | no                                             | — (disabled)             | Enables `POST /reload`; required as the `X-Reload-Token` header value                                        |

`CLOUDFLARED_TOKEN` is no longer used.

The default tunnel name uses the **host's** hostname: the Compose file sets `uts: host` on cf-watcher, so it sees the VPS hostname instead of the random container ID (which would create a new tunnel every time the container is recreated). If you run cf-watcher without `uts: host`, set `CF_TUNNEL_NAME` explicitly.

## Multi-VPS deployment

Run the same `docker-compose.yaml` with the same `.env` on every VPS (only `DOCKER_GID` usually differs). Each VPS gets **its own tunnel**, created automatically, and all tunnels live in the same Cloudflare account.

Rules:

- **Tunnel names must be unique per VPS.** The default `cf-watcher-<hostname>` is unique as long as hostnames are. If two VPSes could share a hostname (e.g. cloned images left as `ubuntu`), set a distinct `CF_TUNNEL_NAME` on each.
- **Never set the same `CF_TUNNEL_NAME` or `CF_TUNNEL_ID` on two VPSes.** Both watchers would manage the same tunnel: each overwrites the other's ingress with its own containers, and each deletes the other's DNS records as orphans.
- **Public hostnames (`cf.hostname`) must be unique across all VPSes.** A hostname's CNAME can point at only one tunnel.
- **cf-watcher owns its tunnel's ingress entirely.** Public Hostnames added manually in the dashboard for that tunnel are wiped on the next sync.
- **Decommissioned VPSes leave their tunnel behind.** cf-watcher never deletes tunnels. When you retire a VPS, delete its tunnel (Zero Trust → Networks → Tunnels) and any CNAMEs pointing at it manually.
- If two live tunnels with the same name exist in the account, cf-watcher refuses to guess and exits with an error listing their IDs — delete the duplicate or pin one with `CF_TUNNEL_ID`.

## Exposing a service

Add the `cloudflared-network` and `cf.*` labels to any service:

```yaml
services:
  myapp:
    image: myapp:latest
    networks:
      - cloudflared-network
    labels:
      cf.hostname: "app.yourdomain.com"
      cf.port: "8080"
      cf.dns: "true" # optional: auto-create DNS CNAME
```

### Label reference

| Label         | Required | Description                                                                      |
| ------------- | -------- | -------------------------------------------------------------------------------- |
| `cf.hostname` | yes      | Public FQDN to expose                                                            |
| `cf.port`     | yes      | Internal container port (1-65535)                                                |
| `cf.service`  | no       | `http` (default) or `https`                                                      |
| `cf.dns`      | no       | `true` to auto-create a proxied CNAME via Cloudflare DNS (requires `CF_ZONE_ID`) |

### Connecting external Compose files

```yaml
networks:
  cloudflared-network:
    external: true
    name: cloudflared-network
```

Then add `networks: [cloudflared-network]` and `cf.*` labels to the service.

### Before first boot

cf-watcher **replaces the entire tunnel ingress** on startup with whatever labeled containers it finds running. Any ingress rules added manually via the dashboard will be lost. Make sure all services you want exposed have `cf.*` labels before starting.

## Architecture

```
Docker daemon ──► cf-watcher ──► Cloudflare API ──► tunnel (find or create by name)
                      │                          ├─► tunnel ingress config
                      │                          └─► DNS CNAMEs (cf.dns=true only)
                      │
                      ├─ writes connector token ──► cloudflared-token volume ──► cloudflared (--token-file)
                      │
                 detects start/stop of
                 labeled containers,
                 debounces changes (2s),
                 issues full-state PUT
```

DNS CNAMEs are created/deleted per-container only for services with `cf.dns=true`. Records created outside cf-watcher are never deleted. On every startup (and every `/reload`), cf-watcher also re-scans the zone for CNAMEs pointing at its own tunnel and deletes any left over from a previous process session whose container is no longer running — see [Operations](#operations).

cf-watcher also exposes an internal control/observability plane over HTTP (default `:9090`, reachable only from other containers on `cloudflared-network`):

```
                    :9090
  /healthz          503 until the tunnel token is written, then reflects the last sync attempt's outcome
  /metrics          Prometheus counters
  POST /reload      force a full resync now (token-gated, rate-limited)
```

See [Operations](#operations) for details on each.

## Operations

### Health & metrics

cf-watcher serves an internal HTTP endpoint (default `:9090`, reachable from other containers on `cloudflared-network`):

| Endpoint   | Purpose                                                                                                                   |
| ---------- | ------------------------------------------------------------------------------------------------------------------------- |
| `/healthz` | `503` until the tunnel connector token is written; then `200` if the last sync attempt succeeded (or none has run yet), `503` if it failed. cloudflared's `depends_on: condition: service_healthy` relies on this. |
| `/metrics` | Prometheus text-format counters: readiness, containers tracked, DNS entries tracked, sync totals/errors, last sync timestamp.                                                                    |

Set `HEALTH_ADDR=off` to disable the server entirely. Not recommended: the healthcheck then passes immediately, so cloudflared may start before the token exists (it restarts until the token appears).

The Compose file probes every 2s during the first 60s (`start_interval`, Docker Engine 25+), so cloudflared starts within seconds of the token being written. Older engines fall back to the 30s interval.

The image also ships a Docker `HEALTHCHECK` (a tiny static Go binary probing `/healthz`, since the distroless image has no shell/curl/wget) — `docker ps` shows cf-watcher's health status directly.

### DNS orphan cleanup

cf-watcher only remembers the CNAMEs it created **during the current process**. Restarting the container used to mean any CNAME whose owning container was removed _while cf-watcher was down_ stayed orphaned in Cloudflare DNS forever.

On every startup reconcile (and every `/reload`), cf-watcher now lists all CNAMEs in the zone pointing at `{tunnel ID}.cfargotunnel.com` (the resolved tunnel — provisioned by name or pinned via `CF_TUNNEL_ID`), adopts the ones that still match a running `cf.dns=true` container, and deletes the rest. Records unrelated to this tunnel are never touched.

### Manual reload

`POST /reload` fully re-syncs desired state from the live Docker daemon and issues a fresh tunnel + DNS reconcile, without restarting the container. Use it when a running container's `cf.*` labels changed in place (e.g. `docker update`, or an in-place Compose label edit) — those don't fire a Docker event, so the live watcher never notices on its own.

Disabled by default. Set `RELOAD_TOKEN` to enable it, then call it with the matching header:

```bash
curl -X POST -H "X-Reload-Token: $RELOAD_TOKEN" http://cf-watcher:9090/reload
```

Without `RELOAD_TOKEN` set, `/reload` returns `404`. With it set but a missing/wrong header, it returns `401`.

### Token file permissions

cf-watcher runs as uid `1000` (primary gid `65532`); the official cloudflared image runs as `65532:65532`. The shared volume's directory is `1000:65532` mode `0750` (copied from the cf-watcher image on first mount), and the token file is written atomically with mode `0640` — owner writes, cloudflared's group reads, nobody else. If the volume was created before this layout (wrong ownership), remove it: `docker compose down && docker volume rm <project>_cloudflared-token`.
