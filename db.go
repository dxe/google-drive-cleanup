package main

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver, no cgo
)

// nodeTypes lists every valid nodes.type value. It is the single source of
// truth for the column's CHECK constraint below.
var nodeTypes = []string{typeFolder, typeShortcut, typeGoogleDoc, typeBinary}

// sqlQuoteList renders values as a comma-separated list of single-quoted SQL
// string literals, e.g. ['a','b'] -> "'a','b'". The values here are trusted
// in-code constants, not user input.
func sqlQuoteList(values []string) string {
	quoted := make([]string, len(values))
	for i, v := range values {
		quoted[i] = "'" + v + "'"
	}
	return strings.Join(quoted, ",")
}

var schemaSQL = fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS nodes (
	id                 INTEGER PRIMARY KEY AUTOINCREMENT,
	drive_id           TEXT NOT NULL UNIQUE,
	name               TEXT NOT NULL,
	type               TEXT NOT NULL CHECK(type IN (%s)),
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
`, sqlQuoteList(nodeTypes))

func openDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	// SQLite allows a single writer; one connection avoids SQLITE_BUSY
	// contention between our own statements.
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{
		"PRAGMA journal_mode = WAL",
		"PRAGMA foreign_keys = ON",
		"PRAGMA busy_timeout = 10000",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("%s: %w", pragma, err)
		}
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("creating schema: %w", err)
	}
	return db, nil
}

func now() string {
	return time.Now().UTC().Format(time.RFC3339)
}

type node struct {
	driveID        string
	name           string
	typ            string
	mimeType       string
	ownerEmail     sql.NullString
	ownerID        sql.NullString
	ownerDisplay   sql.NullString
	parentID       sql.NullInt64 // internal row id of the folder we found it under; NULL for the root
	shortcutTarget sql.NullString
	canEdit        bool // whether the crawling account can edit the node
}

// upsertNode inserts n or refreshes the existing row with the same drive_id.
// The upsert never regresses progress: an existing non-NULL parent_id wins
// over the new one (first-discovered parent is kept), and children_done can
// only go up. Name, owner, mime_type and crawled_at are refreshed to the
// latest values. It returns the row id plus what was stored before the call
// (existed, prevParent, prevDone) so the caller can detect re-discovery under
// a different parent and skip folders whose children are already listed.
func upsertNode(tx *sql.Tx, n node) (rowID int64, existed bool, prevParent sql.NullInt64, prevDone bool, err error) {
	var prevDoneInt int
	err = tx.QueryRow(
		`SELECT id, parent_id, children_done FROM nodes WHERE drive_id = ?`, n.driveID,
	).Scan(&rowID, &prevParent, &prevDoneInt)
	switch err {
	case nil:
		existed = true
		prevDone = prevDoneInt != 0
	case sql.ErrNoRows:
		err = nil
	default:
		return 0, false, sql.NullInt64{}, false, err
	}

	err = tx.QueryRow(`
		INSERT INTO nodes (drive_id, name, type, mime_type, owner_email, owner_id,
			owner_display_name, parent_id, shortcut_target_id, children_done, can_edit, crawled_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?)
		ON CONFLICT(drive_id) DO UPDATE SET
			name               = excluded.name,
			type               = excluded.type,
			mime_type          = excluded.mime_type,
			owner_email        = excluded.owner_email,
			owner_id           = excluded.owner_id,
			owner_display_name = excluded.owner_display_name,
			parent_id          = COALESCE(nodes.parent_id, excluded.parent_id),
			shortcut_target_id = excluded.shortcut_target_id,
			children_done      = MAX(nodes.children_done, excluded.children_done),
			can_edit           = excluded.can_edit,
			crawled_at         = excluded.crawled_at
		RETURNING id`,
		n.driveID, n.name, n.typ, n.mimeType, n.ownerEmail, n.ownerID,
		n.ownerDisplay, n.parentID, n.shortcutTarget, n.canEdit, now(),
	).Scan(&rowID)
	return rowID, existed, prevParent, prevDone, err
}

func markChildrenDone(tx *sql.Tx, rowID int64) error {
	_, err := tx.Exec(`UPDATE nodes SET children_done = 1 WHERE id = ?`, rowID)
	return err
}

type folderRef struct {
	rowID   int64
	driveID string
	name    string
}

// pendingFolders returns every folder whose children have not been fully listed
// yet — the resumable work queue. When subtreeRoot is non-empty the queue is
// limited to that subtree (inclusive), the work list for a scoped re-index.
// Either way folders are seeded directly, not only through parent discovery, so
// a folder deleted from Drive (whose parent no longer lists it) is still listed,
// comes back empty, is marked done, and can then be pruned.
func pendingFolders(db *sql.DB, subtreeRoot string) ([]folderRef, error) {
	query := fmt.Sprintf(
		`SELECT id, drive_id, name FROM nodes WHERE type = '%s' AND children_done = 0 ORDER BY id`, typeFolder)
	args := []any{}
	if subtreeRoot != "" {
		query = fmt.Sprintf(`WITH RECURSIVE subtree(id) AS (
			SELECT id FROM nodes WHERE drive_id = ?
			UNION ALL
			SELECT n.id FROM nodes n JOIN subtree s ON n.parent_id = s.id
		)
		SELECT id, drive_id, name FROM nodes
		WHERE type = '%s' AND children_done = 0 AND id IN (SELECT id FROM subtree)
		ORDER BY id`, typeFolder)
		args = []any{subtreeRoot}
	}
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var refs []folderRef
	for rows.Next() {
		var f folderRef
		if err := rows.Scan(&f.rowID, &f.driveID, &f.name); err != nil {
			return nil, err
		}
		refs = append(refs, f)
	}
	return refs, rows.Err()
}

// countPendingFolders counts folders still awaiting listing (children_done = 0),
// restricted to the subtree rooted at subtreeRoot (inclusive) when non-empty.
func countPendingFolders(db *sql.DB, subtreeRoot string) (int, error) {
	query := fmt.Sprintf(`SELECT COUNT(*) FROM nodes WHERE type = '%s' AND children_done = 0`, typeFolder)
	args := []any{}
	if subtreeRoot != "" {
		query = fmt.Sprintf(`WITH RECURSIVE subtree(id) AS (
			SELECT id FROM nodes WHERE drive_id = ?
			UNION ALL
			SELECT n.id FROM nodes n JOIN subtree s ON n.parent_id = s.id
		)
		SELECT COUNT(*) FROM nodes
		WHERE type = '%s' AND children_done = 0 AND id IN (SELECT id FROM subtree)`, typeFolder)
		args = []any{subtreeRoot}
	}
	var n int
	err := db.QueryRow(query, args...).Scan(&n)
	return n, err
}

// resetChildrenDone resets children_done to 0 on every folder, forcing them to
// be re-listed. When subtreeRoot is non-empty only folders within that subtree
// (inclusive) are reset, so a scoped re-index re-lists just that subtree rather
// than skipping folders an earlier crawl already marked done.
func resetChildrenDone(db *sql.DB, subtreeRoot string) error {
	query := fmt.Sprintf(`UPDATE nodes SET children_done = 0 WHERE type = '%s'`, typeFolder)
	args := []any{}
	if subtreeRoot != "" {
		query = fmt.Sprintf(`WITH RECURSIVE subtree(id) AS (
			SELECT id FROM nodes WHERE drive_id = ?
			UNION ALL
			SELECT n.id FROM nodes n JOIN subtree s ON n.parent_id = s.id
		)
		UPDATE nodes SET children_done = 0
		WHERE type = '%s' AND id IN (SELECT id FROM subtree)`, typeFolder)
		args = []any{subtreeRoot}
	}
	_, err := db.Exec(query, args...)
	return err
}

// folderRefByDriveID returns the queue entry for the folder with the given
// Drive ID. Returns sql.ErrNoRows if it was not crawled.
func folderRefByDriveID(db *sql.DB, driveID string) (folderRef, error) {
	var f folderRef
	err := db.QueryRow(`SELECT id, drive_id, name FROM nodes WHERE drive_id = ?`, driveID).
		Scan(&f.rowID, &f.driveID, &f.name)
	return f, err
}

func recordExtraParent(tx *sql.Tx, nodeDriveID, parentDriveID string) error {
	_, err := tx.Exec(
		`INSERT OR IGNORE INTO extra_parents (node_drive_id, parent_drive_id, observed_at) VALUES (?, ?, ?)`,
		nodeDriveID, parentDriveID, now())
	return err
}

// permission is one access-control entry on a folder, captured for later
// recreation on a clone. It mirrors the fields of a drive.Permission we care
// about.
type permission struct {
	permissionID       string
	typ                string // user, group, domain, anyone
	role               string // owner, organizer, fileOrganizer, writer, commenter, reader
	emailAddress       sql.NullString
	domain             sql.NullString
	displayName        sql.NullString
	allowFileDiscovery sql.NullBool
	deleted            bool
}

// replacePermissions rewrites the full permission set recorded for a folder.
// It deletes any existing rows for the folder and inserts the supplied ones, so
// a re-crawl always leaves an exact snapshot of the folder's current sharing
// rather than accumulating stale grants.
func replacePermissions(tx *sql.Tx, nodeDriveID string, perms []permission) error {
	if _, err := tx.Exec(`DELETE FROM folder_permissions WHERE node_drive_id = ?`, nodeDriveID); err != nil {
		return err
	}
	ts := now()
	for _, p := range perms {
		if _, err := tx.Exec(`
			INSERT INTO folder_permissions (node_drive_id, permission_id, type, role,
				email_address, domain, display_name, allow_file_discovery, deleted, crawled_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			nodeDriveID, p.permissionID, p.typ, p.role,
			p.emailAddress, p.domain, p.displayName, p.allowFileDiscovery, p.deleted, ts,
		); err != nil {
			return fmt.Errorf("inserting permission %s on %s: %w", p.permissionID, nodeDriveID, err)
		}
	}
	return nil
}

