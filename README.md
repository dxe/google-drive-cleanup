# google-drive-cleanup

Crawls a Google Drive folder tree into a SQLite database, reports on file
ownership, and migrates each user's files to org ownership through a shared
drive — then puts everything back where it was.

The problem: many files inside our org folders are owned by external/personal
accounts. Moving an item into a shared drive transfers its ownership to the
org, but only its *owner* can move it there. So per user, this tool gathers
everything they own into one **Container** folder, the user drags that single
folder into a shared drive (one drag — ownership of the whole tree flips), and
the tool moves everything back to the original parents recorded in the crawl
snapshot.

## The migration flow at a glance

One-time, for the whole tree:

```
  crawl   ──▶  drive.db        snapshot every file's owner + original parent
  owners  ──▶  who owns what   sorted by file count, to prioritize outreach
```

Then, per user being migrated:

```
               (1) pack                            (3) user drags Container
  original  ─────────────▶  Packing/<user>/  ───────────────────────────▶  Dropoff (shared drive)
  locations                  ├─ pickup-<user>     (2) admin transfers       (user is granted Manager
      ▲                      │   └─ Container         Container ownership    access; ownership of
      │                      │       owned subtrees,  to the user first      everything inside flips
      │                      │       moved intact     (Drive UI)             to the org)
      │                      └─ Stash                                             │
      │                          third-party items,                               │
      │                          flat                                             │
      │                             │                                             │
      └───────────── (4) unpack ◀───┴─────────────────────────────────────────────┘
                     every item returns to its original parent from drive.db
                     (Container contents first, then the Stash), then the
                     emptied scaffolding is deleted
```

