-- pruned_nodes is the backup of every node row the crawl's stale-row sweep
-- removes from nodes. A node is pruned when a completed crawl did not re-observe
-- it, i.e. it no longer exists under the crawl root -- but that also happens when
-- a crawl is misconfigured or a folder is temporarily unreadable, and the row may
-- carry work that cannot be recovered from Drive (a review decision, the frozen
-- original owner, archive bookkeeping). Every prune therefore copies the row here
-- before deleting it.
--
-- Append-only history: a node pruned, re-crawled and pruned again gets one row
-- per prune, so there is no unique constraint on drive_id. Nothing in the tool
-- reads this table -- it exists to be inspected (and copied back by hand) when a
-- prune turns out to have been wrong.
CREATE TABLE IF NOT EXISTS pruned_nodes (
	id                          INTEGER PRIMARY KEY AUTOINCREMENT,
	pruned_at                   TEXT NOT NULL, -- RFC3339 UTC, when the sweep ran
	pruned_by                   TEXT NOT NULL, -- which sweep: 'crawl' or 'crawl_folder'

	-- The nodes row as it stood, verbatim. node_id is its old nodes.id: no longer
	-- a foreign key (the row is gone) and not reusable on restore, but it makes
	-- pruned rows joinable to each other via parent_id.
	node_id                     INTEGER NOT NULL,
	drive_id                    TEXT NOT NULL,
	name                        TEXT NOT NULL,
	type                        TEXT NOT NULL,
	mime_type                   TEXT NOT NULL,
	owner_email                 TEXT,
	owner_id                    TEXT,
	owner_display_name          TEXT,
	parent_id                   INTEGER,
	-- parent_drive_id resolves parent_id to the parent's Drive ID at prune time.
	-- Row ids do not survive a re-crawl, so this is what actually identifies the
	-- folder the item was last seen in. NULL when the node had no parent.
	parent_drive_id             TEXT,
	shortcut_target_id          TEXT,
	children_done               INTEGER NOT NULL,
	can_edit                    INTEGER NOT NULL,
	crawled_at                  TEXT NOT NULL, -- last time the node was observed, i.e. why it was pruned
	decision                    TEXT NOT NULL,
	last_modified               TEXT,
	original_owner_id           TEXT,
	original_owner_display_name TEXT,
	original_owner_email        TEXT,
	manual_transfer_performed   INTEGER NOT NULL,
	original_parent_drive_id    TEXT,
	archive_folder_drive_id     TEXT,
	delete_skipped              INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_pruned_nodes_drive_id  ON pruned_nodes(drive_id);
CREATE INDEX IF NOT EXISTS idx_pruned_nodes_pruned_at ON pruned_nodes(pruned_at);
CREATE INDEX IF NOT EXISTS idx_pruned_nodes_decision  ON pruned_nodes(decision);
