# Central Container Manager (CCM)

CCM is a single-binary control plane for Docker Compose homelabs. It connects to Docker hosts over SSH, shows live inventory in a UI, and provides API endpoints for deploy/redeploy/container control.

## Section 1: First Install, UI, and Day-1 Operations

### 1) Manual first installation on the CCM host

Create a deployment directory and initial compose file manually.

```bash
sudo mkdir -p /opt/ccm
cd /opt/ccm
```

Create `/opt/ccm/docker-compose.yml` (minimal install, no reverse proxy):

```yaml
services:
  ccm:
    image: ghcr.io/llllogan/ccm:latest
    container_name: ccm
    restart: unless-stopped
    volumes:
      - /etc/ccm:/etc/ccm:ro
      - /home/logan/.ssh:/home/logan/.ssh:ro
    environment:
      - CCM_SSH_KEY=/home/logan/.ssh/id_ed25519
    ports:
      - "8080:8080"
```

Optional: add Caddy in front of CCM for TLS. Add the `caddy` service from [`docker-compose.yml`](docker-compose.yml), then create:

- `/opt/ccm/.env`
```env
CLOUDFLARE_API_TOKEN=replace-me
ACME_EMAIL=you@example.com
```

- `/opt/ccm/Caddyfile`
```caddy
{
  email {$ACME_EMAIL}
}

ccm.example.internal {
  tls {
    dns cloudflare {env.CLOUDFLARE_API_TOKEN}
  }
  encode zstd gzip
  reverse_proxy ccm:8080
}
```

### 2) Boot the CCM stack

```bash
cd /opt/ccm
docker compose up -d
docker compose ps
curl -sS http://127.0.0.1:8080/healthz
```

Expected:

```json
{"status":"ok"}
```

### 3) Add CCM config file

CCM defaults to `/etc/ccm/config.yml`. Start from [`examples/config.yml`](examples/config.yml).

Minimal points to verify:

- `targets` contains every Docker host CCM should manage.
- SSH `user` can run Docker commands on each target.
- `deploy_root` + stack `deploy_subdir` resolves to the real compose folder.
- Keep `pull: true` for stack `ccm` if you want self-updates with `:latest`.
- For special self-redeploy behavior, stack id must be exactly `ccm`.

### 4) Navigate the UI

Open `http://<ccm-host>:8080`.

What you can do in UI:

- Browse targets, compose projects, and containers.
- Open container details.
- Stream container logs.
- Run container controls:
  - `start`
  - `stop`
  - `restart`
- Run compose stack redeploy.

Log indicator (top-left):

- `green`: log stream connected
- `yellow`: reconnecting
- `gray`: disconnected

### 5) Understand stack controls and self-redeploy

CCM has two deploy modes:

- `POST /v1/deploy`: write `docker-compose.yml` and optional `.env`/`Caddyfile`, then run compose.
- `POST /v1/compose/{id}/redeploy`: do not rewrite files; just re-run compose in place.

For `ccm` stack id specifically, redeploy runs with detached worker semantics so CCM can restart itself safely without cutting off the operation. Logs are written to `ccm-redeploy-ccm.log` in stack directory (or `/tmp` fallback).

If self-redeploy fails:

```bash
cd /opt/ccm
tail -n 200 ccm-redeploy-ccm.log
docker compose ps
docker compose logs --tail=200 ccm
docker compose pull ccm
docker compose up -d ccm
```

## Section 2: API and GitHub Actions Integration

### API endpoints

- `GET /healthz`
- `GET /v1/stacks`
- `GET /v1/inventory`
- `GET /v1/items/{id}/children`
- `GET /v1/containers/{id}`
- `POST /v1/containers/{id}/start`
- `POST /v1/containers/{id}/stop`
- `POST /v1/containers/{id}/restart`
- `POST /v1/compose/{id}/redeploy`
- `POST /v1/deploy`
- `GET /v1/containers/{id}/logs/stream?tail=200`

`POST /v1/deploy` request fields:

- `ccm_stack`: stack id from config.
- `repo`: source repo name.
- `sha`: source commit sha.
- `compose_yml`: full compose file content.
- `env_file` optional raw `.env` text.
- `env` optional key/value map merged into `.env` (overrides duplicate keys from `env_file`).
- `caddyfile` optional Caddyfile content.

Reference payloads:

- [`examples/payload.basic.json`](examples/payload.basic.json)
- [`examples/payload.env-file.json`](examples/payload.env-file.json)
- [`examples/payload.env-map.json`](examples/payload.env-map.json)
- [`examples/payload.full.json`](examples/payload.full.json)
- [`examples/payload.ccm-self-deploy.json`](examples/payload.ccm-self-deploy.json)

### GitHub Actions patterns from another repo

CCM is designed to be called from app/ops repos (for example: a `dockerops` repo containing stack compose files).

Included examples:

- Reusable composite action:
  - [`examples/github/actions/ccm-deploy/action.yml`](examples/github/actions/ccm-deploy/action.yml)
- Stack workflow example:
  - [`examples/github/workflows/deploy-jellyfin.yml`](examples/github/workflows/deploy-jellyfin.yml)
- Manual stack redeploy workflow:
  - [`examples/github/workflows/redeploy-stack.yml`](examples/github/workflows/redeploy-stack.yml)

These examples send compose/caddy/env to CCM, then optionally force container restarts after deploy.

### Security note

`auth_token` exists in config examples, but current CCM API router does not enforce authentication headers. Deploy CCM only on trusted networks/VPN, or put it behind an authenticated reverse proxy.

## Local dev

Run CCM from source:

```bash
go mod tidy
go run ./cmd/ccm -config ./examples/config.yml -listen :8080
```
