CREATE TABLE IF NOT EXISTS nodes (
	id                 INTEGER PRIMARY KEY AUTOINCREMENT,
	drive_id           TEXT NOT NULL UNIQUE,
	name               TEXT NOT NULL,
	type               TEXT NOT NULL CHECK(type IN ('folder','shortcut','google_doc','binary')),
	mime_type          TEXT NOT NULL,
	owner_email        TEXT,
	owner_id           TEXT,
	owner_display_name TEXT,
	parent_id          INTEGER REFERENCES nodes(id),
	shortcut_target_id TEXT,
	children_done      INTEGER NOT NULL DEFAULT 0,
	-- can_edit records whether the account that ran the crawl can edit the node:
	-- 1 = editable, 0 = not. It drives the check-edit-access command.
	can_edit           INTEGER NOT NULL,
	crawled_at         TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_nodes_owner_email   ON nodes(owner_email);
CREATE INDEX IF NOT EXISTS idx_nodes_owner_id      ON nodes(owner_id);
CREATE INDEX IF NOT EXISTS idx_nodes_parent_id     ON nodes(parent_id);
CREATE INDEX IF NOT EXISTS idx_nodes_drive_id      ON nodes(drive_id);
CREATE INDEX IF NOT EXISTS idx_nodes_children_done ON nodes(children_done);
CREATE INDEX IF NOT EXISTS idx_nodes_can_edit      ON nodes(can_edit);

-- Extra sightings of a node under additional parents (legacy Drive
-- multi-parenting, or rediscovery through a second crawled folder).
-- nodes.parent_id keeps the first-discovered parent; every other observed
-- parent lands here for manual review.
CREATE TABLE IF NOT EXISTS extra_parents (
	node_drive_id   TEXT NOT NULL,
	parent_drive_id TEXT NOT NULL,
	observed_at     TEXT NOT NULL,
	PRIMARY KEY (node_drive_id, parent_drive_id)
);

-- Folder access-control entries captured during the crawl.
-- Only folder permissions are recorded -- files are excluded.
CREATE TABLE IF NOT EXISTS folder_permissions (
	node_drive_id        TEXT NOT NULL,
	permission_id        TEXT NOT NULL,
	type                 TEXT NOT NULL, -- user, group, domain, anyone
	role                 TEXT NOT NULL, -- owner, organizer, fileOrganizer, writer, commenter, reader
	email_address        TEXT,
	domain               TEXT,
	display_name         TEXT,
	allow_file_discovery INTEGER,       -- only meaningful for domain/anyone grants
	deleted              INTEGER NOT NULL DEFAULT 0,
	crawled_at           TEXT NOT NULL,
	PRIMARY KEY (node_drive_id, permission_id)
);
CREATE INDEX IF NOT EXISTS idx_folder_permissions_node ON folder_permissions(node_drive_id);

-- One row per user migration: where that user's Pickup folder, Container, and
-- Stash live and how far the pack/unpack cycle has progressed. Written by
-- pack, read by unpack -- the Container must be findable by ID after the user
-- drags it out of the packing folder into the shared drive.
CREATE TABLE IF NOT EXISTS user_migrations (
	account        TEXT PRIMARY KEY,   -- as passed to pack (email or owner id)
	user_folder_id TEXT NOT NULL,
	pickup_id      TEXT NOT NULL,
	container_id   TEXT NOT NULL,
	stash_id       TEXT NOT NULL,
	packed_at      TEXT,               -- set once pack finishes with zero failures
	unpacked_at    TEXT                -- set once unpack finishes with zero failures
);

-- Items pack swept into a Stash that have no nodes row (created after the
-- crawl). The live parent they were taken from is recorded so unpack can
-- quarantine them under Errors/<parent id> for manual restore.
CREATE TABLE IF NOT EXISTS pack_orphans (
	account              TEXT NOT NULL,
	item_drive_id        TEXT NOT NULL,
	from_parent_drive_id TEXT NOT NULL,
	swept_at             TEXT NOT NULL,
	PRIMARY KEY (account, item_drive_id)
);

-- Append-only audit log of every Google Drive write the tool performs (folder
-- creates, moves, deletes, permission grants/revokes). One row per operation,
-- tied to the migration (account) and command (pack/unpack) that issued it, with
-- the before/after parents and the outcome. Never read by the tool's own logic;
-- it exists purely for debugging and for manually restoring items that a move
-- left in the wrong place -- from_parent/to_parent record where an item was and
-- where it was headed, so a failed move can be reversed or completed by hand.
CREATE TABLE IF NOT EXISTS drive_ops (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	account     TEXT NOT NULL,          -- the migration this belongs to (user_migrations.account)
	command     TEXT NOT NULL,          -- 'pack' or 'unpack'
	operation   TEXT NOT NULL,          -- create_folder, move, move_verified, delete, grant_permission, revoke_permission
	item_id     TEXT,                   -- Drive id acted on (created folder, moved/deleted file, folder whose permissions changed)
	item_name   TEXT,                   -- human label when known (folder name on create); recover others via nodes.drive_id
	from_parent TEXT,                   -- parent(s) removed by a move (comma-separated), else NULL
	to_parent   TEXT,                   -- parent added by a move, or the containing folder on create, else NULL
	detail      TEXT,                   -- freeform extra context (permission email/role/id)
	status      TEXT NOT NULL,          -- 'ok' or 'error'
	error       TEXT,                   -- error message when status = 'error'
	started_at  TEXT NOT NULL,          -- RFC3339 UTC, just before the Drive call
	finished_at TEXT NOT NULL           -- RFC3339 UTC, just after it returned
);
CREATE INDEX IF NOT EXISTS idx_drive_ops_account ON drive_ops(account);
CREATE INDEX IF NOT EXISTS idx_drive_ops_item    ON drive_ops(item_id);
CREATE INDEX IF NOT EXISTS idx_drive_ops_status  ON drive_ops(status);

-- Single-row bookkeeping for the crawl. root_drive_id is the root the current
-- snapshot was built for -- a change in the configured root means the snapshot
-- belongs to a different tree. session_started_at marks when the in-progress
-- crawl session began; it is the cutoff for stale-row cleanup (any node not
-- re-observed since then is deleted when the crawl completes) and is persisted
-- so an interrupted crawl resumes the same session instead of resetting the
-- cutoff and deleting rows written before the interruption.
CREATE TABLE IF NOT EXISTS crawl_meta (
	id                 INTEGER PRIMARY KEY CHECK (id = 1),
	root_drive_id      TEXT NOT NULL,
	session_started_at TEXT NOT NULL
);
