# Central Container Manager (CCM)

CCM is a single-binary control plane for homelab Docker targets.

## What CCM does

- Connects to one or more Docker hosts over SSH.
- Shows live container and compose inventory in a built-in web UI.
- Runs container actions (`start`, `stop`, `restart`).
- Runs compose redeploys for known stacks.
- Streams live container logs to the UI.

## Quick mental model

CCM has two deployment paths:

- `POST /v1/deploy`: writes `docker-compose.yml` / `.env` / `Caddyfile` to the stack deploy directory, then runs compose (`pull` + `up`, based on flags). Use this when you want to change compose content or move to a specific image tag/SHA.
- `POST /v1/compose/{id}/redeploy`: does not rewrite files; it re-runs compose in the existing deploy directory. Use this when your compose already points to `:latest` and you only want to pull/restart.

## Configuration

Default path: `/etc/ccm/config.yml`.

Example: [`examples/config.yml`](examples/config.yml).

Key fields:

- `targets` define SSH connection and deployment roots.
- `stacks` map stack IDs to targets and deploy subdirectories.
- `defaults.pull` controls whether redeploy runs `docker compose pull`.
- `profiles` can override defaults per stack.
- For a stack deploy path, CCM writes `docker-compose.yml`, `Caddyfile`, and `.env` in that directory when using `POST /v1/deploy`.

Important:
- If `pull: false`, redeploy will not fetch new remote images.
- For newcomers, keep `pull: true` for your `ccm` stack.

## First-time setup checklist

1. Create `/etc/ccm/config.yml` from [`examples/config.yml`](examples/config.yml).
2. Verify the SSH user in each `target` can run Docker commands on the host.
3. Ensure `deploy_root` + `deploy_subdir` points to the compose directory you expect (example: `/opt/ccm`).
4. For self-updates with `:latest`, keep `pull: true` for stack `ccm`.
5. Start CCM and confirm `GET /healthz` returns `{"status":"ok"}`.

## Run locally

```bash
go mod tidy
go run ./cmd/ccm -config ./examples/config.yml -listen :8080
```

## Run with Docker Compose

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

## UI notes

- The top-left status square shows log stream state.
- `green`: log stream connected
- `yellow`: connecting/reconnecting
- `gray`: disconnected
- If you leave the tab and return, CCM reconnects the log stream automatically.

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

## Redeploy behavior details

`POST /v1/compose/{id}/redeploy`:
- Uses the stack's resolved flags (`pull`, `remove_orphans`, `recreate`).
- For non-CCM stacks: runs compose synchronously.
- For stack id `ccm`: starts a detached remote compose job and returns:
- `async: true`
- `log_path` (remote log file, usually `/tmp/ccm-redeploy-<timestamp>.log`)

This protects self-redeploy from dying mid-request while CCM restarts.

## Self-redeploy workflow (GitHub Actions)

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

## Troubleshooting self-redeploy

If `ccm` does not come back after redeploy:

1. Check that `pull` is enabled for stack `ccm` in `/etc/ccm/config.yml`.
2. On the Docker host, inspect the detached redeploy log:
```bash
tail -n 200 /tmp/ccm-redeploy-*.log
```
3. Check compose state:
```bash
cd /opt/ccm
docker compose ps
docker compose logs --tail=200 ccm
```
4. Recover manually:
```bash
cd /opt/ccm
docker compose pull ccm
docker compose up -d ccm
```
