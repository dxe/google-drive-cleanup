package main

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := openDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func mustUpsert(t *testing.T, db *sql.DB, n node) (rowID int64, existed bool, prevParent sql.NullInt64, prevDone bool) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	rowID, existed, prevParent, prevDone, err = upsertNode(tx, n)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return rowID, existed, prevParent, prevDone
}

func TestClassify(t *testing.T) {
	cases := map[string]string{
		"application/vnd.google-apps.folder":      "folder",
		"application/vnd.google-apps.shortcut":    "shortcut",
		"application/vnd.google-apps.document":    "google_doc",
		"application/vnd.google-apps.spreadsheet": "google_doc",
		"application/pdf":                         "binary",
		"image/png":                               "binary",
	}
	for mime, want := range cases {
		if got := classify(mime); got != want {
			t.Errorf("classify(%q) = %q, want %q", mime, got, want)
		}
	}
}

func TestUpsertIdempotentAndProgressPreserving(t *testing.T) {
	db := testDB(t)

	rootID, existed, _, _ := mustUpsert(t, db, node{
		driveID: "root", name: "Root", typ: typeFolder, mimeType: folderMimeType,
	})
	if existed {
		t.Fatal("fresh insert reported existed=true")
	}

	childN := node{
		driveID: "child", name: "Child", typ: typeBinary, mimeType: "application/pdf",
		ownerEmail: nullString("a@example.com"),
		parentID:   sql.NullInt64{Int64: rootID, Valid: true},
	}
	childID, _, _, _ := mustUpsert(t, db, childN)

	// Mark root done, then re-upsert both: progress and parent must survive.
	tx, _ := db.Begin()
	if err := markChildrenDone(tx, rootID); err != nil {
		t.Fatal(err)
	}
	tx.Commit()

	mustUpsert(t, db, node{driveID: "root", name: "Root renamed", typ: typeFolder, mimeType: folderMimeType})

	otherID, _, _, _ := mustUpsert(t, db, node{
		driveID: "other", name: "Other", typ: typeFolder, mimeType: folderMimeType,
	})
	reN := childN
	reN.name = "Child renamed"
	reN.ownerEmail = nullString("b@example.com")
	reN.parentID = sql.NullInt64{Int64: otherID, Valid: true} // rediscovered under a different parent
	reID, existed, prevParent, _ := mustUpsert(t, db, reN)

	if reID != childID {
		t.Errorf("re-upsert returned row %d, want %d", reID, childID)
	}
	if !existed || !prevParent.Valid || prevParent.Int64 != rootID {
		t.Errorf("re-upsert: existed=%v prevParent=%+v, want existed under root row %d", existed, prevParent, rootID)
	}

	var name, email string
	var parent int64
	var done int
	if err := db.QueryRow(`SELECT name, owner_email, parent_id FROM nodes WHERE drive_id='child'`).
		Scan(&name, &email, &parent); err != nil {
		t.Fatal(err)
	}
	if name != "Child renamed" || email != "b@example.com" {
		t.Errorf("metadata not refreshed: name=%q email=%q", name, email)
	}
	if parent != rootID {
		t.Errorf("parent_id was reparented to %d, want first-discovered %d", parent, rootID)
	}
	if err := db.QueryRow(`SELECT children_done FROM nodes WHERE drive_id='root'`).Scan(&done); err != nil {
		t.Fatal(err)
	}
	if done != 1 {
		t.Error("re-upsert regressed children_done on root")
	}
}

func TestPendingFolders(t *testing.T) {
	db := testDB(t)
	rootID, _, _, _ := mustUpsert(t, db, node{driveID: "root", name: "Root", typ: typeFolder, mimeType: folderMimeType})
	mustUpsert(t, db, node{driveID: "sub", name: "Sub", typ: typeFolder, mimeType: folderMimeType,
		parentID: sql.NullInt64{Int64: rootID, Valid: true}})
	mustUpsert(t, db, node{driveID: "f", name: "f.pdf", typ: typeBinary, mimeType: "application/pdf",
		parentID: sql.NullInt64{Int64: rootID, Valid: true}})

	pending, err := pendingFolders(db)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 {
		t.Fatalf("pending = %d folders, want 2 (root, sub)", len(pending))
	}

	tx, _ := db.Begin()
	markChildrenDone(tx, rootID)
	tx.Commit()
	if n, _ := countPendingFolders(db); n != 1 {
		t.Errorf("after marking root done, %d pending, want 1", n)
	}
	if err := resetChildrenDone(db); err != nil {
		t.Fatal(err)
	}
	if n, _ := countPendingFolders(db); n != 2 {
		t.Errorf("after refresh, %d pending, want 2", n)
	}
}