The Stash exists because a shared drive cannot hold items owned by third
parties: anything inside the user's folders that the user does *not* own is
parked flat in the Stash for the duration of the round trip, so it cannot
block the drag. Run either moving step with `--dry-run` first to preview it.

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
   request `https://www.googleapis.com/auth/drive.readonly` only. The `pack`
   and `unpack` commands need the full `drive` scope. The scopes a token was granted are recorded in `token.json`,
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
  "migration": {
    "packing-folder": { "id": "1PaCkInG...", "name": "Packing" },
    "dropoff-folder": { "id": "0ADrOpOfF...", "name": "Dropoff" }
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

# Same, but limited to a folder and its descendants (must be in the database)
drive-cleanup owners 1AbCdEfGh...

# Full folder path of a node (used later to restore original locations)
drive-cleanup path 1AbCdEfGh...

# List files/folders the crawling account can't edit (run before moving files)
drive-cleanup check-edit-access

# Build a self-contained, emailable HTML tree of everything an account owns
# (email or owner id), written to out/explore-owned-files/<account>.html
drive-cleanup explore-owned-files someone@gmail.com

# Omit the account to generate one HTML file per owner in the database
drive-cleanup explore-owned-files

# Gather everything a user owns into their Container (and every third-party
# item inside their folders into their Stash), ready for a single drag
drive-cleanup pack someone@gmail.com

# After the user drags their Container into the Dropoff folder: move
# everything back to its original location and clean up
drive-cleanup unpack someone@gmail.com

# Abort a migration the user never completed the drag for: restore files to
# their original locations from the packing folder, ownership unchanged
drive-cleanup unpack someone@gmail.com --allow-not-moved

# Serve the keep/delete review API locally (dev only, no auth), then run the
# UI with `cd web && npm run dev` and open http://localhost:3000
# Or use VS Code run/debug panel (launch.json).
drive-cleanup review

# Export the keep/delete decisions as one self-contained HTML file to send
# to teammates (same red/green/yellow coloring as the review UI)
drive-cleanup export-review --out out/review.html
```

The two commands that move files — `pack` and `unpack` — accept `--dry-run`,
which reports every `WOULD move/create/delete` action without changing
anything. A dry run authenticates with the read-only scope (so it never
triggers a write-scope consent) and skips the confirmation prompt. Run one
first to preview a migration step. They also accept `--max-errors` (default 5):
once more than that many items fail to move, the run aborts — an error burst
means something systemic (wrong account, wrong config, revoked access), not
per-item drift. Pass `--verbose`/`-v` to any command to log every item it
touches instead of just progress and errors.

`explore-owned-files` produces a single offline HTML file: an interactive,
collapsible tree of every file and folder the account owns plus their ancestor
folders, per-folder counts of owned descendants, Drive links, and keyboard
navigation — handy to attach when reaching out to an owner. Run it without an
account argument to generate one such file per owner in the database (owners
with neither an email nor an owner id are skipped).

### Reviewing what to keep vs delete (`review` / `export-review`)

`review` serves a local JSON API (default `127.0.0.1:8844`) over the crawled
database, and the Next.js app in `web/` is its UI: folder tree on the left,
the selected folder's files on the right, with per-row Keep (✓) / Delete (✕) /
Clear (–) buttons, bulk file actions, and keyboard triage (↑/↓ move, →/←
expand/collapse, Enter opens a folder, `k` keep, `d` delete, `u` clear, Tab
switches panes, ⌘Z undo). Decisions land in the `nodes.decision` column.

```sh
drive-cleanup review          # terminal 1: the API
cd web && npm install && npm run dev   # terminal 2: the UI on :3000
```

Rules the server maintains:

- Marking a folder propagates to its whole subtree. A folder marked **delete**
  must have every descendant deleted too (no kept item may be orphaned inside
  a deleted subtree) — deleting over kept descendants asks for confirmation.
- Marking a folder **keep** marks undecided descendants keep; if some
  descendants are already delete, it asks whether to keep them as delete
  (subtree shows yellow/mixed) or overwrite everything to keep. Descendants
  can still be flipped to delete afterwards.
- Marking something keep (or clearing it) *inside* a delete subtree un-marks
  the delete ancestors and re-decides them from their children.
- Once every child of a folder is decided, the folder auto-decides: delete if
  all children are delete subtrees, keep otherwise.
- Undo (in-memory, last 200 actions) reverses whole actions while the server
  is running.

Colors: red = delete subtree, green = keep, yellow = contains both, pale
red/green = partially decided, no color = undecided. Each row has a small ↗
icon that opens the item in Google Drive.

`export-review` writes the same tree, coloring, and per-folder tallies into a
single self-contained HTML file (default `out/review.html`) that can be sent
to teammates for review. Note it includes every crawled node, so the file is
large (tens of MB for tens of thousands of nodes); it opens fine locally and
compresses well for sending.

`check-edit-access` reads only the database and prints every node whose
`can_edit` flag (Drive's `capabilities.canEdit`, captured during the crawl for
the account that ran it) is false — the items you would be unable to move.
Folders are listed first, each with its full path and owner. Nodes whose edit
capability could not be determined (`can_edit = NULL`) are not reported.

## Migrating a user (`pack` / `unpack`)

The two folders in the `migration` config section frame the round trip:

- **Packing** (`migration.packing-folder`) — a regular My-Drive folder holding
  one folder per user being migrated. It must **not** be in a shared drive
  (each user's Stash parks third-party-owned files, which a shared drive
  cannot hold) and must **not** be inside the crawl root (a re-crawl would
  ingest mid-migration scaffolding). Share it, as editor, with the admin's
  personal Gmail account — see below.
- **Dropoff** (`migration.dropoff-folder`) — the shared drive the user drags
  their Container into. Point this at the shared drive itself (its ID, which
  begins `0A…`), not a folder inside it. The drag is what transfers ownership to
  the org. `pack` grants the migrating user **Manager** access on the shared
  drive so they can move (drag) their Container in; `unpack` revokes it once the
  round trip is done.

Inside the packing folder, each user gets:

```
Packing/
  someone@gmail.com/                     created by pack, org-owned; user has NO access
    pickup-someone@gmail.com/            created by pack, org-owned; user gets READ access
      someone@gmail.com-Container/       created MANUALLY by the admin's personal Gmail
    Stash/                               created by pack, org-owned
```

The Container must be named exactly `<user>-Container` (e.g.
`someone@gmail.com-Container`) — `pack` looks it up by that name and refuses to
proceed until it exists. Scoping the name by account keeps each user's Container
distinguishable once several have been dragged into the same shared drive.

**Why the Pickup folder:** when a user accepts ownership of an item whose
parent folder they cannot see, Drive relocates the item to their My Drive root
— the Container would then be missing from where a `pack` re-run or `unpack`
looks for it. The user cannot simply be given access to their Packing folder,
because the Stash inside it holds third-party files they must not see. So the
Container lives in a `pickup-<user>` folder in between, and `pack` grants the
user read access to it (idempotently, like the dropoff grant): the Container
stays put when they accept ownership, and the Pickup folder — named for its
counterpart, the Dropoff folder — is where they grab it for the drag. `unpack`
deletes it with the rest of the scaffolding, which also removes the access.

**Why the Container is created manually:** the user must *own* the Container
to drag it into the shared drive, and Google only allows ownership transfers
between two personal accounts (or two accounts of the same Workspace). So the
admin creates the Container with a personal Gmail account and transfers its
ownership to the user's personal account via the Drive UI (an invite the user
must accept). On a first run, `pack` creates the user's Packing folder and Stash,
then stops with instructions to create the Container.

### `pack <user>`

`pack` gathers everything `<user>` (email or owner id) owns:

1. **Owned subtrees move intact.** Only *owned roots* — items the user owns
   whose parent they do not own — are moved into the Container; owned items
   nested below them ride along, so a deep owned tree costs one move, not
   thousands.
2. **A live sweep clears the drag blockers.** The Container tree is then walked
   via live Drive listings, and every item inside it the user does *not* own is
   moved into the flat Stash. Working from live state (not the database) also
   catches items created or re-owned after the crawl, so the sweep doubles as
   the pre-drag verification that nothing can block the move. Swept items with
   no database row are logged and recorded (the `pack_orphans` table) together
   with the folder they came from, so they can still be put back later.
3. **A straggler check catches drift.** Anything the database says the user
   owns that never showed up inside the Container tree (it moved since the
   crawl, so no owned ancestor carried it in) is fetched individually and moved
   flat into the Container.

Moves are optimistic — one `files.update` per item, no per-item pre-reads — and
a failed move triggers a single live lookup to diagnose (deleted, trashed,
already packed, ownership changed, or moved: retried from its live parent).
Like the old stash flow, `pack` first runs the `check-edit-access` scan and
asks for confirmation if any crawled item is not editable. It refuses to run
as the account being migrated, and warns when moving an item that has extra
parents recorded (`extra_parents`): the round trip collapses multi-parent
items to the single traversal parent.

`pack` also **refuses to run if `crawl.root.id` no longer matches the root the
database was crawled for**: it moves live files based on the snapshot's recorded
original parents, so a config pointed at a different tree would make every
placement decision suspect. Re-run `crawl` (which rebuilds the snapshot for the
new root) before packing. `unpack` deliberately skips this check — it finishes
an in-flight migration from the per-user state recorded at pack time, not from
the crawl root.

Pass **`--folder <id>`** (a Drive folder ID that was crawled into the database)
to scope the pack to just the user's items within that subfolder of the crawl
root, leaving the rest of their files in place. Owned roots are then
computed relative to the subfolder — the subfolder acts as the boundary, so an
owned item whose owned parent lies *outside* it still moves — and both the
edit-access pre-check and the dry-run sweep preview are limited to that subtree.
The confirmation message shows the subfolder's path relative to the crawl root.

Before scaffolding the pack, `pack` also grants the migrating user **Manager**
access on the dropoff folder (the shared drive) so they can drag their Container
in, and **read** access on their Pickup folder so the Container stays in place
when they accept ownership of it (both idempotent — a re-run does not re-notify
them). This needs the user's email; an owner-id-only account is warned about
and must be granted access manually.

Note Manager access is required to move folders into a Shared Drive.
"Content manager" access only allows creating files, not folders.

`pack` ends by printing the manual steps: transfer Container ownership to the
user (invite + accept), then have them drag the Container into the Dropoff
folder — one drag, and the org owns everything inside.

### `unpack <user>`

`unpack` finishes the migration after the drag. It looks for the Container in
the dropoff folder and verifies it now lives in that shared drive (and that the
running account can move items back *out* of it — that needs **manager** access
on the shared drive), then:

1. **Restores the Container's contents.** Each direct child moves back to the
   original parent recorded in `drive.db`; owned items nested deeper ride
   along with their folders. Because the drag flipped ownership of everything
   inside the Container to the org, the database's owner columns are updated to
   the running account for each restored item and its subtree (only
   the rows the snapshot attributed to the migrating user — nested third-party
   items were parked in the Stash and keep their owners), without a per-file
   Drive lookup.
2. **Restores the Stash.** Same mechanism. The Container must come first:
   Stash items are owned by third parties, and a shared drive cannot hold
   those, so their destination folders have to be back in the regular tree
   before they can follow. If a Stash move fails because its destination is
   still in the shared drive, re-running `unpack` converges.
3. **Quarantines what it cannot place.** An item with no database row, or
   whose original parent no longer exists, is moved to
   `Packing/<user>/Errors/<original-parent-id>/` (the subfolder name records
   where it belongs — from `pack_orphans` for never-crawled items, `unknown`
   when even that is missing) instead of blocking cleanup.
4. **Cleans up.** Once live listings confirm they are empty, the Stash, the
   Container, the Pickup folder (which also removes the user's read access on
   it), and the user's Packing folder are deleted, and the **Manager**
   access `pack` granted the user on the dropoff folder is revoked. The Container
   is only deleted, and the access only revoked, when the Container was dragged
   into the shared drive (a `--allow-not-moved` abort leaves them in place,
   including the Pickup folder still holding the un-dragged Container).
   Anything left over — a non-empty Errors folder, or items that failed to move
   — is reported and left in place.

**Aborting a stuck migration (`--allow-not-moved`).** Normally `unpack` refuses
to run until the Container is confirmed inside the dropoff folder's shared
drive. If the user becomes unavailable and never performs the drag, pass
`--allow-not-moved` to abort the migration instead: `unpack` restores every item
to its original location straight from the packing folder, so the user's files
are usable again in place until they can retry. Because nothing was dragged,
ownership never flipped to the org, so restored items keep their current owners
and the database owner columns are left unchanged (the normal post-drag path
records the org account as the new owner). Cleanup runs as usual, and re-running
`pack` later starts a fresh cycle.

### Order of operations (per user)

1. **`drive-cleanup crawl`** — fresh snapshot (re-run just before migrating;
   `pack` warns if the crawl is incomplete).
2. **`drive-cleanup check-edit-access`** — confirm the running account can edit
   everything that will move.
3. **`drive-cleanup pack <user>`** — first run scaffolds and asks for the
   Container; create it inside the Pickup folder with the admin's personal
   Gmail and re-run.
4. **Transfer Container ownership** to the user's personal account (Drive UI;
   the user must accept the invite).
5. **User drags the Container into the Dropoff folder** (the shared drive, where
   `pack` gave them Manager access) — one drag.
6. **`drive-cleanup unpack <user>`** — restore everything and clean up.

The crawl and pack are meant to run with the user on standby, with unpack
immediately after the drag, so the window in which the user's folders are
absent from the org tree (and third parties briefly lose folder-inherited
access to stashed files) stays short.

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

## Stale-row cleanup

Each crawl session is stamped with a start time (recorded in a single-row
`crawl_meta` table alongside the root it is snapshotting). Every node upsert
refreshes its `crawled_at`, so when a crawl **completes cleanly** any row whose
`crawled_at` predates the session start was not re-observed — the item was
deleted from Drive (or moved out of the tree) since the previous crawl — and is
removed, along with the `folder_permissions` and `extra_parents` rows that
referenced it. The snapshot therefore reflects the tree as it is now rather than
accumulating ghosts of deleted files.

The cutoff is persisted so an interrupted crawl **resumes the same session**
instead of resetting it (which would delete everything written before the
interruption). Cleanup runs only on a fully successful completion — never after
an interruption or a folder error, when some live nodes have not been re-listed
yet. A `--refresh` or a fresh database begins a new session; a plain resume
keeps the recorded one.

If `crawl.root.id` **changes** between crawls, the existing snapshot describes a
different tree, so `crawl` discards it entirely (nodes, permissions, extra
parents) and starts fresh for the new root. Per-user migration state
(`user_migrations`, `pack_orphans`) is left untouched; note that `pack` refuses
to run when the config root no longer matches the crawled root (see below).

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

**Migration state:** `pack` records each user's scaffolding in a
`user_migrations` table (one row per account: the user's Packing folder, Pickup
folder, Container, and Stash Drive IDs, plus `packed_at`/`unpacked_at`
timestamps set when each
half finishes with zero failures) — `unpack` needs the Container's ID because
by then the user has dragged it away from the packing folder. Swept items with
no `nodes` row land in `pack_orphans` with the live parent they were taken
from, which is where `unpack` gets the Errors subfolder name for them:

```sql
SELECT * FROM user_migrations;
SELECT * FROM pack_orphans;
```

## Rate limiting

A token-bucket limiter caps API calls, and every call retries with exponential
backoff plus jitter on `403 rateLimitExceeded`, `429`, and 5xx responses
(retries pass back through the limiter).

`crawl`, `pack`, and `unpack` all run their Drive calls through a bounded worker
pool (default 8, set with `--concurrency`) behind a shared ~20/sec limiter —
still far under Drive's per-user ceiling (~12k/min). Each `files.list` (crawl) or
`files.update` (pack/unpack) carries hundreds of ms of latency, so a single
sequential worker only reaches a few requests per second no matter the rate
limit; a handful of workers keeps enough in flight to reach the limiter's
ceiling, roughly 6x the old sequential throughput. All workers share the one
limiter, so it (not the worker count) is the quota cap; backoff self-throttles if
a burst overshoots. (The Drive API has no bulk call, and HTTP batching wouldn't
help — batched sub-requests still each count against quota.)

`crawl` fans out its breadth-first walk over the pool: workers share one folder
queue, each listing a folder and pushing the subfolders it discovers back onto
the queue for any worker to pick up, until the queue drains and no worker is
still listing. Its shared state is synchronized — a folder is listed once even
when reached through multiple parents, the file counter is atomic, and per-page
database writes serialize on the single SQLite connection — so listing (the
latency-bound part) is the only thing that actually runs in parallel. Ctrl-C
still stops cleanly: in-flight listings finish their current page, and any folder
not fully listed keeps `children_done = 0` so a re-run resumes it.

`pack` and `unpack` enumerate each phase's work first (listing + database
lookups, single-threaded) and only then fan the moves out, so the database
bookkeeping and ownership classification stay lock-free; `unpack` also serializes
creation of the `Errors` quarantine folders so concurrent workers can't make
duplicates. Their phase ordering is unchanged — `pack` fills the Container before
sweeping the Stash, `unpack` restores the Container before the Stash — only the
moves *within* a phase run in parallel. `--max-errors` applies across the pool:
exceeding the budget cancels the shared context, stopping in-flight moves and
short-circuiting the remaining phases.

## TODO

- **`status` command.** The migration is a stateful, multi-step sequence per
  user but nothing tells you where you are. Add a `drive-cleanup status` that
  reads the DB + config and reports in one place: whether the crawl is complete
  or how many folders are still pending, the owner count, how many items lack
  edit access, and whether each config section is present and its configured
  folders resolve in Drive. This would be the easiest on-ramp for "I forgot
  where I left off."
