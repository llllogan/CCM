# Central Container Manager (CCM)

CCM is a single-binary control plane for homelab Docker targets.

## What it provides

- API-driven Docker Compose deployments via named `ccm_stack` mappings.
- Live inventory of containers and compose projects across multiple remote Docker hosts.
- Container control endpoints (`start`, `stop`, `restart`) and compose redeploy endpoint.
- SSE log streaming endpoint for real-time container logs.
- Raw HTML/CSS/JS UI served directly by CCM (no frontend framework).

## Configuration

Default path: `/etc/ccm/config.yml`.

Example config is in [`examples/config.yml`](examples/config.yml).

Key rules:

- `targets` define SSH connection and deployment roots.
- `stacks` map stack IDs to targets and deploy subdirectories.
- Repos should only include a pointer file, e.g. [`examples/ccm.ref.yml`](examples/ccm.ref.yml).
- For a given stack deploy path, CCM writes `docker-compose.yml`, `Caddyfile`, and `.env` in that same directory.

## Run locally

```bash
go mod tidy
go run ./cmd/ccm -config ./examples/config.yml -listen :8080
```

## Docker run

```bash
docker compose up -d
```

`docker-compose.yml` runs two services:

- `ccm` app container
- `caddy` reverse proxy with TLS for `ccm.janssen.host`

Before first run, create `/opt/ccm/.env` with:

```bash
CLOUDFLARE_API_TOKEN=replace-with-cloudflare-token
ACME_EMAIL=you@example.com
```

And place a Caddyfile at `/opt/ccm/Caddyfile` (or use the sample at `caddy/Caddyfile.example`).

## API summary

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

`POST /v1/deploy` payload fields:

- `ccm_stack`
- `repo`
- `sha`
- `compose_yml`
- `env_file` (optional raw `.env` content)
- `env` (optional key/value map merged into `.env`, overrides `env_file` duplicates)
- `caddyfile` (optional content written to `Caddyfile` in deploy directory)

## Self-redeploy workflow

Workflow file: `.github/workflows/release-and-self-deploy.yml`

It:

1. Builds and pushes CCM image to GHCR.
2. Renders compose with new tag.
3. Calls CCM `POST /v1/deploy` for stack `ccm`.

Required secrets:

- `CCM_URL` (example `http://ccm.internal:8080`)
- `CLOUDFLARE_API_TOKEN`
- `ACME_EMAIL`

Note: CCM currently runs without API authentication for internal-network use.
