# Cloudflare Tunnel Gateway

Docker Compose stack that exposes your services via Cloudflare Tunnel with automatic ingress and optional DNS management.

## Services

- **cloudflared** — Cloudflare Tunnel connector (token mode)
- **cf-watcher** — Go sidecar (~17MB) that watches Docker containers with `cf.*` labels and auto-updates the tunnel ingress + DNS CNAMEs

No dashboard manual steps: label a container, cf-watcher detects it, updates the tunnel.

## Quick start

```bash
cp .env.example .env
# Fill in your credentials (see "Getting credentials" below)

docker compose up -d
```

## Getting credentials

### Cloudflare Tunnel token

```bash
cloudflared tunnel login
cloudflared tunnel token <YOUR_TUNNEL_ID>
```

Paste the output into `CLOUDFLARED_TOKEN` in `.env`.

### Cloudflare API token

Create a token at [dash.cloudflare.com/profile/api-tokens](https://dash.cloudflare.com/profile/api-tokens) with these permissions:

- **Account > Cloudflare Tunnel > Edit**
- **Zone > DNS > Edit** (only if you use `cf.dns=true`)

### IDs

| Variable        | Where to find it                                                                           |
| --------------- | ------------------------------------------------------------------------------------------ |
| `CF_ACCOUNT_ID` | Dashboard > right sidebar > Account ID                                                     |
| `CF_TUNNEL_ID`  | ID of your tunnel (the UUID)                                                               |
| `CF_ZONE_ID`    | Dashboard > your domain > Overview > Zone ID (right sidebar) — only if using `cf.dns=true` |
| `DOCKER_GID`    | `getent group docker \| cut -d: -f3` on the host                                           |

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
Docker daemon ──► cf-watcher ──► Cloudflare API ──► tunnel config
                      │
                 detects start/stop of
                 labeled containers,
                 debounces changes (2s),
                 issues full-state PUT
```

DNS CNAMEs are created/deleted per-container only for services with `cf.dns=true`. Records created outside cf-watcher are never deleted.
