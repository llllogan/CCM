# Central Container Manager

Central Container Manager (CCM) is a small control plane for Docker Compose
hosts. It connects to remote Docker hosts over SSH and gives you:

- a web UI for targets, stacks, containers, disk usage, and logs;
- API endpoints for deploying and redeploying Compose stacks;
- container start, stop, and restart controls;
- scheduled container restarts and host scripts;
- optional notifications through a separate notification service.

CCM runs as one Go binary or as the Docker Compose stack described below.

## Install CCM

Create a directory on the machine that will run CCM:

```bash
sudo mkdir -p /opt/ccm
cd /opt/ccm
```

Create `/opt/ccm/docker-compose.yml`:

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
      CCM_SSH_KEY: /home/logan/.ssh/id_ed25519
    ports:
      - "8080:8080"
```

Start CCM and check it:

```bash
cd /opt/ccm
docker compose up -d
docker compose ps
curl -sS http://127.0.0.1:8080/healthz
```

The health check should return:

```json
{"status":"ok"}
```

For HTTPS, put a reverse proxy such as Caddy in front of CCM. The CCM
container itself listens on port `8080`.

## Configure targets and stacks

CCM reads `/etc/ccm/config.yml`. The easiest starting point is
[`examples/config.yml`](examples/config.yml).

A target is a remote Docker host. A stack is a Compose directory on that
target. For example:

```yaml
targets:
  app-host:
    host: 10.10.10.11
    port: 22
    user: logan
    deploy_root: /home/logan
    disk_path: /
    defaults:
      pull: true
      remove_orphans: true
      recreate: default

stacks:
  portfolio:
    target: app-host
    deploy_subdir: portfolio
```

This maps the `portfolio` stack to `/home/logan/portfolio` on `app-host`.

The SSH user must be able to run Docker and Docker Compose on the target.
`deploy_subdir` must be relative to `deploy_root`.

The stack id `ccm` is special. Deploying to it writes updated files without
restarting CCM. Apply the update separately with the self-redeploy endpoint.

## Use the web UI

Open `http://<ccm-host>:8080`.

The UI can:

- browse targets, Compose projects, and containers;
- show container details and stream container logs;
- show disk usage for a stack's configured filesystem;
- start, stop, and restart containers;
- redeploy a Compose stack;
- manage discovered self-hosted GitHub Actions runners when runner discovery is
  enabled for a target.

## Deploy a stack

CCM has two deployment endpoints.

### Deploy new files

`POST /v1/deploy` writes the supplied files and, by default, runs Compose.

Required request fields:

```json
{
  "ccm_stack": "portfolio",
  "repo": "owner/repository",
  "sha": "git-commit-sha",
  "compose_yml": "services:\n  app:\n    image: example/app:latest\n"
}
```

Optional fields:

- `env_file`: raw `.env` text;
- `env`: key/value pairs merged into `.env`;
- `caddyfile`: Caddyfile content;
- `scripts`: host scripts to write under `ccm_scripts/`;
- `run_compose`: set to `false` to write files without applying them.

The `env` map overrides duplicate keys from `env_file`. CCM writes the final
`.env` with sorted keys and mode `0600`. Script files must end in `.sh` and are
written with executable permissions.

For the `ccm` stack, `run_compose` defaults to `false` so CCM can update its
own files safely.

Example:

```bash
curl -sS \
  -H 'Content-Type: application/json' \
  -X POST http://ccm.example.internal:8080/v1/deploy \
  --data @examples/payload.basic.json
```

### Redeploy existing files

`POST /v1/compose/{stack_id}/redeploy` leaves the files in place and reruns
Compose in the stack directory.

For the `ccm` stack, the redeploy runs in a detached worker so CCM can restart
itself safely. Its log is written to `ccm-redeploy-ccm.log` in the stack
directory, or under `/tmp` if the stack directory is not writable.

## Stream deployment output

Add `Accept: text/event-stream` to `POST /v1/deploy` to receive live
Server-Sent Events while the deployment runs:

```bash
curl --no-buffer -sS \
  -H 'Accept: text/event-stream' \
  -H 'Content-Type: application/json' \
  -X POST "$CCM_URL/v1/deploy" \
  --data @ccm-deploy-payload.json
```

The stream includes file-writing phases and live stdout/stderr from
`docker compose pull` and `docker compose up`. It ends with a `complete` event
whose status is either `succeeded` or `failed`.

Once streaming starts, the HTTP status is already `200`. Callers must inspect
the final event and treat these as failures:

- a final status of `failed`;
- a curl or network error;
- the connection closing before a `complete` event.

The reusable example Action parses the stream and prints only Compose output
to the GitHub Actions log. It hides the SSE `event:` and `data:` metadata.
See [`examples/github/actions/ccm-deploy/action.yml`](examples/github/actions/ccm-deploy/action.yml).

If `docker compose pull` fails, CCM stops the deployment and does not run
`docker compose up` or image cleanup.

After a successful API-triggered Compose deployment, CCM runs
`docker image prune -a -f`. Cleanup status is returned separately and a
cleanup failure does not turn an otherwise successful deployment into a failed
deployment.

