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
  },
  "archive": {
    "root": { "id": "1ArChIvE...", "name": "Archive root" }
  },
  "externals": {
    "root": { "id": "1ExTeRnAl...", "name": "zz Externally-owned files" }
  }
}
```

`archive.root` (optional; required by `archive`/`delete`/`restore`) is the
My-Drive folder soft-deleted files are moved into. It must live **outside** the
crawl root — inside it, the archive would inherit the crawl root's sharing, and
`archive` refuses to run. When configured, `crawl` also crawls the archive tree
(after the crawl root) so archived files stay in the snapshot; the review UI,
`keep-recent`, and `export-review` exclude it from decision marking.

`externals.root` (optional; required by `evict-externals`) is the My-Drive
folder externally-owned files are moved into to clear a subtree for a shared
drive. It must be a regular My-Drive folder — a shared drive cannot hold
externally-owned files, which is the whole point — and must not be the crawl
root or the archive root. Unlike `archive.root` it **may** sit inside the crawl
root, and that is the recommended place: evicted files then stay searchable from
the crawl root, stay in the snapshot on every crawl (no separate tree to crawl),
and stay migratable by `pack`/`unpack`. It must not sit inside the subtree you
are preparing, though — moving that subtree into a shared drive would take the
externals tree along with it, and `evict-externals` refuses to run in that case.

`migration.dropoff-folder` is required by `archive` and `delete` as well as by
`pack`/`unpack`: both route internally-owned items through the shared drive it
lives in to move their ownership to the org.

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

# Snapshot the database to db-backups/drive-<yyyymmdd-hhmm>.db before doing
# something risky; the optional description is appended to db-backups/log.txt
drive-cleanup backup
drive-cleanup backup before deleting the 2017 archive

# Soft-delete everything marked delete: move it into the archive folder,
# mirroring the original folder structure as "ARCH " replicas
drive-cleanup archive
drive-cleanup archive --folder 1AbCdEfGh...   # only that subtree

# Permanently delete archived items (asks for confirmation with counts)
drive-cleanup delete
drive-cleanup delete --remove-unowned          # also handle externally-owned items

# Bring one archived item back to its original folder and mark it keep
drive-cleanup restore 1AbCdEfGh...

# Replace every folder owned by someone else with an identically-named folder
# owned by you, moving the contents and the sharing across
drive-cleanup reclaim-folders alice@example.com
drive-cleanup reclaim-folders alice@example.com --subtree 1AbCdEfGh... # only that subtree
drive-cleanup reclaim-folders alice@example.com --folder 1AbCdEfGh...  # only that one folder

# Clear a folder of externally-owned items so it can be moved to a shared
# drive: they go to the externals folder, each leaving a shortcut behind
drive-cleanup evict-externals 1AbCdEfGh... --dry-run
drive-cleanup evict-externals 1AbCdEfGh...
```

The commands that change Drive — `pack`, `unpack`, `archive`, `delete`,
`restore`, `reclaim-folders`, and `evict-externals` — accept `--dry-run`, which reports every `WOULD
move/create/delete` action without changing anything. A dry run authenticates
with the read-only scope (so it never triggers a write-scope consent) and skips
the confirmation prompt. Run one first to preview a step. They also accept
`--max-errors` (default 5, except single-item `restore`): once more than that
many items fail to move, the run aborts — an error burst means something
systemic (wrong account, wrong config, revoked access), not per-item drift.
Pass `--verbose`/`-v` to any command to log every item it touches instead of
just progress and errors.

`backup` copies the database into `db-backups/` (next to the database itself)
as `drive-<yyyymmdd-hhmm>.db` in local time, and appends `<filename> -
<description>` to `db-backups/log.txt`. Everything after the subcommand is the
description, so it needs no quoting; a backup without one is still logged. The
copy is taken with SQLite's `VACUUM INTO`, so it is a consistent single-file
snapshot that includes anything still in the write-ahead log — no `-wal`/`-shm`
files are needed to restore it, and it is safe to run while `review` is
serving. It is the one command that does not apply pending schema migrations
first: it captures the database as it is on disk. `db-backups/` is gitignored.

