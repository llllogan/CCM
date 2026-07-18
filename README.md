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
```

- `/opt/ccm/Caddyfile`
```caddy
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
- Set `targets.<id>.disk_path` to the filesystem, mount path, or device to use for host disk usage (for example `/`, `/mnt/media`, or `/dev/sda1`). It defaults to `/`.
- Keep `pull: true` for stack `ccm` if you want self-updates with `:latest`.
- For special self-redeploy behavior, stack id must be exactly `ccm`.

### Restart Strategy (scheduled container restarts)

CCM supports first-class restart strategies in `config.yml`:

- Define reusable strategies under top-level `restart_strategies`.
- Attach a strategy to an entire stack via `stacks.<id>.restart.strategy`.
- Override or disable per-container behavior via `stacks.<id>.restart.containers`.

Example:

```yaml
restart_state_file: /tmp/ccm-restart-state.json

restart_strategies:
  nightly_4am_local:
    cron: "0 4 * * *"
  sunday_430_utc:
    cron: "30 4 * * 0"
    timezone: UTC

stacks:
  arrbox:
    target: arrbox
    deploy_subdir: arrbox
    restart:
      strategy: nightly_4am_local
      containers:
        caddy:
          strategy: none
        sonarr:
          strategy: sunday_430_utc
```

Cron format is `minute hour day-of-month month day-of-week` (5 fields).

Container restart matching rules:

- Container key is matched against compose service name first.
- If not found by service label, it is matched against container name.
- `strategy: none` disables scheduled restart for that container even if stack strategy exists.
- `strategy: inherit` (or empty) means use stack strategy.

Time tracking behavior:

- Scheduler checks cron matches every ~15s and executes at most once per matching minute per assignment.
- State is persisted in `restart_state_file` so data survives CCM restarts.
- Tracking keeps last attempt/success time, last result, last exit code, and consecutive failures.
- Endpoint: `GET /v1/restarts/tracking`.

### Clive notifications

CCM can notify Clive when targets, stacks, containers, scheduled restarts, or scheduled scripts look broken. This is optional and disabled by default.

```yaml
notifications:
  clive:
    enabled: true
    webhook_url: "http://clive.local:3456/webhooks/inbound"
    token: "same-value-as-clive-INBOUND_WEBHOOK_TOKEN"
    user_number: "+15555555555"
    min_severity: warning
    cooldown: 15m
    include_logs_tail: 80
```

When enabled, CCM posts to Clive's inbound webhook on unhealthy state transitions and after the configured cooldown if the issue is still active. If `include_logs_tail` is greater than zero, container alerts include a bounded recent log slice for Clive to summarize.

### Deployment notifications

CCM can optionally notify the standalone `notify_service` after every successful deployment request. Set the exact notify endpoint in `/etc/ccm/config.yml`; the path selects a configured user or group:

```yaml
notification_service_url: "http://notify:8081/notify/household"
notification_service_key: "same-value-as-notify-api_key"
```

The notification contains the stack, target, deploy path, repository, commit SHA, compose status, environment count, and script count. A notification failure is reported in the deploy response but does not roll back or mark the deployment failed.

To send a particular stack to a different notify endpoint, add an endpoint override under that stack. It uses the global `notification_service_key`; there is no per-stack key:

```yaml
stacks:
  app:
    target: app-host
    deploy_subdir: app
    notification_service_url: "http://notify:8081/notify/another-group"
```

Stacks without an override continue to use the global `notification_service_url`.

When the global notification service URL and key are configured, CCM checks each target's configured `disk_path` every five minutes. It sends a disk alert when usage exceeds 80%, including the host, filesystem, path, percentage, and capacity details. Each target is alerted only once until usage returns to 80% or below, after which a new crossing triggers another alert. Alert state is stored in `disk_alert_state_file` (default `/tmp/ccm-disk-alert-state.json`) using atomic JSON writes; set this path on persistent storage if it must survive container recreation. Failed checks and notification requests are logged and retried on the next check.

