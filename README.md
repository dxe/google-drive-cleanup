# google-drive-cleanup

Crawls a Google Drive folder tree into a SQLite database and reports on file
ownership.

This is step one of an ownership-migration project: many files inside our org
folders are owned by external/personal accounts. Before we move files into
shared drives (to transfer ownership) and later move them back, this tool
takes a durable snapshot of every file's location and owner so original
parents can be reconstructed. The move/transfer tooling will come later and
build on the same database.

## The migration flow at a glance

One-time, for the whole tree:

```
  crawl   ──▶  drive.db        snapshot every file's owner + original parent
  owners  ──▶  who owns what   sorted by file count, to prioritize outreach
```

Then, per user being migrated, the contents of a folder make this round trip:

```
                         (3) user drags the empty folder + their own
                             loose files in; ownership flips to the org
     original folder  ───────────────────────────────────────────────▶  staging folder
        │      ▲                                                          (shared drive)
        │      │                                                              │
   (2) stash   │ (5) stash pop                                      (4) restore-locations
       push    │ refills third-party-                              looks up each file's
   empties the │ owned files after the                            original parent in
   folder so   │ folder is back in the                            drive.db and moves it
   it can      │ regular tree                                      back there
   transit     │                                                              │
        ▼      │                                                              ▼
        stash folder (My-Drive, inside the crawl root) ◀──────────  back to original parent
```

`stash push`/`stash pop` exist only because a shared drive cannot hold files
owned by third parties; loose files the user owns round-trip without the stash.
The numbered steps map to the per-user order of operations near the end of this
README. Run any of the moving steps with `--dry-run` first to preview them.

## Setup