func TestOwnersReport(t *testing.T) {
	db := testDB(t)
	rootID, _, _, _ := mustUpsert(t, db, node{driveID: "root", name: "Root", typ: typeFolder, mimeType: folderMimeType,
		ownerEmail: nullString("owner@example.com")}) // a folder-only owner
	parent := sql.NullInt64{Int64: rootID, Valid: true}

	for i, n := range []node{
		{ownerEmail: nullString("alice@example.com"), ownerID: nullString("111"), ownerDisplay: nullString("Alice")},
		{ownerEmail: nullString("alice@example.com"), ownerID: nullString("111"), ownerDisplay: nullString("Alice")},
		{ownerID: nullString("222"), ownerDisplay: nullString("Bob")}, // email missing -> id bucket
		{}, // both missing -> unknown bucket
	} {
		n.driveID = string(rune('a' + i))
		n.name = n.driveID
		n.typ = typeBinary
		n.mimeType = "application/pdf"
		n.parentID = parent
		mustUpsert(t, db, n)
	}

	counts, err := ownersReport(db, "")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, oc := range counts {
		got = append(got, ownerLabel(oc))
	}
	want := []string{"alice@example.com (Alice)", "id:222 (Bob)", "(unknown)", "owner@example.com"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("owners = %v, want %v", got, want)
	}
	if counts[0].fileCount != 2 || counts[1].fileCount != 1 || counts[2].fileCount != 1 || counts[3].fileCount != 0 {
		t.Errorf("file counts = %d,%d,%d,%d, want 2,1,1,0",
			counts[0].fileCount, counts[1].fileCount, counts[2].fileCount, counts[3].fileCount)
	}
	if counts[3].folderCount != 1 || counts[3].total != 1 {
		t.Errorf("owner@example.com = %d folders, %d total, want 1,1", counts[3].folderCount, counts[3].total)
	}
}

func TestOwnersReportScopedToParent(t *testing.T) {
	db := testDB(t)
	// Root (bob) ─┬─ Finance (alice) ── budget.xlsx (alice), notes.txt (bob)
	//             └─ Shared  (bob)   ── plan.doc    (alice)
	buildExploreTree(t, db)

	// Scope to Finance: only Finance itself (folder, alice) plus its two files
	// (budget→alice, notes→bob) count. Shared and plan.doc are excluded.
	counts, err := ownersReport(db, "fin")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]ownerCount{}
	for _, oc := range counts {
		got[oc.email.String] = oc
	}
	if len(counts) != 2 {
		t.Fatalf("got %d owners under Finance, want 2 (alice, bob): %v", len(counts), counts)
	}
	if a := got["alice@example.com"]; a.folderCount != 1 || a.fileCount != 1 {
		t.Errorf("alice under Finance = %d folders, %d files, want 1, 1", a.folderCount, a.fileCount)
	}
	if b := got["bob@example.com"]; b.folderCount != 0 || b.fileCount != 1 {
		t.Errorf("bob under Finance = %d folders, %d files, want 0, 1", b.folderCount, b.fileCount)
	}
}

func TestReplacePermissionsSnapshots(t *testing.T) {
	db := testDB(t)
	mustUpsert(t, db, node{driveID: "folder", name: "Folder", typ: typeFolder, mimeType: folderMimeType})

	write := func(perms []permission) {
		t.Helper()
		tx, _ := db.Begin()
		if err := replacePermissions(tx, "folder", perms); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
	}

	write([]permission{
		{permissionID: "p1", typ: "user", role: "writer", emailAddress: nullString("a@example.com")},
		{permissionID: "p2", typ: "anyone", role: "reader", allowFileDiscovery: sql.NullBool{Bool: true, Valid: true}},
	})

	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM folder_permissions WHERE node_drive_id='folder'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("after first write: %d permissions, want 2", n)
	}

	// A re-crawl with a smaller set must replace, not accumulate.
	write([]permission{{permissionID: "p1", typ: "user", role: "reader", emailAddress: nullString("a@example.com")}})

	var role string
	if err := db.QueryRow(`SELECT role FROM folder_permissions WHERE node_drive_id='folder' AND permission_id='p1'`).Scan(&role); err != nil {
		t.Fatal(err)
	}
	if role != "reader" {
		t.Errorf("role = %q, want reader (refreshed)", role)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM folder_permissions WHERE node_drive_id='folder'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("after second write: %d permissions, want 1 (stale p2 dropped)", n)
	}
}