### Host Script Schedules (scheduled host commands)

CCM can also run host-side shell scripts on a cron schedule per stack.

- Define scripts under `stacks.<id>.scripts`.
- Each script has:
  - `name` unique within the stack
  - `cron` schedule (5-field format)
  - `file` script filename (must end in `.sh`)
  - optional `timezone` (IANA timezone, defaults to Local)
- The script file must be sent in `POST /v1/deploy` payload `scripts`.
- CCM writes scripts to `<deploy_root>/<deploy_subdir>/ccm_scripts/<file>`.
- Scheduler executes the script on the target host via `/bin/sh`.

Example:

```yaml
stacks:
  arrbox:
    target: arrbox
    deploy_subdir: arrbox
    scripts:
      - name: backup-metadata
        cron: "15 2 * * *"
        file: backup-metadata.sh
      - name: weekly-maintenance
        cron: "0 5 * * 0"
        file: weekly-maintenance.sh
```

### 4) Navigate the UI

Open `http://<ccm-host>:8080`.

What you can do in UI:

- Browse targets, compose projects, and containers.
- Open container details.
- Open a compose stack to see the configured host filesystem's current disk usage, including a progress bar and exact percentage.
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

### How deployment artifacts are assembled

In a typical `dockerops`-style repo, each stack directory contains:

- `docker-compose.yml` (required)
- `Caddyfile` (optional)
- `env.json` (optional committed non-secret defaults)

During workflow execution, the reusable action reads files from that stack directory and sends them to CCM:

- `docker-compose.yml` content is sent as `compose_yml`.
- If present, `Caddyfile` content is sent as `caddyfile`.
- Environment data is sent as `env` (JSON key/value map).
- If configured, `ccm_scripts/*.sh` files are sent as payload `scripts`.

CCM writes the received payload into the stack deploy directory on the target host:

- always writes `docker-compose.yml`
- writes `Caddyfile` only when `caddyfile` is present
- writes `.env` when merged env data is non-empty
- writes `ccm_scripts/<file>.sh` for each payload script

After a successful API-triggered compose deployment, CCM runs
`docker image prune -a -f` on the target host. Image pruning is serialized with
other Docker maintenance on that target. Its outcome is returned separately in
the `image_prune` response field, and a cleanup failure does not change a
successful deployment into a failed deployment. Pruning is skipped when
`run_compose` is `false`; compose redeploys, including CCM's detached
self-redeploy, are unchanged.

### How `.env` is built from secrets and `env.json`

It is possible to hold multiple compose stacks in the same repo and keep their secrets separate. Create a GitHub environment per stack to store its specific secrets, and run a GitHub workflow against that environment to include those secrets in the .env file. Shared secrets can be stored at the repo level and will also be included.

#### Recommended GitHub pattern:

- Use `all_secrets_json: ${{ toJSON(secrets) }}` in workflow.
- Set repo-level shared secrets (example: `CLOUDFLARE_API_TOKEN`).
- Set environment-level stack secrets (example: `LASTFM_API_KEY`, `LASTFM_SECRET` in `navidrome` environment).
- Optionally commit stack `env.json` for non-secret defaults.

Merge flow in reusable action:

1. Read `env.json` if file exists and parse as object.
2. Read `toJSON(secrets)` (already includes repo + environment secrets available to the job).
3. Merge objects with this precedence:
   - `env.json` first
   - secrets second (override duplicate keys)
4. Send merged map to CCM as payload `env`.

CCM `.env` creation behavior:

- CCM takes payload `env_file` (if used) and `env` map.
- Keys from `env` override duplicate keys from `env_file`.
- CCM writes one final `.env` file sorted by key.

If your workflow uses only merged `env` (no `env_file`), the final `.env` is generated directly from that merged map.

### API endpoints