`explore-owned-files` produces a single offline HTML file: an interactive,
collapsible tree of every file and folder the account owns plus their ancestor
folders, per-folder counts of owned descendants, Drive links, and keyboard
navigation — handy to attach when reaching out to an owner. Run it without an
account argument to generate one such file per owner in the database (owners
with neither an email nor an owner id are skipped).

### Taking over someone's folders (`reclaim-folders`)

Drive lets you move a file into a folder you own, but it never lets you take a
*folder's* ownership away from its owner — only they can hand it over.
`reclaim-folders` works around that: it supersedes each of their folders with an
identically-named folder that you own, and moves the contents across.

```bash
drive-cleanup reclaim-folders alice@example.com --dry-run
drive-cleanup reclaim-folders alice@example.com
drive-cleanup reclaim-folders alice@example.com --subtree 1AbCdEfGh...
drive-cleanup reclaim-folders alice@example.com --folder 1AbCdEfGh...
```

The email is matched against `owner_email` (case-insensitively) or `owner_id`.
Unscoped, every folder they own under the crawl root is replaced (the crawl root
itself never is, even if they own it). `--subtree` narrows that to one crawled
folder's subtree, the folder itself included when they own it; if an earlier run
already replaced that folder, the run follows the `(new) <name>` shortcut into
the replacement too, since the emptied folder no longer holds what is left to
reclaim. `--folder`
narrows it further, to that one crawled folder and nothing below it; the folder
must be one the snapshot says they own, and unlike the other two forms an id
that names no target of theirs is an error rather than an empty run. The two
flags cannot be combined. For each of their folders, shallowest first:

1. it is renamed `(old) <name>` — skipped if it already carries the prefix, so
   a re-run is a no-op rather than `(old) (old) <name>`;
2. a folder `<name>` owned by you is created under the same parent, or an
   existing one you own there is adopted;
3. their folder's sharing is copied onto yours, **without sending anybody a
   notification email**, so nobody loses access when the contents move;
4. everything directly inside their folder moves into yours;
5. a `(new) <name>` shortcut to your folder is left inside theirs — always, so
   anyone who lands on the old folder finds the new one;
6. if anything could not be moved, an `(old) <name>` shortcut back to their
   folder is created inside yours;
7. their folder is marked `keep`, so it is never archived or deleted.

Their emptied folder is deliberately left standing. It costs nothing, and with
the `(new) <name>` shortcut inside it, every link and bookmark that points at
the old folder goes on working and leads whoever follows it to the replacement.
The keep uses the same propagation the review UI applies, so any leftovers still
stuck inside are kept with it (leftovers already marked `delete` stay `delete`)
and `delete` decisions on its ancestors are cleared — nothing kept is ever left
inside a delete subtree.

The sharing copy runs before anything moves, so nobody is locked out even
briefly. The owner grant, grants naming you, and grants whose user or group
Drive reports as deleted are all skipped. Drive reports a My-Drive folder's
inherited grants alongside its own, and your replacement sits under the same
parent, so it starts with the same inherited access — only what their folder
has *beyond* that gets created on yours. Roles are compared, not just
grantees, so a folder of theirs that widens an inherited grant (shared with
`anyone` as writer inside a parent shared as reader, say) is matched by an
explicit grant on yours; anything your folder already provides at an equal or
stronger role is left alone, which keeps a re-run a no-op.

Because a folder of theirs nested inside another is carried into the new parent
in step 4 before its own turn comes, its replacement is created in the *new*
tree, not the old one. The snapshot is updated as the run goes — the new
folders and shortcuts become `nodes` rows, the moved items are reparented, and
the replacement's sharing is written to `folder_permissions` — so decisions and
later `archive`/`delete` runs see where things really are. Re-crawl afterwards
to pick up anything created since the last crawl.

### Clearing a folder for a shared drive (`evict-externals`)

