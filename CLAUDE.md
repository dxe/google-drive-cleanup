# Google Drive Cleanup

## Running the tool

Use the `drive-cleanup` shell function instead of building a binary. It is configured in the devcontainer and under the hood runs:

```bash
go run /workspaces/google-drive-cleanup "$@"
```
