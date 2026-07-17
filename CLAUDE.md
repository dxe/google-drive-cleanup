# Google Drive Cleanup

## Running the tool

Use the `drive-cleanup` shell function instead of building a binary. It is configured in the devcontainer and under the hood runs:

```bash
go run /workspaces/google-drive-cleanup "$@"
```

## Changing the database schema

See [migrations/README.md](migrations/README.md) for how to add and reverse schema migrations.

## Review web UI

The keep/delete review UI is a Next.js app in [web/](web/) that talks to the Go API served by `drive-cleanup review` (default `127.0.0.1:8844`; the Next dev server proxies `/api/*` to it). Run both for development:

```bash
drive-cleanup review          # API
cd web && npm run dev         # UI on http://localhost:3000
```