## GitHub Actions integration

The files under [`examples/github`](examples/github) show how another
repository can call CCM:

- [`actions/ccm-deploy/action.yml`](examples/github/actions/ccm-deploy/action.yml)
  builds the payload, streams deployment output, and optionally restarts the
  stack's containers;
- [`workflows/deploy-jellyfin.yml`](examples/github/workflows/deploy-jellyfin.yml)
  shows a stack workflow;
- [`workflows/redeploy-stack.yml`](examples/github/workflows/redeploy-stack.yml)
  shows a manual redeploy workflow.

Typical deployment files in the caller repository are:

```text
deploy/
  docker-compose.yml       # required
  env.json                 # optional non-secret defaults
  Caddyfile                # optional
  ccm_scripts/             # optional host scripts
```

Keep secrets in GitHub repository or environment secrets. Do not commit them
to `env.json`.

## Notifications

CCM can send messages to a separate notification service after successful
deployments and when disk usage crosses the alert threshold.

Configure the service URL and API key in `/etc/ccm/config.yml`:

```yaml
notification_service_url: "http://notify:8081/notify/household"
notification_service_key: "your-notification-service-key"
```

A stack can override the destination path:

```yaml
stacks:
  portfolio:
    target: app-host
    deploy_subdir: portfolio
    notification_service_url: "http://notify:8081/notify/portfolio"
```

Deployment notifications use this format:

```text
portfolio deployed.
target: app-host
stack: portfolio
path: /home/logan/portfolio
repo: owner/repository
sha: git-commit-sha
compose: true
env_count: 2
scripts: 0
```

Redeploy notifications use this format:

```text
portfolio redeployed.
target: app-host
stack: portfolio
path: /home/logan/portfolio
mode: synchronous
```

Disk alerts include the host, filesystem, usage, capacity, and check time.
Check times are always shown in Brisbane time (`Australia/Brisbane`):

```text
app-host at 89% disk usage.
used: 20G
available: 2.6G
size: 22G
host: app-host
path: /
mount: /
filesystem: /dev/sda1
usage: 89%
checked: 15:03:33 2026-07-16
```

Disk alerts are sent once when usage rises above 80%. A new alert is sent
after usage returns to 80% or below and later crosses the threshold again.
The alert state is stored in `disk_alert_state_file`, which defaults to
`/tmp/ccm-disk-alert-state.json`.

## Scheduled restarts and scripts

CCM can restart containers on a schedule:

```yaml
restart_state_file: /tmp/ccm-restart-state.json

restart_strategies:
  nightly:
    cron: "0 4 * * *"
    timezone: Australia/Brisbane

stacks:
  portfolio:
    target: app-host
    deploy_subdir: portfolio
    restart:
      strategy: nightly
```

Cron expressions use five fields:
`minute hour day-of-month month day-of-week`.

Host scripts are configured under a stack and must be included in the deploy
payload:

```yaml
stacks:
  portfolio:
    target: app-host
    deploy_subdir: portfolio
    scripts:
      - name: backup-metadata
        cron: "15 2 * * *"
        file: backup-metadata.sh
        timezone: Australia/Brisbane
```

CCM writes the script to
`<deploy_root>/<deploy_subdir>/ccm_scripts/backup-metadata.sh` and runs it on
the target with `/bin/sh`.

## API reference

Health and inventory:

- `GET /healthz`
- `GET /v1/summary`
- `GET /v1/updates/ccm` (compares the installed CCM image digest with GHCR)
- `GET /v1/stacks`
- `GET /v1/inventory`
- `GET /v1/items/{id}/children`
- `GET /v1/targets/{id}/disk`
- `GET /v1/targets/{id}/ip`

Containers and stacks:

- `GET /v1/containers/{id}`
- `GET /v1/containers/{id}/logs?tail=250`
- `GET /v1/containers/{id}/logs/stream?tail=200`
- `POST /v1/containers/{id}/start`
- `POST /v1/containers/{id}/stop`
- `POST /v1/containers/{id}/restart`
- `POST /v1/compose/{id}/redeploy`
- `POST /v1/deploy`

Maintenance and schedules:

- `GET /v1/targets/{id}/docker/df`
- `POST /v1/targets/{id}/docker/safe-prune`
- `GET /v1/restarts/tracking`

If `auth_token` is configured, safe-prune requires:

```http
Authorization: Bearer <auth_token>
```

Other routes should be protected by a trusted network, VPN, or authenticated
reverse proxy.

## Run locally

```bash
go mod tidy
go run ./cmd/ccm -config ./examples/config.yml -listen :8080
```

## Self-hosted GitHub runners

Targets can optionally expose self-hosted GitHub Actions runners in the UI:

```yaml
targets:
  runner-vm:
    host: 10.10.10.50
    port: 22
    user: logan
    deploy_root: /home/logan
    github_runners:
      enabled: true
      user: github-runner
      home: /home/github-runner
```

CCM reads safe runner metadata and can start, stop, restart, or uninstall the
runner service. Credentials and token files are never returned. Configure
passwordless sudo for the runner service actions.