A shared drive can only hold items the org owns, which makes an
externally-owned file inside a folder a problem the moment you want to move that
folder into one. Drive's own handling is worse than useless: up to 25 such items
it lets the move through and **takes those items out of the folder**, dropping
them into their own owners' My Drives, where we can no longer see them, let
alone migrate them later. Past 25 items it refuses the move outright.

`evict-externals` gets ahead of that. It moves every externally-owned item out
of the subtree itself, into a parallel **externals tree** (config
`externals.root`) that *we* own — so the files stay visible, stay shared with
the same people, and stay available to the ordinary `pack`/`unpack` migration if
their owner ever hands them over. There is no 25-item ceiling on this: they are
our own moves, one file at a time.

```bash
drive-cleanup evict-externals 1AbCdEfGh... --dry-run
drive-cleanup evict-externals 1AbCdEfGh...
```

The argument is a crawled folder under the crawl root — the one you are about to
move to a shared drive. "Externally owned" means what it means everywhere else
here: an owner that is neither the account running the command nor on one of the
configured `internal-domains`.

**Two things must be settled first**, and the run refuses to start (changing
nothing) until they are:

1. **Nothing in the subtree may still be marked `delete`.** Run `archive
   --folder <id>` first. Those items are unwanted and quite possibly unowned;
   archiving is both the cheaper way to get them out of the subtree and the
   honest one — evicting an unwanted file and leaving a shortcut to it would
   dress it up as something worth keeping.
2. **No externally-owned folder in the subtree may still hold content.** Run
   `reclaim-folders <owner>` first (the refusal names the owners). Moving such a
   folder out would take everything inside it — owned material included — along
   with it. After `reclaim-folders` each of their folders is empty except for
   the `(new) <name>` shortcut pointing at the replacement, and a folder like
   that carries nothing but itself. Empty, or holding nothing but a single
   shortcut, is the test.

   Every folder that still holds content is listed by path, owner, and how much
   it holds, so you can see what is at stake before choosing. If taking those
   folders wholesale is what you want, pass `--allow-unowned-folders`: the
   refusal becomes a warning, each such folder is evicted like an empty one, and
   everything inside it travels along rather than being evicted separately. Such
   a folder has no `(new) <name>` link of its own to travel with it, so a
   shortcut to it is left behind where it used to be, exactly as for an evicted
   file.

   The folder you pass must itself be owned by the org, too — nothing inside it
   can fix a folder that cannot go to a shared drive at all.

With those out of the way, every externally-owned folder left in the subtree is
an empty leaf, and the run proceeds:

* **Files** move into their folder's replica inside the externals tree, and a
  shortcut to the moved file is created in the folder it came from, so it is
  still reachable from where people expect it. (An evicted *shortcut* gets none
  in return — Drive shortcuts cannot point at other shortcuts, and nothing is
  lost: a shortcut is only a pointer, and what it pointed at has not moved.)
* **Emptied folders** move into their parent's replica as they stand, once a
  live listing confirms they really are still empty (or still hold just the one
  shortcut) — a folder that has gained content since the crawl is skipped rather
  than dragged out with it. No shortcut is created for them: `reclaim-folders`
  already left a `(new) <name>` link inside pointing at the folder that took
  over, and that link travels along. Links and bookmarks aimed at the folder
  keep working regardless, since its Drive ID does not change when it moves.

The externals tree mirrors the crawl root's folder structure, so an evicted file
keeps its original location and name. Each replica folder is named `(ext)
<original>` — the counterpart of the archive tree's `ARCH ` prefix — so it is
never mistaken for the canonical folder it mirrors, and it holds a `((new))
<original>` shortcut pointing at that folder, so the way back is one click from
wherever an evicted file landed. (`delete`'s replica prune discounts those
shortcuts, so a replica holding nothing else still counts as empty and goes,
taking its own signpost with it.) A replica is found again by the Drive ID
recorded for it rather than by its name, so a replica renamed by hand — or left
under its bare name by a run from before the `(ext) ` prefix — is found, renamed
back to `(ext) <original>`, and reused rather than built a second time. Only
ancestor folders that actually receive something are created — the tree never
fills up with empty placeholders. Each replica folder gets the original
folder's sharing copied onto it, **without sending anybody a notification
email**, so everyone who could reach the file before still can. Because the
replicas are built top-down, each one inherits what its parent replica provides
and only the delta is created; roles are compared, not just grantees, so a
folder that gives someone *more* access than its parent does (write where the
parent gave read) is matched by an explicit grant on the replica. A dry run
cannot ask folders that do not exist yet what they inherit, so it simulates the
same walk to estimate the counts.