1. **Enable the Drive API.** In the [Google Cloud console](https://console.cloud.google.com/),
   create (or pick) a project and enable the **Google Drive API**
   (APIs & Services → Library → Google Drive API → Enable).
2. **Create OAuth credentials.** APIs & Services → Credentials → Create
   Credentials → **OAuth client ID**, application type **Desktop app**.
   Download the JSON and save it as `credentials.json` in the directory you
   run the tool from. (You may need to configure the OAuth consent screen
   first; add your account as a test user if the app is in testing mode.)
3. **Scopes.** The `crawl`, `owners`, `path`, and `explore-owned-files` commands
   request `https://www.googleapis.com/auth/drive.readonly` only. The
   `restore-locations`, `stash push`, and `stash pop` commands need the full
   `drive` scope. The scopes a token was granted are recorded in `token.json`,
   so when you run a command that needs broader access than the cached token
   has, the tool re-runs consent automatically — no manual `rm token.json`.
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

## Config

Run `drive-cleanup init` to scaffold a `config.json` with every section and
`REPLACE_WITH_…` placeholders to fill in (it refuses to clobber an existing file
without `--force`). Any command run against an un-edited placeholder fails with
a message naming the field to fix.

Configuration comes from a single JSON file (default `config.json`, override
with `--config`). Each subcommand has its own top-level section so new commands
can add settings without colliding:

```json
{
  "crawl": {
    "root": { "id": "0ABCdef...", "name": "DxE General" }
  },
  "restore-locations": {
    "staging-folder": { "id": "1AbCdEfGh...", "name": "Staging" }
  },
  "stash": {
    "folder": { "id": "1StAsHfOlDeR...", "name": "Stash" }
  }
}
```

`crawl.root.id` and `crawl.root.name` are both required. Before crawling, the
tool fetches the folder from Drive and verifies it really is a folder **and**
that its name exactly matches `name`. A mismatch aborts with both names
printed — this guards against pointing the crawler at the wrong folder ID.

## Usage

In the devcontainer a `drive-cleanup` shell function (defined in
`.devcontainer/.bash_googledrivecleanup_functions` and loaded by both
interactive and login shells) wraps `go run`, so no build step is needed:

```sh
# Crawl (or resume a previous crawl) into drive.db
drive-cleanup crawl
drive-cleanup crawl --db drive.db --config config.json

# Force a full re-crawl of an already-completed database
drive-cleanup crawl --refresh

# Who owns how many files (non-folders), most first — drives outreach priority
drive-cleanup owners

# Full folder path of a node (used later to restore original locations)
drive-cleanup path 1AbCdEfGh...

# List files/folders the crawling account can't edit (run before moving files)
drive-cleanup check-edit-access

# Build a self-contained, emailable HTML tree of everything an account owns
# (email or owner id), written to out/explore-owned-files/<account>.html
drive-cleanup explore-owned-files someone@gmail.com

# Move files from the staging folder back to their original locations
drive-cleanup restore-locations

# Park the contents of every folder a user owns into the stash folder
# (run before the user drags their folders into the staging folder)
drive-cleanup stash push someone@gmail.com

# Move stashed files back into their original folders and clean up
# (run after restore-locations)
drive-cleanup stash pop
```

The three commands that move files — `restore-locations`, `stash push`, and
`stash pop` — accept `--dry-run`, which reports every `WOULD move/create/delete`
action without changing anything. A dry run authenticates with the read-only
scope (so it never triggers a write-scope consent) and skips the confirmation
prompt. Run one first to preview a migration step. Pass `--verbose`/`-v` to any
command to log every item it touches instead of just progress and errors.

`explore-owned-files` produces a single offline HTML file: an interactive,
collapsible tree of every file and folder the account owns plus their ancestor
folders, per-folder counts of owned descendants, Drive links, and keyboard
navigation — handy to attach when reaching out to an owner.

`check-edit-access` reads only the database and prints every node whose
`can_edit` flag (Drive's `capabilities.canEdit`, captured during the crawl for
the account that ran it) is false — the items you would be unable to move.
Folders are listed first, each with its full path and owner. Nodes whose edit
capability could not be determined (`can_edit = NULL`) are not reported.

`restore-locations` is the second half of the ownership-migration flow. Once
an owner has dragged their files into the shared-drive staging folder (which
transfers ownership to the org account), run this command to move each file
back to the folder it lived in before the transfer. It scans the staging folder
one level deep, looks up each file's original parent in the database, and calls
the Drive `files.update` API to reparent it. Files not found in the database
are skipped with a warning; the final line reports a count of moved, skipped,
and failed items. Configure the staging folder in `config.json`:

```json
{
  "restore-locations": {
    "staging-folder": { "id": "1AbCdEfGh...", "name": "Staging" }
  }
}
```

## Stashing folder contents (`stash push` / `stash pop`)

Loose files round-trip through the staging shared drive cleanly: the owner drags
the file in (ownership flips to the org), then `restore-locations` moves it back.
**Folders** are the hard case. A folder a user owns usually contains files owned
by *other* accounts, and a shared drive cannot hold items the dragging user does
not own — so when the user drags the folder in, Drive blocks the move or orphans
those files.

`stash push <user>` clears the way. For every folder owned by `<user>` in the
database, it:

1. creates a subfolder of the configured **stash folder**, named after the
   original folder's Drive ID;
2. recreates the original folder's sharing on that subfolder so everyone keeps
   the access they had;
3. moves **all** of the original folder's files into the subfolder; and
4. leaves a shortcut named **"Contents temporarily moved"** in the now-empty
   original folder, pointing at the stash subfolder.

The emptied folder can then transit the staging shared drive. `stash pop` (no
argument — it drains the whole stash) reverses step 3 for every subfolder,
moving each subfolder's files back into the original folder whose ID names it,
then removes the shortcut and deletes the empty subfolder.

Before moving anything, `stash push` runs the same scan as `check-edit-access`
and, if any crawled item is not editable by the running account, prints the
count and asks you to confirm (those items would fail to move). It also verifies
the stash folder's name matches config, that it is **inside the crawl root**, and
that it is **not in a shared drive**.

> **The stash folder must be a regular My-Drive folder, never a shared drive.**
> The files it parks are owned by third parties, which a shared drive cannot
> hold — that is the whole reason the stash exists.

**Why the stash lives inside the crawl root.** The user's *own* files get parked
in the stash too. Because the stash folder is under the crawl root, those files
still surface when the user searches their Drive for `in:<crawl-root-id>
owner:me`, so the user drags them into the staging folder along with their
(now-empty) folders, exactly like any other loose file they own —
`restore-locations` then returns them to their original parents. Files owned by
other third parties stay in the stash until `stash pop` puts them back (they are
migrated later, on that owner's own pass). Configure the stash folder in
`config.json`:

```json
{
  "stash": {
    "folder": { "id": "1StAsHfOlDeR...", "name": "Stash" }
  }
}
```

### Order of operations (per user)

Run these in order for each user being migrated:

1. **`drive-cleanup check-edit-access`** — confirm the running account can edit
   everything that will move.
2. **`drive-cleanup stash push <user>`** — park the contents of the user's
   folders in the stash.
3. **User moves files and (empty) folders they own to the staging folder** —
   manually, in the Drive web/desktop app. This includes their own files now
   sitting in the stash, which they find via the `in:<crawl-root-id> owner:me`
   search. The drag flips ownership of those items to the org.
4. **`drive-cleanup restore-locations`** — move everything just dragged into the
   staging folder back to its original parent in the regular tree.
5. **`drive-cleanup stash pop`** — refill the folders with the remaining stashed
   (third-party-owned) files and clean up the stash subfolders and shortcuts.

`stash pop` must run **after** `restore-locations`: the folders have to be back
out of the shared drive and in the regular tree before externally-owned files
can be returned to them.

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
| `can_edit` | Drive `capabilities.canEdit` for the crawling account: 1 editable, 0 not, NULL unknown — drives `check-edit-access` |
| `crawled_at` | RFC3339 timestamp |

`parent_id` is deliberately the traversal parent, not `files.parents[0]`, so
it always references a row we actually crawled — `path` walks this chain to
reconstruct original locations.

**Folder permissions:** for every folder, the crawl snapshots its full sharing
into a `folder_permissions` table (one row per grant: `type`, `role`,
`email_address`/`domain`, `display_name`, `allow_file_discovery`, `deleted`), so
the sharing can be recreated on a clone of the folder later. The whole set for a
folder is rewritten on each re-crawl, so it always reflects the folder's current
sharing. Only folders are tracked — files inherit their clone folder's sharing.
Grants are only as complete as the crawling account can read.

```sql
SELECT type, role, email_address FROM folder_permissions WHERE node_drive_id = '1AbCdEfGh...';
```

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

## TODO

- **`status` command.** The migration is a stateful, multi-step sequence per
  user but nothing tells you where you are. Add a `drive-cleanup status` that
  reads the DB + config and reports in one place: whether the crawl is complete
  or how many folders are still pending, the owner count, how many items lack
  edit access, and whether each config section is present and its configured
  folders resolve in Drive. This would be the easiest on-ramp for "I forgot
  where I left off."