func TestNodesLackingEditAccess(t *testing.T) {
	db := testDB(t)
	rootID, _, _, _ := mustUpsert(t, db, node{driveID: "root", name: "Root", typ: typeFolder, mimeType: folderMimeType,
		canEdit: true})
	parent := sql.NullInt64{Int64: rootID, Valid: true}

	// Editable file — excluded.
	mustUpsert(t, db, node{driveID: "ok", name: "ok.pdf", typ: typeBinary, mimeType: "application/pdf",
		parentID: parent, canEdit: true})
	// Not editable — reported, with owner label and full path.
	mustUpsert(t, db, node{driveID: "locked", name: "locked.pdf", typ: typeBinary, mimeType: "application/pdf",
		parentID: parent, ownerEmail: nullString("ext@other.com"), canEdit: false})
	// Not editable folder — reported, and sorts before the file.
	mustUpsert(t, db, node{driveID: "lockedfolder", name: "Shared", typ: typeFolder, mimeType: folderMimeType,
		parentID: parent, canEdit: false})

	rows, err := nodesLackingEditAccess(db)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2 (locked folder + locked file)", len(rows))
	}
	if rows[0].typ != typeFolder || rows[0].path != "Root / Shared" {
		t.Errorf("row[0] = %+v, want folder at Root / Shared", rows[0])
	}
	if rows[1].driveID != "locked" || rows[1].path != "Root / locked.pdf" {
		t.Errorf("row[1] path = %q, want Root / locked.pdf", rows[1].path)
	}
	if rows[1].owner != "ext@other.com" {
		t.Errorf("row[1] owner = %q, want ext@other.com", rows[1].owner)
	}
}

// buildPackTree builds the migration fixture used by the pack query tests:
//
//	Root (bob) ─┬─ A (alice) ─┬─ a1.pdf (alice)        nested owned: rides along
//	            │             ├─ B (alice) ── b1 (bob) owned folder + unowned child
//	            │             ├─ E (carol) ── e1 (alice, via owner_id)
//	            │             └─ x1.pdf (no owner)     unowned child of owned
//	            ├─ loose.pdf (alice)
//	            └─ C (bob) ── c1.pdf (alice)
func buildPackTree(t *testing.T, db *sql.DB) {
	t.Helper()
	alice := nullString("alice@example.com")
	rootID, _, _, _ := mustUpsert(t, db, node{driveID: "root", name: "Root", typ: typeFolder, mimeType: folderMimeType,
		ownerEmail: nullString("bob@example.com")})
	aID, _, _, _ := mustUpsert(t, db, node{driveID: "A", name: "A", typ: typeFolder, mimeType: folderMimeType,
		parentID: sql.NullInt64{Int64: rootID, Valid: true}, ownerEmail: alice})
	mustUpsert(t, db, node{driveID: "a1", name: "a1.pdf", typ: typeBinary, mimeType: "application/pdf",
		parentID: sql.NullInt64{Int64: aID, Valid: true}, ownerEmail: alice})
	bID, _, _, _ := mustUpsert(t, db, node{driveID: "B", name: "B", typ: typeFolder, mimeType: folderMimeType,
		parentID: sql.NullInt64{Int64: aID, Valid: true}, ownerEmail: alice})
	mustUpsert(t, db, node{driveID: "b1", name: "b1.pdf", typ: typeBinary, mimeType: "application/pdf",
		parentID: sql.NullInt64{Int64: bID, Valid: true}, ownerEmail: nullString("bob@example.com")})
	eID, _, _, _ := mustUpsert(t, db, node{driveID: "E", name: "E", typ: typeFolder, mimeType: folderMimeType,
		parentID: sql.NullInt64{Int64: aID, Valid: true}, ownerEmail: nullString("carol@example.com")})
	// Owned via owner_id rather than email, inside an unowned folder.
	mustUpsert(t, db, node{driveID: "e1", name: "e1.pdf", typ: typeBinary, mimeType: "application/pdf",
		parentID: sql.NullInt64{Int64: eID, Valid: true}, ownerID: nullString("alice@example.com")})
	// No owner at all: must count as not-owned (IS NOT handles the NULLs).
	mustUpsert(t, db, node{driveID: "x1", name: "x1.pdf", typ: typeBinary, mimeType: "application/pdf",
		parentID: sql.NullInt64{Int64: aID, Valid: true}})
	mustUpsert(t, db, node{driveID: "loose", name: "loose.pdf", typ: typeBinary, mimeType: "application/pdf",
		parentID: sql.NullInt64{Int64: rootID, Valid: true}, ownerEmail: alice})
	cID, _, _, _ := mustUpsert(t, db, node{driveID: "C", name: "C", typ: typeFolder, mimeType: folderMimeType,
		parentID: sql.NullInt64{Int64: rootID, Valid: true}, ownerEmail: nullString("bob@example.com")})
	mustUpsert(t, db, node{driveID: "c1", name: "c1.pdf", typ: typeBinary, mimeType: "application/pdf",
		parentID: sql.NullInt64{Int64: cID, Valid: true}, ownerEmail: alice})
}