The snapshot is updated as the run goes — replica folders and the shortcuts
become `nodes` rows, replica sharing is written to `folder_permissions`, and
every moved item is reparented under its replica and stamped with
`evicted_from_drive_id` — so later runs and the review UI see where things
really are. Re-crawl afterwards to pick up anything created since the last
crawl, and re-run to confirm the subtree is clean before you move it.

Re-runs are safe: an existing replica folder is adopted rather than duplicated,
an existing shortcut pointing at the right target is reused, and an item a
previous run already moved is simply finished off in the database.

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

Colors: red = delete subtree, green = keep, yellow = contains both with
nothing left undecided, pale yellow = contains both but still has undecided
items, pale red/green = partially decided, no color = undecided. A legend sits
under the header. Each row has a small ↗ icon that opens the item in Google
Drive.

`export-review` writes the same tree, coloring, and per-folder tallies into a
single self-contained HTML file (default `out/review.html`) that can be sent
to teammates for review. Note it includes every crawled node, so the file is
large (tens of MB for tens of thousands of nodes); it opens fine locally and
compresses well for sending.

#### Sharing the live UI over the internet

`export-review` produces a read-only snapshot. To let a teammate *make*
decisions, `scripts/share-review.sh` publishes the running UI through an ngrok
tunnel that requires a Google sign-in and accepts only
`@directactioneverywhere.com` accounts:

```sh
drive-cleanup review              # terminal 1: the API on :8844
cd web && npm run dev             # terminal 2: the UI on :3000
scripts/share-review.sh           # terminal 3: the public tunnel
```

Authentication happens at the ngrok edge, configured by
[ngrok/traffic-policy.yml](ngrok/traffic-policy.yml) — an unauthenticated or
out-of-domain request never reaches Next or the Go API, and neither server's
localhost binding changes. The policy uses ngrok's managed Google OAuth app, so
there is no OAuth client to register; the domain restriction is a fail-closed
CEL expression over the verified identity, and sessions expire after 8h idle /
24h absolute.

