package main

import (
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/sqlite"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	_ "modernc.org/sqlite" // pure-Go SQLite driver, no cgo
)

// migrationsFS holds the golang-migrate SQL files. They are the single source
// of truth for the schema; see CLAUDE.md for how to add one.
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

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
	if err := migrateDB(db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// migrateDB brings db up to the latest schema by applying any pending
// golang-migrate migrations from migrationsFS. It is idempotent: a database
// already at the latest version is left untouched. Migration bookkeeping lives
// in the schema_migrations table that golang-migrate manages.
func migrateDB(db *sql.DB) error {
	src, err := iofs.New(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("loading migrations: %w", err)
	}
	driver, err := sqlite.WithInstance(db, &sqlite.Config{})
	if err != nil {
		return fmt.Errorf("migration driver: %w", err)
	}
	m, err := migrate.NewWithInstance("iofs", src, "sqlite", driver)
	if err != nil {
		return fmt.Errorf("migration init: %w", err)
	}
	// Do not m.Close(): that would close the *sql.DB we return to the caller.
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("applying migrations: %w", err)
	}
	return nil
}

func now() string {
	return time.Now().UTC().Format(time.RFC3339)
}

// boolToInt maps a Go bool to the 0/1 SQLite stores in an INTEGER flag column.
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
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
	// lastModified is the estimated time of the node's last content change,
	// recorded by every `crawl`. When invalid (only if the crawl could not
	// determine a time), upsertNode leaves any existing value untouched (see the
	// COALESCE below) rather than clearing it.
	lastModified sql.NullString
	// ownerIsTransfer reports whether ownerEmail is one of the configured
	// ownership-transfer accounts. It decides how upsertNode updates the
	// original_owner_* columns: when false the original owner is refreshed to this
	// (real) owner; when true the previously recorded original owner is kept, so
	// once a file lands on a transfer account its original owner is frozen at the
	// last real owner. On insert the original owner is seeded to this owner
	// regardless (there is no prior value to preserve).
	ownerIsTransfer bool
	// ownerIsManualTransfer reports whether ownerEmail is one of the configured
	// manual-ownership-transfer accounts. When true the row's
	// manual_transfer_performed flag is set (and never cleared thereafter).
	ownerIsManualTransfer bool
}