func TestNodesOwnedBy(t *testing.T) {
	db := testDB(t)
	buildPackTree(t, db)

	nodes, err := nodesOwnedBy(db, "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, n := range nodes {
		got = append(got, n.driveID)
	}
	if strings.Join(got, ",") != "A,a1,B,e1,loose,c1" {
		t.Errorf("nodesOwnedBy = %v, want [A a1 B e1 loose c1]", got)
	}
}

func TestOwnedRoots(t *testing.T) {
	db := testDB(t)
	buildPackTree(t, db)

	roots, err := ownedRoots(db, "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range roots {
		got[r.driveID] = r.parentDriveID
	}
	// A, loose, c1 hang off unowned parents; e1's parent E is carol's; a1 and B
	// live inside owned A and ride along.
	want := map[string]string{"A": "root", "e1": "E", "loose": "root", "c1": "C"}
	if len(got) != len(want) {
		t.Fatalf("ownedRoots = %v, want %v", got, want)
	}
	for id, parent := range want {
		if got[id] != parent {
			t.Errorf("ownedRoots[%s] parent = %q, want %q", id, got[id], parent)
		}
	}
}

func TestOwnedRootsExcludesCrawlRoot(t *testing.T) {
	db := testDB(t)
	// The crawl root itself owned by the migrating user must never be a root.
	rootID, _, _, _ := mustUpsert(t, db, node{driveID: "root", name: "Root", typ: typeFolder, mimeType: folderMimeType,
		ownerEmail: nullString("alice@example.com")})
	mustUpsert(t, db, node{driveID: "f", name: "f.pdf", typ: typeBinary, mimeType: "application/pdf",
		parentID: sql.NullInt64{Int64: rootID, Valid: true}, ownerEmail: nullString("alice@example.com")})

	roots, err := ownedRoots(db, "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if len(roots) != 0 {
		t.Errorf("ownedRoots = %v, want none (root has no parent, f's parent is owned)", roots)
	}
}

func TestUnownedChildrenOfOwned(t *testing.T) {
	db := testDB(t)
	buildPackTree(t, db)

	items, err := unownedChildrenOfOwned(db, "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, s := range items {
		got[s.driveID] = s.parentDriveID
	}
	// b1 (bob's, in alice's B), E (carol's, in alice's A), and ownerless x1.
	want := map[string]string{"b1": "B", "E": "A", "x1": "A"}
	if len(got) != len(want) {
		t.Fatalf("unownedChildrenOfOwned = %v, want %v", got, want)
	}
	for id, parent := range want {
		if got[id] != parent {
			t.Errorf("unownedChildrenOfOwned[%s] parent = %q, want %q", id, got[id], parent)
		}
	}
}