// accessRow is one node the crawling account cannot edit, reported by
// check-edit-access with its full path for context.
type accessRow struct {
	driveID string
	name    string
	typ     string
	path    string
	owner   string
}

// nodesLackingEditAccess returns every node whose recorded can_edit flag is 0
// (the crawling account cannot edit it), ordered folders-first then by path.
// The full path is resolved from the in-memory node tree.
func nodesLackingEditAccess(db *sql.DB) ([]accessRow, error) {
	names, parents, err := loadNodeTree(db)
	if err != nil {
		return nil, err
	}

	rows, err := db.Query(fmt.Sprintf(`
		SELECT id, drive_id, name, type, owner_email, owner_id, owner_display_name
		FROM nodes
		WHERE can_edit = 0
		ORDER BY (type <> '%s'), name COLLATE NOCASE`, typeFolder))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []accessRow
	for rows.Next() {
		var (
			id int64
			oc ownerCount
			r  accessRow
		)
		if err := rows.Scan(&id, &r.driveID, &r.name, &r.typ, &oc.email, &oc.ownerID, &oc.displayName); err != nil {
			return nil, err
		}
		r.path = strings.Join(buildPath(id, names, parents), " / ")
		r.owner = ownerLabel(oc)
		out = append(out, r)
	}
	return out, rows.Err()
}

