# Google Drive Cleanup

## Running the tool

Use the `drive-cleanup` shell function instead of building a binary. It is configured in the devcontainer and under the hood runs:

```bash
go run /workspaces/google-drive-cleanup "$@"
```

## Changing the database schema

See [migrations/README.md](migrations/README.md) for how to add and reverse schema migrations.