func TestUserMigrationLifecycle(t *testing.T) {
	db := testDB(t)

	if m, err := getUserMigration(db, "alice@example.com"); err != nil || m != nil {
		t.Fatalf("before pack: migration = %v, err = %v; want nil, nil", m, err)
	}

	if err := upsertUserMigration(db, "alice@example.com", "uf", "cont", "stash"); err != nil {
		t.Fatal(err)
	}
	if err := markPacked(db, "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	m, err := getUserMigration(db, "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if m.containerID != "cont" || m.stashID != "stash" || m.userFolderID != "uf" {
		t.Errorf("migration ids = %+v", m)
	}
	if !m.packedAt.Valid || m.unpackedAt.Valid {
		t.Errorf("after markPacked: packedAt=%v unpackedAt=%v, want set/unset", m.packedAt, m.unpackedAt)
	}

	if err := markUnpacked(db, "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	if m, _ = getUserMigration(db, "alice@example.com"); !m.unpackedAt.Valid {
		t.Error("after markUnpacked: unpackedAt not set")
	}

	// Re-running pack re-records the scaffolding and restarts the cycle.
	if err := upsertUserMigration(db, "alice@example.com", "uf2", "cont2", "stash2"); err != nil {
		t.Fatal(err)
	}
	m, _ = getUserMigration(db, "alice@example.com")
	if m.containerID != "cont2" || m.packedAt.Valid || m.unpackedAt.Valid {
		t.Errorf("after re-upsert: %+v, want new ids and cleared timestamps", m)
	}
}

func TestPackOrphans(t *testing.T) {
	db := testDB(t)

	if _, err := packOrphanParent(db, "alice@example.com", "x"); err != sql.ErrNoRows {
		t.Errorf("missing orphan: err = %v, want sql.ErrNoRows", err)
	}
	if err := recordPackOrphan(db, "alice@example.com", "x", "parent1"); err != nil {
		t.Fatal(err)
	}
	// Re-sweeping the same item from a different live parent must overwrite.
	if err := recordPackOrphan(db, "alice@example.com", "x", "parent2"); err != nil {
		t.Fatal(err)
	}
	parent, err := packOrphanParent(db, "alice@example.com", "x")
	if err != nil {
		t.Fatal(err)
	}
	if parent != "parent2" {
		t.Errorf("orphan parent = %q, want parent2", parent)
	}
}

func TestFolderPermissionsFor(t *testing.T) {
	db := testDB(t)
	mustUpsert(t, db, node{driveID: "folder", name: "Folder", typ: typeFolder, mimeType: folderMimeType})

	tx, _ := db.Begin()
	if err := replacePermissions(tx, "folder", []permission{
		{permissionID: "p1", typ: "user", role: "writer", emailAddress: nullString("a@example.com")},
		{permissionID: "p2", typ: "anyone", role: "reader", allowFileDiscovery: sql.NullBool{Bool: false, Valid: true}},
		{permissionID: "p3", typ: "user", role: "owner", emailAddress: nullString("owner@gmail.com"), deleted: true},
	}); err != nil {
		t.Fatal(err)
	}
	tx.Commit()

	perms, err := folderPermissionsFor(db, "folder")
	if err != nil {
		t.Fatal(err)
	}
	if len(perms) != 3 {
		t.Fatalf("got %d permissions, want 3", len(perms))
	}
	byID := map[string]permission{}
	for _, p := range perms {
		byID[p.permissionID] = p
	}
	if p := byID["p1"]; p.typ != "user" || p.role != "writer" || p.emailAddress.String != "a@example.com" {
		t.Errorf("p1 = %+v", p)
	}
	if p := byID["p2"]; !p.allowFileDiscovery.Valid || p.allowFileDiscovery.Bool {
		t.Errorf("p2 allowFileDiscovery = %+v, want valid false", p.allowFileDiscovery)
	}
	if p := byID["p3"]; !p.deleted {
		t.Errorf("p3 deleted = %v, want true", p.deleted)
	}
}

func TestNodePath(t *testing.T) {
	db := testDB(t)
	rootID, _, _, _ := mustUpsert(t, db, node{driveID: "root", name: "DxE General", typ: typeFolder, mimeType: folderMimeType})
	subID, _, _, _ := mustUpsert(t, db, node{driveID: "sub", name: "Finance", typ: typeFolder, mimeType: folderMimeType,
		parentID: sql.NullInt64{Int64: rootID, Valid: true}})
	mustUpsert(t, db, node{driveID: "doc", name: "budget.xlsx", typ: typeBinary, mimeType: "application/x",
		parentID: sql.NullInt64{Int64: subID, Valid: true}})

	segments, err := nodePath(db, "doc")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(segments, " / "), "DxE General / Finance / budget.xlsx"; got != want {
		t.Errorf("path = %q, want %q", got, want)
	}
	if _, err := nodePath(db, "missing"); err == nil {
		t.Error("expected error for unknown drive id")
	}
}