One-time setup: sign in at [ngrok](https://dashboard.ngrok.com/) and follow the
setup instructions there to install the agent and run
`ngrok config add-authtoken <token>`. Neither the binary nor the token survives
a devcontainer rebuild, so expect to redo both; `share-review.sh` checks for
each and points you here if either is missing.

This works on ngrok's free plan — OAuth is included there. The limit that
actually bites is [3 traffic identities (OAuth MAU) per
month](https://ngrok.com/docs/pricing-limits/free-plan-limits/): distinct people
signing in, counted per billing cycle. Up to three reviewers a month costs
nothing; beyond that you need Hobbyist or pay-as-you-go. ngrok's docs do not say
how the cap is enforced once reached, so if a fourth person cannot get in, check
[usage](https://dashboard.ngrok.com/usage) before assuming the policy is broken.

Free accounts also get one auto-assigned dev domain
(`something.ngrok-free.app`) and it is the only domain they may use — *random*
URLs are the paid feature now — so the link is already stable between runs.
`NGROK_DOMAIN` is there for a reserved or custom domain on a paid plan:

```sh
NGROK_DOMAIN=dxe-drive-review.ngrok.app scripts/share-review.sh
```

Two things worth knowing before you rely on this:

- The script does not take the gate on trust. Once the tunnel is up it requests
  its own public URL anonymously, and if that returns HTTP 200 — the app being
  reachable without signing in — it tears the tunnel down and refuses to print
  the URL, rather than quietly publishing the Drive database.
- Everyone who signs in shares one database with no per-user attribution, and
  undo history is per-server and in-memory. Treat it as a tool for collaborators
  you trust, not as a public app.

On the free plan visitors also see ngrok's interstitial warning page once per
browser (suppressed for 7 days after they click through). It is cosmetic and
does not affect the sign-in gate.

`check-edit-access` reads only the database and prints every node whose
`can_edit` flag (Drive's `capabilities.canEdit`, captured during the crawl for
the account that ran it) is false — the items you would be unable to move.
Folders are listed first, each with its full path and owner. Nodes whose edit
capability could not be determined (`can_edit = NULL`) are not reported.

## Archiving and deleting (`archive` / `delete` / `restore`)

Once the review is done, deletion happens in two stages to allow time for someone
to realize something important is missing:
* `archive` soft-deletes (moves marked files into the archive folder), and
* `delete` permanently deletes what was archived.

`restore` reverses the archival of one file.

The recommended order of operations is **crawl → pack/unpack → archive →
delete**: transferring ownership to the org first (pack/unpack) means `archive`
and `delete` can act on most items directly instead of routing them through the
dropoff shared drive file by file. Packing and unpacking *after* archiving works
too — the archive tree is part of the crawl snapshot, so archived files pack
like any others.
Just treat `pack` and `unpack` as one atomic pair: don't archive between a pack
and its unpack, while files sit in mid-migration scaffolding.

### `archive`

`archive` moves every file marked **delete** into the archive folder
(`archive.root`), recreating the file's ancestor chain beneath it as replica
folders prefixed with `ARCH ` (truncated if very long) so nothing is confused
with an original and every archived file keeps its original location and name.
Replica folders are created *without* the originals' sharing, and each folder's
replica ID is cached on its row (`archive_folder_drive_id`) so re-runs don't
repeat lookups or create duplicates; a cached replica that was deleted on Drive
is re-created.

After each file moves, its individually-added permissions (user and group
grants) are replaced with the running account's own: the account grants itself
access **first** — it may only have access via a Google Group, and dropping
that group's grant while holding no direct one would lock it out — and only
once that grant is in place are the other grants revoked. The owner's
permission and domain/anyone grants are left alone.

A file **owned by another internal account** (an owner whose email ends in one
of the `internal-domains`) is unsharable from that owner: an owner permission
cannot be revoked. Those files therefore take a detour before they are archived:

1. the file is moved into an `Archival pending` folder inside the dropoff shared
   drive (`migration.dropoff-folder`), which transfers its ownership to the org;
2. `archive` waits for that transfer to land — Drive applies it asynchronously,
   file by file, the same lag `unpack` waits out after a Container drag, and a
   file moved back out too early would land in the archive still owned by its
   original owner. One listing of the pending folder answers "has it
   transferred?" for the whole batch at once (a shared drive's files report no
   owner), and the wait gives up on the remainder only once a full round brings
   nothing new;
3. the file then moves on into its archive replica, which makes the running
   account its owner, and the previous owner's now-ordinary grant is revoked
   with the rest of its individual permissions.

The snapshot's `owner_*` columns follow the transfer; `original_owner_*` stays
frozen at what the crawl discovered, as always. The `Archival pending` folder is
removed again once the run leaves it empty; files still in it (a transfer that
never landed, or an interrupted run) are reported, and re-running `archive`
picks them up from there. This needs the Google Workspace privilege **"Move any
file or folder into shared drives"** and **manager access on the dropoff shared
drive** (to move files back out); the latter is checked before any file goes in.
Externally-owned files are archived as before — their ownership cannot be taken
over, so they keep it and their owner keeps access.

Delete-marked **folders** are archived too once a live check shows them empty
(descendants are archived before ancestors, so a folder's turn comes after its
contents). Folders keep their permissions. Drive listings are eventually
consistent, so a folder that emptied a moment ago may be skipped once — re-run
`archive` to pick it up. `--folder <id>` scopes the run to one crawled subtree.

The archived state is recorded in `original_parent_drive_id` (the restore
target; also what marks a row "archived"), and the row is reparented under the
replica's row so the snapshot keeps describing where things actually live.

### `delete`

`delete` permanently deletes archived items that are still marked delete —
only rows whose `original_parent_drive_id` is set are ever touched — after a
confirmation prompt with counts. Ownership decides the mechanics:

- Items **owned by the running account** are deleted directly.
- Items owned by **other internal accounts** (`internal-domains` in config)
  cannot be deleted directly, but moving an item into a shared drive flips its
  ownership to the org: they are moved into a `Deletion pending` folder inside
  the dropoff shared drive (`migration.dropoff-folder`), deleted there, and the
  folder itself is removed once it empties. In practice this path now mostly
  handles archived **folders**, which keep their owner — `archive` already took
  ownership of internally-owned files on the way in. **This requires the Google
  Workspace privilege "Move any file or folder into shared drives"** for the
  OAuth user (or the admin enabling it for all users); without it these moves
  fail.
- **Externally-owned** items are skipped and counted (flagged `delete_skipped`)
  unless `--remove-unowned` is passed, which removes the item from its archive
  folder — Drive relocates it to its owner's My Drive with sharing intact —
  and then drops every direct permission the account can revoke, its own last.

A really-deleted item's database row is removed. Empty `ARCH ` replica folders
are pruned afterwards (deepest first) and their originals' caches cleared;
folders that still look non-empty (eventual consistency again) are left for a
re-run. `--folder <id>` scopes the run to one subtree of the archive tree — a
crawled folder's ID is accepted too and resolved to its replica.

### `restore <drive_id>`

`restore` moves one archived item back under its recorded original parent,
clears the archived state, and marks the item **keep** (so the next `archive`
run doesn't immediately re-archive it). It fails loudly, changing nothing, if
the original parent no longer exists. Permissions removed during archiving are
not restored — re-share by hand if needed — and neither is ownership: a file
whose ownership `archive` transferred to the org comes back owned by the running
account.

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

**Archived rows are exempt** from this pruning. Normally that changes nothing —
the archive tree is crawled too, so archived rows are re-observed like any
others — but if the `archive` config section is ever removed or misconfigured,
the exemption keeps a full crawl from destroying the archival records that
`restore` and `delete` depend on. A row whose item truly vanished from Drive
self-heals later: `delete` treats a 404 as already deleted and drops the row.

**Evicted rows** (`evicted_from_drive_id` set) are exempt for the same reason.
An externals tree inside the crawl root is crawled like anything else, but one
placed outside it is never visited, and the row is the only record of where the
item came from.

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
| `owner_email` / `owner_id` / `owner_display_name` | current owner identity; email can be missing, `owner_id` is the stable Drive permission id. Updated when ownership moves to the org: by `unpack` after a Container drag, and by `archive` when it takes over an internally-owned file |
| `parent_id` | row id of the folder this node was **discovered through** (NULL for the root) |
| `shortcut_target_id` | for shortcuts, the Drive ID they point to (shortcuts are recorded, never followed) |
| `children_done` | 1 once all children are fully listed — the resume unit |
| `can_edit` | Drive `capabilities.canEdit` for the crawling account: 1 editable, 0 not, NULL unknown — drives `check-edit-access` |
| `crawled_at` | RFC3339 timestamp |
| `original_parent_drive_id` | Drive ID of the folder the item lived in before `archive` moved it; non-NULL means "archived" — the `restore` target and `delete` prerequisite |
| `archive_folder_drive_id` | folders only: cached Drive ID of the folder's `ARCH ` replica in the archive tree; cleared when `delete` prunes the replica |
| `delete_skipped` | 1 when `delete` skipped the archived item because it is externally owned (re-run with `--remove-unowned`) |
| `externals_folder_drive_id` | folders only: cached Drive ID of the folder's replica in the externals tree, the `evict-externals` counterpart of `archive_folder_drive_id` |
| `evicted_from_drive_id` | Drive ID of the folder the item lived in before `evict-externals` moved it into the externals tree; non-NULL means "evicted", and is where the shortcut left in its place lives |

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
