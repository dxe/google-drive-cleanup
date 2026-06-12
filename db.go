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
	crawled_at         TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_nodes_owner_email   ON nodes(owner_email);
CREATE INDEX IF NOT EXISTS idx_nodes_owner_id      ON nodes(owner_id);
CREATE INDEX IF NOT EXISTS idx_nodes_parent_id     ON nodes(parent_id);
CREATE INDEX IF NOT EXISTS idx_nodes_drive_id      ON nodes(drive_id);
CREATE INDEX IF NOT EXISTS idx_nodes_children_done ON nodes(children_done);

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
			owner_display_name, parent_id, shortcut_target_id, children_done, crawled_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?)
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
			crawled_at         = excluded.crawled_at
		RETURNING id`,
		n.driveID, n.name, n.typ, n.mimeType, n.ownerEmail, n.ownerID,
		n.ownerDisplay, n.parentID, n.shortcutTarget, now(),
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

// pendingFolders returns every folder whose children have not been fully
// listed yet — the resumable work queue.
func pendingFolders(db *sql.DB) ([]folderRef, error) {
	rows, err := db.Query(fmt.Sprintf(
		`SELECT id, drive_id, name FROM nodes WHERE type = '%s' AND children_done = 0 ORDER BY id`, typeFolder))
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

func countPendingFolders(db *sql.DB) (int, error) {
	var n int
	err := db.QueryRow(fmt.Sprintf(
		`SELECT COUNT(*) FROM nodes WHERE type = '%s' AND children_done = 0`, typeFolder)).Scan(&n)
	return n, err
}

func resetChildrenDone(db *sql.DB) error {
	_, err := db.Exec(fmt.Sprintf(`UPDATE nodes SET children_done = 0 WHERE type = '%s'`, typeFolder))
	return err
}

func recordExtraParent(tx *sql.Tx, nodeDriveID, parentDriveID string) error {
	_, err := tx.Exec(
		`INSERT OR IGNORE INTO extra_parents (node_drive_id, parent_drive_id, observed_at) VALUES (?, ?, ?)`,
		nodeDriveID, parentDriveID, now())
	return err
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

type ownerCount struct {
	email       sql.NullString
	ownerID     sql.NullString
	displayName sql.NullString
	folderCount int64
	fileCount   int64
	total       int64
}

// ownersReport counts nodes per owner, split into folders and files, grouped by
// owner_email, falling back to owner_id when the email is missing, and to a
// single "(unknown)" bucket when both are NULL. Rows are ordered by file count
// descending.
func ownersReport(db *sql.DB) ([]ownerCount, error) {
	rows, err := db.Query(fmt.Sprintf(`
		SELECT MAX(owner_email) AS email, MAX(owner_id) AS oid, MAX(owner_display_name),
			SUM(type = '%[1]s') AS folders,
			SUM(type <> '%[1]s') AS files,
			COUNT(*) AS total
		FROM nodes
		GROUP BY COALESCE(owner_email, owner_id, '(unknown)')
		ORDER BY files DESC, total DESC, (email IS NULL AND oid IS NULL), COALESCE(email, oid)`, typeFolder))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var counts []ownerCount
	for rows.Next() {
		var oc ownerCount
		if err := rows.Scan(&oc.email, &oc.ownerID, &oc.displayName, &oc.folderCount, &oc.fileCount, &oc.total); err != nil {
			return nil, err
		}
		counts = append(counts, oc)
	}
	return counts, rows.Err()
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