// upsertNode inserts n or refreshes the existing row with the same drive_id.
// children_done never regresses (it can only go up) and name, owner, mime_type
// and crawled_at are refreshed to the latest values. It returns the row id plus
// what was stored before the call (existed, prevParent, prevDone) so the caller
// can detect re-discovery under a different parent and skip folders whose
// children are already listed.
//
// setParent decides what happens to parent_id when the row already exists. When
// true (a node Drive reports under exactly one parent — the folder we just
// listed it under), parent_id is updated to n.parentID, so a node that moved
// between crawls is reparented to where it lives now. When false the existing
// parent_id is preserved (COALESCE): folders fetched directly (the crawl root, a
// scoped re-index root) pass no parent and must keep the one they already have,
// and a genuinely multi-parent node keeps its first-discovered parent while the
// caller records the others in extra_parents.
func upsertNode(tx *sql.Tx, n node, setParent bool) (rowID int64, existed bool, prevParent sql.NullInt64, prevDone bool, err error) {
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

	// original_owner_* seed to this owner on insert (excluded.*); on update they
	// are refreshed to this owner unless it is a transfer account, in which case
	// the previously recorded original owner is preserved (COALESCE) so the
	// original owner freezes at the last real owner before the transfer.
	err = tx.QueryRow(`
		INSERT INTO nodes (drive_id, name, type, mime_type, owner_email, owner_id,
			owner_display_name, parent_id, shortcut_target_id, children_done, can_edit, crawled_at,
			last_modified, original_owner_email, original_owner_id, original_owner_display_name,
			manual_transfer_performed)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(drive_id) DO UPDATE SET
			name               = excluded.name,
			type               = excluded.type,
			mime_type          = excluded.mime_type,
			owner_email        = excluded.owner_email,
			owner_id           = excluded.owner_id,
			owner_display_name = excluded.owner_display_name,
			parent_id          = CASE WHEN ? THEN COALESCE(excluded.parent_id, nodes.parent_id)
			                          ELSE COALESCE(nodes.parent_id, excluded.parent_id) END,
			shortcut_target_id = excluded.shortcut_target_id,
			children_done      = MAX(nodes.children_done, excluded.children_done),
			can_edit           = excluded.can_edit,
			crawled_at         = excluded.crawled_at,
			-- Preserve any previously recorded last_modified in the rare case this
			-- crawl could not compute one, rather than wiping it.
			last_modified      = COALESCE(excluded.last_modified, nodes.last_modified),
			-- When this owner is a transfer account, keep the original owner already
			-- on record (the last real owner); otherwise refresh it to this owner.
			-- The email/id/display columns move together so they always describe one
			-- person.
			original_owner_email        = CASE WHEN ? THEN COALESCE(nodes.original_owner_email, excluded.original_owner_email)
			                                   ELSE COALESCE(excluded.original_owner_email, nodes.original_owner_email) END,
			original_owner_id           = CASE WHEN ? THEN COALESCE(nodes.original_owner_id, excluded.original_owner_id)
			                                   ELSE COALESCE(excluded.original_owner_id, nodes.original_owner_id) END,
			original_owner_display_name = CASE WHEN ? THEN COALESCE(nodes.original_owner_display_name, excluded.original_owner_display_name)
			                                   ELSE COALESCE(excluded.original_owner_display_name, nodes.original_owner_display_name) END,
			-- Once a manual transfer has been seen the flag stays set forever.
			manual_transfer_performed   = MAX(nodes.manual_transfer_performed, excluded.manual_transfer_performed)
		RETURNING id`,
		n.driveID, n.name, n.typ, n.mimeType, n.ownerEmail, n.ownerID,
		n.ownerDisplay, n.parentID, n.shortcutTarget, n.canEdit, now(), n.lastModified,
		n.ownerEmail, n.ownerID, n.ownerDisplay, boolToInt(n.ownerIsManualTransfer),
		setParent, n.ownerIsTransfer, n.ownerIsTransfer, n.ownerIsTransfer,
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
// A node whose parent IS the crawl root also counts as a root, even when the
// account owns the crawl root: the crawl root is the migration boundary and is
// never moved, so an owned item directly under it is the top of an owned
// subtree that must move. Without this, a user who owns the crawl root has no
// owned roots at all (every owned item's parent chain runs up to the owned
// root), Phase A moves nothing, and pack only rescues the tree item-by-item as
// flattened Phase C stragglers. The crawl root is the unique node with a NULL
// parent_id, so p.parent_id IS NULL identifies "parent is the crawl root".
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
		  AND (
		    (p.owner_email IS NOT ? AND p.owner_id IS NOT ?)
		    OR p.parent_id IS NULL
		  )
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
		    OR p.parent_id IS NULL
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

// manualTransferPerformedByDriveID returns, for each of driveIDs already present
// in the nodes table, whether its persisted manual_transfer_performed flag is
// set. IDs not yet in the table (a file seen for the first time this crawl) are
// simply absent from the map, which callers treat as false. The flag is sticky,
// so a file manually transferred in a past crawl keeps it even after moving to a
// non-manual account — which is exactly when its modifiedTime stays bogus and
// last_modified must be read from revisions instead.
func manualTransferPerformedByDriveID(db *sql.DB, driveIDs []string) (map[string]bool, error) {
	flags := make(map[string]bool, len(driveIDs))
	if len(driveIDs) == 0 {
		return flags, nil
	}
	placeholders := make([]string, len(driveIDs))
	args := make([]any, len(driveIDs))
	for i, id := range driveIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	rows, err := db.Query(
		`SELECT drive_id, manual_transfer_performed FROM nodes WHERE drive_id IN (`+
			strings.Join(placeholders, ",")+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var flag int
		if err := rows.Scan(&id, &flag); err != nil {
			return nil, err
		}
		flags[id] = flag != 0
	}
	return flags, rows.Err()
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
	// The node's recorded owner, used to build per-owner breakdowns.
	ownerEmail, ownerID, ownerDisplay sql.NullString
	// decision is the node's keep/delete state as marked in the review UI
	// (decisionNone when undecided).
	decision string
	// subtree tallies decisions over the owned items in this node's subtree, the
	// node itself included when it is owned. Drives the red/green/yellow
	// coloring. See computeExploreDecisions.
	subtree decisionCounts
	// ownedFolders/ownedFiles count owned descendants in this node's subtree
	// (excluding the node itself), split by type. Only meaningful for folders.
	ownedFolders int
	ownedFiles   int
	// breakdown, when set (currently only for the all-external report), is the
	// per-owner count of owned items in this folder's subtree, excluding the
	// folder itself — the same population as ownedFolders/ownedFiles.
	breakdown []personCount
}

// personCount is one row of a folder's per-owner breakdown: how many folders and
// files a single owner owns within that folder's subtree, and how those items
// are split across keep / delete / undecided.
type personCount struct {
	Label   string `json:"label"`
	Folders int    `json:"folders"`
	Files   int    `json:"files"`
	// decisionCounts is embedded (and so flattens into the JSON the popover
	// script reads) and covers the same items as Folders+Files.
	decisionCounts
}

// ownerMatcher decides, for a node's recorded owner, whether the node counts as
// "owned" for an ownership tree. display is the node's owner_display_name; a
// matcher may return a display name to surface in the report header.
type ownerMatcher func(email, oid, display sql.NullString) (owned bool, displayName string)

// ownedAndAncestors builds the forest of every node owned by account (matched
// against owner_email OR owner_id) together with all of its ancestor folders,
// so each owned item is shown in the context of where it lives. It returns the
// root nodes, the owner's display name, and per-folder owned-descendant counts,
// and errors if the account owns nothing.
func ownedAndAncestors(db *sql.DB, account string) (roots []*exploreNode, displayName string, err error) {
	roots, displayName, err = ownedAndAncestorsMatching(db, func(email, oid, display sql.NullString) (bool, string) {
		owned := (email.Valid && email.String == account) || (oid.Valid && oid.String == account)
		if owned && display.Valid {
			return true, display.String
		}
		return owned, ""
	})
	if err != nil {
		return nil, "", err
	}
	if len(roots) == 0 {
		return nil, "", fmt.Errorf("no files or folders owned by %q in the database", account)
	}
	return roots, displayName, nil
}

// externalOwnedAndAncestors builds the ownership forest of every node whose
// owner is external — has a recorded owner (email or id) that is not on one of
// internalDomains — combining every external account into one tree. Nodes with
// no recorded owner are excluded. Returns empty roots if nothing is externally
// owned.
func externalOwnedAndAncestors(db *sql.DB, internalDomains []string) (roots []*exploreNode, err error) {
	roots, _, err = ownedAndAncestorsMatching(db, func(email, oid, display sql.NullString) (bool, string) {
		if !email.Valid && !oid.Valid {
			return false, "" // no recorded owner
		}
		return !isInternalEmail(email, internalDomains), ""
	})
	return roots, err
}

// ownedAndAncestorsMatching builds the forest of every node the matcher marks as
// owned together with all of its ancestor folders, so each owned item is shown
// in the context of where it lives. It loads the whole nodes table into memory
// once — the dataset is a single Drive — and returns the root nodes (those with
// no included parent), the first display name the matcher surfaces, and
// per-folder owned-descendant counts. Roots is empty when nothing matches.
func ownedAndAncestorsMatching(db *sql.DB, match ownerMatcher) (roots []*exploreNode, displayName string, err error) {
	rows, err := db.Query(`SELECT id, drive_id, name, type, parent_id, owner_email, owner_id, owner_display_name,
		decision FROM nodes`)
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
		if err := rows.Scan(&n.rowID, &n.driveID, &n.name, &n.typ, &n.parentID, &email, &oid, &display,
			&n.decision); err != nil {
			return nil, "", err
		}
		n.ownerEmail, n.ownerID, n.ownerDisplay = email, oid, display
		var dn string
		n.owned, dn = match(email, oid, display)
		if n.owned && displayName == "" && dn != "" {
			displayName = dn
		}
		all[n.rowID] = &n
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}

	// Collect the included set: every owned node plus all of its ancestors.
	included := make(map[int64]bool)
	for _, n := range all {
		if !n.owned {
			continue
		}
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
		computeExploreDecisions(r)
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

// computeExploreDecisions fills in each node's subtree decision tally bottom-up.
// Only *owned* nodes are counted — the ancestor folders in the tree belong to
// somebody else and are there for context — so a folder's tally covers exactly
// the items its ownedFolders/ownedFiles counts and its per-owner breakdown do.
// The coloring derived from these tallies therefore describes the owned items
// the report shows, not the whole Drive folder.
func computeExploreDecisions(n *exploreNode) decisionCounts {
	var c decisionCounts
	if n.owned {
		c.add(n.decision, 1)
	}
	for _, ch := range n.children {
		c.addAll(computeExploreDecisions(ch))
	}
	n.subtree = c
	return c
}

// buildOwnerBreakdowns fills each folder node's breakdown with the per-owner
// counts of the owned items in its subtree (excluding the folder itself), split
// by type and by keep/delete/undecided decision, mirroring what
// `owners --folder <that folder>` reports but restricted to the owned set the
// tree was built from (external accounts, for the all-external report). Rows are
// ordered files-desc, then total-desc, then label, matching the owners report.
func buildOwnerBreakdowns(roots []*exploreNode) {
	for _, r := range roots {
		accumulateOwners(r)
	}
}

// accumulateOwners returns the per-owner counts for the subtree rooted at n,
// including n's own owned contribution, and — for folders — records the
// descendants-only breakdown on n.breakdown along the way.
func accumulateOwners(n *exploreNode) map[string]*personCount {
	agg := make(map[string]*personCount)
	add := func(key, label string, folder bool, decision string) {
		e := agg[key]
		if e == nil {
			e = &personCount{Label: label}
			agg[key] = e
		}
		if folder {
			e.Folders++
		} else {
			e.Files++
		}
		e.add(decision, 1)
	}
	for _, c := range n.children {
		for key, v := range accumulateOwners(c) {
			e := agg[key]
			if e == nil {
				e = &personCount{Label: v.Label}
				agg[key] = e
			}
			e.Folders += v.Folders
			e.Files += v.Files
			e.addAll(v.decisionCounts)
		}
	}
	// At this point agg holds descendants only, which is exactly the folder's
	// breakdown (excluding itself).
	if n.typ == typeFolder && len(agg) > 0 {
		n.breakdown = sortedBreakdown(agg)
	}
	if n.owned {
		key, label := ownerKeyLabel(n)
		add(key, label, n.typ == typeFolder, n.decision)
	}
	return agg
}

// ownerKeyLabel returns a stable grouping key and a human-readable label for a
// node's owner, matching ownerLabel's formatting (email, else id, with the
// display name in parentheses when present).
func ownerKeyLabel(n *exploreNode) (key, label string) {
	switch {
	case n.ownerEmail.Valid:
		key = n.ownerEmail.String
		label = n.ownerEmail.String
		if n.ownerDisplay.Valid {
			label = fmt.Sprintf("%s (%s)", n.ownerEmail.String, n.ownerDisplay.String)
		}
	case n.ownerID.Valid:
		key = "id:" + n.ownerID.String
		label = "id:" + n.ownerID.String
		if n.ownerDisplay.Valid {
			label = fmt.Sprintf("id:%s (%s)", n.ownerID.String, n.ownerDisplay.String)
		}
	}
	return key, label
}

// sortedBreakdown flattens an owner-count map into the owners-report order:
// files desc, then total desc, then label ascending.
func sortedBreakdown(agg map[string]*personCount) []personCount {
	out := make([]personCount, 0, len(agg))
	for _, v := range agg {
		out = append(out, *v)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Files != b.Files {
			return a.Files > b.Files
		}
		if a.Folders+a.Files != b.Folders+b.Files {
			return a.Folders+a.Files > b.Folders+b.Files
		}
		return a.Label < b.Label
	})
	return out
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

// crawlRootDriveID returns the Drive ID of the crawl root. The authoritative
// record is crawl_meta (the tree the snapshot describes); a database from
// before crawl_meta existed falls back to the oldest parentless node. "The"
// parentless node alone is no longer authoritative because the archive tree,
// when configured, is crawled as a second parentless root.
func crawlRootDriveID(db *sql.DB) (string, error) {
	var id string
	err := db.QueryRow(`SELECT root_drive_id FROM crawl_meta WHERE id = 1`).Scan(&id)
	if err == nil {
		return id, nil
	}
	if err != sql.ErrNoRows {
		return "", err
	}
	err = db.QueryRow(`SELECT drive_id FROM nodes WHERE parent_id IS NULL ORDER BY id LIMIT 1`).Scan(&id)
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

// Values recorded in pruned_nodes.pruned_by: which stale-row sweep removed the
// row (a whole-tree crawl, or a --folder re-index).
const (
	prunedByCrawl       = "crawl"
	prunedByCrawlFolder = "crawl_folder"
)

// backupNodes copies into pruned_nodes every nodes row matching whereSQL — an
// expression over the alias n, with its own placeholders bound from args —
// before the caller deletes them. Nothing reads pruned_nodes; it is a safety net
// for a prune that turns out to be wrong (a node whose row carries a review
// decision or archive bookkeeping that Drive cannot supply again).
//
// Call it as the first statement of the deleting transaction, before any row is
// detached or removed, so the copy records the tree as the crawl found it.
func backupNodes(tx *sql.Tx, prunedBy, whereSQL string, args ...any) error {
	_, err := tx.Exec(`
		INSERT INTO pruned_nodes (
			pruned_at, pruned_by,
			node_id, drive_id, name, type, mime_type,
			owner_email, owner_id, owner_display_name,
			parent_id, parent_drive_id, shortcut_target_id, children_done, can_edit,
			crawled_at, decision, last_modified,
			original_owner_id, original_owner_display_name, original_owner_email,
			manual_transfer_performed, original_parent_drive_id, archive_folder_drive_id,
			delete_skipped)
		SELECT
			?, ?,
			n.id, n.drive_id, n.name, n.type, n.mime_type,
			n.owner_email, n.owner_id, n.owner_display_name,
			n.parent_id, (SELECT p.drive_id FROM nodes p WHERE p.id = n.parent_id),
			n.shortcut_target_id, n.children_done, n.can_edit,
			n.crawled_at, n.decision, n.last_modified,
			n.original_owner_id, n.original_owner_display_name, n.original_owner_email,
			n.manual_transfer_performed, n.original_parent_drive_id, n.archive_folder_drive_id,
			n.delete_skipped
		FROM nodes n WHERE `+whereSQL,
		append([]any{now(), prunedBy}, args...)...)
	if err != nil {
		return fmt.Errorf("backing up nodes about to be pruned: %w", err)
	}
	return nil
}

// deleteStaleNodes removes every node not re-observed since sessionStart (its
// crawled_at predates the current crawl session), i.e. items that no longer
// exist under the root, together with the auxiliary rows that referenced them.
// Each removed row is first copied to pruned_nodes (see backupNodes) so a wrong
// prune is recoverable; the auxiliary rows are not backed up. It is only safe to
// call after a crawl completes fully: a partial crawl has not re-observed every
// live node yet. Returns the number of nodes removed.
func deleteStaleNodes(db *sql.DB, sessionStart string) (int64, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`PRAGMA defer_foreign_keys = ON`); err != nil {
		return 0, err
	}
	// Back up before the detach below rewrites any parent_id, so pruned_nodes
	// records the parentage the crawl actually saw.
	if err := backupNodes(tx, prunedByCrawl,
		`n.crawled_at < ? AND n.original_parent_drive_id IS NULL`, sessionStart); err != nil {
		return 0, err
	}
	// A surviving node whose first-discovered parent is going away (a
	// re-discovered multi-parent item whose original parent vanished) would
	// dangle; detach it to a root rather than block the delete. Archived rows
	// (original_parent_drive_id set) survive the sweep below even when stale,
	// so they too must be detached when their parent is pruned.
	if _, err := tx.Exec(`
		UPDATE nodes SET parent_id = NULL
		WHERE (crawled_at >= ? OR original_parent_drive_id IS NOT NULL)
		  AND parent_id IN (SELECT id FROM nodes
		                    WHERE crawled_at < ? AND original_parent_drive_id IS NULL)`,
		sessionStart, sessionStart); err != nil {
		return 0, err
	}
	// Archived rows are exempt from pruning: when the archive tree is crawled
	// (the normal case) they are re-observed and fresh anyway, but if the
	// archive section was removed or misconfigured this keeps a full crawl from
	// destroying the archival records that restore and delete depend on. A row
	// whose item truly vanished from Drive self-heals later: delete treats a
	// 404 as already deleted and removes the row.
	res, err := tx.Exec(
		`DELETE FROM nodes WHERE crawled_at < ? AND original_parent_drive_id IS NULL`, sessionStart)
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
// rest of the snapshot alone. Removed rows are copied to pruned_nodes first,
// exactly as in deleteStaleNodes. Like deleteStaleNodes it is only safe after
// the subtree has been fully re-listed. Returns the number of nodes removed.
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
	// Back up before the detach below rewrites any parent_id, so pruned_nodes
	// records the parentage the re-index actually saw.
	if err := backupNodes(tx, prunedByCrawlFolder,
		`n.id IN (SELECT id FROM scoped_subtree)
		   AND n.crawled_at < ? AND n.original_parent_drive_id IS NULL`, cutoff); err != nil {
		return 0, err
	}
	// A surviving node whose first-discovered parent (within the subtree) is
	// going away — a re-discovered multi-parent item whose original parent
	// vanished — would dangle; detach it to a root rather than block the delete.
	// Archived rows are kept even when stale (see deleteStaleNodes), so they are
	// detached too when their parent is pruned.
	if _, err := tx.Exec(`
		UPDATE nodes SET parent_id = NULL
		WHERE id IN (SELECT id FROM scoped_subtree)
		  AND (crawled_at >= ? OR original_parent_drive_id IS NOT NULL)
		  AND parent_id IN (
			SELECT s.id FROM scoped_subtree s JOIN nodes n ON n.id = s.id
			WHERE n.crawled_at < ? AND n.original_parent_drive_id IS NULL
		  )`, cutoff, cutoff); err != nil {
		return 0, err
	}
	// Archived rows are exempt, mirroring deleteStaleNodes.
	res, err := tx.Exec(`
		DELETE FROM nodes WHERE id IN (SELECT id FROM scoped_subtree)
		  AND crawled_at < ? AND original_parent_drive_id IS NULL`, cutoff)
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

// setNodeOwner overwrites one node's current-owner columns. Called by archive
// after it takes ownership of an internally-owned file by routing it through the
// dropoff shared drive, so the snapshot names the account that owns the archived
// file now. The original_owner_* columns are deliberately untouched: they record
// who held the file when the crawl discovered it and are never updated after.
func setNodeOwner(db *sql.DB, driveID, email, permissionID, displayName string) error {
	_, err := db.Exec(`
		UPDATE nodes SET owner_email = ?, owner_id = ?, owner_display_name = ?
		WHERE drive_id = ?`, email, permissionID, displayName, driveID)
	return err
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

// --- archive / delete / restore persistence ---

// archiveTarget is one row the archive and delete commands act on: a
// delete-marked item to archive, or an archived item to permanently delete.
type archiveTarget struct {
	rowID   int64
	driveID string
	name    string
	typ     string
	// parentDriveID is the recorded parent's Drive ID: the original folder for
	// an unarchived item, the archive replica for an archived one. Invalid when
	// the parent row was pruned (a detached row).
	parentDriveID sql.NullString
	ownerEmail    sql.NullString
	ownerID       sql.NullString
	canEdit       bool
	// originalParent is original_parent_drive_id — set once the item is archived.
	originalParent sql.NullString
	// archiveFolder is the folder's cached replica Drive ID (folders only).
	archiveFolder sql.NullString
	deleteSkipped bool
	depth         int
}

// archiveTargetQuery is the shared SELECT for archiveTarget rows. depths ranks
// every node by distance from its root (the crawl root or the archive root),
// used to order folders deepest-first so descendants are handled before
// ancestors. subtree scopes the result to one folder's subtree; when
// subfolder is empty it degenerates to the whole table.
const archiveTargetQuery = `
	WITH RECURSIVE depths(id, depth) AS (
		SELECT id, 0 FROM nodes WHERE parent_id IS NULL
		UNION ALL
		SELECT n.id, d.depth + 1 FROM nodes n JOIN depths d ON n.parent_id = d.id
	), subtree(id) AS (
		SELECT id FROM nodes WHERE (?1 = '' AND parent_id IS NULL) OR drive_id = ?1
		UNION ALL
		SELECT n.id FROM nodes n JOIN subtree s ON n.parent_id = s.id
	)
	SELECT n.id, n.drive_id, n.name, n.type, p.drive_id, n.owner_email, n.owner_id,
	       n.can_edit, n.original_parent_drive_id, n.archive_folder_drive_id,
	       n.delete_skipped, COALESCE(d.depth, 0)
	FROM nodes n
	LEFT JOIN nodes p ON p.id = n.parent_id
	LEFT JOIN depths d ON d.id = n.id
	WHERE n.id IN (SELECT id FROM subtree)`

func scanArchiveTargets(rows *sql.Rows) ([]archiveTarget, error) {
	defer rows.Close()
	var out []archiveTarget
	for rows.Next() {
		var (
			t          archiveTarget
			canEdit    int
			skippedInt int
		)
		if err := rows.Scan(&t.rowID, &t.driveID, &t.name, &t.typ, &t.parentDriveID,
			&t.ownerEmail, &t.ownerID, &canEdit, &t.originalParent, &t.archiveFolder,
			&skippedInt, &t.depth); err != nil {
			return nil, err
		}
		t.canEdit = canEdit != 0
		t.deleteSkipped = skippedInt != 0
		out = append(out, t)
	}
	return out, rows.Err()
}

// archivableFiles returns every delete-marked, not-yet-archived file (any
// non-folder type), optionally scoped to the subtree rooted at subfolder.
// Rows without a parent (detached survivors) are excluded — archiving needs
// the recorded parent to build the replica path.
func archivableFiles(db *sql.DB, subfolder string) ([]archiveTarget, error) {
	rows, err := db.Query(archiveTargetQuery+`
		  AND n.decision = ? AND n.type <> ? AND n.original_parent_drive_id IS NULL
		  AND p.drive_id IS NOT NULL
		ORDER BY n.id`, subfolder, decisionDelete, typeFolder)
	if err != nil {
		return nil, err
	}
	return scanArchiveTargets(rows)
}

// archivableFolders returns every delete-marked, not-yet-archived folder,
// deepest-first so descendants are archived before their ancestors. The crawl
// and archive roots have no parent row and so are never included.
func archivableFolders(db *sql.DB, subfolder string) ([]archiveTarget, error) {
	rows, err := db.Query(archiveTargetQuery+`
		  AND n.decision = ? AND n.type = ? AND n.original_parent_drive_id IS NULL
		  AND p.drive_id IS NOT NULL
		ORDER BY depth DESC, n.id`, subfolder, decisionDelete, typeFolder)
	if err != nil {
		return nil, err
	}
	return scanArchiveTargets(rows)
}

// archivedForDeletion returns every archived item still marked delete — the
// delete command's work list — split into files and folders, folders deepest-
// first. subfolder (optional) scopes to a subtree of the archive tree; archived
// items are reparented under their replica rows, so the subtree walk finds them.
func archivedForDeletion(db *sql.DB, subfolder string) (files, folders []archiveTarget, err error) {
	rows, err := db.Query(archiveTargetQuery+`
		  AND n.decision = ? AND n.original_parent_drive_id IS NOT NULL
		ORDER BY depth DESC, n.id`, subfolder, decisionDelete)
	if err != nil {
		return nil, nil, err
	}
	all, err := scanArchiveTargets(rows)
	if err != nil {
		return nil, nil, err
	}
	for _, t := range all {
		if t.typ == typeFolder {
			folders = append(folders, t)
		} else {
			files = append(files, t)
		}
	}
	return files, folders, nil
}

// foldersWithReplicas returns every folder with a cached archive replica,
// deepest-first — the order replicas must be pruned in, since a child folder's
// replica nests inside its parent's.
func foldersWithReplicas(db *sql.DB) ([]archiveTarget, error) {
	rows, err := db.Query(archiveTargetQuery+`
		  AND n.archive_folder_drive_id IS NOT NULL
		ORDER BY depth DESC, n.id`, "")
	if err != nil {
		return nil, err
	}
	return scanArchiveTargets(rows)
}

// folderChainToRoot returns the folder rows from just below the root down to
// folderDriveID (inclusive) — the ancestors whose replicas must exist before a
// child of folderDriveID can be archived. Empty when folderDriveID is a root
// itself (its replica is the archive root). Cycle-guarded like nodePath.
func folderChainToRoot(db *sql.DB, folderDriveID string) ([]archiveTarget, error) {
	var chain []archiveTarget
	seen := make(map[string]bool)
	cur := folderDriveID
	for {
		if seen[cur] {
			return nil, fmt.Errorf("cycle detected in parent chain at %s", cur)
		}
		seen[cur] = true
		var (
			t      archiveTarget
			parent sql.NullString
		)
		err := db.QueryRow(`
			SELECT n.id, n.drive_id, n.name, n.archive_folder_drive_id, p.drive_id
			FROM nodes n LEFT JOIN nodes p ON p.id = n.parent_id
			WHERE n.drive_id = ?`, cur).
			Scan(&t.rowID, &t.driveID, &t.name, &t.archiveFolder, &parent)
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("folder %s not found in database while walking ancestors of %s", cur, folderDriveID)
		}
		if err != nil {
			return nil, err
		}
		if !parent.Valid {
			// cur is a root; roots have no replica of their own.
			break
		}
		chain = append(chain, t)
		cur = parent.String
	}
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	return chain, nil
}

// nodeInSubtree reports whether the node with driveID lies within the subtree
// rooted at rootDriveID (inclusive), walking parent_id upward. False when
// either node is missing or rootDriveID is empty.
func nodeInSubtree(db *sql.DB, rootDriveID, driveID string) (bool, error) {
	if rootDriveID == "" || driveID == "" {
		return false, nil
	}
	seen := make(map[string]bool)
	cur := driveID
	for {
		if cur == rootDriveID {
			return true, nil
		}
		if seen[cur] {
			return false, nil // cycle: corrupt chain, but definitely not under the root
		}
		seen[cur] = true
		var parent sql.NullString
		err := db.QueryRow(`
			SELECT p.drive_id FROM nodes n LEFT JOIN nodes p ON p.id = n.parent_id
			WHERE n.drive_id = ?`, cur).Scan(&parent)
		if err == sql.ErrNoRows {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if !parent.Valid {
			return false, nil
		}
		cur = parent.String
	}
}

// markArchived records that the item was moved into the archive: the original
// parent's Drive ID is stamped (first value wins, so a re-archive of an
// already-archived item cannot overwrite the true origin) and the row is
// reparented under the replica folder's row.
func markArchived(db *sql.DB, driveID, originalParentDriveID string, replicaRowID int64) error {
	_, err := db.Exec(`
		UPDATE nodes SET
			original_parent_drive_id = COALESCE(original_parent_drive_id, ?),
			parent_id = ?
		WHERE drive_id = ?`, originalParentDriveID, replicaRowID, driveID)
	return err
}

// clearArchived undoes markArchived after a restore: the archived marker and
// delete_skipped flag reset, and parent_id re-points at the original parent's
// row when it still exists (NULL otherwise; the next crawl re-observes the
// node in place and fixes it).
func clearArchived(db *sql.DB, driveID, originalParentDriveID string) error {
	_, err := db.Exec(`
		UPDATE nodes SET
			original_parent_drive_id = NULL,
			delete_skipped = 0,
			parent_id = (SELECT id FROM nodes WHERE drive_id = ?)
		WHERE drive_id = ?`, originalParentDriveID, driveID)
	return err
}

// setArchiveFolder caches a folder's replica Drive ID (see the
// archive_folder_drive_id migration comment).
func setArchiveFolder(db *sql.DB, folderDriveID, replicaDriveID string) error {
	_, err := db.Exec(`UPDATE nodes SET archive_folder_drive_id = ? WHERE drive_id = ?`,
		replicaDriveID, folderDriveID)
	return err
}

// clearArchiveFolder drops a folder's replica cache after the replica is pruned.
func clearArchiveFolder(db *sql.DB, folderDriveID string) error {
	_, err := db.Exec(`UPDATE nodes SET archive_folder_drive_id = NULL WHERE drive_id = ?`, folderDriveID)
	return err
}

// markDeleteSkipped records that the delete command skipped this archived item
// because it is externally owned.
func markDeleteSkipped(db *sql.DB, driveID string) error {
	_, err := db.Exec(`UPDATE nodes SET delete_skipped = 1 WHERE drive_id = ?`, driveID)
	return err
}

// deleteNodeRow removes a really-deleted item's row and sweeps the auxiliary
// rows that referenced it. Any children still recorded under it (stale rows
// for items that vanished from Drive outside our control) are detached to
// roots rather than blocking on the foreign key.
func deleteNodeRow(db *sql.DB, driveID string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`
		UPDATE nodes SET parent_id = NULL
		WHERE parent_id = (SELECT id FROM nodes WHERE drive_id = ?)`, driveID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM nodes WHERE drive_id = ?`, driveID); err != nil {
		return err
	}
	if err := pruneOrphanedAuxRows(tx); err != nil {
		return err
	}
	return tx.Commit()
}
