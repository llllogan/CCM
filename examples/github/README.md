# GitHub Integration Examples

These examples show how to call CCM from a different repository (for example a `dockerops` repo).

Included files:

- `actions/ccm-deploy/action.yml`: reusable composite action that:
  - builds CCM deploy payload from compose/caddy/env data
  - calls `POST /v1/deploy`
  - optionally forces restart of all containers in the stack
- `workflows/deploy-jellyfin.yml`: stack-specific workflow example
- `workflows/redeploy-stack.yml`: manual redeploy workflow (`POST /v1/compose/{id}/redeploy`)

Expected GitHub setup in caller repo:

- Repo variable:
  - `CCM_URL` (for example `http://10.10.10.21:8080`)
- Repo secret:
  - `CLOUDFLARE_API_TOKEN` (or other shared secrets)
- Optional environment secrets per stack:
  - `LASTFM_API_KEY`
  - `LASTFM_SECRET`
  - etc.

The workflow passes `toJSON(secrets)` into the reusable action. The action merges those secrets with optional committed `<stack>/env.json` and sends the result as `env` in the CCM deploy payload.
