# Google Drive Cleanup

## Database changes

Don't worry about migrating the existing database. Reconstructing the database with a new crawl is cheap. Just take a
backup of the database (move it to `db-<timestamp>.bak`) and then let the next crawl recreate the tables with the new
schema.

## Running the tool

Use the `drive-cleanup` shell function instead of building a binary. It is configured in the devcontainer and under the hood runs:

```bash
go run /workspaces/google-drive-cleanup "$@"
```
