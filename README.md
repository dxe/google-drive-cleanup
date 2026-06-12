# google-drive-cleanup

Crawls a Google Drive folder tree into a SQLite database and reports on file
ownership.

This is step one of an ownership-migration project: many files inside our org
folders are owned by external/personal accounts. Before we move files into
shared drives (to transfer ownership) and later move them back, this tool
takes a durable snapshot of every file's location and owner so original
parents can be reconstructed. The move/transfer tooling will come later and
build on the same database.

## Setup

1. **Enable the Drive API.** In the [Google Cloud console](https://console.cloud.google.com/),
   create (or pick) a project and enable the **Google Drive API**
   (APIs & Services → Library → Google Drive API → Enable).
2. **Create OAuth credentials.** APIs & Services → Credentials → Create
   Credentials → **OAuth client ID**, application type **Desktop app**.
   Download the JSON and save it as `credentials.json` in the directory you
   run the tool from. (You may need to configure the OAuth consent screen
   first; add your account as a test user if the app is in testing mode.)
3. **Scopes.** The crawler requests `https://www.googleapis.com/auth/drive.readonly`
   only. The later move/transfer tooling will need the full `drive` scope —
   when we widen it, delete `token.json` and re-consent.
4. **First run auth.** On first run the tool starts a small loopback server on
   port `8765`, prints a consent URL, and waits. Open the URL in a browser,
   authorize, and Google redirects back to the server, which captures the auth
   code automatically — no copy/paste needed. The token is cached in
   `token.json` (0600) so subsequent runs are non-interactive.

   In a devcontainer this works because `.devcontainer/devcontainer.json`
   forwards port `8765` from the container to the host, so the host browser's
   redirect to `http://localhost:8765` reaches the server inside the container.
   (Desktop-app OAuth clients accept `http://localhost` on any port, so this
   port does not need to be registered in the Google console. If you change it,
   update both `callbackPort` in `auth.go` and `forwardPorts` in the
   devcontainer config.)

   **Fallback:** if the browser still can't reach the callback, paste the auth
   code — or the entire `http://localhost:8765/?...` redirect URL — on stdin
   and press enter.

`credentials.json`, `token.json`, and `*.db` are gitignored — never commit them.

## Root config

The crawl root comes from a JSON file (default `root.json`, override with
`--root-config`):

```json
{ "id": "0ABCdef...", "name": "DxE General" }
```

Both fields are required. Before crawling, the tool fetches the folder from
Drive and verifies it really is a folder **and** that its name exactly matches
`name`. A mismatch aborts with both names printed — this guards against
pointing the crawler at the wrong folder ID.

## Usage

In the devcontainer a `drive-cleanup` shell function (defined in
`.devcontainer/.bash_googledrivecleanup_functions` and loaded by both
interactive and login shells) wraps `go run`, so no build step is needed:

```sh
# Crawl (or resume a previous crawl) into drive.db
drive-cleanup crawl
drive-cleanup crawl --db drive.db --root-config root.json

# Force a full re-crawl of an already-completed database
drive-cleanup crawl --refresh

# Who owns how many files (non-folders), most first — drives outreach priority
drive-cleanup owners

# Full folder path of a node (used later to restore original locations)
drive-cleanup path 1AbCdEfGh...
```

The CLI is built with [Cobra](https://github.com/spf13/cobra): run
`drive-cleanup help` (or `drive-cleanup <command> --help`) for full usage, and
`drive-cleanup completion <shell>` to generate shell completions.

## Interrupting and resuming

Ctrl-C (or SIGTERM) stops the crawl cleanly: the current database transaction
finishes, the number of folders still pending is printed, and the process
exits. Just re-run `crawl` to resume — no flags needed.

How it works: each folder row has a `children_done` flag that is flipped to 1
**in the same transaction that inserts the final page of its children**, so
the database never claims a folder is done while child rows are missing. On
startup the work queue is rebuilt from all folders with `children_done = 0`.
All inserts are idempotent upserts keyed on `drive_id`, so re-listing a
partially-crawled folder is safe; re-running after a completed crawl is a
cheap no-op.

A folder that repeatedly errors (e.g. deleted from Drive mid-crawl) is logged
and skipped for the run; it stays `children_done = 0` and is retried on the
next run.

## Database schema

Single `nodes` table (SQLite, pure-Go `modernc.org/sqlite` driver):

| column | meaning |
|---|---|
| `id` | internal row id |
| `drive_id` | Drive file/folder ID (unique) |
| `name` | item name |
| `type` | `folder`, `shortcut`, `google_doc` (native Google file), or `binary` |
| `mime_type` | raw Drive mimeType, kept for debugging |
| `owner_email` / `owner_id` / `owner_display_name` | owner identity; email can be missing, `owner_id` is the stable Drive permission id |
| `parent_id` | row id of the folder this node was **discovered through** (NULL for the root) |
| `shortcut_target_id` | for shortcuts, the Drive ID they point to (shortcuts are recorded, never followed) |
| `children_done` | 1 once all children are fully listed — the resume unit |
| `crawled_at` | RFC3339 timestamp |

`parent_id` is deliberately the traversal parent, not `files.parents[0]`, so
it always references a row we actually crawled — `path` walks this chain to
reconstruct original locations.

**Multi-parenting / re-discovery:** Drive items can (legacy) live under
multiple parents. A node keeps its first-discovered `parent_id`; any later
sighting under a different crawled parent, and any `files.parents` entry
beyond the first that's inside our crawl, is logged to stderr as a warning and
recorded in the `extra_parents` table for manual review:

```sql
SELECT * FROM extra_parents;
```

## Rate limiting

A token-bucket limiter caps API calls at ~3/sec, and every call retries with
exponential backoff plus jitter on `403 rateLimitExceeded`, `429`, and 5xx
responses (retries pass back through the limiter). The crawl is intentionally
single-threaded — durability over speed.