- `GET /healthz`
- `GET /v1/summary`
- `GET /v1/stacks`
- `GET /v1/inventory`
- `GET /v1/items/{id}/children`
- `GET /v1/targets/{id}/disk`
- `GET /v1/targets/{id}/docker/df`
- `POST /v1/targets/{id}/docker/safe-prune`
- `GET /v1/containers/{id}`
- `GET /v1/containers/{id}/logs?tail=250`
- `GET /v1/containers/{id}/logs/stream?tail=200`
- `POST /v1/containers/{id}/start`
- `POST /v1/containers/{id}/stop`
- `POST /v1/containers/{id}/restart`
- `POST /v1/compose/{id}/redeploy`
- `POST /v1/deploy`
- `GET /v1/containers/{id}/logs/stream?tail=200`
- `GET /v1/restarts/tracking`

`GET /v1/targets/{id}/disk` runs `df -P -h` against that target's configured `disk_path` and returns the parsed filesystem, size, used, available, mountpoint, and exact integer usage percentage. `GET /v1/targets/{id}/ip` returns the configured target host address and the target host's public IPv4 address, discovered through `api.ipify.org` over SSH. Docker maintenance endpoints are fixed-command operations. `GET /v1/targets/{id}/docker/df` runs a read-only disk usage report. `POST /v1/targets/{id}/docker/safe-prune` prunes stopped containers older than 24 hours and unused images, build cache, and networks older than 7 days. It intentionally does not prune Docker volumes. If `auth_token` is configured, safe-prune requires `Authorization: Bearer <auth_token>`.

`POST /v1/deploy` request fields:

- `ccm_stack`: stack id from config.
- `repo`: source repo name.
- `sha`: source commit sha.
- `compose_yml`: full compose file content.
- `env_file` optional raw `.env` text.
- `env` optional key/value map merged into `.env` (overrides duplicate keys from `env_file`).
- `caddyfile` optional Caddyfile content.
- `scripts` optional array of host script files:
  - each item: `{ "file": "job.sh", "content": "#!/bin/sh\n..." }`
  - files are written to `<deploy_subdir>/ccm_scripts/` with executable permissions.
- `run_compose` optional boolean.
  - For non-`ccm` stacks, default is `true`.
- For `ccm` stack, default is `false` (write files only; use `POST /v1/compose/ccm/redeploy` to apply safely).

### Streaming deployment output

API callers that need live deployment output can send `Accept: text/event-stream` with
`POST /v1/deploy`. CCM keeps the request open and emits Server-Sent Events as files
are written and as `docker compose pull` and `docker compose up` produce output. The
`data` field of each event is JSON; output events include `stream`, `command`, and
`line` fields.

The stream ends with a `complete` event whose `status` is either `succeeded` or
`failed`. Once the stream has started, the HTTP status remains `200`, so callers
must inspect that terminal event and fail their job when its status is `failed` or
when the connection closes before a terminal event. Non-streaming callers continue
to receive the normal JSON response.

For example:

```bash
curl --no-buffer -sS \
  -H 'Accept: text/event-stream' \
  -H 'Content-Type: application/json' \
  -X POST "$CCM_URL/v1/deploy" \
  --data @ccm-deploy-payload.json
```

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

`auth_token` is currently enforced for Docker safe-prune only. Other CCM API routes should still be deployed only on trusted networks/VPN, or behind an authenticated reverse proxy.

## Local dev

Run CCM from source:

```bash
go mod tidy
go run ./cmd/ccm -config ./examples/config.yml -listen :8080
```
# Self-hosted GitHub runners

Targets may optionally expose GitHub Actions runners in the inventory:

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

CCM scans immediate directories under `home`, matching `actions.runner.*.service` systemd units, and reads only safe fields from each runner's `.runner` metadata. Credentials and token files are never read or returned. Runner actions use `sudo systemctl start|stop|restart <validated-unit>`. The runner Uninstall action runs `sudo ./svc.sh stop` followed by `sudo ./svc.sh uninstall` from the discovered runner directory; configure passwordless sudo accordingly. If discovery fails, the runner host remains visible with an error status. Check `systemctl`, the configured home path, service naming, and sudoers permissions when troubleshooting.