// loadNodeTree loads the id -> name and id -> parent_id maps for the whole
// nodes table, used to resolve paths without a query per node.
func loadNodeTree(db *sql.DB) (names map[int64]string, parents map[int64]sql.NullInt64, err error) {
	rows, err := db.Query(`SELECT id, name, parent_id FROM nodes`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	names = make(map[int64]string)
	parents = make(map[int64]sql.NullInt64)
	for rows.Next() {
		var (
			id     int64
			name   string
			parent sql.NullInt64
		)
		if err := rows.Scan(&id, &name, &parent); err != nil {
			return nil, nil, err
		}
		names[id] = name
		parents[id] = parent
	}
	return names, parents, rows.Err()
}

// buildPath walks parent_id upwards from id, returning the names from the crawl
// root down to the node. It is cycle-guarded so a corrupt parent chain yields a
// truncated path instead of looping forever.
func buildPath(id int64, names map[int64]string, parents map[int64]sql.NullInt64) []string {
	var segments []string
	seen := make(map[int64]bool)
	for cur := id; ; {
		if seen[cur] {
			break
		}
		seen[cur] = true
		name, ok := names[cur]
		if !ok {
			break
		}
		segments = append(segments, name)
		p := parents[cur]
		if !p.Valid {
			break
		}
		cur = p.Int64
	}
	for i, j := 0, len(segments)-1; i < j; i, j = i+1, j-1 {
		segments[i], segments[j] = segments[j], segments[i]
	}
	return segments
}

// knownDriveIDs reports which of ids already have a row in nodes.
func knownDriveIDs(tx *sql.Tx, ids []string) (map[string]bool, error) {
	known := make(map[string]bool, len(ids))
	if len(ids) == 0 {
		return known, nil
	}
	placeholders := strings.Repeat("?,", len(ids)-1) + "?"
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := tx.Query(`SELECT drive_id FROM nodes WHERE drive_id IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		known[id] = true
	}
	return known, rows.Err()
}

// ownedNode is one node owned by the migrating user, of any type.
type ownedNode struct {
	driveID string
	name    string
	typ     string
}

// nodesOwnedBy returns every node owned by account (matched against
// owner_email OR owner_id), of any type, ordered by row id. When subtreeRoot is
// non-empty, only nodes within the subtree rooted at that Drive ID (inclusive)
// are returned, so a pack can be scoped to one subfolder of the crawl root.
func nodesOwnedBy(db *sql.DB, account, subtreeRoot string) ([]ownedNode, error) {
	query := `SELECT drive_id, name, type FROM nodes
		 WHERE (owner_email = ? OR owner_id = ?)
		 ORDER BY id`
	queryArgs := []any{account, account}
	if subtreeRoot != "" {
		query = `WITH RECURSIVE subtree(id) AS (
			SELECT id FROM nodes WHERE drive_id = ?
			UNION ALL
			SELECT n.id FROM nodes n JOIN subtree s ON n.parent_id = s.id
		)
		SELECT drive_id, name, type FROM nodes
		WHERE id IN (SELECT id FROM subtree)
		  AND (owner_email = ? OR owner_id = ?)
		ORDER BY id`
		queryArgs = []any{subtreeRoot, account, account}
	}
	rows, err := db.Query(query, queryArgs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var nodes []ownedNode
	for rows.Next() {
		var n ownedNode
		if err := rows.Scan(&n.driveID, &n.name, &n.typ); err != nil {
			return nil, err
		}
		nodes = append(nodes, n)
	}
	return nodes, rows.Err()
}

// ownedRoot is an item owned by the migrating user whose parent is not: the
// top of an owned subtree. pack moves exactly these into the Container; owned
// items nested below them ride along.
type ownedRoot struct {
	driveID       string
	name          string
	typ           string
	parentDriveID string // the traversal parent recorded by the crawl
}

// ownedRoots returns every node owned by account whose parent is NOT owned by
// account, ordered by row id. The crawl root is excluded by the JOIN (it has
// no parent row). IS NOT treats a parent with NULL owner fields as not-owned.
//
// When subtreeRoot is non-empty the roots are computed relative to that
// subfolder: only owned nodes within its subtree are considered, and the
// subfolder itself acts as a boundary — an owned node whose parent lies outside
// the subtree (i.e. the subfolder itself, when it is owned) counts as a root
// even though that parent is owned, so scoping to a subfolder always moves the
// owned material inside it intact.
func ownedRoots(db *sql.DB, account, subtreeRoot string) ([]ownedRoot, error) {
	query := `
		SELECT n.drive_id, n.name, n.type, p.drive_id
		FROM nodes n
		JOIN nodes p ON p.id = n.parent_id
		WHERE (n.owner_email = ? OR n.owner_id = ?)
		  AND p.owner_email IS NOT ? AND p.owner_id IS NOT ?
		ORDER BY n.id`
	queryArgs := []any{account, account, account, account}
	if subtreeRoot != "" {
		query = `WITH RECURSIVE subtree(id) AS (
			SELECT id FROM nodes WHERE drive_id = ?
			UNION ALL
			SELECT n.id FROM nodes n JOIN subtree s ON n.parent_id = s.id
		)
		SELECT n.drive_id, n.name, n.type, p.drive_id
		FROM nodes n
		JOIN nodes p ON p.id = n.parent_id
		WHERE n.id IN (SELECT id FROM subtree)
		  AND (n.owner_email = ? OR n.owner_id = ?)
		  AND (
		    p.id NOT IN (SELECT id FROM subtree)
		    OR (p.owner_email IS NOT ? AND p.owner_id IS NOT ?)
		  )
		ORDER BY n.id`
		queryArgs = []any{subtreeRoot, account, account, account, account}
	}
	rows, err := db.Query(query, queryArgs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var roots []ownedRoot
	for rows.Next() {
		var r ownedRoot
		if err := rows.Scan(&r.driveID, &r.name, &r.typ, &r.parentDriveID); err != nil {
			return nil, err
		}
		roots = append(roots, r)
	}
	return roots, rows.Err()
}

// sweepPreview is one item pack's Stash sweep is expected to move: a node not
// owned by the migrating user sitting directly inside a folder that is.
type sweepPreview struct {
	driveID       string
	name          string
	parentDriveID string
}

// unownedChildrenOfOwned returns nodes NOT owned by account whose parent IS
// owned by account. This is the dry-run preview of pack's Stash sweep; the
// real sweep works from live listings so it also catches post-crawl items.
// When subtreeRoot is non-empty the preview is limited to that subfolder's
// subtree, matching a subfolder-scoped pack.
func unownedChildrenOfOwned(db *sql.DB, account, subtreeRoot string) ([]sweepPreview, error) {
	query := `
		SELECT n.drive_id, n.name, p.drive_id
		FROM nodes n
		JOIN nodes p ON p.id = n.parent_id
		WHERE (p.owner_email = ? OR p.owner_id = ?)
		  AND n.owner_email IS NOT ? AND n.owner_id IS NOT ?
		ORDER BY n.id`
	queryArgs := []any{account, account, account, account}
	if subtreeRoot != "" {
		query = `WITH RECURSIVE subtree(id) AS (
			SELECT id FROM nodes WHERE drive_id = ?
			UNION ALL
			SELECT n.id FROM nodes n JOIN subtree s ON n.parent_id = s.id
		)
		SELECT n.drive_id, n.name, p.drive_id
		FROM nodes n
		JOIN nodes p ON p.id = n.parent_id
		WHERE n.id IN (SELECT id FROM subtree)
		  AND (p.owner_email = ? OR p.owner_id = ?)
		  AND n.owner_email IS NOT ? AND n.owner_id IS NOT ?
		ORDER BY n.id`
		queryArgs = []any{subtreeRoot, account, account, account, account}
	}
	rows, err := db.Query(query, queryArgs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []sweepPreview
	for rows.Next() {
		var s sweepPreview
		if err := rows.Scan(&s.driveID, &s.name, &s.parentDriveID); err != nil {
			return nil, err
		}
		items = append(items, s)
	}
	return items, rows.Err()
}

// extraParentNodeIDs returns the Drive IDs of every node observed under more
// than one parent. pack warns when moving these: the round trip through the
// shared drive collapses an item to a single parent, and unpack restores only
// the traversal parent recorded in nodes.
func extraParentNodeIDs(db *sql.DB) (map[string]bool, error) {
	rows, err := db.Query(`SELECT DISTINCT node_drive_id FROM extra_parents`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := make(map[string]bool)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids[id] = true
	}
	return ids, rows.Err()
}

// userMigration is the recorded state of one user's pack/unpack cycle.
type userMigration struct {
	account      string
	userFolderID string
	pickupID     string
	containerID  string
	stashID      string
	packedAt     sql.NullString
	unpackedAt   sql.NullString
}

// getUserMigration returns the recorded migration for account, or nil if pack
// has never run for it.
func getUserMigration(db *sql.DB, account string) (*userMigration, error) {
	m := userMigration{account: account}
	err := db.QueryRow(`
		SELECT user_folder_id, pickup_id, container_id, stash_id, packed_at, unpacked_at
		FROM user_migrations WHERE account = ?`, account).
		Scan(&m.userFolderID, &m.pickupID, &m.containerID, &m.stashID, &m.packedAt, &m.unpackedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// upsertUserMigration records where account's Pickup folder, Container, and
// Stash live. It clears the packed/unpacked timestamps: a (re-)running pack
// means the cycle is in progress again, and markPacked/markUnpacked re-set
// them on success.
func upsertUserMigration(db *sql.DB, account, userFolderID, pickupID, containerID, stashID string) error {
	_, err := db.Exec(`
		INSERT INTO user_migrations (account, user_folder_id, pickup_id, container_id, stash_id)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(account) DO UPDATE SET
			user_folder_id = excluded.user_folder_id,
			pickup_id      = excluded.pickup_id,
			container_id   = excluded.container_id,
			stash_id       = excluded.stash_id,
			packed_at      = NULL,
			unpacked_at    = NULL`,
		account, userFolderID, pickupID, containerID, stashID)
	return err
}

func markPacked(db *sql.DB, account string) error {
	_, err := db.Exec(`UPDATE user_migrations SET packed_at = ? WHERE account = ?`, now(), account)
	return err
}

func markUnpacked(db *sql.DB, account string) error {
	_, err := db.Exec(`UPDATE user_migrations SET unpacked_at = ? WHERE account = ?`, now(), account)
	return err
}

// recordPackOrphan notes that an item with no nodes row was swept to the Stash
// from the given live parent, so unpack can quarantine it under that parent's
// Errors subfolder later.
func recordPackOrphan(db *sql.DB, account, itemDriveID, fromParentDriveID string) error {
	_, err := db.Exec(`
		INSERT INTO pack_orphans (account, item_drive_id, from_parent_drive_id, swept_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(account, item_drive_id) DO UPDATE SET
			from_parent_drive_id = excluded.from_parent_drive_id,
			swept_at             = excluded.swept_at`,
		account, itemDriveID, fromParentDriveID, now())
	return err
}

// packOrphanParent returns the live parent an orphan was swept from. Returns
// sql.ErrNoRows if the item was never recorded as an orphan.
func packOrphanParent(db *sql.DB, account, itemDriveID string) (string, error) {
	var parent string
	err := db.QueryRow(`
		SELECT from_parent_drive_id FROM pack_orphans
		WHERE account = ? AND item_drive_id = ?`, account, itemDriveID).Scan(&parent)
	return parent, err
}

// folderPermissionsFor returns the crawled permission set for the folder with
// the given Drive ID.
func folderPermissionsFor(db *sql.DB, folderDriveID string) ([]permission, error) {
	rows, err := db.Query(`
		SELECT permission_id, type, role, email_address, domain, display_name,
			allow_file_discovery, deleted
		FROM folder_permissions WHERE node_drive_id = ?`, folderDriveID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var perms []permission
	for rows.Next() {
		var (
			p          permission
			afd        sql.NullInt64
			deletedInt int
		)
		if err := rows.Scan(&p.permissionID, &p.typ, &p.role, &p.emailAddress,
			&p.domain, &p.displayName, &afd, &deletedInt); err != nil {
			return nil, err
		}
		if afd.Valid {
			p.allowFileDiscovery = sql.NullBool{Bool: afd.Int64 != 0, Valid: true}
		}
		p.deleted = deletedInt != 0
		perms = append(perms, p)
	}
	return perms, rows.Err()
}

// nodeTypeByDriveID returns the recorded type of the node with the given Drive
// ID. Returns sql.ErrNoRows if no such node was crawled.
func nodeTypeByDriveID(db *sql.DB, driveID string) (string, error) {
	var typ string
	err := db.QueryRow(`SELECT type FROM nodes WHERE drive_id = ?`, driveID).Scan(&typ)
	return typ, err
}

// exploreNode is one node in the ownership tree built by ownedAndAncestors:
// every item the target account owns, plus the ancestor folders that hold them.
type exploreNode struct {
	rowID    int64
	driveID  string
	name     string
	typ      string
	parentID sql.NullInt64
	owned    bool // owned by the target account
	children []*exploreNode
	// ownedFolders/ownedFiles count owned descendants in this node's subtree
	// (excluding the node itself), split by type. Only meaningful for folders.
	ownedFolders int
	ownedFiles   int
}

// ownedAndAncestors builds the forest of every node owned by account (matched
// against owner_email OR owner_id) together with all of its ancestor folders,
// so each owned item is shown in the context of where it lives. It loads the
// whole nodes table into memory once — the dataset is a single Drive — and
// returns the root nodes (those with no included parent), the owner's display
// name, and per-folder owned-descendant counts. It errors if the account owns
// nothing.
func ownedAndAncestors(db *sql.DB, account string) (roots []*exploreNode, displayName string, err error) {
	rows, err := db.Query(`SELECT id, drive_id, name, type, parent_id, owner_email, owner_id, owner_display_name FROM nodes`)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()

	all := make(map[int64]*exploreNode)
	for rows.Next() {
		var (
			n                   exploreNode
			email, oid, display sql.NullString
		)
		if err := rows.Scan(&n.rowID, &n.driveID, &n.name, &n.typ, &n.parentID, &email, &oid, &display); err != nil {
			return nil, "", err
		}
		n.owned = (email.Valid && email.String == account) || (oid.Valid && oid.String == account)
		if n.owned && displayName == "" && display.Valid {
			displayName = display.String
		}
		all[n.rowID] = &n
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}

	// Collect the included set: every owned node plus all of its ancestors.
	included := make(map[int64]bool)
	anyOwned := false
	for _, n := range all {
		if !n.owned {
			continue
		}
		anyOwned = true
		seen := make(map[int64]bool)
		for cur := n; cur != nil; {
			if included[cur.rowID] {
				break // ancestors already added by an earlier owned node
			}
			included[cur.rowID] = true
			if !cur.parentID.Valid || seen[cur.rowID] {
				break // reached a root, or guarded against a parent cycle
			}
			seen[cur.rowID] = true
			cur = all[cur.parentID.Int64]
		}
	}
	if !anyOwned {
		return nil, "", fmt.Errorf("no files or folders owned by %q in the database", account)
	}

	// Wire children among included nodes and collect roots (included nodes
	// whose parent is missing or not itself included).
	for id := range included {
		n := all[id]
		if n.parentID.Valid && included[n.parentID.Int64] {
			parent := all[n.parentID.Int64]
			parent.children = append(parent.children, n)
		} else {
			roots = append(roots, n)
		}
	}

	for _, r := range roots {
		sortChildren(r)
		countOwned(r)
	}
	sortNodes(roots)
	return roots, displayName, nil
}

// sortNodes orders a sibling slice folders-first, then case-insensitively by
// name, matching a typical file browser.
func sortNodes(nodes []*exploreNode) {
	sort.SliceStable(nodes, func(i, j int) bool {
		a, b := nodes[i], nodes[j]
		if (a.typ == typeFolder) != (b.typ == typeFolder) {
			return a.typ == typeFolder
		}
		return strings.ToLower(a.name) < strings.ToLower(b.name)
	})
}

// sortChildren recursively orders every node's children.
func sortChildren(n *exploreNode) {
	sortNodes(n.children)
	for _, c := range n.children {
		sortChildren(c)
	}
}

// countOwned fills in ownedFolders/ownedFiles for n as the number of owned
// descendants in its subtree, split by type, and returns those two counts so
// parents can accumulate them.
func countOwned(n *exploreNode) (folders, files int) {
	for _, c := range n.children {
		cf, cFiles := countOwned(c)
		folders += cf
		files += cFiles
		if c.owned {
			if c.typ == typeFolder {
				folders++
			} else {
				files++
			}
		}
	}
	n.ownedFolders = folders
	n.ownedFiles = files
	return folders, files
}

// subtreeDriveIDs returns the set of Drive IDs of every node within the subtree
// rooted at rootDriveID (inclusive), walking parent_id downwards. Used to scope
// whole-tree reports (e.g. the edit-access pre-check) to one subfolder.
func subtreeDriveIDs(db *sql.DB, rootDriveID string) (map[string]bool, error) {
	rows, err := db.Query(`
		WITH RECURSIVE subtree(id) AS (
			SELECT id FROM nodes WHERE drive_id = ?
			UNION ALL
			SELECT n.id FROM nodes n JOIN subtree s ON n.parent_id = s.id
		)
		SELECT drive_id FROM nodes WHERE id IN (SELECT id FROM subtree)`, rootDriveID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := make(map[string]bool)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids[id] = true
	}
	return ids, rows.Err()
}

// crawlRootDriveID returns the Drive ID of the crawl root (the node with no
// parent). Returns sql.ErrNoRows if the database is empty.
func crawlRootDriveID(db *sql.DB) (string, error) {
	var id string
	err := db.QueryRow(`SELECT drive_id FROM nodes WHERE parent_id IS NULL LIMIT 1`).Scan(&id)
	return id, err
}

// getCrawlMeta returns the recorded crawl root and session start. ok is false
// when no crawl has ever recorded metadata (a fresh or pre-crawl_meta database).
func getCrawlMeta(db *sql.DB) (rootDriveID, sessionStart string, ok bool, err error) {
	err = db.QueryRow(`SELECT root_drive_id, session_started_at FROM crawl_meta WHERE id = 1`).
		Scan(&rootDriveID, &sessionStart)
	if err == sql.ErrNoRows {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	return rootDriveID, sessionStart, true, nil
}

// setCrawlMeta records the root a crawl is snapshotting and when the session
// began, so a resume reuses the same stale-row cutoff.
func setCrawlMeta(db *sql.DB, rootDriveID, sessionStart string) error {
	_, err := db.Exec(`
		INSERT INTO crawl_meta (id, root_drive_id, session_started_at)
		VALUES (1, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			root_drive_id      = excluded.root_drive_id,
			session_started_at = excluded.session_started_at`,
		rootDriveID, sessionStart)
	return err
}

// wipeCrawlSnapshot deletes the entire crawl snapshot: every node and the
// per-folder auxiliary rows (permissions, extra parents). Migration state
// (user_migrations, pack_orphans) is left untouched. Used when the configured
// root changes and the existing snapshot describes a different tree.
func wipeCrawlSnapshot(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// nodes.parent_id self-references nodes; defer the check so deleting every
	// row at once does not trip on parent-before-child ordering.
	if _, err := tx.Exec(`PRAGMA defer_foreign_keys = ON`); err != nil {
		return err
	}
	for _, stmt := range []string{
		`DELETE FROM nodes`,
		`DELETE FROM folder_permissions`,
		`DELETE FROM extra_parents`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// deleteStaleNodes removes every node not re-observed since sessionStart (its
// crawled_at predates the current crawl session), i.e. items that no longer
// exist under the root, together with the auxiliary rows that referenced them.
// It is only safe to call after a crawl completes fully: a partial crawl has
// not re-observed every live node yet. Returns the number of nodes removed.
func deleteStaleNodes(db *sql.DB, sessionStart string) (int64, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`PRAGMA defer_foreign_keys = ON`); err != nil {
		return 0, err
	}
	// A surviving node whose first-discovered parent is going away (a
	// re-discovered multi-parent item whose original parent vanished) would
	// dangle; detach it to a root rather than block the delete.
	if _, err := tx.Exec(`
		UPDATE nodes SET parent_id = NULL
		WHERE crawled_at >= ?
		  AND parent_id IN (SELECT id FROM nodes WHERE crawled_at < ?)`,
		sessionStart, sessionStart); err != nil {
		return 0, err
	}
	res, err := tx.Exec(`DELETE FROM nodes WHERE crawled_at < ?`, sessionStart)
	if err != nil {
		return 0, err
	}
	removed, _ := res.RowsAffected()
	if err := pruneOrphanedAuxRows(tx); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return removed, nil
}

// pruneOrphanedAuxRows deletes folder_permissions and extra_parents rows whose
// referenced node no longer exists. These tables key on drive_id with no foreign
// key, so they must be swept by hand after nodes are deleted.
func pruneOrphanedAuxRows(tx *sql.Tx) error {
	if _, err := tx.Exec(
		`DELETE FROM folder_permissions WHERE node_drive_id NOT IN (SELECT drive_id FROM nodes)`); err != nil {
		return err
	}
	_, err := tx.Exec(`
		DELETE FROM extra_parents
		WHERE node_drive_id NOT IN (SELECT drive_id FROM nodes)
		   OR parent_drive_id NOT IN (SELECT drive_id FROM nodes)`)
	return err
}

// deleteStaleNodesUnder is the subtree-scoped counterpart of deleteStaleNodes,
// used by a --folder re-index: it removes only the nodes within the subtree
// rooted at subfolderDriveID that were not re-observed since cutoff, leaving the
// rest of the snapshot alone. Like deleteStaleNodes it is only safe after the
// subtree has been fully re-listed. Returns the number of nodes removed.
func deleteStaleNodesUnder(db *sql.DB, subfolderDriveID, cutoff string) (int64, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`PRAGMA defer_foreign_keys = ON`); err != nil {
		return 0, err
	}
	// Snapshot the subtree membership before mutating anything. Detaching a
	// survivor below (next statement) severs its parent link, which would
	// otherwise hide still-deeper stale rows from a recursive walk recomputed
	// after the detach. A rollback drops this temp table with the rest of the tx.
	if _, err := tx.Exec(`
		CREATE TEMP TABLE scoped_subtree AS
		WITH RECURSIVE subtree(id) AS (
			SELECT id FROM nodes WHERE drive_id = ?
			UNION ALL
			SELECT n.id FROM nodes n JOIN subtree s ON n.parent_id = s.id
		)
		SELECT id FROM subtree`, subfolderDriveID); err != nil {
		return 0, err
	}
	// A surviving node whose first-discovered parent (within the subtree) is
	// going away — a re-discovered multi-parent item whose original parent
	// vanished — would dangle; detach it to a root rather than block the delete.
	if _, err := tx.Exec(`
		UPDATE nodes SET parent_id = NULL
		WHERE id IN (SELECT id FROM scoped_subtree)
		  AND crawled_at >= ?
		  AND parent_id IN (
			SELECT s.id FROM scoped_subtree s JOIN nodes n ON n.id = s.id
			WHERE n.crawled_at < ?
		  )`, cutoff, cutoff); err != nil {
		return 0, err
	}
	res, err := tx.Exec(
		`DELETE FROM nodes WHERE id IN (SELECT id FROM scoped_subtree) AND crawled_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	removed, _ := res.RowsAffected()
	if _, err := tx.Exec(`DROP TABLE scoped_subtree`); err != nil {
		return 0, err
	}
	if err := pruneOrphanedAuxRows(tx); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return removed, nil
}

// updateSubtreeOwner overwrites the owner fields for every node in the subtree
// rooted at rootDriveID (inclusive) that the database currently records as owned
// by fromAccount (matched against owner_email OR owner_id). It returns the number
// of rows updated.
//
// Called after unpack moves an item back from a Container that was dragged into
// the shared drive: that drag flips ownership of everything physically inside the
// Container to the org, so we can record the new owner for the whole restored
// subtree without a per-file Drive lookup. Scoping to fromAccount leaves the
// third-party items that were parked in the Stash (nested elsewhere in the same
// crawled subtree) untouched — those never entered the Container, so their
// ownership did not change and they are restored separately.
func updateSubtreeOwner(db *sql.DB, rootDriveID, fromAccount, email, permissionID, displayName string) (int64, error) {
	res, err := db.Exec(`
		WITH RECURSIVE subtree(id) AS (
			SELECT id FROM nodes WHERE drive_id = ?
			UNION ALL
			SELECT n.id FROM nodes n JOIN subtree s ON n.parent_id = s.id
		)
		UPDATE nodes SET owner_email = ?, owner_id = ?, owner_display_name = ?
		WHERE id IN (SELECT id FROM subtree)
		  AND (owner_email = ? OR owner_id = ?)`,
		rootDriveID, email, permissionID, displayName, fromAccount, fromAccount)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// originalParentDriveID returns the Drive ID of the folder the given file was
// crawled under. Returns sql.ErrNoRows if the file is not in the database or
// has no recorded parent (i.e. it is the crawl root).
func originalParentDriveID(db *sql.DB, driveID string) (string, error) {
	var parentDriveID string
	err := db.QueryRow(`
		SELECT p.drive_id
		FROM nodes n
		JOIN nodes p ON p.id = n.parent_id
		WHERE n.drive_id = ?`, driveID).Scan(&parentDriveID)
	return parentDriveID, err
}

// nodePath returns the names from the crawl root down to the node with the
// given Drive ID, walking parent_id upwards. For a shortcut this is where the
// shortcut row lives in our crawled tree, not where its target lives.
func nodePath(db *sql.DB, driveID string) ([]string, error) {
	var (
		name   string
		parent sql.NullInt64
	)
	err := db.QueryRow(`SELECT name, parent_id FROM nodes WHERE drive_id = ?`, driveID).Scan(&name, &parent)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("drive id %s not found in database", driveID)
	}
	if err != nil {
		return nil, err
	}
	segments := []string{name}
	seen := make(map[int64]bool)
	for parent.Valid {
		id := parent.Int64
		if seen[id] {
			return nil, fmt.Errorf("cycle detected in parent chain at row %d", id)
		}
		seen[id] = true
		if err := db.QueryRow(`SELECT name, parent_id FROM nodes WHERE id = ?`, id).Scan(&name, &parent); err != nil {
			return nil, fmt.Errorf("walking parent chain (row %d): %w", id, err)
		}
		segments = append(segments, name)
	}
	for i, j := 0, len(segments)-1; i < j; i, j = i+1, j-1 {
		segments[i], segments[j] = segments[j], segments[i]
	}
	return segments, nil
}
